package app

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	backupFormat          = "hermes-box-v2"
	restoreCleanupTimeout = 2 * time.Minute
	// Final restore and rollback include the full Executor startup path, plus
	// machine creation and archive transfer around it.
	restoreRecoveryTimeout = 3 * time.Hour
)

var backupFiles = []string{
	"rootfs.tar.gz",
	"rootfs-files.txt",
	"workspace.tar.gz",
	"snapshot-warnings.log",
	"manifest.txt",
}

func (a *App) cmdSnapshot(ctx context.Context, args []string) error {
	if len(args) > 1 {
		return errors.New("snapshot accepts at most one label")
	}
	label := "manual"
	if len(args) == 1 {
		label = args[0]
	}
	backupDir, err := a.snapshotInternal(ctx, label, true)
	if err != nil {
		return err
	}
	fmt.Fprintln(a.stdout, backupDir)
	return nil
}

func (a *App) snapshotInternal(ctx context.Context, label string, restartAfter bool) (backupDir string, err error) {
	originalStdout := a.stdout
	a.stdout = a.stderr
	defer func() {
		a.stdout = originalStdout
	}()

	name := a.config.MachineName
	if err := a.requireSSHKey(ctx); err != nil {
		return "", err
	}
	sshFingerprint, err := a.sshFingerprint(ctx)
	if err != nil {
		return "", err
	}
	exists, err := a.machineExists(ctx, name)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fmt.Errorf("machine does not exist: %s", name)
	}
	wasRunning, err := a.isRunning(ctx, name)
	if err != nil {
		return "", err
	}
	if !wasRunning {
		if err := a.startNamedMachine(ctx, name, a.config.SSHPort); err != nil {
			return "", err
		}
	}

	servicesStopped := false
	machineStopped := false
	createdBackupDir := ""
	defer func() {
		if err == nil {
			return
		}
		if createdBackupDir != "" {
			_ = os.RemoveAll(createdBackupDir)
		}
		var recoveryErr error
		if wasRunning {
			if machineStopped {
				recoveryCtx, cancel := context.WithTimeout(context.Background(), restoreRecoveryTimeout)
				recoveryErr = a.startNamedMachine(recoveryCtx, name, a.config.SSHPort)
				cancel()
			} else if servicesStopped {
				var running bool
				recoveryErr = withRestoreCleanupContext(func(cleanupCtx context.Context) error {
					var stateErr error
					running, stateErr = a.isRunning(cleanupCtx, name)
					return stateErr
				})
				if recoveryErr == nil && !running {
					recoveryCtx, cancel := context.WithTimeout(context.Background(), restoreRecoveryTimeout)
					recoveryErr = a.startNamedMachine(recoveryCtx, name, a.config.SSHPort)
					cancel()
				} else if recoveryErr == nil {
					recoveryErr = withRestoreCleanupContext(func(cleanupCtx context.Context) error {
						return a.runQuiet(cleanupCtx, "smolvm", "machine", "exec", "--name", name, "--", "supervisorctl", "start", "all")
					})
				}
			}
		} else if !machineStopped {
			recoveryErr = withRestoreCleanupContext(func(cleanupCtx context.Context) error {
				return a.stopNamedMachine(cleanupCtx, name)
			})
		}
		if recoveryErr != nil {
			err = errors.Join(err, fmt.Errorf("recover machine after snapshot failure: %w", recoveryErr))
		}
	}()

	stamp := time.Now().Format("20060102-150405")
	safeLabel := sanitizeLabel(label)
	if safeLabel == "" {
		safeLabel = "snapshot"
	}
	backupDir, err = createUniqueBackupDirectory(
		a.backupsDir,
		fmt.Sprintf("%s-%s-%s", name, stamp, safeLabel),
	)
	if err != nil {
		return "", err
	}
	createdBackupDir = backupDir

	a.log("quiescing Hermes and archiving the merged root filesystem")
	remoteScript := "/tmp/hermes-box-snapshot.sh"
	if err := a.run(ctx, "smolvm", "machine", "cp", filepath.Join(a.root, "guest", "snapshot.sh"), name+":"+remoteScript); err != nil {
		return "", err
	}
	cleanupRemoteSnapshot := func(cleanupCtx context.Context) error {
		return a.runQuiet(
			cleanupCtx,
			"smolvm",
			"machine", "exec",
			"--name", name,
			"--",
			"rm", "-f",
			remoteScript,
			"/workspace/.hermes-box-rootfs.tar.gz",
			"/tmp/hermes-box-workspace.tar.gz",
			"/tmp/hermes-box-snapshot-warnings.log",
		)
	}
	remoteCleanupPending := true
	defer func() {
		if remoteCleanupPending {
			_ = withRestoreCleanupContext(cleanupRemoteSnapshot)
		}
	}()

	servicesStopped = true
	if err := a.run(ctx, "smolvm", "machine", "exec", "--name", name, "--", "supervisorctl", "stop", "all"); err != nil {
		return "", fmt.Errorf("stop guest services: %w", err)
	}
	if err := a.run(
		ctx,
		"smolvm",
		"machine", "exec",
		"--stream",
		"--name", name,
		"--",
		"bash", remoteScript,
	); err != nil {
		return "", fmt.Errorf("archive guest: %w", err)
	}

	transfers := [][2]string{
		{name + ":/workspace/.hermes-box-rootfs.tar.gz", filepath.Join(backupDir, "rootfs.tar.gz")},
		{name + ":/tmp/hermes-box-workspace.tar.gz", filepath.Join(backupDir, "workspace.tar.gz")},
		{name + ":/tmp/hermes-box-snapshot-warnings.log", filepath.Join(backupDir, "snapshot-warnings.log")},
	}
	for _, transfer := range transfers {
		if err := a.run(ctx, "smolvm", "machine", "cp", transfer[0], transfer[1]); err != nil {
			return "", err
		}
	}

	warnings, err := os.ReadFile(filepath.Join(backupDir, "snapshot-warnings.log"))
	if err != nil {
		return "", fmt.Errorf("read snapshot warnings: %w", err)
	}
	if len(warnings) != 0 {
		return "", errors.New("snapshot produced warnings; backup discarded")
	}
	rootfsFiles, err := archiveFileNamesContext(ctx, filepath.Join(backupDir, "rootfs.tar.gz"))
	if err != nil {
		return "", fmt.Errorf("inspect rootfs archive: %w", err)
	}
	workspaceFiles, err := archiveFileNamesContext(ctx, filepath.Join(backupDir, "workspace.tar.gz"))
	if err != nil {
		return "", fmt.Errorf("inspect workspace archive: %w", err)
	}
	if err := verifyWorkspaceContentsContext(ctx, workspaceFiles, true); err != nil {
		return "", err
	}
	if err := writeLines(filepath.Join(backupDir, "rootfs-files.txt"), rootfsFiles); err != nil {
		return "", err
	}

	if err := withRestoreCleanupContext(cleanupRemoteSnapshot); err != nil {
		return "", fmt.Errorf("remove guest snapshot artifacts: %w", err)
	}
	remoteCleanupPending = false
	if err := a.run(ctx, "smolvm", "machine", "stop", "--name", name); err != nil {
		return "", err
	}
	machineStopped = true

	manifest := fmt.Sprintf(
		"format=%s\ncreated=%s\nmachine=%s\nsmolvm=%s\ngoos=%s\nssh_key_fingerprint=%s\n",
		backupFormat,
		time.Now().UTC().Format(time.RFC3339),
		name,
		a.smolvmVersion(ctx),
		goos(),
		sshFingerprint,
	)
	if err := os.WriteFile(filepath.Join(backupDir, "manifest.txt"), []byte(manifest), 0o600); err != nil {
		return "", fmt.Errorf("write backup manifest: %w", err)
	}
	if err := writeChecksumsContext(ctx, backupDir); err != nil {
		return "", err
	}
	for _, file := range append(append([]string{}, backupFiles...), "SHA256SUMS") {
		if err := ensurePrivateFile(filepath.Join(backupDir, file)); err != nil {
			return "", err
		}
	}
	createdBackupDir = ""

	if restartAfter && wasRunning {
		if err := a.startNamedMachine(ctx, name, a.config.SSHPort); err != nil {
			return "", fmt.Errorf("snapshot saved at %s, but failed to restart machine: %w", backupDir, err)
		}
		machineStopped = false
		servicesStopped = false
	}
	return backupDir, nil
}

