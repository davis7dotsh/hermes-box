package app

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"filippo.io/age"
	"github.com/davis7dotsh/hermes-box/internal/artifacts"
	"github.com/davis7dotsh/hermes-box/internal/backup"
	"github.com/davis7dotsh/hermes-box/internal/component"
	"github.com/davis7dotsh/hermes-box/internal/config"
	"github.com/davis7dotsh/hermes-box/internal/guestupdate"
	"github.com/davis7dotsh/hermes-box/internal/keychain"
	"github.com/davis7dotsh/hermes-box/internal/process"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"gopkg.in/yaml.v3"
)

type defaultBackups struct {
	operations *defaultOperations
}

type bufferedReadCloser struct {
	io.Reader
	io.Closer
}

type transactionSnapshot struct {
	Backup      BackupResult `json:"backup"`
	AppliedLock string       `json:"applied_lock"`
}

func (b *defaultBackups) Create(ctx context.Context, def Definition, label string) (BackupResult, error) {
	if b.operations.keys == nil {
		return BackupResult{}, errors.New("macOS Keychain is unavailable")
	}
	identity, identityCreated, err := keychain.LoadOrCreateIdentity(b.operations.keys, identityAccount(def))
	if err != nil {
		return BackupResult{}, err
	}
	retainIdentity := false
	defer func() {
		if identityCreated && !retainIdentity {
			_ = b.operations.keys.Delete(identityAccount(def))
		}
	}()
	staging, err := os.MkdirTemp(def.Home, ".backup-source-")
	if err != nil {
		return BackupResult{}, err
	}
	defer os.RemoveAll(staging)
	componentName := ""
	for _, prefix := range []string{"transaction-", "pre-rollback-"} {
		if strings.HasPrefix(label, prefix) {
			componentName = strings.TrimPrefix(label, prefix)
		}
	}
	var dataPaths []string
	if componentName != "" {
		dataPaths = component.SnapshotPaths(component.Name(componentName))
		if len(dataPaths) == 0 {
			return BackupResult{}, fmt.Errorf("component %q has no snapshot contract", componentName)
		}
	}
	appliedLock := filepath.Join(staging, "applied.lock")
	client, err := b.operations.client(def)
	if err != nil {
		return BackupResult{}, err
	}
	if err := client.Copy(ctx, false, []string{def.Name + ":/var/lib/hermes-box/applied.lock"}, appliedLock); err != nil {
		return BackupResult{}, err
	}
	applied, err := config.LoadLock(appliedLock)
	if err != nil {
		return BackupResult{}, err
	}
	closure, err := materializeBackupClosure(ctx, def, applied)
	if err != nil {
		return BackupResult{}, err
	}
	directory := filepath.Join(def.Home, "backups", def.Name)
	keep := def.Bundle.Config.Backup.Keep
	if strings.HasPrefix(label, "transaction-") || strings.HasPrefix(label, "pre-rollback-") {
		directory = filepath.Join(directory, "transactions", "archives", sanitizePin(label))
		keep = 1
	}
	dataTar, streamDone, err := b.openGuestDataTar(ctx, def, componentName)
	if err != nil {
		return BackupResult{}, err
	}
	var streamErr error
	var waitStreamOnce sync.Once
	waitStream := func() error {
		waitStreamOnce.Do(func() { streamErr = <-streamDone })
		return streamErr
	}
	defer func() {
		_ = dataTar.Close()
		_ = waitStream()
	}()
	result, createErr := backup.Create(ctx, backup.Source{DataTar: dataTar, WaitDataTar: waitStream, DataPaths: dataPaths, AppliedLock: appliedLock, Artifacts: closure}, backup.CreateOptions{
		Directory: directory, Box: def.Name, Label: label, Keep: keep,
		Recipient: identity.Recipient(), Identity: identity,
		RecipientFingerprint: keychain.RecipientFingerprint(identity.Recipient()),
		ValidateClosure:      validateBackupClosure,
	})
	mapped, retain, mappedErr := mapBackupCreateResult(result, createErr, b.operations.stderr)
	retainIdentity = retain
	return mapped, mappedErr
}

