package app

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/davis7dotsh/hermes-box/internal/backup"
	"github.com/davis7dotsh/hermes-box/internal/box"
	"github.com/davis7dotsh/hermes-box/internal/component"
	"github.com/davis7dotsh/hermes-box/internal/config"
	"github.com/davis7dotsh/hermes-box/internal/guestupdate"
	"github.com/davis7dotsh/hermes-box/internal/keychain"
	"github.com/davis7dotsh/hermes-box/internal/lima"
	"github.com/davis7dotsh/hermes-box/internal/process"
)

const hostCleanupTimeout = 2 * time.Minute
const maximumGuestResponseBytes = 4 << 20
const maximumGuestExecResponseBytes = 64 << 20

type boundedProtocolBuffer struct {
	buffer   bytes.Buffer
	exceeded bool
	maximum  int
}

func (buffer *boundedProtocolBuffer) Write(value []byte) (int, error) {
	maximum := buffer.maximum
	if maximum == 0 {
		maximum = maximumGuestResponseBytes
	}
	remaining := maximum - buffer.buffer.Len()
	if remaining <= 0 {
		buffer.exceeded = true
		return 0, fmt.Errorf("guest response exceeds %d-byte protocol limit", maximum)
	}
	if len(value) > remaining {
		written, _ := buffer.buffer.Write(value[:remaining])
		buffer.exceeded = true
		return written, fmt.Errorf("guest response exceeds %d-byte protocol limit", maximum)
	}
	return buffer.buffer.Write(value)
}

func decodeGuestResponse(data []byte) (guestupdate.Response, error) {
	return decodeGuestResponseLimit(data, maximumGuestResponseBytes)
}

func decodeGuestResponseLimit(data []byte, maximum int) (guestupdate.Response, error) {
	if len(data) == 0 {
		return guestupdate.Response{}, errors.New("guest response is empty")
	}
	if len(data) > maximum {
		return guestupdate.Response{}, fmt.Errorf("guest response exceeds %d-byte protocol limit", maximum)
	}
	var response guestupdate.Response
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return guestupdate.Response{}, fmt.Errorf("decode guest response: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return guestupdate.Response{}, errors.New("guest response contains multiple JSON values")
		}
		return guestupdate.Response{}, fmt.Errorf("decode guest response trailer: %w", err)
	}
	if err := response.Validate(); err != nil {
		return guestupdate.Response{}, fmt.Errorf("validate guest response: %w", err)
	}
	return response, nil
}

func decodeGuestResult(value any, target any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("guest result contains trailing JSON")
	}
	return nil
}

type defaultOperations struct {
	runner           process.Runner
	limaRunner       lima.Runner
	stdout           io.Writer
	stderr           io.Writer
	keys             keychain.Store
	releaseDiscovery func(context.Context, Definition) []any
}

func (o *defaultOperations) client(def Definition) (*lima.Client, error) {
	runner := o.limaRunner
	if runner == nil {
		runner = lima.OSRunner{}
	}
	return lima.New(filepath.Join(def.Home, "lima"), runner)
}

func (o *defaultOperations) store(def Definition) (*box.Store, error) {
	return box.NewStore(def.Home)
}

func definitionDigest(definition []byte) string {
	digest := sha256.Sum256(definition)
	return hex.EncodeToString(digest[:])
}

func ownershipMarker() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate data ownership marker: %w", err)
	}
	return hex.EncodeToString(value), nil
}

func sizeBytes(value string) (int64, error) {
	match := regexp.MustCompile(`^([1-9][0-9]*)(MiB|GiB|TiB)$`).FindStringSubmatch(value)
	if match == nil {
		return 0, fmt.Errorf("invalid disk size %q", value)
	}
	amount, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil {
		return 0, err
	}
	multiplier := int64(1 << 20)
	switch match[2] {
	case "GiB":
		multiplier = 1 << 30
	case "TiB":
		multiplier = 1 << 40
	}
	if amount > (1<<63-1)/multiplier {
		return 0, errors.New("disk size overflows int64")
	}
	return amount * multiplier, nil
}

func (o *defaultOperations) Preflight(ctx context.Context, def Definition, action string) error {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return errors.New("Hermes Box requires macOS on ARM64")
	}
	client, err := o.client(def)
	if err != nil {
		return err
	}
	if _, err := client.Version(ctx); err != nil {
		return err
	}
	if (action == "create" || action == "restore") && !config.PortAvailable(def.Bundle.Config.Ports.Executor) {
		return fmt.Errorf("host loopback port %d is already in use", def.Bundle.Config.Ports.Executor)
	}
	if action == "start" {
		state, err := o.Ownership(ctx, def)
		if err != nil {
			return err
		}
		if !state.Running && !config.PortAvailable(def.Bundle.Config.Ports.Executor) {
			return fmt.Errorf("host loopback port %d is already in use", def.Bundle.Config.Ports.Executor)
		}
	}
	if action == "create" || action == "rebuild" {
		if err := materializeLockClosure(ctx, def); err != nil {
			return fmt.Errorf("materialize reviewed lock closure: %w", err)
		}
	}
	return nil
}