func createUniqueBackupDirectory(parent, base string) (string, error) {
	for sequence := 1; ; sequence++ {
		suffix := ""
		if sequence > 1 {
			suffix = fmt.Sprintf("-%d", sequence)
		}
		directory := filepath.Join(parent, base+suffix+".hermesbox")
		if err := os.Mkdir(directory, 0o700); err != nil {
			if os.IsExist(err) {
				continue
			}
			return "", fmt.Errorf("create backup directory: %w", err)
		}
		return directory, nil
	}
}

func (a *App) stageVerifiedBackup(source string) (string, error) {
	return a.stageVerifiedBackupContext(context.Background(), source)
}

func (a *App) stageVerifiedBackupContext(
	ctx context.Context,
	source string,
) (staged string, err error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := requireDirectory(source); err != nil {
		return "", err
	}
	checksumPath := filepath.Join(source, "SHA256SUMS")
	if err := requireRegularFile(checksumPath); err != nil {
		return "", err
	}
	checksumContent, err := os.ReadFile(checksumPath)
	if err != nil {
		return "", fmt.Errorf("read backup checksums: %w", err)
	}
	expected, err := parseBackupChecksums(checksumContent)
	if err != nil {
		return "", err
	}

	staged, err = os.MkdirTemp(a.stateDir, ".restore-backup-")
	if err != nil {
		return "", fmt.Errorf("create staged restore backup: %w", err)
	}
	if err := os.Chmod(staged, 0o700); err != nil {
		cleanupErr := withRestoreCleanupContext(func(cleanupCtx context.Context) error {
			return cleanupStagedBackup(cleanupCtx, staged)
		})
		return "", errors.Join(
			fmt.Errorf("secure staged restore backup: %w", err),
			cleanupErr,
		)
	}
	stagedPath := staged
	defer func() {
		if err == nil {
			return
		}
		cleanupErr := withRestoreCleanupContext(func(cleanupCtx context.Context) error {
			return cleanupStagedBackup(cleanupCtx, stagedPath)
		})
		if cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("remove partial staged backup: %w", cleanupErr))
		}
	}()

	if err := os.WriteFile(filepath.Join(staged, "SHA256SUMS"), checksumContent, 0o600); err != nil {
		return "", fmt.Errorf("stage backup checksums: %w", err)
	}
	buffer := make([]byte, 1024*1024)
	for _, name := range backupFiles {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		sourcePath := filepath.Join(source, name)
		if err := requireRegularFile(sourcePath); err != nil {
			return "", err
		}
		actual, err := copyFileWithChecksum(
			ctx,
			sourcePath,
			filepath.Join(staged, name),
			buffer,
		)
		if err != nil {
			return "", err
		}
		if actual != expected[name] {
			return "", fmt.Errorf("checksum mismatch while staging: %s", name)
		}
	}

	manifest, err := validateBackupLayout(staged)
	if err != nil {
		return "", err
	}
	if err := verifyBackupContentsContext(ctx, staged, manifest); err != nil {
		return "", err
	}
	for _, name := range append(append([]string{}, backupFiles...), "SHA256SUMS") {
		if err := os.Chmod(filepath.Join(staged, name), 0o400); err != nil {
			return "", fmt.Errorf("make staged backup read-only: %w", err)
		}
	}
	if err := os.Chmod(staged, 0o500); err != nil {
		return "", fmt.Errorf("make staged backup directory read-only: %w", err)
	}
	return staged, nil
}