func mapBackupCreateResult(result backup.Result, createErr error, stderr io.Writer) (BackupResult, bool, error) {
	mapped := BackupResult{Archive: result.Archive, Envelope: result.Envelope, ArchiveSHA256: result.ArchiveSHA256}
	complete := mapped.Archive != "" && mapped.Envelope != "" && mapped.ArchiveSHA256 != ""
	if createErr == nil {
		if !complete {
			return mapped, false, errors.New("backup creation returned an incomplete verified result")
		}
		return mapped, true, nil
	}
	var warning *backup.RetentionWarning
	if errors.As(createErr, &warning) && complete {
		if stderr != nil {
			fmt.Fprintf(stderr, "[hermes-box] WARNING: %v\n", warning)
		}
		return mapped, true, nil
	}
	return mapped, complete || publishedBackupPresent(createErr), createErr
}

func publishedBackupPresent(err error) bool {
	var publicationError *backup.PublicationError
	return errors.As(err, &publicationError) && (publicationError.ArchivePresent || publicationError.EnvelopePresent)
}

func (b *defaultBackups) LatestVerified(ctx context.Context, def Definition) (*BackupResult, error) {
	if b.operations.keys == nil {
		return nil, errors.New("macOS Keychain is unavailable")
	}
	identity, err := keychain.LoadIdentity(b.operations.keys, identityAccount(def))
	if err != nil {
		return nil, err
	}
	envelopes, err := filepath.Glob(filepath.Join(def.Home, "backups", def.Name, "*.envelope.json"))
	if err != nil {
		return nil, err
	}
	if len(envelopes) == 0 {
		return nil, errors.New("no valid backup exists")
	}
	var latest *BackupResult
	var latestCreated time.Time
	for _, envelopePath := range envelopes {
		data, readErr := os.ReadFile(envelopePath)
		if readErr != nil {
			continue
		}
		var envelope backup.Envelope
		if json.Unmarshal(data, &envelope) != nil {
			continue
		}
		archivePath := filepath.Join(filepath.Dir(envelopePath), envelope.Archive)
		verified, verifyErr := backup.Verify(ctx, archivePath, envelopePath, identity, validateBackupClosure)
		if verifyErr != nil {
			continue
		}
		created := verified.Manifest.CreatedAt
		_ = verified.Cleanup()
		// CreatedAt is the semantic ordering authority. The filename has only
		// second precision, so use the full envelope path as a stable tie-breaker
		// when two independently valid backups share the same timestamp.
		if latest == nil || newerVerifiedBackup(created, envelopePath, latestCreated, latest.Envelope) {
			latestCreated = created
			latest = &BackupResult{Archive: archivePath, Envelope: envelopePath, ArchiveSHA256: envelope.ArchiveSHA256}
		}
	}
	if latest != nil {
		return latest, nil
	}
	return nil, errors.New("no valid backup exists")
}

func newerVerifiedBackup(candidateCreated time.Time, candidatePath string, currentCreated time.Time, currentPath string) bool {
	return candidateCreated.After(currentCreated) || candidateCreated.Equal(currentCreated) && candidatePath > currentPath
}