func (o *defaultOperations) ResumeInterruptedMutation(ctx context.Context, def Definition) error {
	store, err := o.store(def)
	if err != nil {
		return err
	}
	journal, found, err := store.LoadJournal(def.Name)
	if err != nil || !found {
		return err
	}
	if journal.Operation == "create" {
		if _, metadataFound, metadataErr := store.LoadMetadata(def.Name); metadataErr != nil {
			return metadataErr
		} else if !metadataFound {
			return o.CleanupCreate(ctx, def)
		}
	}
	if _, err := store.VerifyOwnership(def.Name, def.ConfigDir); err != nil {
		return fmt.Errorf("refuse recovery of unowned %s journal: %w", journal.Operation, err)
	}
	switch journal.Operation {
	case "create":
		ownership, ownershipErr := o.Ownership(ctx, def)
		if ownershipErr == nil && ownership.Owned && ownership.Running {
			latest, backupErr := (&defaultBackups{operations: o}).LatestVerified(ctx, def)
			if backupErr == nil && latest != nil {
				info, statErr := os.Stat(latest.Archive)
				health, healthErr := o.Health(ctx, def)
				if statErr == nil && !info.ModTime().Before(journal.StartedAt) && healthErr == nil && health.Healthy {
					return o.CompleteCreate(ctx, def)
				}
			}
		}
		return o.CleanupCreate(ctx, def)
	case "rebuild":
		if err := o.verifyRebuildJournal(ctx, def, journal); err != nil {
			return err
		}
		ownership, err := o.Ownership(ctx, def)
		if err != nil {
			return err
		}
		if journal.Phase == "prepared" && ownership.Exists && ownership.Owned {
			client, err := o.client(def)
			if err != nil {
				return err
			}
			_, vmFound, err := client.InspectInstance(ctx, def.Name)
			if err != nil {
				return err
			}
			if vmFound {
				return o.CompleteRebuild(ctx, def)
			}
		}
		artifacts := make([]string, 0, len(journal.Recovery.Artifacts))
		for _, artifact := range journal.Recovery.Artifacts {
			artifacts = append(artifacts, artifact.Path)
		}
		recovery := RecoveryState{AppliedLock: journal.Recovery.AppliedLock, Artifacts: artifacts}
		snapshot := BackupResult{
			Archive: journal.Recovery.BackupArchive, Envelope: journal.Recovery.BackupEnvelope,
			ArchiveSHA256: journal.Recovery.BackupSHA256,
		}
		if err := o.RecoverRebuild(ctx, def, recovery, snapshot); err != nil {
			return fmt.Errorf("recover interrupted rebuild: %w", err)
		}
		return o.CompleteRebuild(ctx, def)
	case "rollback":
		if _, err := o.completeHostRollback(ctx, def, journal); err != nil {
			return fmt.Errorf("recover interrupted rollback: %w", err)
		}
		return nil
	case "update":
		if err := o.completeHostUpdateRecovery(ctx, def, journal); err != nil {
			return fmt.Errorf("recover interrupted update: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("unsupported interrupted operation %q", journal.Operation)
	}
}

func (o *defaultOperations) Ownership(ctx context.Context, def Definition) (Ownership, error) {
	client, err := o.client(def)
	if err != nil {
		return Ownership{}, err
	}
	store, err := o.store(def)
	if err != nil {
		return Ownership{}, err
	}
	names := box.ResourceNames(def.Name)
	instance, vmExists, err := client.InspectInstance(ctx, names.VM)
	if err != nil {
		return Ownership{}, err
	}
	disk, diskExists, err := client.InspectDisk(ctx, names.DataDisk)
	if err != nil {
		return Ownership{}, err
	}
	metadata, metadataFound, metadataErr := store.LoadMetadata(def.Name)
	if metadataErr != nil {
		return Ownership{}, metadataErr
	}
	exists := vmExists || diskExists || metadataFound
	if !metadataFound {
		return Ownership{Exists: exists}, nil
	}
	if _, err := store.VerifyOwnership(def.Name, def.ConfigDir); err != nil {
		if !vmExists && !diskExists {
			return Ownership{}, err
		}
		return Ownership{Exists: true}, nil
	}
	if err := o.verifyOwnedResources(ctx, def, metadata, instance, vmExists, disk, diskExists, false); err != nil {
		return Ownership{}, err
	}
	return Ownership{Exists: exists, Owned: true, Running: vmExists && strings.EqualFold(instance.Status, "running")}, nil
}

func (o *defaultOperations) verifyOwnedResources(ctx context.Context, def Definition, metadata box.Metadata, instance lima.Instance, vmExists bool, disk lima.Disk, diskExists, allowUnboundDataDisk bool) error {
	if metadata.VM != def.Name || metadata.DataDisk != box.ResourceNames(def.Name).DataDisk {
		return errors.New("owned resource names do not match metadata")
	}
	client, err := o.client(def)
	if err != nil {
		return err
	}
	definitionPath, err := client.DefinitionPath(metadata.VM)
	if err != nil {
		return err
	}
	definition, err := os.ReadFile(definitionPath)
	if err != nil {
		return fmt.Errorf("read owned Lima definition: %w", err)
	}
	if definitionDigest(definition) != metadata.DefinitionSHA256 {
		return errors.New("owned Lima definition does not match its recorded SHA-256")
	}
	if vmExists && (instance.Arch != metadata.Arch || instance.VMType != metadata.VMType) {
		return fmt.Errorf("same-name VM has unexpected platform %s/%s", instance.VMType, instance.Arch)
	}
	if vmExists && !diskExists {
		return errors.New("same-name VM is missing its bound data disk")
	}
	if diskExists {
		if disk.Size != metadata.DataDiskSize || (disk.Format != "" && disk.Format != metadata.DataDiskFormat) {
			return fmt.Errorf("same-name data disk has unexpected size or format")
		}
		if metadata.DataDiskDir == "" && !allowUnboundDataDisk || metadata.DataDiskDir != "" && filepath.Clean(disk.Dir) != metadata.DataDiskDir {
			return errors.New("same-name data disk does not match its recorded Lima directory")
		}
		if disk.InUseBy != "" && disk.InUseBy != metadata.VM {
			return fmt.Errorf("same-name data disk is associated with %q, not %q", disk.InUseBy, metadata.VM)
		}
		if vmExists && disk.InUseBy != metadata.VM {
			return errors.New("same-name VM is not associated with its bound data disk")
		}
	}
	return nil
}

func (o *defaultOperations) verifyGuestOwnershipMarker(ctx context.Context, def Definition, expected string) error {
	marker, err := o.boundedGuestOutput(ctx, def, "guest and data ownership markers", "sudo", "/bin/sh", "-ceu", `
root=$(cat /etc/hermes-box-owner)
data=$(cat /data/.hermes-box-owner)
[ "$root" = "$data" ]
printf '%s' "$root"
`)
	if err != nil {
		return err
	}
	if marker != expected {
		return errors.New("running guest ownership marker does not match metadata")
	}
	return nil
}

func (o *defaultOperations) CreateInfrastructure(ctx context.Context, def Definition) error {
	client, err := o.client(def)
	if err != nil {
		return err
	}
	store, err := o.store(def)
	if err != nil {
		return err
	}
	image, err := materializeOne(ctx, def.Home, "ubuntu-image", def.Bundle.Lock.Ubuntu.Image, def.Bundle.Lock.Ubuntu.SHA256)
	if err != nil {
		return err
	}
	definition, err := lima.GenerateYAMLWithImage(def.Bundle.Config, def.Bundle.Lock, image)
	if err != nil {
		return err
	}
	if _, err := client.SaveDefinition(def.Name, definition); err != nil {
		return err
	}
	diskSize, err := sizeBytes(def.Bundle.Config.VM.DataDisk)
	if err != nil {
		return err
	}
	marker, err := ownershipMarker()
	if err != nil {
		return err
	}
	metadata, err := box.NewMetadata(def.Name, def.ConfigDir, box.OwnershipBinding{
		DefinitionSHA256: definitionDigest(definition), DataDiskSize: diskSize, DataOwnershipMarker: marker,
	}, time.Now())
	if err != nil {
		return err
	}
	journal := box.Journal{Schema: box.JournalSchema, Operation: "create", Phase: "incomplete", StartedAt: time.Now().UTC()}
	if err := store.SaveJournal(def.Name, journal); err != nil {
		return err
	}
	if err := store.CreateMetadata(metadata); err != nil {
		return err
	}
	// Record exact intent before asking Lima to create the disk. limactl may
	// create the resource and then fail or the host may crash before returning;
	// cleanup must still know that this invocation owns the partial resource.
	journal.Resources = []string{metadata.DataDisk}
	if err := store.SaveJournal(def.Name, journal); err != nil {
		return err
	}
	if err := client.CreateDisk(ctx, metadata.DataDisk, def.Bundle.Config.VM.DataDisk); err != nil {
		return err
	}
	disk, found, err := client.InspectDisk(ctx, metadata.DataDisk)
	if err != nil || !found {
		return errors.Join(errors.New("new data disk is absent after creation"), err)
	}
	if disk.Size != metadata.DataDiskSize || disk.Format != metadata.DataDiskFormat || disk.Dir == "" {
		return errors.New("new data disk does not match its requested identity")
	}
	if err := store.BindDataDisk(def.Name, def.ConfigDir, disk.Dir); err != nil {
		return err
	}
	journal.Resources = []string{metadata.VM, metadata.DataDisk}
	if err := store.SaveJournal(def.Name, journal); err != nil {
		return err
	}
	if err := client.Create(ctx, metadata.VM, definition); err != nil {
		return err
	}
	if err := client.Start(ctx, metadata.VM); err != nil {
		return err
	}
	if err := o.guestShell(ctx, def, strings.NewReader(metadata.DataOwnershipMarker+"\n"), "sudo", "/bin/sh", "-ceu", `
umask 077
data_mount=$1
test -d "$data_mount"
IFS= read -r marker
[ -n "$marker" ]
printf '%s\n' "$marker" > "$data_mount/.hermes-box-owner"
printf '%s\n' "$marker" > /etc/hermes-box-owner
chmod 0600 "$data_mount/.hermes-box-owner"
chmod 0600 /etc/hermes-box-owner
sync -f "$data_mount/.hermes-box-owner" /etc/hermes-box-owner
`, "ownership-marker", "/mnt/lima-"+metadata.DataDisk); err != nil {
		return err
	}
	return o.installProvisioner(ctx, def)
}

func (o *defaultOperations) CompleteCreate(_ context.Context, def Definition) error {
	store, err := o.store(def)
	if err != nil {
		return err
	}
	metadata, err := store.VerifyOwnership(def.Name, def.ConfigDir)
	if err != nil {
		return err
	}
	if metadata.DataDiskDir == "" {
		return errors.New("cannot complete create without a bound data disk")
	}
	return store.ClearJournal(def.Name)
}

func (o *defaultOperations) installProvisioner(ctx context.Context, def Definition) error {
	// The reviewed provisioner is the sole source of guest helper, units, and
	// foundational packages. The host only verifies, uploads, and invokes it.
	artifact, err := materializeOne(ctx, def.Home, "provisioner", def.Bundle.Lock.Ubuntu.Provisioner, def.Bundle.Lock.Ubuntu.ProvisionerSHA256)
	if err != nil {
		return err
	}
	client, err := o.client(def)
	if err != nil {
		return err
	}
	target := def.Name + ":/tmp/hermes-box-provisioner.tar.zst"
	if err := client.Copy(ctx, false, []string{artifact}, target); err != nil {
		return err
	}
	script := fmt.Sprintf(`
stage=$(mktemp -d)
trap 'rm -rf "$stage" /tmp/hermes-box-provisioner.tar.zst' EXIT
tar --zstd -xf /tmp/hermes-box-provisioner.tar.zst -C "$stage"
test -x "$stage/bootstrap.sh"
PROVISIONER_DIR="$stage" "$stage/bootstrap.sh" "$stage" %q
`, "/mnt/lima-"+box.ResourceNames(def.Name).DataDisk)
	return o.guestShell(ctx, def, nil, "sudo", "/bin/sh", "-ceu", script)
}

func (o *defaultOperations) CleanupCreate(_ context.Context, def Definition) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), hostCleanupTimeout)
	defer cancel()
	client, clientErr := o.client(def)
	store, storeErr := o.store(def)
	if clientErr != nil || storeErr != nil {
		return errors.Join(clientErr, storeErr)
	}
	journal, journalFound, err := store.LoadJournal(def.Name)
	if err != nil {
		return err
	}
	if !journalFound || journal.Operation != "create" {
		return errors.New("refusing create cleanup without its durable create journal")
	}
	names := box.ResourceNames(def.Name)
	createdVM := contains(journal.Resources, names.VM)
	createdDisk := contains(journal.Resources, names.DataDisk)
	if !createdVM && !createdDisk {
		return cleanupCreateState(store, def)
	}
	_, metadataFound, err := store.LoadMetadata(def.Name)
	if err != nil {
		return err
	}
	if !metadataFound {
		// Metadata is deliberately removed before the journal. This is the only
		// safe crash window without ownership metadata: cleanup may finish only
		// after proving both exact names absent.
		_, vmExists, vmErr := client.InspectInstance(cleanupCtx, names.VM)
		_, diskExists, diskErr := client.InspectDisk(cleanupCtx, names.DataDisk)
		if vmErr != nil || diskErr != nil {
			return errors.Join(vmErr, diskErr)
		}
		if vmExists || diskExists {
			return errors.New("refusing metadata-free create cleanup while a same-name resource exists")
		}
		return cleanupCreateState(store, def)
	}
	metadata, err := store.VerifyOwnership(def.Name, def.ConfigDir)
	if err != nil {
		return err
	}
	instance, vmExists, err := client.InspectInstance(cleanupCtx, names.VM)
	if err != nil {
		return err
	}
	disk, diskExists, err := client.InspectDisk(cleanupCtx, names.DataDisk)
	if err != nil {
		return err
	}
	if !vmExists && !diskExists {
		return cleanupCreateState(store, def)
	}
	if vmExists && !createdVM || diskExists && !createdDisk {
		return errors.New("refusing incomplete-create cleanup because an unjournaled same-name resource exists")
	}
	// The create journal is written before each Lima call. If Lima created the
	// disk but the host crashed before recording its directory, exact name,
	// definition, size, format, platform, and journal intent are still enough
	// to remove only that invocation's partial resource.
	if err := o.verifyOwnedResources(cleanupCtx, def, metadata, instance, vmExists, disk, diskExists, true); err != nil {
		return fmt.Errorf("refusing incomplete-create cleanup without exact host ownership proof: %w", err)
	}
	if createdVM {
		_ = client.Stop(cleanupCtx, names.VM, true)
		_ = client.Delete(cleanupCtx, names.VM)
	}
	if createdDisk {
		_ = client.DeleteDisk(cleanupCtx, names.DataDisk)
	}
	if err := verifySelectedResourcesAbsent(cleanupCtx, client, names, createdVM, createdDisk); err != nil {
		return err
	}
	return cleanupCreateState(store, def)
}

func cleanupCreateState(store *box.Store, def Definition) error {
	appliedErr := os.Remove(hostAppliedLockPath(def))
	if errors.Is(appliedErr, os.ErrNotExist) {
		appliedErr = nil
	}
	if appliedErr != nil {
		return appliedErr
	}
	// Remove ownership metadata first. If the host crashes before clearing the
	// journal, the durable journal remains an exact, retryable cleanup intent.
	if err := store.RemoveMetadata(def.Name); err != nil {
		return err
	}
	return store.ClearJournal(def.Name)
}