func copyFileWithChecksum(
	ctx context.Context,
	source,
	destination string,
	buffer []byte,
) (sum string, err error) {
	input, err := os.Open(source)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", filepath.Base(source), err)
	}
	defer func() { _ = input.Close() }()
	info, err := input.Stat()
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w", filepath.Base(source), err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("backup requires a regular file: %s", source)
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("create staged %s: %w", filepath.Base(destination), err)
	}
	owned := true
	defer func() {
		if err != nil {
			_ = output.Close()
			if owned {
				_ = os.Remove(destination)
			}
		}
	}()
	hash := sha256.New()
	if _, err := io.CopyBuffer(
		io.MultiWriter(output, hash),
		contextReader{ctx: ctx, reader: input},
		buffer,
	); err != nil {
		return "", fmt.Errorf("stage %s: %w", filepath.Base(source), err)
	}
	if err := output.Close(); err != nil {
		return "", fmt.Errorf("finish staged %s: %w", filepath.Base(source), err)
	}
	owned = false
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func withRestoreCleanupContext(cleanup func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), restoreCleanupTimeout)
	defer cancel()
	return cleanup(ctx)
}

func withRestoreOperationContext(
	ctx context.Context,
	operation func(context.Context) error,
) error {
	operationCtx, cancel := context.WithTimeout(ctx, restoreCleanupTimeout)
	defer cancel()
	return operation(operationCtx)
}

func cleanupStagedBackup(ctx context.Context, directory string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Chmod(directory, 0o700); err != nil && !os.IsNotExist(err) {
		return err
	}
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := filepath.Join(directory, entry.Name())
		if entry.IsDir() {
			return fmt.Errorf("staged backup contains unexpected directory: %s", path)
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.Remove(directory)
}

func (a *App) cmdRestore(ctx context.Context, args []string) (err error) {
	return a.cmdRestoreValidated(ctx, args, a.validateCurrentHost)
}

func (a *App) cmdRestoreValidated(
	ctx context.Context,
	args []string,
	validateHost func(context.Context) error,
) (err error) {
	if len(args) != 1 {
		return errors.New("restore requires one .hermesbox directory")
	}
	if err := validateHost(ctx); err != nil {
		return fmt.Errorf("restore preflight: %w", err)
	}
	if _, err := a.readValidatedSecretMappings(); err != nil {
		return fmt.Errorf("restore preflight: %w", err)
	}
	if err := a.validateNetworkMode(); err != nil {
		return err
	}
	sourceBackupDir, err := filepath.Abs(args[0])
	if err != nil {
		return fmt.Errorf("resolve backup path: %w", err)
	}
	if err := requireRegularFile(a.baseArtifact); err != nil {
		return fmt.Errorf("restore requires the local base artifact: %s", a.baseArtifact)
	}
	if err := a.requireSSHKey(ctx); err != nil {
		return err
	}
	backupDir, err := a.stageVerifiedBackupContext(ctx, sourceBackupDir)
	if err != nil {
		return err
	}
	defer func() {
		cleanupErr := withRestoreCleanupContext(func(cleanupCtx context.Context) error {
			return cleanupStagedBackup(cleanupCtx, backupDir)
		})
		if cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("remove staged restore backup: %w", cleanupErr))
		}
	}()
	manifest, err := readKeyValues(filepath.Join(backupDir, "manifest.txt"))
	if err != nil {
		return err
	}
	actualFingerprint, err := a.sshFingerprint(ctx)
	if err != nil {
		return err
	}
	if expectedFingerprint := manifest["ssh_key_fingerprint"]; expectedFingerprint != "" {
		if actualFingerprint != expectedFingerprint {
			return fmt.Errorf(
				"SSH key fingerprint mismatch: backup requires %s, supplied key is %s",
				expectedFingerprint,
				actualFingerprint,
			)
		}
	} else {
		a.log("warning: legacy backup does not record its SSH key fingerprint")
	}
	err = restoreVerifiedBackup(
		ctx,
		a,
		a.config.MachineName,
		a.config.SSHPort,
		backupDir,
	)
	return err
}