func (b *defaultBackups) openGuestDataTar(ctx context.Context, def Definition, componentName string) (io.ReadCloser, <-chan error, error) {
	request := guestupdate.Request{Schema: 1, Operation: "backup-stream"}
	if componentName != "" {
		request.Component = component.Name(componentName)
	}
	requestData, err := json.Marshal(request)
	if err != nil {
		return nil, nil, err
	}
	pipeReader, pipeWriter := io.Pipe()
	runDone := make(chan error, 1)
	go func() {
		runDone <- b.operations.runner.Run(ctx, process.Spec{
			Name: "limactl", Args: []string{"shell", def.Name, "--", "sudo", "systemd-run", "--pipe", "--wait", "--quiet", "--collect", "--service-type=exec", "--unit", fmt.Sprintf("hermes-box-backup-%d-%d", os.Getpid(), time.Now().UnixNano()), "/usr/local/libexec/hermes-box-guest"},
			Env: []string{"LIMA_HOME=" + filepath.Join(def.Home, "lima")}, Stdin: strings.NewReader(string(requestData) + "\n"),
			Stdout: pipeWriter, Stderr: b.operations.stderr,
		})
		_ = pipeWriter.Close()
	}()
	reader := bufio.NewReaderSize(pipeReader, 64*1024)
	headerLine, err := reader.ReadSlice('\n')
	if err != nil {
		_ = pipeReader.CloseWithError(err)
		<-runDone
		return nil, nil, err
	}
	response, decodeErr := decodeGuestResponse(headerLine)
	result, resultOK := response.Result.(map[string]any)
	validHeader := decodeErr == nil && response.OK && resultOK && len(result) == 3 &&
		result["stream"] == "tar" && result["framing"] == "direct" && result["owner"] == "guest"
	if !validHeader {
		_ = pipeReader.Close()
		<-runDone
		if decodeErr != nil {
			return nil, nil, decodeErr
		}
		return nil, nil, fmt.Errorf("guest backup returned an invalid direct-stream header: error=%v result=%v", response.Error, response.Result)
	}
	return &bufferedReadCloser{Reader: reader, Closer: pipeReader}, runDone, nil
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func validateBackupClosure(lockData []byte, closure []backup.Artifact) error {
	var lock config.Lock
	decoder := yaml.NewDecoder(bytes.NewReader(lockData))
	decoder.KnownFields(true)
	if err := decoder.Decode(&lock); err != nil {
		return fmt.Errorf("decode archived applied lock: %w", err)
	}
	if err := lock.Validate(); err != nil {
		return fmt.Errorf("validate archived applied lock: %w", err)
	}
	if len(closure) != 10 {
		return fmt.Errorf("backup artifact closure has %d entries, want 10", len(closure))
	}
	available := make(map[string]backup.Artifact, len(closure))
	for _, artifact := range closure {
		if artifact.Name == "" || artifact.SHA256 == "" || artifact.Path == "" {
			return errors.New("backup artifact closure contains an empty identity")
		}
		if _, duplicate := available[artifact.Name]; duplicate {
			return fmt.Errorf("backup artifact closure duplicates %q", artifact.Name)
		}
		actual, err := hashFile(artifact.Path)
		if err != nil || actual != artifact.SHA256 {
			return fmt.Errorf("backup artifact %q bytes do not match its SHA-256", artifact.Name)
		}
		available[artifact.Name] = artifact
	}
	required := map[string]string{
		"ubuntu-image": lock.Ubuntu.SHA256, "provisioner": lock.Ubuntu.ProvisionerSHA256,
		"node": lock.Tooling.Node.SHA256, "uv": lock.Tooling.UV.SHA256, "codex": lock.Codex.SHA256,
		"hermes": lock.Hermes.SHA256, "hermes-python": lock.Hermes.PythonSHA256,
		"hermes-wheels": lock.Hermes.WheelsSHA256,
	}
	for name, digest := range required {
		artifact, ok := available[name]
		if !ok || artifact.SHA256 != digest {
			return fmt.Errorf("backup artifact closure does not exactly bind %s", name)
		}
	}
	encodedSRI, ok := strings.CutPrefix(lock.Claude.Integrity, "sha512-")
	if !ok {
		return errors.New("archived Claude integrity is not sha512 SRI")
	}
	sri, err := base64.StdEncoding.DecodeString(encodedSRI)
	if err != nil || len(sri) != sha512.Size {
		return errors.New("archived Claude integrity is invalid")
	}
	claude, ok := available["claude"]
	if !ok {
		return errors.New("backup artifact closure is missing Claude")
	}
	info, err := os.Lstat(claude.Path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxClaudeTarballBytes {
		return errors.New("backup Claude artifact is not a bounded regular file")
	}
	claudeFile, err := os.Open(claude.Path)
	if err != nil {
		return err
	}
	claudeHash := sha512.New()
	written, readErr := io.Copy(claudeHash, io.LimitReader(claudeFile, maxClaudeTarballBytes+1))
	closeErr := claudeFile.Close()
	if readErr != nil || closeErr != nil || written != info.Size() || written > maxClaudeTarballBytes {
		return errors.Join(readErr, closeErr, errors.New("read bounded backup Claude artifact"))
	}
	if !bytes.Equal(claudeHash.Sum(nil), sri) {
		return errors.New("backup Claude bytes do not match reviewed SRI")
	}
	executor, ok := available["executor"]
	if !ok {
		return errors.New("backup artifact closure is missing Executor")
	}
	image, err := tarball.ImageFromPath(executor.Path, nil)
	if err != nil {
		return fmt.Errorf("read Executor OCI tar: %w", err)
	}
	digest, err := image.Digest()
	if err != nil || digest.String() != lock.Executor.LinuxARM64Digest {
		return errors.New("backup Executor OCI bytes do not match reviewed child digest")
	}
	return nil
}

func materializeBackupClosure(ctx context.Context, def Definition, lock config.Lock) ([]backup.Artifact, error) {
	store := artifacts.Store{Root: filepath.Join(def.Home, "artifacts")}
	references := []artifacts.Reference{
		{Name: "ubuntu-image", URL: lock.Ubuntu.Image, SHA256: lock.Ubuntu.SHA256},
		{Name: "provisioner", URL: lock.Ubuntu.Provisioner, SHA256: lock.Ubuntu.ProvisionerSHA256},
		{Name: "node", URL: lock.Tooling.Node.Archive, SHA256: lock.Tooling.Node.SHA256},
		{Name: "uv", URL: lock.Tooling.UV.Archive, SHA256: lock.Tooling.UV.SHA256},
		{Name: "codex", URL: lock.Codex.Archive, SHA256: lock.Codex.SHA256},
		{Name: "hermes", URL: lock.Hermes.Archive, SHA256: lock.Hermes.SHA256},
		{Name: "hermes-python", URL: lock.Hermes.PythonArchive, SHA256: lock.Hermes.PythonSHA256},
		{Name: "hermes-wheels", URL: lock.Hermes.WheelsArchive, SHA256: lock.Hermes.WheelsSHA256},
	}
	set, err := store.Materialize(ctx, references)
	if err != nil {
		return nil, err
	}
	closure := make([]backup.Artifact, 0, 10)
	for _, reference := range references {
		artifact := set[reference.Name]
		closure = append(closure, backup.Artifact{Name: reference.Name, SHA256: reference.SHA256, Path: artifact.Path})
	}
	claudePath, err := fetchSRI(ctx, store.Root, lock.Claude.Tarball, lock.Claude.Integrity)
	if err != nil {
		return nil, err
	}
	claudeSHA, err := hashFile(claudePath)
	if err != nil {
		return nil, err
	}
	closure = append(closure, backup.Artifact{Name: "claude", SHA256: claudeSHA, Path: claudePath})
	indexDigest := strings.SplitN(lock.Executor.Image, "@", 2)[1]
	executor, err := store.MaterializeOCI(ctx, lock.Executor.Image, indexDigest, lock.Executor.LinuxARM64Digest)
	if err != nil {
		return nil, err
	}
	closure = append(closure, backup.Artifact{Name: "executor", SHA256: executor.ArchiveSHA256, Path: executor.Path})
	return closure, validateBackupClosure(mustEncodeLock(lock), closure)
}

func mustEncodeLock(lock config.Lock) []byte {
	data, _ := yaml.Marshal(lock)
	return data
}

func parseIdentity(path string) (*age.X25519Identity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return age.ParseX25519Identity(strings.TrimSpace(string(data)))
}

func saveTransactionSnapshot(def Definition, target string, result BackupResult, appliedLock string) error {
	directory := filepath.Join(def.Home, "backups", def.Name, "transactions", sanitizePin(target))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	lockData, err := os.ReadFile(appliedLock)
	if err != nil {
		return err
	}
	return writeTransactionSnapshot(filepath.Join(directory, "latest.json"), transactionSnapshot{Backup: result, AppliedLock: string(lockData)})
}

func writeTransactionSnapshot(path string, snapshot transactionSnapshot) error {
	if snapshot.Backup.Archive == "" || snapshot.Backup.Envelope == "" || snapshot.Backup.ArchiveSHA256 == "" || snapshot.AppliedLock == "" {
		return errors.New("transaction snapshot record is incomplete")
	}
	var lock config.Lock
	decoder := yaml.NewDecoder(strings.NewReader(snapshot.AppliedLock))
	decoder.KnownFields(true)
	if err := decoder.Decode(&lock); err != nil {
		return fmt.Errorf("decode transaction snapshot lock: %w", err)
	}
	if err := lock.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".snapshot-")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	directoryHandle, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer directoryHandle.Close()
	return directoryHandle.Sync()
}

func (o *defaultOperations) restoreTransactionSnapshot(ctx context.Context, def Definition, target string) error {
	return o.restoreTransactionSnapshotRecord(ctx, def, target, transactionSnapshotPath(def, target))
}

func transactionSnapshotPath(def Definition, target string) string {
	return filepath.Join(def.Home, "backups", def.Name, "transactions", sanitizePin(target), "latest.json")
}

func invalidateOverlappingTransactionSnapshots(def Definition, target component.Name) error {
	for _, overlapping := range component.OverlappingSnapshotComponents(target) {
		path := transactionSnapshotPath(def, string(overlapping))
		if err := os.Remove(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return fmt.Errorf("invalidate overlapping %s transaction snapshot: %w", overlapping, err)
		}
		directory, err := os.Open(filepath.Dir(path))
		if err != nil {
			return fmt.Errorf("open overlapping %s snapshot directory: %w", overlapping, err)
		}
		syncErr := directory.Sync()
		closeErr := directory.Close()
		if syncErr != nil || closeErr != nil {
			return fmt.Errorf("persist overlapping %s snapshot invalidation: %w", overlapping, errors.Join(syncErr, closeErr))
		}
	}
	return nil
}

func loadTransactionSnapshot(path string) (transactionSnapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return transactionSnapshot{}, err
	}
	var snapshot transactionSnapshot
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return transactionSnapshot{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return transactionSnapshot{}, errors.New("transaction snapshot contains trailing JSON")
	}
	if snapshot.Backup.Archive == "" || snapshot.Backup.Envelope == "" || snapshot.Backup.ArchiveSHA256 == "" || snapshot.AppliedLock == "" {
		return transactionSnapshot{}, errors.New("transaction snapshot record is incomplete")
	}
	return snapshot, nil
}