func (o *defaultOperations) RecreateVM(ctx context.Context, def Definition) error {
	client, err := o.client(def)
	if err != nil {
		return err
	}
	store, err := o.store(def)
	if err != nil {
		return err
	}
	metadata, err := store.VerifyOwnership(def.Name, def.ConfigDir)
	if err != nil {
		return err
	}
	requestedDiskSize, err := sizeBytes(def.Bundle.Config.VM.DataDisk)
	if err != nil {
		return err
	}
	if requestedDiskSize != metadata.DataDiskSize {
		return errors.New("rebuild cannot resize the persistent data disk")
	}
	image, err := materializeOne(ctx, def.Home, "ubuntu-image", def.Bundle.Lock.Ubuntu.Image, def.Bundle.Lock.Ubuntu.SHA256)
	if err != nil {
		return err
	}
	definition, err := lima.GenerateYAMLWithImage(def.Bundle.Config, def.Bundle.Lock, image)
	if err != nil {
		return err
	}
	if _, err := client.SaveDefinition(metadata.VM, definition); err != nil {
		return err
	}
	if err := store.UpdateDefinitionSHA256(def.Name, def.ConfigDir, definitionDigest(definition)); err != nil {
		return err
	}
	if err := client.Create(ctx, metadata.VM, definition); err != nil {
		return err
	}
	if err := client.Start(ctx, metadata.VM); err != nil {
		return err
	}
	if err := o.guestShell(ctx, def, strings.NewReader(metadata.DataOwnershipMarker+"\n"), "sudo", "/bin/sh", "-ceu", `
umask 077
data_mount=$1
test -d "$data_mount"
IFS= read -r marker
[ -n "$marker" ]
printf '%s\n' "$marker" > "$data_mount/.hermes-box-owner"
printf '%s\n' "$marker" > /etc/hermes-box-owner
chmod 0600 "$data_mount/.hermes-box-owner"
chmod 0600 /etc/hermes-box-owner
sync -f "$data_mount/.hermes-box-owner" /etc/hermes-box-owner
`, "ownership-marker", "/mnt/lima-"+metadata.DataDisk); err != nil {
		return err
	}
	return o.installProvisioner(ctx, def)
}

func (o *defaultOperations) StartVM(ctx context.Context, def Definition) error {
	client, err := o.client(def)
	if err != nil {
		return err
	}
	state, err := o.Ownership(ctx, def)
	if err == nil && state.Running {
		return nil
	}
	return client.Start(ctx, def.Name)
}

func (o *defaultOperations) StopVM(ctx context.Context, def Definition) error {
	client, err := o.client(def)
	if err != nil {
		return err
	}
	return client.Stop(ctx, def.Name, false)
}

func (o *defaultOperations) RemoveVM(ctx context.Context, def Definition, preserveData bool) error {
	client, err := o.client(def)
	if err != nil {
		return err
	}
	state, err := o.Ownership(ctx, def)
	if err != nil {
		return err
	}
	if !state.Owned {
		return errors.New("refusing to remove an unowned VM")
	}
	names := box.ResourceNames(def.Name)
	_ = client.Delete(ctx, names.VM)
	if err := verifyResourcesAbsent(ctx, client, names, false); err != nil {
		return err
	}
	if !preserveData {
		_ = client.DeleteDisk(ctx, names.DataDisk)
		return verifyResourcesAbsent(ctx, client, names, true)
	}
	store, err := o.store(def)
	if err != nil {
		return err
	}
	journal, found, err := store.LoadJournal(def.Name)
	if err != nil {
		return err
	}
	if found && journal.Operation == "rebuild" {
		journal.Phase = "root-removed"
		if err := store.SaveJournal(def.Name, journal); err != nil {
			return err
		}
	}
	return nil
}

func (o *defaultOperations) RemoveAll(ctx context.Context, def Definition) error {
	client, err := o.client(def)
	if err != nil {
		return err
	}
	store, err := o.store(def)
	if err != nil {
		return err
	}
	state, err := o.Ownership(ctx, def)
	if err != nil {
		return err
	}
	if !state.Owned {
		return errors.New("refusing to destroy unowned resources")
	}
	names := box.ResourceNames(def.Name)
	_ = client.Stop(ctx, names.VM, true)
	_ = client.Delete(ctx, names.VM)
	_ = client.DeleteDisk(ctx, names.DataDisk)
	if err := verifyResourcesAbsent(ctx, client, names, true); err != nil {
		return err
	}
	appliedErr := os.Remove(hostAppliedLockPath(def))
	if errors.Is(appliedErr, os.ErrNotExist) {
		appliedErr = nil
	}
	if appliedErr != nil {
		return appliedErr
	}
	if err := store.ClearJournal(def.Name); err != nil {
		return err
	}
	return store.RemoveMetadata(def.Name)
}

func verifyResourcesAbsent(ctx context.Context, client *lima.Client, names box.Names, includeDisk bool) error {
	return verifySelectedResourcesAbsent(ctx, client, names, true, includeDisk)
}

func verifySelectedResourcesAbsent(ctx context.Context, client *lima.Client, names box.Names, includeVM, includeDisk bool) error {
	if includeVM {
		_, vmFound, vmErr := client.InspectInstance(ctx, names.VM)
		if vmErr != nil {
			return fmt.Errorf("verify VM removal: %w", vmErr)
		}
		if vmFound {
			return fmt.Errorf("Lima VM %q still exists after removal", names.VM)
		}
	}
	if !includeDisk {
		return nil
	}
	_, diskFound, diskErr := client.InspectDisk(ctx, names.DataDisk)
	if diskErr != nil {
		return fmt.Errorf("verify data-disk removal: %w", diskErr)
	}
	if diskFound {
		return fmt.Errorf("Lima data disk %q still exists after removal", names.DataDisk)
	}
	return nil
}

func (o *defaultOperations) Recover(ctx context.Context, def Definition) error {
	value, err := o.guestRequest(ctx, def, guestupdate.Request{Schema: 1, Operation: "status"})
	if err != nil {
		return err
	}
	var status guestupdate.Status
	if err := decodeGuestResult(value, &status); err != nil {
		return err
	}
	if status.AppliedLock != "" {
		if err := syncHostAppliedLock(def, status.AppliedLock); err != nil {
			return err
		}
	}
	if status.Pending != nil {
		if err := o.restoreTransactionSnapshot(ctx, def, string(status.Pending.Component)); err != nil {
			return fmt.Errorf("restore interrupted %s snapshot: %w", status.Pending.Component, err)
		}
	}
	value, err = o.guestRequest(ctx, def, guestupdate.Request{Schema: 1, Operation: "recover"})
	if err != nil {
		return err
	}
	status = guestupdate.Status{}
	if err := decodeGuestResult(value, &status); err != nil {
		return err
	}
	if status.Pending != nil || status.RestorePending != nil {
		return errors.New("guest recovery returned with an unresolved activation or durable-state restore journal")
	}
	if status.AppliedLock != "" {
		return syncHostAppliedLock(def, status.AppliedLock)
	}
	return nil
}

func (o *defaultOperations) Apply(ctx context.Context, def Definition, target string) (map[string]any, error) {
	specs, cleanup, err := o.prepareAndUpload(ctx, def, target)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	initial, err := o.initialApply(ctx, def)
	if err != nil {
		return nil, err
	}
	if initial {
		reviewedLock, err := encodeLock(def.Bundle.Lock)
		if err != nil {
			return nil, err
		}
		status, err := o.guestRequest(ctx, def, guestupdate.Request{
			Schema: 1, Operation: "apply", Components: specs, Initial: true, SnapshotReady: true, ReviewedLock: reviewedLock,
		})
		if err != nil {
			return nil, err
		}
		if err := updateHostAppliedLock(def, "all"); err != nil {
			return nil, err
		}
		return map[string]any{
			"changed":    []string{"node", "uv", "claude", "codex", "hermes", "executor"},
			"components": o.operationComponents(ctx, def, "all", status),
		}, nil
	}
	changed := make([]string, 0, len(specs))
	var status any
	for _, spec := range specs {
		name := string(spec.Name)
		current, err := config.LoadLock(hostAppliedLockPath(def))
		if err != nil {
			return nil, err
		}
		if componentLockEqual(current, def.Bundle.Lock, name) {
			continue
		}
		snapshot, err := (&defaultBackups{operations: o}).Create(ctx, def, "transaction-"+sanitizePin(name))
		if err != nil {
			return nil, fmt.Errorf("create encrypted transaction snapshot for %s: %w", name, err)
		}
		if err := saveTransactionSnapshot(def, name, snapshot, hostAppliedLockPath(def)); err != nil {
			return nil, err
		}
		store, err := o.store(def)
		if err != nil {
			return nil, err
		}
		journal := box.Journal{
			Schema: box.JournalSchema, Operation: "update", Phase: "prepared", StartedAt: time.Now().UTC(),
			Update: &box.UpdateRecovery{Component: name, Snapshot: transactionSnapshotPath(def, name)},
		}
		if err := store.SaveJournal(def.Name, journal); err != nil {
			return nil, fmt.Errorf("persist %s update recovery state: %w", name, err)
		}
		targetLock, err := targetAppliedLock(def, name)
		if err != nil {
			return nil, err
		}
		reviewedLock, err := encodeLock(targetLock)
		if err != nil {
			return nil, err
		}
		status, err = o.guestRequest(ctx, def, guestupdate.Request{
			Schema: 1, Operation: "apply", Components: []component.Spec{spec}, SnapshotReady: true, ReviewedLock: reviewedLock,
		})
		if err != nil {
			recoverErr := o.completeHostUpdateRecovery(ctx, def, journal)
			failure := classify(err)
			return nil, &Error{
				Code: failure.Code, Message: failure.Message, Recovery: failure.Recovery, Status: failure.Status,
				Cause: errors.Join(err, recoverErr), Details: map[string]any{
					"completed": changed, "failed_component": name, "rolled_back": recoverErr == nil,
				},
			}
		}
		if err := updateHostAppliedLock(def, name); err != nil {
			recoverErr := o.completeHostUpdateRecovery(ctx, def, journal)
			return nil, errors.Join(fmt.Errorf("publish %s host applied lock: %w", name, err), recoverErr)
		}
		if err := invalidateOverlappingTransactionSnapshots(def, spec.Name); err != nil {
			recoverErr := o.completeHostUpdateRecovery(ctx, def, journal)
			return nil, errors.Join(fmt.Errorf("invalidate snapshots overlapping %s: %w", name, err), recoverErr)
		}
		if err := store.ClearJournal(def.Name); err != nil {
			recoverErr := o.completeHostUpdateRecovery(ctx, def, journal)
			return nil, errors.Join(fmt.Errorf("clear %s update recovery state: %w", name, err), recoverErr)
		}
		changed = append(changed, name)
	}
	if status == nil {
		status, err = o.guestRequest(ctx, def, guestupdate.Request{Schema: 1, Operation: "status"})
		if err != nil {
			return nil, err
		}
	}
	return map[string]any{"changed": changed, "components": o.operationComponents(ctx, def, target, status)}, nil
}

func (o *defaultOperations) operationComponents(ctx context.Context, def Definition, target string, value any) map[string]any {
	var status guestupdate.Status
	if err := decodeGuestResult(value, &status); err != nil {
		return map[string]any{}
	}
	all := componentStatus(def, status, o.componentObservations(ctx, def, status))
	if target == "all" {
		return all
	}
	componentResult, ok := all[target]
	if !ok {
		return map[string]any{}
	}
	return map[string]any{target: componentResult}
}