type restoreOperations interface {
	machineExists(context.Context, string) (bool, error)
	isRunning(context.Context, string) (bool, error)
	findRestoreCandidatePort() (int, error)
	snapshotInternal(context.Context, string, bool) (string, error)
	createBlankMachine(context.Context, string, int) error
	applyBackup(context.Context, string, string, int) error
	startNamedMachine(context.Context, string, int) error
	stopNamedMachine(context.Context, string) error
	deleteNamedMachine(context.Context, string) error
	clearKnownHost(context.Context, int) error
	restorePrimary(context.Context, string) (bool, error)
	log(string, ...any)
}

func restoreVerifiedBackup(
	ctx context.Context,
	operations restoreOperations,
	machineName string,
	port int,
	backupDir string,
) (err error) {
	exists, err := operations.machineExists(ctx, machineName)
	if err != nil {
		return err
	}
	if !exists {
		operations.log("no existing machine; restoring directly to %s", machineName)
		if err := operations.createBlankMachine(ctx, machineName, port); err != nil {
			return err
		}
		if err := operations.applyBackup(ctx, machineName, backupDir, port); err != nil {
			cleanupErr := removeCreatedRestoreMachine(operations, machineName, port)
			if cleanupErr != nil {
				return fmt.Errorf(
					"restore failed: %v; remove newly created machine: %w",
					err,
					cleanupErr,
				)
			}
			return fmt.Errorf("restore failed; newly created machine was removed: %w", err)
		}
		operations.log("restore completed")
		return nil
	}

	wasRunning, err := operations.isRunning(ctx, machineName)
	if err != nil {
		return err
	}
	candidatePort, err := operations.findRestoreCandidatePort()
	if err != nil {
		return err
	}
	safetyBackup, err := operations.snapshotInternal(ctx, "pre-restore", false)
	if err != nil {
		return err
	}
	operations.log("safety snapshot: %s", safetyBackup)
	cutoverStarted := false
	candidateOwned := false
	candidateDisposed := false
	candidateName := fmt.Sprintf("%s-restore-%d-%d", machineName, os.Getpid(), time.Now().UnixNano())
	defer func() {
		if err == nil || cutoverStarted {
			return
		}
		var recoveryErrors []error
		if candidateOwned && !candidateDisposed {
			if stopErr := withRestoreCleanupContext(func(cleanupCtx context.Context) error {
				return operations.stopNamedMachine(cleanupCtx, candidateName)
			}); stopErr != nil {
				recoveryErrors = append(recoveryErrors, fmt.Errorf("stop restore candidate: %w", stopErr))
			}
		}
		if candidateOwned && !candidateDisposed {
			if clearErr := withRestoreCleanupContext(func(cleanupCtx context.Context) error {
				return operations.clearKnownHost(cleanupCtx, candidatePort)
			}); clearErr != nil {
				recoveryErrors = append(recoveryErrors, fmt.Errorf("clear restore candidate host key: %w", clearErr))
			}
		}
		if candidateOwned && !candidateDisposed {
			if deleteErr := withRestoreCleanupContext(func(cleanupCtx context.Context) error {
				return operations.deleteNamedMachine(cleanupCtx, candidateName)
			}); deleteErr != nil {
				recoveryErrors = append(recoveryErrors, fmt.Errorf("delete restore candidate: %w", deleteErr))
			} else {
				candidateDisposed = true
			}
		}
		if wasRunning {
			recoveryCtx, cancel := context.WithTimeout(context.Background(), restoreRecoveryTimeout)
			restartErr := operations.startNamedMachine(recoveryCtx, machineName, port)
			cancel()
			if restartErr != nil {
				recoveryErrors = append(recoveryErrors, fmt.Errorf("restart original machine: %w", restartErr))
			}
		}
		if len(recoveryErrors) > 0 {
			err = errors.Join(append([]error{err}, recoveryErrors...)...)
		}
	}()

	operations.log("verifying restore candidate %s on port %d", candidateName, candidatePort)
	if err := operations.createBlankMachine(ctx, candidateName, candidatePort); err != nil {
		return err
	}
	candidateOwned = true
	if err := operations.applyBackup(ctx, candidateName, backupDir, candidatePort); err != nil {
		return fmt.Errorf("restore candidate validation failed: %w", err)
	}
	if err := withRestoreCleanupContext(func(cleanupCtx context.Context) error {
		return operations.stopNamedMachine(cleanupCtx, candidateName)
	}); err != nil {
		return err
	}
	if err := withRestoreCleanupContext(func(cleanupCtx context.Context) error {
		return operations.clearKnownHost(cleanupCtx, candidatePort)
	}); err != nil {
		return err
	}
	if err := withRestoreCleanupContext(func(cleanupCtx context.Context) error {
		return operations.deleteNamedMachine(cleanupCtx, candidateName)
	}); err != nil {
		return err
	}
	candidateDisposed = true

	recoveryCtx, cancelRecovery := context.WithTimeout(context.Background(), restoreRecoveryTimeout)
	defer cancelRecovery()
	err = withRestoreOperationContext(recoveryCtx, func(operationCtx context.Context) error {
		var existsErr error
		exists, existsErr = operations.machineExists(operationCtx, machineName)
		return existsErr
	})
	if err != nil {
		return err
	}
	cutoverStarted = true
	var targetRestoreErr error
	primaryCleanupOwned := false
	if exists {
		deleteErr := withRestoreOperationContext(recoveryCtx, func(operationCtx context.Context) error {
			return operations.deleteNamedMachine(operationCtx, machineName)
		})
		if deleteErr != nil {
			targetRestoreErr = fmt.Errorf("delete original machine: %w", deleteErr)
			cancelRecovery()
			var originalStillExists bool
			inspectErr := withRestoreCleanupContext(func(cleanupCtx context.Context) error {
				var existsErr error
				originalStillExists, existsErr = operations.machineExists(cleanupCtx, machineName)
				return existsErr
			})
			if inspectErr == nil && originalStillExists {
				if !wasRunning {
					return fmt.Errorf("restore aborted; original machine was preserved: %w", targetRestoreErr)
				}
				restartCtx, cancelRestart := context.WithTimeout(
					context.Background(),
					restoreRecoveryTimeout,
				)
				restartErr := operations.startNamedMachine(restartCtx, machineName, port)
				cancelRestart()
				if restartErr == nil {
					return fmt.Errorf(
						"restore aborted; original machine was preserved and restarted: %w",
						targetRestoreErr,
					)
				}
				targetRestoreErr = errors.Join(
					targetRestoreErr,
					fmt.Errorf("verify original machine after ambiguous deletion: %w", restartErr),
				)
				primaryCleanupOwned = true
			}
			if inspectErr != nil {
				targetRestoreErr = errors.Join(
					targetRestoreErr,
					fmt.Errorf("inspect original machine after ambiguous deletion: %w", inspectErr),
				)
			}
		}
	}
	if targetRestoreErr == nil {
		targetRestoreErr = operations.clearKnownHost(recoveryCtx, port)
		if targetRestoreErr != nil {
			targetRestoreErr = fmt.Errorf("clear original host key: %w", targetRestoreErr)
		} else {
			primaryCleanupOwned, targetRestoreErr = operations.restorePrimary(
				recoveryCtx,
				backupDir,
			)
		}
	}
	if targetRestoreErr == nil {
		operations.log("restore completed")
		return nil
	}
	operations.log("final restore validation failed: %v", targetRestoreErr)

	if safetyBackup == "" {
		return fmt.Errorf("restore failed and no original machine existed: %w", targetRestoreErr)
	}
	cancelRecovery()
	var rollbackCleanupErr error
	if primaryCleanupOwned {
		rollbackCleanupErr = removeOwnedRestoreMachine(operations, machineName, port)
	}
	rollbackCtx, cancelRollback := context.WithTimeout(context.Background(), restoreRecoveryTimeout)
	defer cancelRollback()
	rollbackOwned, rollbackErr := operations.restorePrimary(rollbackCtx, safetyBackup)
	if rollbackErr != nil {
		var failedRollbackCleanupErr error
		if rollbackOwned {
			failedRollbackCleanupErr = removeOwnedRestoreMachine(operations, machineName, port)
		}
		return fmt.Errorf(
			"restore failed and original-machine rollback also failed: %w",
			errors.Join(
				targetRestoreErr,
				rollbackCleanupErr,
				rollbackErr,
				failedRollbackCleanupErr,
			),
		)
	}
	if rollbackCleanupErr != nil {
		return fmt.Errorf(
			"restore failed; original machine was recreated from %s after cleanup errors: %w",
			safetyBackup,
			errors.Join(targetRestoreErr, rollbackCleanupErr),
		)
	}
	return fmt.Errorf(
		"restore failed; original machine was recreated from %s: %w",
		safetyBackup,
		targetRestoreErr,
	)
}