func (o *defaultOperations) restoreTransactionSnapshotRecord(ctx context.Context, def Definition, target, recordPath string) error {
	if o.keys == nil {
		return errors.New("macOS Keychain is unavailable")
	}
	record, err := loadTransactionSnapshot(recordPath)
	if err != nil {
		return fmt.Errorf("read retained transaction snapshot: %w", err)
	}
	identity, err := keychain.LoadIdentity(o.keys, identityAccount(def))
	if err != nil {
		return err
	}
	bundle, err := backup.Verify(ctx, record.Backup.Archive, record.Backup.Envelope, identity, validateBackupClosure)
	if err != nil {
		return fmt.Errorf("verify retained transaction snapshot: %w", err)
	}
	defer bundle.Cleanup()
	if err := verifyTransactionSnapshotArchive(record, bundle.Envelope); err != nil {
		return err
	}
	if len(component.SnapshotPaths(component.Name(target))) == 0 {
		return fmt.Errorf("component %q has no snapshot contract", target)
	}
	reader, writer := io.Pipe()
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- errors.Join(backup.WriteDataTar(ctx, bundle.Root, bundle.Manifest, writer), writer.Close())
	}()
	request, err := json.Marshal(guestupdate.Request{Schema: 1, Operation: "restore-paths", Component: component.Name(target)})
	if err != nil {
		return err
	}
	stdin := io.MultiReader(strings.NewReader(string(request)+"\n"), reader)
	var output boundedProtocolBuffer
	runErr := o.runner.Run(ctx, process.Spec{
		Name: "limactl", Args: []string{"shell", def.Name, "--", "sudo", "/usr/local/libexec/hermes-box-guest"},
		Env: []string{"LIMA_HOME=" + filepath.Join(def.Home, "lima")}, Stdin: stdin, Stdout: &output, Stderr: o.stderr,
	})
	_ = reader.CloseWithError(runErr)
	writeErr := <-writeDone
	if runErr != nil || writeErr != nil {
		return errors.Join(runErr, writeErr)
	}
	if output.exceeded {
		return errors.New("guest scoped-restore response exceeds 4 MiB protocol limit")
	}
	response, err := decodeGuestResponse(output.buffer.Bytes())
	if err != nil {
		return err
	}
	if !response.OK {
		return fmt.Errorf("guest scoped restore failed: %v", response.Error)
	}
	var restored struct {
		Restored bool `json:"restored"`
	}
	if err := decodeGuestResult(response.Result, &restored); err != nil || !restored.Restored {
		return errors.Join(err, errors.New("guest scoped restore did not confirm restoration"))
	}
	return nil
}