func (o *defaultOperations) completeHostUpdateRecovery(_ context.Context, def Definition, journal box.Journal) error {
	if journal.Update == nil || !component.Known(component.Name(journal.Update.Component)) {
		return errors.New("update journal is missing a known component recovery state")
	}
	target := journal.Update.Component
	if filepath.Clean(journal.Update.Snapshot) != transactionSnapshotPath(def, target) {
		return errors.New("update journal snapshot is outside the exact component transaction directory")
	}
	recoveryCtx, cancel := context.WithTimeout(context.Background(), hostCleanupTimeout)
	defer cancel()
	if err := o.StopServices(recoveryCtx, def); err != nil {
		return err
	}
	if _, err := o.restoreAndConfirmTransactionSnapshot(recoveryCtx, def, target, journal.Update.Snapshot); err != nil {
		return err
	}
	if err := o.StartServices(recoveryCtx, def); err != nil {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), hostCleanupTimeout)
		defer stopCancel()
		_ = o.StopServices(stopCtx, def)
		return err
	}
	store, err := o.store(def)
	if err != nil {
		return err
	}
	return store.ClearJournal(def.Name)
}

func (o *defaultOperations) Rollback(ctx context.Context, def Definition, target string) (map[string]any, error) {
	before, err := o.readGuestStatus(ctx, def)
	if err != nil {
		return nil, err
	}
	directory := filepath.Join(def.Home, "backups", def.Name, "transactions", sanitizePin(target))
	latestPath := filepath.Join(directory, "latest.json")
	previous, err := loadTransactionSnapshot(latestPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("rollback snapshot for %s is unavailable or was invalidated by a later overlapping mutation", target)
		}
		return nil, fmt.Errorf("load retained pre-update snapshot: %w", err)
	}
	previousPath := filepath.Join(directory, "rollback-previous.json")
	currentPath := filepath.Join(directory, "rollback-current.json")
	if err := writeTransactionSnapshot(previousPath, previous); err != nil {
		return nil, err
	}
	currentSnapshot, err := (&defaultBackups{operations: o}).Create(ctx, def, "pre-rollback-"+sanitizePin(target))
	if err != nil {
		return nil, fmt.Errorf("create pre-rollback snapshot: %w", err)
	}
	currentLock, err := os.ReadFile(hostAppliedLockPath(def))
	if err != nil {
		return nil, err
	}
	if err := writeTransactionSnapshot(currentPath, transactionSnapshot{Backup: currentSnapshot, AppliedLock: string(currentLock)}); err != nil {
		return nil, err
	}
	store, err := o.store(def)
	if err != nil {
		return nil, err
	}
	journal := box.Journal{
		Schema: box.JournalSchema, Operation: "rollback", Phase: "prepared", StartedAt: time.Now().UTC(),
		Rollback: &box.RollbackRecovery{Component: target, PreviousSnapshot: previousPath, CurrentSnapshot: currentPath},
	}
	if err := store.SaveJournal(def.Name, journal); err != nil {
		return nil, err
	}
	status, err := o.completeHostRollback(ctx, def, journal)
	if err != nil {
		return nil, err
	}
	current := ""
	var after guestupdate.Status
	if err := decodeGuestResult(status, &after); err != nil {
		return nil, err
	}
	current = after.Applied.Components[component.Name(target)]
	return map[string]any{
		"component": target,
		"previous":  before.Applied.Components[component.Name(target)],
		"current":   current,
		"desired":   lockPin(def.Bundle.Lock, target),
	}, nil
}

func (o *defaultOperations) completeHostRollback(_ context.Context, def Definition, journal box.Journal) (any, error) {
	if journal.Rollback == nil {
		return nil, errors.New("rollback journal is missing recovery state")
	}
	target := journal.Rollback.Component
	if !component.Known(component.Name(target)) {
		return nil, fmt.Errorf("rollback journal names unknown component %q", target)
	}
	directory := filepath.Join(def.Home, "backups", def.Name, "transactions", sanitizePin(target))
	if filepath.Clean(journal.Rollback.PreviousSnapshot) != filepath.Join(directory, "rollback-previous.json") ||
		filepath.Clean(journal.Rollback.CurrentSnapshot) != filepath.Join(directory, "rollback-current.json") {
		return nil, errors.New("rollback journal snapshot paths are outside the exact component transaction directory")
	}
	if _, err := loadTransactionSnapshot(journal.Rollback.PreviousSnapshot); err != nil {
		return nil, fmt.Errorf("load rollback previous snapshot: %w", err)
	}
	current, err := loadTransactionSnapshot(journal.Rollback.CurrentSnapshot)
	if err != nil {
		return nil, fmt.Errorf("load rollback current snapshot: %w", err)
	}
	recoveryCtx, cancel := context.WithTimeout(context.Background(), hostCleanupTimeout)
	defer cancel()
	if err := o.StopServices(recoveryCtx, def); err != nil {
		return nil, err
	}
	result, err := o.restoreAndConfirmTransactionSnapshot(recoveryCtx, def, target, journal.Rollback.PreviousSnapshot)
	if err != nil {
		return nil, err
	}
	latestPath := filepath.Join(filepath.Dir(journal.Rollback.CurrentSnapshot), "latest.json")
	if err := writeTransactionSnapshot(latestPath, current); err != nil {
		return nil, err
	}
	if err := invalidateOverlappingTransactionSnapshots(def, component.Name(target)); err != nil {
		return nil, err
	}
	if err := o.StartServices(recoveryCtx, def); err != nil {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), hostCleanupTimeout)
		defer stopCancel()
		_ = o.StopServices(stopCtx, def)
		return nil, err
	}
	store, err := o.store(def)
	if err != nil {
		return nil, err
	}
	if err := store.ClearJournal(def.Name); err != nil {
		return nil, err
	}
	_ = os.Remove(journal.Rollback.PreviousSnapshot)
	_ = os.Remove(journal.Rollback.CurrentSnapshot)
	return result, nil
}

func (o *defaultOperations) initialApply(ctx context.Context, def Definition) (bool, error) {
	value, err := o.guestRequest(ctx, def, guestupdate.Request{Schema: 1, Operation: "status"})
	if err != nil {
		return false, err
	}
	var status guestupdate.Status
	if err := decodeGuestResult(value, &status); err != nil {
		return false, err
	}
	if status.AppliedLock != "" {
		if err := syncHostAppliedLock(def, status.AppliedLock); err != nil {
			return false, err
		}
	}
	return len(status.Applied.Components) == 0, nil
}

func (o *defaultOperations) StartServices(ctx context.Context, def Definition) error {
	return o.guestShell(ctx, def, nil, "sudo", "systemctl", "start", "executor.service", "hermes.service")
}

func (o *defaultOperations) StopServices(ctx context.Context, def Definition) error {
	return o.guestShell(ctx, def, nil, "sudo", "systemctl", "stop", "hermes.service", "executor.service")
}

func (o *defaultOperations) SyncData(ctx context.Context, def Definition) error {
	return o.guestShell(ctx, def, nil, "sudo", "sync", "-f", "/data")
}

func (o *defaultOperations) Health(ctx context.Context, def Definition) (Health, error) {
	if def.Bundle.Lock.Schema == 0 {
		lock, err := config.LoadLock(def.LockPath)
		if err != nil {
			return Health{}, fmt.Errorf("load destination desired lock: %w", err)
		}
		def.Bundle.Lock = lock
	}
	guestStatus, err := o.readGuestStatus(ctx, def)
	if err != nil {
		return Health{}, err
	}
	var failures []error
	store, metadataErr := o.store(def)
	if metadataErr == nil {
		var metadata box.Metadata
		metadata, metadataErr = store.VerifyOwnership(def.Name, def.ConfigDir)
		if metadataErr == nil {
			metadataErr = o.verifyGuestOwnershipMarker(ctx, def, metadata.DataOwnershipMarker)
		}
	}
	if metadataErr != nil {
		failures = append(failures, fmt.Errorf("guest ownership: %w", metadataErr))
	}
	if guestStatus.Pending != nil {
		failures = append(failures, fmt.Errorf("interrupted %s activation is pending recovery", guestStatus.Pending.Component))
	}
	_, storageErr := o.boundedGuestOutput(ctx, def, "storage mounts", "sudo", "/bin/sh", "-ceu", `
mountpoint -q /data
mountpoint -q /home/agent
[ "$(findmnt --noheadings --output FSROOT --target /home/agent | tr -d '[:space:]')" = "/home/agent" ]
[ "$(readlink -f /workspace)" = "/home/agent/workspace" ]
`)
	if storageErr != nil {
		failures = append(failures, storageErr)
	}
	if _, err := o.boundedGuestOutput(ctx, def, "application services", "sudo", "systemctl", "is-active", "--quiet", "hermes.service", "executor.service"); err != nil {
		failures = append(failures, err)
	}
	observations := o.componentObservations(ctx, def, guestStatus)
	for name, observation := range observations {
		if observation.err != nil {
			failures = append(failures, fmt.Errorf("%s activation: %w", name, observation.err))
		}
	}
	if _, err := o.boundedGuestOutput(ctx, def, "Hermes gateway", "sudo", "-u", "agent", "-H", "env", "HOME=/home/agent", "HERMES_HOME=/home/agent/.hermes", "/opt/hermes-box/current/hermes/bin/hermes", "gateway", "status"); err != nil {
		failures = append(failures, err)
	}
	if _, err := o.boundedGuestOutput(ctx, def, "Executor HTTP", "/usr/bin/curl", "--fail", "--silent", "--show-error", "--max-time", "5", fmt.Sprintf("http://127.0.0.1:%d/health", lima.ExecutorGuestPort)); err != nil {
		failures = append(failures, err)
	}
	executorConfigured, configuredErr := o.boundedGuestOutput(ctx, def, "Executor setup state", "/bin/sh", "-ceu", `if [ -s /home/agent/.hermes/executor.env ]; then printf configured; else printf missing; fi`)
	if configuredErr != nil {
		failures = append(failures, configuredErr)
	} else if executorConfigured == "configured" {
		if _, err := o.boundedGuestOutput(ctx, def, "authenticated Executor MCP", "sudo", "-u", "agent", "-H", "env", "HOME=/home/agent", "HERMES_HOME=/home/agent/.hermes", "/opt/hermes-box/current/hermes/bin/hermes", "mcp", "test", "executor"); err != nil {
			failures = append(failures, err)
		}
	}
	setupRequired := o.setupRequirements(ctx, def)
	components := componentStatus(def, guestStatus, observations)
	for _, name := range setupRequired {
		componentValue, ok := components[name].(map[string]any)
		if ok && componentValue["state"] == "healthy" {
			componentValue["state"] = "setup-required"
		}
	}
	health := Health{
		Healthy: len(failures) == 0, SetupRequired: setupRequired,
		Components: components,
		Storage: map[string]any{
			"data": "/data", "home": "/home/agent", "workspace": "/home/agent/workspace", "healthy": storageErr == nil,
		},
		Ports: map[string]any{"executor": def.Bundle.Config.Ports.Executor},
	}
	return health, errors.Join(failures...)
}