func (*App) findRestoreCandidatePort() (int, error) {
	return findFreePort()
}

func removeOwnedRestoreMachine(
	operations restoreOperations,
	machineName string,
	port int,
) error {
	var cleanupErrors []error
	if err := withRestoreCleanupContext(func(cleanupCtx context.Context) error {
		return operations.stopNamedMachine(cleanupCtx, machineName)
	}); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("stop failed restore machine: %w", err))
	}
	var exists bool
	if err := withRestoreCleanupContext(func(cleanupCtx context.Context) error {
		var existsErr error
		exists, existsErr = operations.machineExists(cleanupCtx, machineName)
		return existsErr
	}); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("inspect failed restore machine: %w", err))
	} else if exists {
		if err := withRestoreCleanupContext(func(cleanupCtx context.Context) error {
			return operations.deleteNamedMachine(cleanupCtx, machineName)
		}); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("delete failed restore machine: %w", err))
		}
	}
	if err := withRestoreCleanupContext(func(cleanupCtx context.Context) error {
		return operations.clearKnownHost(cleanupCtx, port)
	}); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("clear failed restore host key: %w", err))
	}
	return errors.Join(cleanupErrors...)
}

func removeCreatedRestoreMachine(
	operations restoreOperations,
	machineName string,
	port int,
) error {
	var cleanupErrors []error
	if err := withRestoreCleanupContext(func(cleanupCtx context.Context) error {
		return operations.stopNamedMachine(cleanupCtx, machineName)
	}); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("stop created restore machine: %w", err))
	}
	if err := withRestoreCleanupContext(func(cleanupCtx context.Context) error {
		return operations.clearKnownHost(cleanupCtx, port)
	}); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("clear created restore host key: %w", err))
	}
	if err := withRestoreCleanupContext(func(cleanupCtx context.Context) error {
		return operations.deleteNamedMachine(cleanupCtx, machineName)
	}); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("delete created restore machine: %w", err))
	}
	return errors.Join(cleanupErrors...)
}