func verifyTransactionSnapshotArchive(record transactionSnapshot, envelope backup.Envelope) error {
	if envelope.ArchiveSHA256 != record.Backup.ArchiveSHA256 {
		return errors.New("retained transaction snapshot checksum changed")
	}
	return nil
}

func (o *defaultOperations) restoreAndConfirmTransactionSnapshot(ctx context.Context, def Definition, target, recordPath string) (guestupdate.Status, error) {
	record, err := loadTransactionSnapshot(recordPath)
	if err != nil {
		return guestupdate.Status{}, fmt.Errorf("load transaction recovery state: %w", err)
	}
	var desired config.Lock
	decoder := yaml.NewDecoder(strings.NewReader(record.AppliedLock))
	decoder.KnownFields(true)
	if err := decoder.Decode(&desired); err != nil {
		return guestupdate.Status{}, fmt.Errorf("decode transaction recovery lock: %w", err)
	}
	if err := desired.Validate(); err != nil {
		return guestupdate.Status{}, fmt.Errorf("validate transaction recovery lock: %w", err)
	}
	if err := o.restoreTransactionSnapshotRecord(ctx, def, target, recordPath); err != nil {
		return guestupdate.Status{}, err
	}
	return o.confirmTransactionActivation(ctx, def, target, record, desired)
}