const diagnosticCheckTimeout = 12 * time.Second

func (o *defaultOperations) readGuestStatus(ctx context.Context, def Definition) (guestupdate.Status, error) {
	checkCtx, cancel := context.WithTimeout(ctx, diagnosticCheckTimeout)
	defer cancel()
	value, err := o.guestRequest(checkCtx, def, guestupdate.Request{Schema: 1, Operation: "status"})
	if err != nil {
		if errors.Is(checkCtx.Err(), context.DeadlineExceeded) {
			return guestupdate.Status{}, errors.New("guest status timed out after 12s")
		}
		return guestupdate.Status{}, err
	}
	var status guestupdate.Status
	if err := decodeGuestResult(value, &status); err != nil {
		return guestupdate.Status{}, err
	}
	return status, nil
}

func (o *defaultOperations) boundedGuestOutput(ctx context.Context, def Definition, label string, argv ...string) (string, error) {
	checkCtx, cancel := context.WithTimeout(ctx, diagnosticCheckTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	_, err := o.runGuest(checkCtx, def, nil, &stdout, &stderr, argv...)
	if err == nil {
		return strings.TrimSpace(stdout.String()), nil
	}
	if errors.Is(checkCtx.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf("%s timed out after 12s", label)
	}
	detail := strings.TrimSpace(stderr.String())
	if detail == "" {
		detail = err.Error()
	}
	return "", fmt.Errorf("%s failed: %s", label, detail)
}

func (o *defaultOperations) componentObservations(ctx context.Context, def Definition, status guestupdate.Status) map[component.Name]componentObservation {
	result := make(map[component.Name]componentObservation, len(status.Releases.Components))
	type nativeComponent struct {
		name       component.Name
		activation string
		releases   string
		command    []string
	}
	checks := []nativeComponent{
		{component.Node, "/opt/hermes-box/tooling/current/node", "/opt/hermes-box/tooling/node", []string{"bin/node", "--version"}},
		{component.UV, "/opt/hermes-box/tooling/current/uv", "/opt/hermes-box/tooling/uv", []string{"bin/uv", "--version"}},
		{component.Claude, "/opt/hermes-box/current/claude", "/opt/hermes-box/releases/claude", []string{"bin/claude", "--version"}},
		{component.Codex, "/opt/hermes-box/current/codex", "/opt/hermes-box/releases/codex", []string{"bin/codex", "--strict-config", "--version"}},
		{component.Hermes, "/opt/hermes-box/current/hermes", "/opt/hermes-box/releases/hermes", []string{"bin/hermes", "--version"}},
	}
	for _, check := range checks {
		metadata := status.Releases.Components[check.name]
		if metadata.Current == "" {
			result[check.name] = componentObservation{err: errors.New("current release is missing")}
			continue
		}
		expected := filepath.Join(check.releases, metadata.Current)
		command := filepath.Join(expected, check.command[0])
		argv := []string{"sudo", "/bin/sh", "-ceu", `
[ "$(readlink -f "$1")" = "$2" ]
shift 2
exec sudo -u agent -H env HOME=/home/agent CODEX_HOME=/home/agent/.codex HERMES_HOME=/home/agent/.hermes "$@"
`, "activation-check", check.activation, expected, command}
		argv = append(argv, check.command[1:]...)
		running, err := o.boundedGuestOutput(ctx, def, string(check.name)+" activation", argv...)
		if err == nil && check.name != component.Hermes && !strings.Contains(running, strings.TrimPrefix(metadata.Current, "v")) {
			err = fmt.Errorf("reported version %q does not contain applied pin %q", running, metadata.Current)
		}
		result[check.name] = componentObservation{running: running, err: err}
	}
	executor := status.Releases.Components[component.Executor]
	if executor.Current == "" {
		result[component.Executor] = componentObservation{err: errors.New("current release is missing")}
	} else {
		imageFile := filepath.Join("/opt/hermes-box/releases/executor", executor.Current, "image")
		running, err := o.boundedGuestOutput(ctx, def, "executor activation", "sudo", "/bin/sh", "-ceu", `
image=$(cat "$1")
grep -Fqx "EXECUTOR_IMAGE=$image" /etc/hermes-box/executor.env
running=$(podman container inspect --format '{{.ImageName}}' hermes-box-executor)
[ "$running" = "$image" ]
printf '%s' "$running"
`, "executor-activation-check", imageFile)
		result[component.Executor] = componentObservation{running: running, err: err}
	}
	return result
}

func (o *defaultOperations) setupRequirements(ctx context.Context, def Definition) []string {
	checks := []struct{ name, expression string }{
		{"claude", `[ -s /home/agent/.claude/.credentials.json ] || [ -s /home/agent/.claude.json ]`},
		{"codex", `[ -s /home/agent/.codex/auth.json ]`},
		{"hermes", `[ -s /home/agent/.hermes/config.yaml ] || [ -s /home/agent/.hermes/config.json ]`},
		{"executor", `[ -s /home/agent/.hermes/executor.env ]`},
	}
	missing := make([]string, 0, len(checks))
	for _, check := range checks {
		if _, err := o.boundedGuestOutput(ctx, def, check.name+" setup state", "/bin/sh", "-c", check.expression); err != nil {
			missing = append(missing, check.name)
		}
	}
	return missing
}

func (o *defaultOperations) Status(ctx context.Context, def Definition, check bool) (Status, error) {
	lastBackup := o.lastBackup(ctx, def)
	owned, err := o.Ownership(ctx, def)
	if err != nil {
		return Status{}, err
	}
	if !owned.Exists {
		return Status{State: "absent", SetupRequired: []string{}, Components: map[string]any{}, Storage: map[string]any{}, Ports: map[string]any{}, LastBackup: lastBackup, Updates: []any{}}, nil
	}
	if !owned.Owned {
		return Status{}, errors.New("configured resources are not owned by this configuration directory")
	}
	updates := []any{}
	if check {
		discover := o.releaseDiscovery
		if discover == nil {
			discover = discoverUpdates
		}
		updates = discover(ctx, def)
	}
	if !owned.Running {
		return Status{State: "stopped", SetupRequired: []string{}, Components: stoppedComponentStatus(def), Storage: map[string]any{}, Ports: map[string]any{"executor": def.Bundle.Config.Ports.Executor}, LastBackup: lastBackup, Updates: updates}, nil
	}
	health, healthErr := o.Health(ctx, def)
	state := "running"
	if healthErr != nil {
		state = "degraded"
	}
	return Status{State: state, Healthy: healthErr == nil, SetupRequired: health.SetupRequired, Components: health.Components, Storage: health.Storage, Ports: health.Ports, LastBackup: lastBackup, Updates: updates}, nil
}

func stoppedComponentStatus(def Definition) map[string]any {
	applied, err := config.LoadLock(hostAppliedLockPath(def))
	if err != nil {
		return map[string]any{}
	}
	result := make(map[string]any, len(components))
	for _, name := range []string{"node", "uv", "claude", "codex", "hermes", "executor"} {
		desiredPin := lockPin(def.Bundle.Lock, name)
		appliedPin := lockPin(applied, name)
		state := "healthy"
		if desiredPin != appliedPin {
			state = "drifted"
		}
		result[name] = map[string]any{
			"desired": desiredPin, "applied": appliedPin, "running": "", "previous": nil, "state": state,
		}
	}
	return result
}

func (o *defaultOperations) lastBackup(ctx context.Context, def Definition) any {
	backup, err := (&defaultBackups{operations: o}).LatestVerified(ctx, def)
	if err != nil || backup == nil {
		return nil
	}
	return backup
}

func (o *defaultOperations) SSH(ctx context.Context, def Definition, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	args := []string{"shell", "--workdir", "/workspace", def.Name, "--", "sudo", "-u", "agent", "-H", "env", "TERM=xterm-ghostty"}
	args = append(args, ghosttyEnvironment(os.Getenv)...)
	args = append(args, "/usr/local/bin/tm")
	return o.runLima(ctx, def, stdin, stdout, stderr, args...)
}

func ghosttyEnvironment(getenv func(string) string) []string {
	var environment []string
	if value := getenv("COLORTERM"); value == "truecolor" || value == "24bit" {
		environment = append(environment, "COLORTERM="+value)
	}
	if value := getenv("TERM_PROGRAM"); value == "ghostty" {
		environment = append(environment, "TERM_PROGRAM=ghostty")
		if version := getenv("TERM_PROGRAM_VERSION"); regexp.MustCompile(`^[A-Za-z0-9._+-]{1,64}$`).MatchString(version) {
			environment = append(environment, "TERM_PROGRAM_VERSION="+version)
		}
	}
	return environment
}

func (o *defaultOperations) Exec(ctx context.Context, def Definition, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	request, err := json.Marshal(guestupdate.Request{Schema: 1, Operation: "exec", Argv: argv, Directory: "/home/agent/workspace"})
	if err != nil {
		return 1, err
	}
	output := boundedProtocolBuffer{maximum: maximumGuestExecResponseBytes}
	runErr := o.runner.Run(ctx, process.Spec{
		Name: "limactl", Args: []string{"shell", def.Name, "--", "sudo", "/usr/local/libexec/hermes-box-guest"},
		Env: []string{"LIMA_HOME=" + filepath.Join(def.Home, "lima")}, Stdin: bytes.NewReader(append(request, '\n')), Stdout: &output, Stderr: stderr,
	})
	if output.exceeded {
		return 1, errors.Join(runErr, errors.New("guest exec response exceeds 64 MiB protocol limit"))
	}
	response, decodeErr := decodeGuestResponseLimit(output.buffer.Bytes(), maximumGuestExecResponseBytes)
	if decodeErr != nil {
		return 1, errors.Join(runErr, decodeErr)
	}
	var result struct {
		ExitCode int    `json:"exit_code"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
	}
	if err := decodeGuestResult(response.Result, &result); err != nil {
		return 1, err
	}
	if result.ExitCode < 0 || result.ExitCode > 255 {
		return 1, errors.New("guest exec response contains an invalid exit code")
	}
	if _, err := io.WriteString(stdout, result.Stdout); err != nil {
		return 1, err
	}
	if _, err := io.WriteString(stderr, result.Stderr); err != nil {
		return 1, err
	}
	if response.OK {
		return result.ExitCode, nil
	}
	if response.Error != nil {
		return result.ExitCode, errors.New(response.Error.Message)
	}
	return result.ExitCode, runErr
}

func (o *defaultOperations) Logs(ctx context.Context, def Definition, target string, lines int, follow bool, stdout, stderr io.Writer) error {
	unit := map[string]string{"hermes": "hermes.service", "executor": "executor.service", "recovery": "hermes-box-recover.service"}[target]
	args := []string{"sudo", "journalctl", "-u", unit, "-n", fmt.Sprint(lines), "--no-pager"}
	if follow {
		args = append(args, "-f")
	}
	_, err := o.runGuest(ctx, def, nil, stdout, stderr, args...)
	return err
}

func (o *defaultOperations) OpenExecutor(ctx context.Context, def Definition) (string, error) {
	ownership, err := o.Ownership(ctx, def)
	if err != nil {
		return "", err
	}
	if !ownership.Owned || !ownership.Running {
		return "", errors.New("Executor portal requires a running owned box")
	}
	if _, err := o.checkExecutorForward(ctx, def); err != nil {
		return "", fmt.Errorf("verify Executor health and loopback listener before opening browser: %w", err)
	}
	url := fmt.Sprintf("http://127.0.0.1:%d", def.Bundle.Config.Ports.Executor)
	err = o.runner.Run(ctx, process.Spec{Name: "open", Args: []string{url}, Stdout: io.Discard, Stderr: o.stderr})
	return url, err
}

func (o *defaultOperations) SetupExecutor(ctx context.Context, def Definition, input io.Reader, tokenStdin bool) ([]string, error) {
	token, err := o.readExecutorToken(ctx, input, tokenStdin)
	if err != nil {
		return nil, err
	}
	script := fmt.Sprintf(`umask 077
IFS= read -r token
[ -n "$token" ]
header=$(mktemp)
trap 'rm -f "$header"' EXIT
chmod 0600 "$header"
printf 'Authorization: Bearer %%s\n' "$token" > "$header"
response=$(curl -fsS \
  --header @"$header" \
  -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}' \
  http://127.0.0.1:%d/mcp)
tools=$(printf '%%s' "$response" | jq -r '.result.tools[].name' | sort | tr '\n' ' ')
[ "$tools" = 'execute resume ' ]
install -d -m 0700 /home/agent/.hermes
printf 'EXECUTOR_TOKEN=%%s\n' "$token" > /home/agent/.hermes/executor.env
chmod 0600 /home/agent/.hermes/executor.env
chown -R agent:agent /home/agent/.hermes
sudo -u agent env HOME=/home/agent HERMES_HOME=/home/agent/.hermes \
  /opt/hermes-box/current/hermes/venv/bin/python - <<'PY'
from pathlib import Path
from hermes_cli.config import load_config, save_config, save_env_value
token = Path('/home/agent/.hermes/executor.env').read_text().split('=', 1)[1].strip()
save_env_value('MCP_EXECUTOR_API_KEY', token)
config = load_config()
config.setdefault('mcp_servers', {})['executor'] = {
    'url': 'http://127.0.0.1:%d/mcp',
    'headers': {'Authorization': 'Bearer ${MCP_EXECUTOR_API_KEY}'},
    'enabled': True,
}
save_config(config)
PY
systemctl restart hermes.service
sudo -u agent env HOME=/home/agent HERMES_HOME=/home/agent/.hermes \
  /opt/hermes-box/current/hermes/bin/hermes mcp test executor
`, lima.ExecutorGuestPort, lima.ExecutorGuestPort)
	if err := o.guestShell(ctx, def, strings.NewReader(token+"\n"), "sudo", "/bin/sh", "-ceu", script); err != nil {
		return nil, err
	}
	return []string{"execute", "resume"}, nil
}

func (o *defaultOperations) readExecutorToken(ctx context.Context, input io.Reader, tokenStdin bool) (token string, resultErr error) {
	if input == nil {
		return "", errors.New("Executor token input is unavailable")
	}
	if !tokenStdin {
		file, ok := input.(*os.File)
		if !ok {
			return "", errors.New("a TTY is required for the no-echo token prompt; use --token-stdin for redirected input")
		}
		stateData, err := o.runner.Output(ctx, process.Spec{Name: "stty", Args: []string{"-g"}, Stdin: file, Stderr: o.stderr})
		if err != nil {
			return "", fmt.Errorf("read terminal state before secret prompt: %w", err)
		}
		state := strings.TrimSpace(string(stateData))
		if state == "" || !regexp.MustCompile(`^[A-Za-z0-9:;=,._+-]+$`).MatchString(state) {
			return "", errors.New("terminal returned an unsafe or empty state")
		}
		if err := o.runner.Run(ctx, process.Spec{Name: "stty", Args: []string{"-echo"}, Stdin: file, Stdout: o.stderr, Stderr: o.stderr}); err != nil {
			return "", fmt.Errorf("disable terminal echo for secret prompt: %w", err)
		}
		defer func() {
			restoreCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			restoreErr := o.runner.Run(restoreCtx, process.Spec{Name: "stty", Args: []string{state}, Stdin: file, Stdout: o.stderr, Stderr: o.stderr})
			fmt.Fprintln(o.stderr)
			if restoreErr != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("restore terminal state after secret prompt: %w", restoreErr))
			}
		}()
		fmt.Fprint(o.stderr, "Executor token: ")
	}
	reader := bufio.NewReader(io.LimitReader(input, 16*1024+1))
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if len(line) > 16*1024 {
		return "", errors.New("Executor token exceeds 16 KiB")
	}
	token = strings.TrimSpace(line)
	if token == "" {
		return "", errors.New("Executor token must not be empty")
	}
	if !regexp.MustCompile(`^[\x21-\x7e]+$`).MatchString(token) {
		return "", errors.New("Executor token contains whitespace or control characters")
	}
	return token, nil
}

func (o *defaultOperations) Doctor(ctx context.Context, def Definition) ([]map[string]any, error) {
	checks := make([]map[string]any, 0, 16)
	var failures []error
	add := func(name string, value any, checkErr error, repair string) {
		entry := map[string]any{"name": name, "ok": checkErr == nil}
		if value != nil {
			entry["value"] = value
		}
		if checkErr != nil {
			entry["error"] = checkErr.Error()
			if repair != "" {
				entry["repair"] = repair
			}
			failures = append(failures, fmt.Errorf("%s: %w", name, checkErr))
		}
		checks = append(checks, entry)
	}
	base := operatorCommand(def)
	add("config", map[string]any{"config": def.ConfigPath, "lock": def.LockPath, "config_schema": def.Bundle.Config.Schema, "lock_schema": def.Bundle.Lock.Schema}, nil, "")
	platformErr := error(nil)
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		platformErr = fmt.Errorf("host is %s/%s, want darwin/arm64", runtime.GOOS, runtime.GOARCH)
	}
	client, clientErr := o.client(def)
	version := ""
	if clientErr == nil {
		platformCtx, cancel := context.WithTimeout(ctx, diagnosticCheckTimeout)
		version, clientErr = client.Version(platformCtx)
		cancel()
	}
	add("platform", map[string]any{"host": runtime.GOOS + "/" + runtime.GOARCH, "lima": version}, errors.Join(platformErr, clientErr), "brew install lima")
	ownershipCtx, cancelOwnership := context.WithTimeout(ctx, diagnosticCheckTimeout)
	ownership, ownershipErr := o.Ownership(ownershipCtx, def)
	cancelOwnership()
	add("ownership", ownership, ownershipErr, "$EDITOR "+shellQuote(def.ConfigPath))
	if ownershipErr != nil || !ownership.Exists {
		return checks, errors.Join(failures...)
	}
	keyValue, keyErr := o.backupKeyStatus(def)
	add("backup-key", keyValue, keyErr, base+" backup configured")
	if keyErr == nil {
		backupCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		latest, backupErr := (&defaultBackups{operations: o}).LatestVerified(backupCtx, def)
		cancel()
		add("latest-backup", latest, backupErr, base+" backup doctor-repair")
	}
	if !ownership.Running {
		add("guest", "stopped", nil, "")
		return checks, errors.Join(failures...)
	}
	storage, storageErr := o.boundedGuestOutput(ctx, def, "storage mounts", "sudo", "/bin/sh", "-ceu", doctorStorageScript)
	add("storage", storage, storageErr, base+" rebuild")
	guestPlatform, guestPlatformErr := o.boundedGuestOutput(ctx, def, "guest platform", "/bin/sh", "-ceu", doctorPlatformScript)
	add("guest-platform", guestPlatform, guestPlatformErr, base+" rebuild")
	network, networkErr := o.boundedGuestOutput(ctx, def, "guest network", "/bin/sh", "-ceu", doctorNetworkScript)
	add("network", network, networkErr, base+" stop && "+base+" start")
	_, unitsErr := o.boundedGuestOutput(ctx, def, "failed systemd units", "sudo", "/bin/sh", "-ceu", doctorFailedUnitsScript)
	add("failed-units", "none", unitsErr, base+" rebuild")
	tmux, tmuxErr := o.boundedGuestOutput(ctx, def, "tmux configuration", "/bin/sh", "-ceu", doctorTmuxScript)
	add("tmux", tmux, tmuxErr, base+" rebuild")
	terminfo, terminfoErr := o.boundedGuestOutput(ctx, def, "Ghostty terminfo", "/bin/sh", "-ceu", "infocmp -x xterm-ghostty >/dev/null && printf xterm-ghostty")
	add("terminfo", terminfo, terminfoErr, base+" rebuild")
	guestStatus, statusErr := o.readGuestStatus(ctx, def)
	if statusErr == nil && guestStatus.Pending != nil {
		statusErr = fmt.Errorf("interrupted %s activation is pending recovery", guestStatus.Pending.Component)
	}
	add("guest-state", map[string]any{"pending": guestStatus.Pending}, statusErr, base+" start")
	if statusErr == nil {
		observations := o.componentObservations(ctx, def, guestStatus)
		var activationErrs []error
		for name, observation := range observations {
			if observation.err != nil {
				activationErrs = append(activationErrs, fmt.Errorf("%s: %w", name, observation.err))
			}
		}
		add("components", componentStatus(def, guestStatus, observations), errors.Join(activationErrs...), base+" rebuild")
	}
	_, hermesErr := o.boundedGuestOutput(ctx, def, "Hermes gateway", "sudo", "-u", "agent", "-H", "env", "HOME=/home/agent", "HERMES_HOME=/home/agent/.hermes", "/opt/hermes-box/current/hermes/bin/hermes", "gateway", "status")
	add("hermes-gateway", "healthy", hermesErr, base+" start")
	executor, executorErr := o.boundedGuestOutput(ctx, def, "Executor HTTP", "/usr/bin/curl", "--fail", "--silent", "--show-error", "--max-time", "5", fmt.Sprintf("http://127.0.0.1:%d/health", lima.ExecutorGuestPort))
	add("executor-http", executor, executorErr, base+" start")
	configured, configuredErr := o.boundedGuestOutput(ctx, def, "Executor setup state", "/bin/sh", "-ceu", "if [ -s /home/agent/.hermes/executor.env ]; then printf configured; else printf missing; fi")
	if configuredErr != nil {
		add("executor-mcp", nil, configuredErr, base+" setup executor")
	} else if configured == "configured" {
		mcp, mcpErr := o.boundedGuestOutput(ctx, def, "authenticated Executor MCP", "sudo", "-u", "agent", "-H", "env", "HOME=/home/agent", "HERMES_HOME=/home/agent/.hermes", "/opt/hermes-box/current/hermes/bin/hermes", "mcp", "test", "executor")
		add("executor-mcp", mcp, mcpErr, base+" setup executor")
	} else {
		add("executor-mcp", "setup-required", nil, "")
	}
	forward, forwardErr := o.checkExecutorForward(ctx, def)
	add("loopback-forward", forward, forwardErr, base+" stop && "+base+" start")
	return checks, errors.Join(failures...)
}

const doctorStorageScript = `
mountpoint -q /data
mountpoint -q /home/agent
[ "$(findmnt --noheadings --output FSTYPE --target /data | tr -d '[:space:]')" = ext4 ]
[ "$(findmnt --noheadings --output FSROOT --target /home/agent | tr -d '[:space:]')" = /home/agent ]
[ "$(readlink -f /workspace)" = /home/agent/workspace ]
printf 'data=ext4 home=bind workspace=/home/agent/workspace'
`

const doctorPlatformScript = `
. /etc/os-release
[ "$VERSION_ID" = 26.04 ]
[ "$(uname -m)" = aarch64 ]
printf 'ubuntu=%s arch=%s' "$VERSION_ID" "$(uname -m)"
`

const doctorNetworkScript = `
getent ahostsv4 github.com >/dev/null
curl --fail --silent --show-error --head --max-time 5 https://github.com >/dev/null
printf 'dns=ok https=ok'
`

const doctorFailedUnitsScript = `
failed=$(systemctl --failed --no-legend --plain)
[ -z "$failed" ] || { printf '%s\n' "$failed" >&2; exit 1; }
`

const doctorTmuxScript = `
version=$(tmux -V | awk '{print $2}')
dpkg --compare-versions "$version" ge 3.5
printf 'tmux %s\n' "$version"
grep -Fqx 'set -g mouse on' /etc/tmux.conf
grep -Fqx 'set -g focus-events on' /etc/tmux.conf
grep -Fqx 'set -s extended-keys always' /etc/tmux.conf
grep -Fqx 'set -g allow-passthrough on' /etc/tmux.conf
`

func (o *defaultOperations) backupKeyStatus(def Definition) (any, error) {
	if o.keys == nil {
		return nil, errors.New("macOS Keychain is unavailable")
	}
	identity, err := keychain.LoadIdentity(o.keys, identityAccount(def))
	if err != nil {
		return nil, err
	}
	return map[string]any{"recipient_fingerprint": keychain.RecipientFingerprint(identity.Recipient())}, nil
}

func (o *defaultOperations) checkExecutorForward(ctx context.Context, def Definition) (string, error) {
	checkCtx, cancel := context.WithTimeout(ctx, diagnosticCheckTimeout)
	defer cancel()
	url := fmt.Sprintf("http://127.0.0.1:%d/health", def.Bundle.Config.Ports.Executor)
	request, err := http.NewRequestWithContext(checkCtx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		return "", err
	}
	response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("host Executor health returned %s", response.Status)
	}
	output, err := o.runner.Output(checkCtx, process.Spec{
		Name: "lsof", Args: []string{"-nP", "-a", "-iTCP:" + fmt.Sprint(def.Bundle.Config.Ports.Executor), "-sTCP:LISTEN", "-Fn"},
	})
	if err != nil {
		return "", fmt.Errorf("inspect host listener: %w", err)
	}
	wantIPv4 := fmt.Sprintf("n127.0.0.1:%d", def.Bundle.Config.Ports.Executor)
	wantIPv6 := fmt.Sprintf("n[::1]:%d", def.Bundle.Config.Ports.Executor)
	found := false
	for _, line := range strings.Split(string(output), "\n") {
		if !strings.HasPrefix(line, "n") {
			continue
		}
		if line != wantIPv4 && line != wantIPv6 {
			return "", fmt.Errorf("Executor listener is not loopback-only: %s", strings.TrimPrefix(line, "n"))
		}
		found = true
	}
	if !found {
		return "", errors.New("no host Executor listener found")
	}
	return fmt.Sprintf("%s healthy; listener loopback-only", url), nil
}

func operatorCommand(def Definition) string {
	return "HERMES_BOX_HOME=" + shellQuote(def.Home) + " hermes-box --config " + shellQuote(def.ConfigPath)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func (o *defaultOperations) ExportKey(_ context.Context, def Definition, path string) (string, error) {
	if o.keys == nil {
		return "", errors.New("macOS Keychain is unavailable")
	}
	if err := keychain.ExportIdentity(o.keys, identityAccount(def), path); err != nil {
		return "", err
	}
	identity, err := keychain.LoadIdentity(o.keys, identityAccount(def))
	if err != nil {
		return "", err
	}
	return keychain.RecipientFingerprint(identity.Recipient()), nil
}

func (o *defaultOperations) CaptureRecoveryState(ctx context.Context, def Definition) (RecoveryState, error) {
	directory := filepath.Join(def.Home, "boxes")
	file, err := os.CreateTemp(directory, ".pre-rebuild-lock-")
	if err != nil {
		return RecoveryState{}, err
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		os.Remove(path)
		return RecoveryState{}, err
	}
	if err := os.Remove(path); err != nil {
		return RecoveryState{}, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(path)
		}
	}()
	client, err := o.client(def)
	if err != nil {
		return RecoveryState{}, err
	}
	if err := client.Copy(ctx, false, []string{def.Name + ":/var/lib/hermes-box/applied.lock"}, path); err != nil {
		return RecoveryState{}, err
	}
	applied, err := config.LoadLock(path)
	if err != nil {
		return RecoveryState{}, err
	}
	closure, err := materializeBackupClosure(ctx, def, applied)
	if err != nil {
		return RecoveryState{}, fmt.Errorf("materialize current applied recovery closure: %w", err)
	}
	artifacts := make([]string, 0, len(closure))
	for _, artifact := range closure {
		artifacts = append(artifacts, artifact.Path)
	}
	keep = true
	return RecoveryState{AppliedLock: path, Artifacts: artifacts, Temporary: true}, nil
}

func rebuildAppliedLockPath(def Definition) string {
	return filepath.Join(def.Home, "boxes", def.Name+".rebuild.applied.lock")
}

func (o *defaultOperations) PrepareRebuildRecovery(ctx context.Context, def Definition, recovery RecoveryState, snapshot BackupResult) (RecoveryState, error) {
	bundle, err := o.verifyRebuildBackup(ctx, def, snapshot)
	if err != nil {
		return RecoveryState{}, err
	}
	defer bundle.Cleanup()
	sourceLock := recovery.AppliedLock
	if sourceLock == "" {
		sourceLock = filepath.Join(bundle.Root, "applied.lock")
	}
	lock, err := config.LoadLock(sourceLock)
	if err != nil {
		return RecoveryState{}, fmt.Errorf("load rebuild recovery lock: %w", err)
	}
	stableLock := rebuildAppliedLockPath(def)
	if err := copyReplace(sourceLock, stableLock, 0o600); err != nil {
		return RecoveryState{}, fmt.Errorf("persist rebuild recovery lock: %w", err)
	}
	if err := importRestoredArtifacts(def.Home, filepath.Join(bundle.Root, "artifacts", "sha256"), bundle.Manifest, lock); err != nil {
		return RecoveryState{}, fmt.Errorf("restore rebuild recovery artifacts: %w", err)
	}
	closure, err := materializeBackupClosure(ctx, def, lock)
	if err != nil {
		return RecoveryState{}, fmt.Errorf("persist rebuild recovery artifacts: %w", err)
	}
	artifacts := make([]box.JournalArtifact, 0, len(closure))
	paths := make([]string, 0, len(closure))
	for _, artifact := range closure {
		info, statErr := os.Lstat(artifact.Path)
		if statErr != nil {
			return RecoveryState{}, fmt.Errorf("inspect rebuild artifact %q: %w", artifact.Name, statErr)
		}
		if !info.Mode().IsRegular() {
			return RecoveryState{}, fmt.Errorf("rebuild artifact %q is not a regular file", artifact.Name)
		}
		actual, hashErr := hashFile(artifact.Path)
		if hashErr != nil {
			return RecoveryState{}, fmt.Errorf("verify rebuild artifact %q: %w", artifact.Name, hashErr)
		}
		if actual != artifact.SHA256 {
			return RecoveryState{}, fmt.Errorf("verify rebuild artifact %q: SHA-256 mismatch", artifact.Name)
		}
		artifacts = append(artifacts, box.JournalArtifact{Path: artifact.Path, SHA256: artifact.SHA256})
		paths = append(paths, artifact.Path)
	}
	store, err := o.store(def)
	if err != nil {
		return RecoveryState{}, err
	}
	journal := box.Journal{
		Schema: box.JournalSchema, Operation: "rebuild", Phase: "prepared",
		Resources: []string{def.Name, box.ResourceNames(def.Name).DataDisk}, StartedAt: time.Now().UTC(),
		Recovery: &box.RebuildRecovery{
			BackupArchive: snapshot.Archive, BackupEnvelope: snapshot.Envelope,
			BackupSHA256: snapshot.ArchiveSHA256, AppliedLock: stableLock, Artifacts: artifacts,
		},
	}
	if err := store.SaveJournal(def.Name, journal); err != nil {
		return RecoveryState{}, err
	}
	return RecoveryState{AppliedLock: stableLock, Artifacts: paths}, nil
}

func (o *defaultOperations) CompleteRebuild(_ context.Context, def Definition) error {
	store, err := o.store(def)
	if err != nil {
		return err
	}
	if err := store.ClearJournal(def.Name); err != nil {
		return err
	}
	err = os.Remove(rebuildAppliedLockPath(def))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (o *defaultOperations) verifyRebuildBackup(ctx context.Context, def Definition, snapshot BackupResult) (*backup.VerifiedBundle, error) {
	if snapshot.Archive == "" || snapshot.Envelope == "" || snapshot.ArchiveSHA256 == "" {
		return nil, errors.New("rebuild recovery backup identity is incomplete")
	}
	if o.keys == nil {
		return nil, errors.New("macOS Keychain is unavailable for rebuild recovery")
	}
	identity, err := keychain.LoadIdentity(o.keys, identityAccount(def))
	if err != nil {
		return nil, err
	}
	bundle, err := backup.Verify(ctx, snapshot.Archive, snapshot.Envelope, identity, validateBackupClosure)
	if err != nil {
		return nil, fmt.Errorf("verify rebuild recovery backup: %w", err)
	}
	if bundle.Envelope.ArchiveSHA256 != snapshot.ArchiveSHA256 {
		bundle.Cleanup()
		return nil, errors.New("rebuild recovery backup checksum changed")
	}
	return bundle, nil
}

func (o *defaultOperations) verifyRebuildJournal(ctx context.Context, def Definition, journal box.Journal) error {
	if journal.Recovery == nil {
		return errors.New("rebuild journal has no recovery state")
	}
	snapshot := BackupResult{
		Archive: journal.Recovery.BackupArchive, Envelope: journal.Recovery.BackupEnvelope,
		ArchiveSHA256: journal.Recovery.BackupSHA256,
	}
	bundle, err := o.verifyRebuildBackup(ctx, def, snapshot)
	if err != nil {
		return err
	}
	bundle.Cleanup()
	if _, err := config.LoadLock(journal.Recovery.AppliedLock); err != nil {
		return fmt.Errorf("verify persisted rebuild lock: %w", err)
	}
	for _, artifact := range journal.Recovery.Artifacts {
		info, statErr := os.Lstat(artifact.Path)
		if statErr != nil {
			return fmt.Errorf("inspect persisted rebuild artifact %s: %w", artifact.Path, statErr)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("persisted rebuild artifact %s is not a regular file", artifact.Path)
		}
		actual, err := hashFile(artifact.Path)
		if err != nil || actual != artifact.SHA256 {
			return fmt.Errorf("persisted rebuild artifact %s is missing or corrupt", artifact.Path)
		}
	}
	return nil
}

func (o *defaultOperations) Restore(ctx context.Context, def Definition, archivePath, identityPath, lockPath string) (resultErr error) {
	identity, err := parseIdentity(identityPath)
	if err != nil {
		return err
	}
	envelopePath := strings.TrimSuffix(archivePath, ".tar.zst.age") + ".envelope.json"
	staging := filepath.Join(def.Home, fmt.Sprintf(".restore-%d", time.Now().UnixNano()))
	manifest, err := backup.Restore(ctx, archivePath, envelopePath, identity, validateBackupClosure, staging)
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if manifest.Box == "" {
		return errors.New("restored manifest has no source box")
	}
	restoredLockPath := filepath.Join(staging, "applied.lock")
	archivedLock, err := config.LoadLock(restoredLockPath)
	if err != nil {
		return err
	}
	if lockPath == "" {
		lockPath = restoredLockPath
	}
	desired, err := config.LoadLock(lockPath)
	if err != nil {
		return err
	}
	def.Bundle.Lock = desired
	if err := importRestoredArtifacts(def.Home, filepath.Join(staging, "artifacts", "sha256"), manifest, archivedLock); err != nil {
		return err
	}
	if err := materializeLockClosure(ctx, def); err != nil {
		return fmt.Errorf("materialize restore lock closure: %w", err)
	}
	created := false
	defer func() {
		if resultErr != nil && created {
			resultErr = errors.Join(resultErr, o.CleanupCreate(context.Background(), def))
		}
	}()
	created = true
	if err := o.CreateInfrastructure(ctx, def); err != nil {
		return err
	}
	if err := o.restoreDataStream(ctx, def, staging, manifest, false); err != nil {
		return err
	}
	if _, err := o.Apply(ctx, def, "all"); err != nil {
		return err
	}
	if err := o.StartServices(ctx, def); err != nil {
		return err
	}
	// A config-only restore selects its desired lock from the encrypted backup
	// (or explicit --lock). Publish that exact selection durably beside the
	// destination config before declaring the reconstructed box complete.
	if err := writeLock(def.LockPath, desired); err != nil {
		return fmt.Errorf("persist destination desired lock: %w", err)
	}
	if o.keys != nil {
		if _, _, err := keychain.LoadOrCreateIdentity(o.keys, identityAccount(def)); err != nil {
			return err
		}
	}
	if err := o.CompleteCreate(ctx, def); err != nil {
		return err
	}
	created = false
	return nil
}

func (o *defaultOperations) RecoverRebuild(ctx context.Context, def Definition, recovery RecoveryState, snapshot BackupResult) error {
	if o.keys == nil {
		return errors.New("macOS Keychain is unavailable for rebuild recovery")
	}
	identity, err := keychain.LoadIdentity(o.keys, identityAccount(def))
	if err != nil {
		return err
	}
	staging := filepath.Join(def.Home, fmt.Sprintf(".rebuild-restore-%d", time.Now().UnixNano()))
	manifest, err := backup.Restore(ctx, snapshot.Archive, snapshot.Envelope, identity, validateBackupClosure, staging)
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	previousLock := recovery.AppliedLock
	if previousLock == "" {
		previousLock = filepath.Join(staging, "applied.lock")
	}
	lock, err := config.LoadLock(previousLock)
	if err != nil {
		return err
	}
	def.Bundle.Lock = lock
	if err := importRestoredArtifacts(def.Home, filepath.Join(staging, "artifacts", "sha256"), manifest, lock); err != nil {
		return err
	}
	if err := o.removeFailedRebuildCandidate(ctx, def); err != nil {
		return err
	}
	if err := o.RecreateVM(ctx, def); err != nil {
		return err
	}
	if err := o.restoreDataStream(ctx, def, staging, manifest, true); err != nil {
		return err
	}
	if _, err := o.Apply(ctx, def, "all"); err != nil {
		return err
	}
	if err := o.StartServices(ctx, def); err != nil {
		return err
	}
	health, err := o.Health(ctx, def)
	if err != nil {
		return err
	}
	if !health.Healthy {
		return errors.New("recovered pre-rebuild box is unhealthy")
	}
	return nil
}

func (o *defaultOperations) RestoreRebuildData(ctx context.Context, def Definition, snapshot BackupResult) error {
	if o.keys == nil {
		return errors.New("macOS Keychain is unavailable for rebuild data recovery")
	}
	identity, err := keychain.LoadIdentity(o.keys, identityAccount(def))
	if err != nil {
		return err
	}
	staging := filepath.Join(def.Home, fmt.Sprintf(".rebuild-data-restore-%d", time.Now().UnixNano()))
	manifest, err := backup.Restore(ctx, snapshot.Archive, snapshot.Envelope, identity, validateBackupClosure, staging)
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	return o.restoreDataStream(ctx, def, staging, manifest, true)
}

func (o *defaultOperations) removeFailedRebuildCandidate(ctx context.Context, def Definition) error {
	ownership, err := o.Ownership(ctx, def)
	if err != nil {
		return fmt.Errorf("verify failed rebuild candidate ownership: %w", err)
	}
	if !ownership.Owned {
		return errors.New("refusing to remove an unowned failed rebuild candidate")
	}
	client, err := o.client(def)
	if err != nil {
		return err
	}
	instance, found, err := client.InspectInstance(ctx, def.Name)
	if err != nil {
		return fmt.Errorf("inspect failed rebuild candidate: %w", err)
	}
	if found && !strings.EqualFold(instance.Status, "stopped") {
		if err := client.Stop(ctx, def.Name, true); err != nil {
			return fmt.Errorf("stop failed rebuild candidate: %w", err)
		}
	}
	if found {
		if err := client.Delete(ctx, def.Name); err != nil {
			return fmt.Errorf("remove failed rebuild candidate: %w", err)
		}
	}
	_, stillPresent, err := client.InspectInstance(ctx, def.Name)
	if err != nil {
		return fmt.Errorf("verify failed rebuild candidate removal: %w", err)
	}
	if stillPresent {
		return errors.New("failed rebuild candidate still exists after removal")
	}
	return nil
}

func (o *defaultOperations) Version(ctx context.Context, def Definition) (VersionResult, error) {
	result := VersionResult{CLI: "v2-dev", Lima: "unavailable", ConfigSchema: config.ConfigSchema, LockSchema: config.LockSchema}
	if client, clientErr := o.client(def); clientErr == nil {
		if version, detectErr := client.DetectVersion(ctx); detectErr == nil {
			result.Lima = version
		}
	}
	return result, nil
}

func (o *defaultOperations) guestRequest(ctx context.Context, def Definition, request guestupdate.Request) (any, error) {
	data, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	var output boundedProtocolBuffer
	err = o.runner.Run(ctx, process.Spec{
		Name: "limactl", Args: []string{"shell", def.Name, "--", "sudo", "/usr/local/libexec/hermes-box-guest"},
		Env: []string{"LIMA_HOME=" + filepath.Join(def.Home, "lima")}, Stdin: bytes.NewReader(data), Stdout: &output, Stderr: o.stderr,
	})
	if output.exceeded {
		return nil, errors.Join(err, errors.New("guest response exceeds 4 MiB protocol limit"))
	}
	response, decodeErr := decodeGuestResponse(output.buffer.Bytes())
	if decodeErr != nil {
		return nil, errors.Join(err, fmt.Errorf("decode guest response: %w", decodeErr))
	}
	if !response.OK {
		if response.Error == nil {
			return nil, errors.New("guest helper returned an empty failure")
		}
		return nil, &Error{Code: response.Error.Code, Message: response.Error.Message, Status: 1, Cause: err}
	}
	return response.Result, err
}

func (o *defaultOperations) guestShell(ctx context.Context, def Definition, stdin io.Reader, argv ...string) error {
	_, err := o.runGuest(ctx, def, stdin, o.stdout, o.stderr, argv...)
	return err
}

func (o *defaultOperations) runGuest(ctx context.Context, def Definition, stdin io.Reader, stdout, stderr io.Writer, argv ...string) (int, error) {
	args := []string{"shell", def.Name, "--"}
	args = append(args, argv...)
	return o.runLima(ctx, def, stdin, stdout, stderr, args...)
}

func (o *defaultOperations) runLima(ctx context.Context, def Definition, stdin io.Reader, stdout, stderr io.Writer, args ...string) (int, error) {
	err := o.runner.Run(ctx, process.Spec{Name: "limactl", Args: args, Env: []string{"LIMA_HOME=" + filepath.Join(def.Home, "lima")}, Stdin: stdin, Stdout: stdout, Stderr: stderr})
	if status, ok := process.ExitStatus(err); ok {
		return status, err
	}
	return 1, err
}