func (a *App) deleteNamedMachine(ctx context.Context, name string) error {
	return a.run(ctx, "smolvm", "machine", "delete", "--name", name, "--force")
}

func (a *App) restorePrimary(ctx context.Context, backupDir string) (bool, error) {
	if err := a.createBlankMachine(ctx, a.config.MachineName, a.config.SSHPort); err != nil {
		return false, err
	}
	return true, a.applyBackup(ctx, a.config.MachineName, backupDir, a.config.SSHPort)
}

func (a *App) applyBackup(ctx context.Context, name, backupDir string, port int) error {
	if err := a.run(ctx, "smolvm", "machine", "start", "--name", name); err != nil {
		return err
	}

	// A portable package can contain a base image older than the current host
	// verifier. Before applying the snapshot, only the out-of-band smolvm guest
	// agent must work. Full SSH, Supervisor, Hermes, Codex, and Executor health
	// checks run after the verified snapshot has been applied and restarted.
	if err := a.waitForMachineExec(ctx, name, 240); err != nil {
		return err
	}
	_ = a.runQuiet(ctx, "smolvm", "machine", "exec", "--name", name, "--", "supervisorctl", "stop", "all")
	transfers := [][2]string{
		{filepath.Join(backupDir, "rootfs.tar.gz"), name + ":/tmp/hermes-box-restore-rootfs.tar.gz"},
		{filepath.Join(backupDir, "rootfs-files.txt"), name + ":/tmp/hermes-box-restore-rootfs-files.txt"},
		{filepath.Join(backupDir, "workspace.tar.gz"), name + ":/tmp/hermes-box-restore-workspace.tar.gz"},
		{filepath.Join(a.root, "guest", "restore.sh"), name + ":/tmp/hermes-box-restore.sh"},
		{filepath.Join(a.root, "guest", "entrypoint.sh"), name + ":/tmp/hermes-box-current-entrypoint.sh"},
		{filepath.Join(a.root, "guest", "start.sh"), name + ":/tmp/hermes-box-current-start.sh"},
		{filepath.Join(a.root, "guest", "executor.sh"), name + ":/tmp/hermes-box-current-executor.sh"},
		{filepath.Join(a.root, "guest", "extract-executor.py"), name + ":/tmp/hermes-box-current-extract-executor.py"},
		{filepath.Join(a.root, "guest", "workspace-seed.sh"), name + ":/tmp/hermes-box-current-workspace-seed.sh"},
		{filepath.Join(a.root, "guest", "supervisord.conf"), name + ":/tmp/hermes-box-current-supervisord.conf"},
		{a.sshPublicKey, name + ":/tmp/hermes-box-restore-authorized-key.pub"},
	}
	for _, transfer := range transfers {
		if err := a.run(ctx, "smolvm", "machine", "cp", transfer[0], transfer[1]); err != nil {
			return err
		}
	}
	if err := a.run(
		ctx,
		"smolvm",
		"machine", "exec",
		"--stream",
		"--name", name,
		"--",
		"bash", "/tmp/hermes-box-restore.sh",
	); err != nil {
		return fmt.Errorf("apply backup to %s: %w", name, err)
	}
	if err := a.run(ctx, "smolvm", "machine", "stop", "--name", name); err != nil {
		return err
	}
	if err := a.clearKnownHost(ctx, port); err != nil {
		return err
	}
	return a.startNamedMachine(ctx, name, port)
}