func (o *defaultOperations) confirmTransactionActivation(ctx context.Context, def Definition, target string, record transactionSnapshot, desired config.Lock) (guestupdate.Status, error) {
	_, recoverErr := o.guestRequest(ctx, def, guestupdate.Request{Schema: 1, Operation: "recover"})
	status, statusErr := o.readGuestStatus(ctx, def)
	if statusErr != nil {
		return guestupdate.Status{}, errors.Join(recoverErr, fmt.Errorf("confirm guest state after recovery: %w", statusErr))
	}
	if status.Pending != nil || status.RestorePending != nil {
		return guestupdate.Status{}, errors.Join(recoverErr, errors.New("guest recovery left a durable transaction unresolved"))
	}
	desiredPin := lockPin(desired, target)
	if status.Applied.Components[component.Name(target)] != desiredPin {
		_, rollbackErr := o.guestRequest(ctx, def, guestupdate.Request{
			Schema: 1, Operation: "rollback", Component: component.Name(target), SnapshotReady: true, ReviewedLock: record.AppliedLock,
		})
		status, statusErr = o.readGuestStatus(ctx, def)
		if statusErr != nil {
			return guestupdate.Status{}, errors.Join(recoverErr, rollbackErr, fmt.Errorf("confirm guest state after rollback: %w", statusErr))
		}
		if status.Pending != nil || status.RestorePending != nil || status.Applied.Components[component.Name(target)] != desiredPin {
			return guestupdate.Status{}, errors.Join(recoverErr, rollbackErr, errors.New("guest did not confirm the retained pre-mutation activation"))
		}
	}
	if err := writeLock(hostAppliedLockPath(def), desired); err != nil {
		return guestupdate.Status{}, fmt.Errorf("publish recovered host applied lock: %w", err)
	}
	return status, nil
}