func (a *App) smolvmVersion(ctx context.Context) string {
	output, err := a.output(ctx, "smolvm", "--version")
	if err != nil {
		return "unknown"
	}
	return trimOutput(output)
}

func verifyBackup(directory string) error {
	return verifyBackupContext(context.Background(), directory)
}

func verifyBackupContext(ctx context.Context, directory string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	manifest, err := validateBackupLayout(directory)
	if err != nil {
		return err
	}
	if err := verifyChecksumsContext(ctx, directory); err != nil {
		return err
	}
	return verifyBackupContentsContext(ctx, directory, manifest)
}

func validateBackupLayout(directory string) (map[string]string, error) {
	if err := requireDirectory(directory); err != nil {
		return nil, fmt.Errorf("backup directory is invalid: %w", err)
	}
	for _, name := range append(append([]string{}, backupFiles...), "SHA256SUMS") {
		if err := requireRegularFile(filepath.Join(directory, name)); err != nil {
			return nil, fmt.Errorf("backup file is invalid: %s: %w", name, err)
		}
	}
	manifest, err := readKeyValues(filepath.Join(directory, "manifest.txt"))
	if err != nil {
		return nil, err
	}
	if manifest["format"] != backupFormat {
		return nil, fmt.Errorf("unsupported backup format %q", manifest["format"])
	}
	return manifest, nil
}

func verifyBackupContents(directory string, manifest map[string]string) error {
	return verifyBackupContentsContext(context.Background(), directory, manifest)
}

func verifyBackupContentsContext(
	ctx context.Context,
	directory string,
	manifest map[string]string,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	warnings, err := os.ReadFile(filepath.Join(directory, "snapshot-warnings.log"))
	if err != nil {
		return fmt.Errorf("read snapshot warnings: %w", err)
	}
	if len(warnings) != 0 {
		return errors.New("backup contains snapshot warnings")
	}
	rootfsNames, err := archiveFileNamesContext(ctx, filepath.Join(directory, "rootfs.tar.gz"))
	if err != nil {
		return fmt.Errorf("inspect rootfs archive: %w", err)
	}
	workspaceNames, err := archiveFileNamesContext(ctx, filepath.Join(directory, "workspace.tar.gz"))
	if err != nil {
		return fmt.Errorf("inspect workspace archive: %w", err)
	}
	if err := verifyWorkspaceContentsContext(ctx, workspaceNames, manifest["ssh_key_fingerprint"] != ""); err != nil {
		return err
	}
	manifestNames, err := readLinesContext(ctx, filepath.Join(directory, "rootfs-files.txt"))
	if err != nil {
		return err
	}
	if len(rootfsNames) != len(manifestNames) {
		return errors.New("rootfs archive does not match rootfs-files.txt")
	}
	for index := range rootfsNames {
		if err := ctx.Err(); err != nil {
			return err
		}
		if rootfsNames[index] != manifestNames[index] {
			return errors.New("rootfs archive does not match rootfs-files.txt")
		}
	}
	return nil
}

func verifyWorkspaceContents(names []string, requireCodex bool) error {
	return verifyWorkspaceContentsContext(context.Background(), names, requireCodex)
}

func verifyWorkspaceContentsContext(
	ctx context.Context,
	names []string,
	requireCodex bool,
) error {
	entries := make(map[string]bool, len(names))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		clean := filepath.ToSlash(filepath.Clean(name))
		clean = strings.TrimPrefix(clean, "./")
		clean = strings.TrimSuffix(clean, "/")
		if clean != "" && clean != "." {
			entries[clean] = true
		}
	}
	if !entries["hermes-home/config.yaml"] {
		return errors.New("workspace archive is missing Hermes config")
	}
	hasWorkRoot := entries["work"]
	if !hasWorkRoot {
		for name := range entries {
			if strings.HasPrefix(name, "work/") {
				hasWorkRoot = true
				break
			}
		}
	}
	if !hasWorkRoot {
		return errors.New("workspace archive is missing work root")
	}
	if requireCodex && !entries["codex-home/config.toml"] {
		return errors.New("workspace archive is missing Codex config")
	}
	return nil
}

func archiveFileNames(path string) ([]string, error) {
	return archiveFileNamesContext(context.Background(), path)
}

func archiveFileNamesContext(ctx context.Context, path string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	compressed, err := gzip.NewReader(contextReader{ctx: ctx, reader: file})
	if err != nil {
		return nil, err
	}
	defer compressed.Close()

	var names []string
	reader := tar.NewReader(compressed)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		name := filepath.ToSlash(header.Name)
		clean := filepath.ToSlash(filepath.Clean(name))
		if filepath.IsAbs(name) || clean == ".." || strings.HasPrefix(clean, "../") {
			return nil, fmt.Errorf("archive contains unsafe path %q", header.Name)
		}
		displayName := header.Name
		if header.FileInfo().IsDir() {
			if displayName == "." {
				displayName = "./"
			} else if !strings.HasSuffix(displayName, "/") {
				displayName += "/"
			}
		}
		names = append(names, displayName)
	}
	sort.Strings(names)
	return names, nil
}

func writeChecksums(directory string) error {
	return writeChecksumsContext(context.Background(), directory)
}

func writeChecksumsContext(ctx context.Context, directory string) error {
	var builder strings.Builder
	for _, name := range backupFiles {
		sum, err := fileChecksumContext(ctx, filepath.Join(directory, name))
		if err != nil {
			return err
		}
		fmt.Fprintf(&builder, "%s  %s\n", sum, name)
	}
	if err := os.WriteFile(filepath.Join(directory, "SHA256SUMS"), []byte(builder.String()), 0o600); err != nil {
		return fmt.Errorf("write backup checksums: %w", err)
	}
	return nil
}

func verifyChecksums(directory string) error {
	return verifyChecksumsContext(context.Background(), directory)
}

func verifyChecksumsContext(ctx context.Context, directory string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	content, err := os.ReadFile(filepath.Join(directory, "SHA256SUMS"))
	if err != nil {
		return fmt.Errorf("read backup checksums: %w", err)
	}
	expected, err := parseBackupChecksums(content)
	if err != nil {
		return err
	}
	for _, name := range backupFiles {
		actual, err := fileChecksumContext(ctx, filepath.Join(directory, name))
		if err != nil {
			return err
		}
		if actual != expected[name] {
			return fmt.Errorf("checksum mismatch: %s", name)
		}
	}
	return nil
}

func parseBackupChecksums(content []byte) (map[string]string, error) {
	expectedFiles := make(map[string]bool, len(backupFiles))
	for _, name := range backupFiles {
		expectedFiles[name] = false
	}
	expected := make(map[string]string, len(backupFiles))
	scanner := bufio.NewScanner(strings.NewReader(string(content)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			return nil, errors.New("invalid SHA256SUMS entry")
		}
		digest, err := hex.DecodeString(fields[0])
		if err != nil || len(digest) != sha256.Size {
			return nil, fmt.Errorf("invalid SHA-256 digest for %s", fields[1])
		}
		seen, known := expectedFiles[fields[1]]
		if !known {
			return nil, fmt.Errorf("unexpected checksum entry: %s", fields[1])
		}
		if seen {
			return nil, fmt.Errorf("duplicate checksum entry: %s", fields[1])
		}
		expectedFiles[fields[1]] = true
		expected[fields[1]] = strings.ToLower(fields[0])
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read backup checksums: %w", err)
	}
	for name, seen := range expectedFiles {
		if !seen {
			return nil, fmt.Errorf("checksum entry is missing: %s", name)
		}
	}
	return expected, nil
}

func fileChecksum(path string) (string, error) {
	return fileChecksumContext(context.Background(), path)
}

func fileChecksumContext(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", filepath.Base(path), err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, contextReader{ctx: ctx, reader: file}); err != nil {
		return "", fmt.Errorf("checksum %s: %w", filepath.Base(path), err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func readKeyValues(path string) (map[string]string, error) {
	lines, err := readLines(path)
	if err != nil {
		return nil, err
	}
	values := make(map[string]string, len(lines))
	for _, line := range lines {
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("invalid manifest entry %q", line)
		}
		values[key] = value
	}
	return values, nil
}

func readLines(path string) ([]string, error) {
	return readLinesContext(context.Background(), path)
}

func readLinesContext(ctx context.Context, path string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", filepath.Base(path), err)
	}
	defer file.Close()

	var lines []string
	scanner := bufio.NewScanner(contextReader{ctx: ctx, reader: file})
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	return lines, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

func writeLines(path string, lines []string) error {
	content := strings.Join(lines, "\n")
	if len(lines) > 0 {
		content += "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	return nil
}

func sanitizeLabel(label string) string {
	var builder strings.Builder
	for _, character := range label {
		switch {
		case character == ' ' || character == '\t' || character == '\n':
			builder.WriteByte('-')
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9',
			character == '_',
			character == '.',
			character == '-':
			builder.WriteRune(character)
		}
	}
	return builder.String()
}