func importRestoredArtifacts(home, source string, manifest backup.Manifest, lock config.Lock) error {
	entries, err := os.ReadDir(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || len(entry.Name()) != 64 {
			return fmt.Errorf("invalid restored artifact %q", entry.Name())
		}
		destinationDir := filepath.Join(home, "artifacts", "sha256", entry.Name()[:2])
		if err := os.MkdirAll(destinationDir, 0o700); err != nil {
			return err
		}
		destination := filepath.Join(destinationDir, entry.Name())
		if _, err := os.Stat(destination); err == nil {
			continue
		}
		if err := copyRegularFile(filepath.Join(source, entry.Name()), destination); err != nil {
			return err
		}
	}
	byName := make(map[string]backup.Artifact, len(manifest.Artifacts))
	for _, artifact := range manifest.Artifacts {
		byName[artifact.Name] = artifact
	}
	claude, ok := byName["claude"]
	if !ok {
		return errors.New("restored closure is missing Claude")
	}
	sri, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(lock.Claude.Integrity, "sha512-"))
	if err != nil || len(sri) != sha512.Size {
		return errors.New("restored lock has invalid Claude SRI")
	}
	claudeName := hex.EncodeToString(sri)
	claudeDir := filepath.Join(home, "artifacts", "sha512", claudeName[:2])
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		return err
	}
	if err := copyReplace(filepath.Join(source, claude.SHA256), filepath.Join(claudeDir, claudeName), 0o600); err != nil {
		return err
	}
	executor, ok := byName["executor"]
	if !ok {
		return errors.New("restored closure is missing Executor")
	}
	executorName := strings.TrimPrefix(lock.Executor.LinuxARM64Digest, "sha256:") + ".tar"
	executorDir := filepath.Join(home, "artifacts", "oci")
	if err := os.MkdirAll(executorDir, 0o700); err != nil {
		return err
	}
	if err := copyReplace(filepath.Join(source, executor.SHA256), filepath.Join(executorDir, executorName), 0o600); err != nil {
		return err
	}
	return nil
}

func (o *defaultOperations) restoreDataStream(ctx context.Context, def Definition, bundleRoot string, manifest backup.Manifest, replaceExisting bool) error {
	request, err := json.Marshal(guestupdate.Request{Schema: 1, Operation: "restore-stream", ReplaceExisting: replaceExisting})
	if err != nil {
		return err
	}
	reader, writer := io.Pipe()
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- errors.Join(backup.WriteDataTar(ctx, bundleRoot, manifest, writer), writer.Close())
	}()
	stdin := io.MultiReader(strings.NewReader(string(request)+"\n"), reader)
	var output boundedProtocolBuffer
	runErr := o.runner.Run(ctx, process.Spec{
		Name: "limactl", Args: []string{"shell", def.Name, "--", "sudo", "/usr/local/libexec/hermes-box-guest"},
		Env: []string{"LIMA_HOME=" + filepath.Join(def.Home, "lima")}, Stdin: stdin, Stdout: &output, Stderr: o.stderr,
	})
	_ = reader.CloseWithError(runErr)
	writeErr := <-writeDone
	if runErr != nil || writeErr != nil {
		return errors.Join(runErr, writeErr)
	}
	if output.exceeded {
		return errors.New("guest restore response exceeds 4 MiB protocol limit")
	}
	response, err := decodeGuestResponse(output.buffer.Bytes())
	if err != nil {
		return err
	}
	if !response.OK {
		return fmt.Errorf("guest restore failed: %v", response.Error)
	}
	var restored struct {
		Restored        bool `json:"restored"`
		ReplaceExisting bool `json:"replace_existing"`
	}
	if err := decodeGuestResult(response.Result, &restored); err != nil || !restored.Restored || restored.ReplaceExisting != replaceExisting {
		return errors.Join(err, errors.New("guest restore did not confirm restoration"))
	}
	return nil
}
