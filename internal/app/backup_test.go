package app

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/davis7dotsh/hermes-box/internal/config"
	"github.com/davis7dotsh/hermes-box/internal/process"
)

func TestVerifyBackupAcceptsV2Backup(t *testing.T) {
	directory := createTestBackup(t)
	if err := verifyBackup(directory); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyBackupAcceptsLegacyBashV2Backup(t *testing.T) {
	if _, err := exec.LookPath("shasum"); err != nil {
		t.Skip("shasum is required to generate the legacy fixture")
	}
	directory := filepath.Join(t.TempDir(), "legacy.hermesbox")
	script := `
set -e
directory=$1
mkdir -p \
  "$directory/root/etc" \
  "$directory/workspace/hermes-home" \
  "$directory/workspace/work"
printf legacy-root >"$directory/root/etc/example"
printf 'model: legacy\n' >"$directory/workspace/hermes-home/config.yaml"
printf legacy-workspace >"$directory/workspace/work/example"
tar -C "$directory/root" -czpf "$directory/rootfs.tar.gz" .
tar -tzf "$directory/rootfs.tar.gz" | LC_ALL=C sort >"$directory/rootfs-files.txt"
tar -C "$directory/workspace" -czpf "$directory/workspace.tar.gz" .
: >"$directory/snapshot-warnings.log"
printf '%s\n' \
  'format=hermes-box-v2' \
  'created=2026-06-17T00:00:00Z' \
  'machine=legacy' \
  'smolvm=smolvm 1.0.4' >"$directory/manifest.txt"
(
  cd "$directory"
  shasum -a 256 \
    rootfs.tar.gz \
    rootfs-files.txt \
    workspace.tar.gz \
    snapshot-warnings.log \
    manifest.txt >SHA256SUMS
)
`
	command := exec.Command("bash", "-c", script, "bash", directory)
	command.Env = append(os.Environ(), "COPYFILE_DISABLE=1")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generate legacy backup: %v\n%s", err, output)
	}
	if err := verifyBackup(directory); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyBackupRejectsChecksumMismatch(t *testing.T) {
	directory := createTestBackup(t)
	if err := os.WriteFile(
		filepath.Join(directory, "manifest.txt"),
		[]byte("format=hermes-box-v2\nchanged=true\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := verifyBackup(directory); err == nil {
		t.Fatal("verifyBackup accepted a checksum mismatch")
	}
}

func TestVerifyBackupRejectsRootOnlyWorkspace(t *testing.T) {
	directory := createTestBackup(t)
	writeArchive(t, filepath.Join(directory, "workspace.tar.gz"), []string{"./"})
	if err := writeChecksums(directory); err != nil {
		t.Fatal(err)
	}
	if err := verifyBackup(directory); err == nil || !strings.Contains(err.Error(), "Hermes config") {
		t.Fatalf("verifyBackup error = %v", err)
	}
}

func TestVerifyBackupModernRequiresCodexConfig(t *testing.T) {
	directory := createTestBackup(t)
	writeArchive(t, filepath.Join(directory, "workspace.tar.gz"), []string{
		"./",
		"./hermes-home/",
		"./hermes-home/config.yaml",
		"./work/",
		"./work/example.txt",
	})
	if err := writeChecksums(directory); err != nil {
		t.Fatal(err)
	}
	if err := verifyBackup(directory); err == nil || !strings.Contains(err.Error(), "Codex config") {
		t.Fatalf("verifyBackup error = %v", err)
	}
}

func TestVerifyBackupRejectsSnapshotWarnings(t *testing.T) {
	directory := createTestBackup(t)
	if err := os.WriteFile(
		filepath.Join(directory, "snapshot-warnings.log"),
		[]byte("tar reported an incomplete archive\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := writeChecksums(directory); err != nil {
		t.Fatal(err)
	}
	if err := verifyBackup(directory); err == nil || !strings.Contains(err.Error(), "snapshot warnings") {
		t.Fatalf("verifyBackup error = %v", err)
	}
}

func TestStageVerifiedBackupCreatesIndependentReadOnlyCopy(t *testing.T) {
	root := t.TempDir()
	application := New(root, config.Config{}, process.OSRunner{}, io.Discard, io.Discard)
	if err := application.prepareDirs(); err != nil {
		t.Fatal(err)
	}
	source := createTestBackup(t)
	sourceChecksums, err := os.ReadFile(filepath.Join(source, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	staged, err := application.stageVerifiedBackup(source)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := withRestoreCleanupContext(func(ctx context.Context) error {
			return cleanupStagedBackup(ctx, staged)
		}); err != nil {
			t.Errorf("cleanup staged backup: %v", err)
		}
	}()

	if err := os.WriteFile(filepath.Join(source, "workspace.tar.gz"), []byte("mutated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyBackup(staged); err != nil {
		t.Fatalf("staged backup changed with source: %v", err)
	}
	stagedChecksums, err := os.ReadFile(filepath.Join(staged, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	if string(stagedChecksums) != string(sourceChecksums) {
		t.Fatal("staging changed SHA256SUMS content")
	}
	info, err := os.Stat(staged)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o500 {
		t.Fatalf("staged directory mode = %o, want 500", info.Mode().Perm())
	}
	for _, name := range append(append([]string{}, backupFiles...), "SHA256SUMS") {
		info, err := os.Stat(filepath.Join(staged, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o400 {
			t.Errorf("staged %s mode = %o, want 400", name, info.Mode().Perm())
		}
	}
}

func TestStageVerifiedBackupRejectsMutationAndCleansPartialCopy(t *testing.T) {
	root := t.TempDir()
	application := New(root, config.Config{}, process.OSRunner{}, io.Discard, io.Discard)
	if err := application.prepareDirs(); err != nil {
		t.Fatal(err)
	}
	source := createTestBackup(t)
	if err := os.WriteFile(filepath.Join(source, "workspace.tar.gz"), []byte("mutated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := application.stageVerifiedBackup(source); err == nil ||
		!strings.Contains(err.Error(), "checksum mismatch while staging") {
		t.Fatalf("stageVerifiedBackup error = %v", err)
	}
	entries, err := os.ReadDir(application.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".restore-backup-") {
			t.Fatalf("partial staged backup was retained: %s", entry.Name())
		}
	}
}

func TestBackupLargeIOHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader := &cancelAfterRead{cancel: cancel}
	if _, err := io.ReadAll(contextReader{ctx: ctx, reader: reader}); !errors.Is(err, context.Canceled) {
		t.Fatalf("context reader error = %v", err)
	}
	if reader.reads != 1 {
		t.Fatalf("underlying reader calls after cancellation = %d, want 1", reader.reads)
	}

	root := t.TempDir()
	application := New(root, config.Config{}, process.OSRunner{}, io.Discard, io.Discard)
	if err := application.prepareDirs(); err != nil {
		t.Fatal(err)
	}
	backup := createTestBackup(t)
	canceled, cancelImmediately := context.WithCancel(context.Background())
	cancelImmediately()
	for name, operation := range map[string]func() error{
		"stage": func() error {
			_, err := application.stageVerifiedBackupContext(canceled, backup)
			return err
		},
		"verify": func() error {
			return verifyBackupContext(canceled, backup)
		},
		"archive scan": func() error {
			_, err := archiveFileNamesContext(canceled, filepath.Join(backup, "rootfs.tar.gz"))
			return err
		},
		"checksum": func() error {
			_, err := fileChecksumContext(canceled, filepath.Join(backup, "rootfs.tar.gz"))
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := operation(); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled operation error = %v", err)
			}
		})
	}
	entries, err := os.ReadDir(application.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".restore-backup-") {
			t.Fatalf("canceled staging retained %s", entry.Name())
		}
	}
}

func TestArchiveFileNamesRejectsTraversal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unsafe.tar.gz")
	writeArchive(t, path, []string{"../host-file"})
	if _, err := archiveFileNames(path); err == nil {
		t.Fatal("archiveFileNames accepted path traversal")
	}
}

func TestRestorePreflightFailsBeforeBackupOrMachineMutation(t *testing.T) {
	runner := &restorePreflightRunner{}
	application := New(
		t.TempDir(),
		config.Config{NetworkMode: "full"},
		runner,
		io.Discard,
		io.Discard,
	)
	err := application.cmdRestoreValidated(
		context.Background(),
		[]string{filepath.Join(t.TempDir(), "missing.hermesbox")},
		func(context.Context) error { return errors.New("unsupported smolvm version") },
	)
	if err == nil || !strings.Contains(err.Error(), "restore preflight") ||
		!strings.Contains(err.Error(), "unsupported smolvm version") {
		t.Fatalf("restore error = %v", err)
	}
	if len(runner.specs) != 0 {
		t.Fatalf("restore touched external state before preflight completed: %#v", runner.specs)
	}
}

func TestRestoreRecoveryDeadlineExceedsExecutorStartupDeadline(t *testing.T) {
	application := New(
		t.TempDir(),
		config.Config{ExecutorEnabled: true},
		process.OSRunner{},
		io.Discard,
		io.Discard,
	)
	if restoreRecoveryTimeout <= application.startupDeadline() {
		t.Fatalf(
			"restore recovery timeout %s must exceed Executor startup deadline %s",
			restoreRecoveryTimeout,
			application.startupDeadline(),
		)
	}
}

func TestRestoreSecretPreflightFailsBeforeBackupOrMachineMutation(t *testing.T) {
	runner := &restorePreflightRunner{}
	application := New(
		t.TempDir(),
		config.Config{NetworkMode: "full"},
		runner,
		io.Discard,
		io.Discard,
	)
	if err := os.WriteFile(
		application.secretEnvFile,
		[]byte("OPENAI_API_KEY=HERMES_BOX_MISSING_RESTORE_SECRET\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERMES_BOX_MISSING_RESTORE_SECRET", "")
	err := application.cmdRestoreValidated(
		context.Background(),
		[]string{filepath.Join(t.TempDir(), "missing.hermesbox")},
		func(context.Context) error { return nil },
	)
	if err == nil || !strings.Contains(err.Error(), "restore preflight") ||
		!strings.Contains(err.Error(), "HERMES_BOX_MISSING_RESTORE_SECRET") {
		t.Fatalf("restore error = %v", err)
	}
	if len(runner.specs) != 0 {
		t.Fatalf("restore touched external state before secret preflight completed: %#v", runner.specs)
	}
}

func TestRestoreHostValidationRejectsWrongSmolVMVersion(t *testing.T) {
	runner := &restorePreflightRunner{wrongSmolVMVersion: true}
	application := New(t.TempDir(), config.Config{}, runner, io.Discard, io.Discard)
	err := application.validateSupportedHost(
		context.Background(),
		func(command string) (string, error) { return "/usr/bin/" + command, nil },
		"darwin",
		"arm64",
	)
	if err == nil || !strings.Contains(err.Error(), "requires exactly smolvm 1.0.4") {
		t.Fatalf("host validation error = %v", err)
	}
	for _, spec := range runner.specs {
		if spec.Name == "smolvm" && containsArgument(spec.Args, "machine") {
			t.Fatalf("host validation inspected or mutated a machine: %#v", spec)
		}
	}
}

func TestSanitizeLabel(t *testing.T) {
	if got := sanitizeLabel(" ready / now! "); got != "-ready--now-" {
		t.Fatalf("sanitizeLabel() = %q", got)
	}
}

func TestCreateUniqueBackupDirectorySequencesCollisions(t *testing.T) {
	parent := t.TempDir()
	base := "test-box-20260620-120000-pre-restore"
	first := filepath.Join(parent, base+".hermesbox")
	if err := os.Mkdir(first, 0o700); err != nil {
		t.Fatal(err)
	}
	second, err := createUniqueBackupDirectory(parent, base)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(parent, base+"-2.hermesbox"); second != want {
		t.Fatalf("second backup directory = %s, want %s", second, want)
	}
	third, err := createUniqueBackupDirectory(parent, base)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(parent, base+"-3.hermesbox"); third != want {
		t.Fatalf("third backup directory = %s, want %s", third, want)
	}
}

func TestSnapshotFailureDiscardsPartialBackup(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{
		MachineName: "test-box",
		BuilderName: "test-builder",
		SSHPort:     2223,
		CPUs:        1,
		MemoryMiB:   1,
		StorageGB:   1,
		OverlayGB:   1,
		NetworkMode: "full",
	}
	application := New(root, cfg, failingSnapshotRunner{}, io.Discard, io.Discard)
	if err := application.prepareDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(application.sshKey, []byte("test key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := application.snapshotInternal(context.Background(), "failure", true); err == nil {
		t.Fatal("snapshotInternal succeeded")
	}
	entries, err := os.ReadDir(application.backupsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("partial backup was retained: %s", entries[0].Name())
	}
}

func TestSnapshotPartialServiceStopRestartsServices(t *testing.T) {
	root := t.TempDir()
	runner := &partialStopSnapshotRunner{}
	application := New(root, config.Config{
		MachineName: "test-box",
		SSHPort:     2223,
	}, runner, io.Discard, io.Discard)
	if err := application.prepareDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(application.sshKey, []byte("test key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := application.snapshotInternal(context.Background(), "partial-stop", true); err == nil ||
		!strings.Contains(err.Error(), "stop guest services") {
		t.Fatalf("snapshot error = %v", err)
	}
	stopIndex := commandIndex(runner.runs, "supervisorctl", "stop", "all")
	startIndex := commandIndex(runner.runs, "supervisorctl", "start", "all")
	if stopIndex < 0 || startIndex < 0 || startIndex <= stopIndex {
		t.Fatalf("partial service stop was not recovered: %#v", runner.runs)
	}
}

func TestSnapshotAmbiguousMachineStopRestartsStoppedMachine(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	root := t.TempDir()
	runner := &ambiguousStopSnapshotRunner{
		t:               t,
		port:            port,
		running:         true,
		failMachineStop: true,
	}
	application := New(root, config.Config{
		MachineName: "test-box",
		SSHPort:     port,
	}, runner, io.Discard, io.Discard)
	if err := application.prepareDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(application.sshKey, []byte("test key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := application.snapshotInternal(context.Background(), "ambiguous-stop", true); err == nil ||
		!strings.Contains(err.Error(), "injected machine stop timeout") {
		t.Fatalf("snapshot error = %v", err)
	}
	if !runner.running {
		t.Fatalf("ambiguous stop left the original machine offline: %#v", runner.runs)
	}
	stopIndex := commandIndex(runner.runs, "machine", "stop", "--name", "test-box")
	startIndex := commandIndex(runner.runs, "machine", "start", "--name", "test-box")
	if stopIndex < 0 || startIndex <= stopIndex {
		t.Fatalf("stopped machine was not restarted after ambiguous stop: %#v", runner.runs)
	}
	if commandIndex(runner.runs, "supervisorctl", "start", "all") >= 0 {
		t.Fatalf("recovery tried to start services inside a stopped machine: %#v", runner.runs)
	}
}

func TestSnapshotSuccessCleansRemoteArtifactsOnce(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	root := t.TempDir()
	runner := &ambiguousStopSnapshotRunner{t: t, port: port, running: true}
	application := New(root, config.Config{
		MachineName: "test-box",
		SSHPort:     port,
	}, runner, io.Discard, io.Discard)
	if err := application.prepareDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(application.sshKey, []byte("test key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := application.snapshotInternal(context.Background(), "cleanup-once", true); err != nil {
		t.Fatal(err)
	}
	if runner.remoteCleanupCalls != 1 {
		t.Fatalf("remote cleanup calls = %d, want 1", runner.remoteCleanupCalls)
	}
}

func TestSnapshotCancellationUsesIndependentRemoteCleanupContext(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	runner := &ambiguousStopSnapshotRunner{t: t, running: true, cancelAfterTransfer: cancel}
	application := New(root, config.Config{
		MachineName: "test-box",
		SSHPort:     2223,
	}, runner, io.Discard, io.Discard)
	if err := application.prepareDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(application.sshKey, []byte("test key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := application.snapshotInternal(ctx, "cancel-cleanup", true); !errors.Is(err, context.Canceled) {
		t.Fatalf("snapshot cancellation error = %v", err)
	}
	cleanupIndex := commandIndex(
		runner.runs,
		"rm", "-f",
		"/tmp/hermes-box-snapshot.sh",
		"/workspace/.hermes-box-rootfs.tar.gz",
		"/tmp/hermes-box-workspace.tar.gz",
		"/tmp/hermes-box-snapshot-warnings.log",
	)
	if cleanupIndex < 0 {
		t.Fatalf("remote snapshot artifacts were not cleaned: %#v", runner.runs)
	}
	cleanupCtx := runner.runContexts[cleanupIndex]
	if cleanupCtx == ctx {
		t.Fatal("remote cleanup reused the canceled caller context")
	}
	if _, ok := cleanupCtx.Deadline(); !ok {
		t.Fatal("remote cleanup context is unbounded")
	}
}

func TestSnapshotRemoteCleanupFailureStopsBeforeMachineStop(t *testing.T) {
	root := t.TempDir()
	runner := &ambiguousStopSnapshotRunner{
		t:                  t,
		running:            true,
		remoteCleanupError: errors.New("injected remote cleanup failure"),
	}
	application := New(root, config.Config{
		MachineName: "test-box",
		SSHPort:     2223,
	}, runner, io.Discard, io.Discard)
	if err := application.prepareDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(application.sshKey, []byte("test key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := application.snapshotInternal(context.Background(), "cleanup-failure", true); err == nil ||
		!strings.Contains(err.Error(), "remove guest snapshot artifacts") ||
		!strings.Contains(err.Error(), "injected remote cleanup failure") {
		t.Fatalf("snapshot cleanup error = %v", err)
	}
	if commandIndex(runner.runs, "machine", "stop", "--name", "test-box") >= 0 {
		t.Fatalf("snapshot stopped the machine before guest cleanup succeeded: %#v", runner.runs)
	}
	if runner.remoteCleanupCalls != 2 {
		t.Fatalf("remote cleanup calls = %d, want surfaced attempt plus deferred retry", runner.remoteCleanupCalls)
	}
}

func TestSnapshotRestartFailureRetainsCompleteBackup(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{
		MachineName: "test-box",
		BuilderName: "test-builder",
		SSHPort:     2223,
		CPUs:        1,
		MemoryMiB:   1,
		StorageGB:   1,
		OverlayGB:   1,
		NetworkMode: "full",
	}
	runner := restartFailingSnapshotRunner{t: t}
	application := New(root, cfg, runner, io.Discard, io.Discard)
	if err := application.prepareDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(application.sshKey, []byte("test key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := application.snapshotInternal(context.Background(), "restart-failure", true); err == nil {
		t.Fatal("snapshotInternal succeeded")
	}
	entries, err := os.ReadDir(application.backupsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("backup count = %d, want 1", len(entries))
	}
	if err := verifyBackup(filepath.Join(application.backupsDir, entries[0].Name())); err != nil {
		t.Fatalf("retained backup is invalid: %v", err)
	}
}

func TestSnapshotRejectsInvalidWorkspaceArchive(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{
		MachineName: "test-box",
		BuilderName: "test-builder",
		SSHPort:     2223,
		CPUs:        1,
		MemoryMiB:   1,
		StorageGB:   1,
		OverlayGB:   1,
		NetworkMode: "full",
	}
	runner := restartFailingSnapshotRunner{t: t, invalidWorkspace: true}
	application := New(root, cfg, runner, io.Discard, io.Discard)
	if err := application.prepareDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(application.sshKey, []byte("test key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := application.snapshotInternal(context.Background(), "invalid-workspace", true); err == nil ||
		!strings.Contains(err.Error(), "inspect workspace archive") {
		t.Fatalf("snapshotInternal error = %v", err)
	}
	entries, err := os.ReadDir(application.backupsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("invalid backup was retained: %s", entries[0].Name())
	}
}

func TestSnapshotRejectsRootOnlyWorkspaceArchive(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{
		MachineName: "test-box",
		BuilderName: "test-builder",
		SSHPort:     2223,
		CPUs:        1,
		MemoryMiB:   1,
		StorageGB:   1,
		OverlayGB:   1,
		NetworkMode: "full",
	}
	runner := restartFailingSnapshotRunner{t: t, rootOnlyWorkspace: true}
	application := New(root, cfg, runner, io.Discard, io.Discard)
	if err := application.prepareDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(application.sshKey, []byte("test key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := application.snapshotInternal(context.Background(), "root-only", true); err == nil ||
		!strings.Contains(err.Error(), "Hermes config") {
		t.Fatalf("snapshotInternal error = %v", err)
	}
	entries, err := os.ReadDir(application.backupsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("root-only backup was retained: %s", entries[0].Name())
	}
}

func TestRestoreVerifiedBackupFreshRestoresPrimaryDirectly(t *testing.T) {
	operations := newRestoreRecorder("test-box")
	backupDir := "/backups/fresh.hermesbox"

	if err := restoreVerifiedBackup(
		context.Background(),
		operations,
		operations.machineName,
		2223,
		backupDir,
	); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"exists:test-box",
		"create:test-box:2223",
		"apply:test-box:2223:" + backupDir,
	}
	if strings.Join(operations.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf(
			"fresh restore calls:\n%s\nwant:\n%s",
			strings.Join(operations.calls, "\n"),
			strings.Join(want, "\n"),
		)
	}
	for _, call := range operations.calls {
		if strings.Contains(call, "-restore-") {
			t.Fatalf("fresh restore created a candidate: %s", call)
		}
	}
}

func TestRestoreVerifiedBackupFreshFailureDeletesOnlyCreatedPrimary(t *testing.T) {
	operations := newRestoreRecorder("test-box")
	operations.existing["unrelated-box"] = true
	operations.applyErrors["test-box"] = errors.New("injected validation failure")

	err := restoreVerifiedBackup(
		context.Background(),
		operations,
		operations.machineName,
		2223,
		"/backups/fresh.hermesbox",
	)
	if err == nil || !strings.Contains(err.Error(), "newly created machine was removed") {
		t.Fatalf("fresh restore error = %v", err)
	}

	var deleted []string
	for _, call := range operations.calls {
		if name, found := strings.CutPrefix(call, "delete:"); found {
			deleted = append(deleted, name)
		}
	}
	if strings.Join(deleted, "\n") != "test-box" {
		t.Fatalf("deleted machines = %v, want only test-box", deleted)
	}
	if !operations.existing["unrelated-box"] {
		t.Fatal("fresh restore touched an unrelated machine")
	}
}

func TestRestoreVerifiedBackupFreshCreateFailureDoesNotClaimExistingName(t *testing.T) {
	operations := newRestoreRecorder("test-box")
	operations.createErrors[operations.machineName] = errors.New("machine already exists")

	err := restoreVerifiedBackup(
		context.Background(),
		operations,
		operations.machineName,
		2223,
		"/backups/fresh.hermesbox",
	)
	if err == nil || !strings.Contains(err.Error(), "machine already exists") {
		t.Fatalf("fresh create error = %v", err)
	}
	if got := countCalls(operations.calls, "delete:"); got != 0 {
		t.Fatalf("fresh create failure deleted a machine:\n%s", strings.Join(operations.calls, "\n"))
	}
}

func TestRestoreVerifiedBackupExistingMachineRemainsTwoPhase(t *testing.T) {
	operations := newRestoreRecorder("test-box")
	operations.existing[operations.machineName] = true
	operations.running[operations.machineName] = true
	backupDir := "/backups/replacement.hermesbox"

	if err := restoreVerifiedBackup(
		context.Background(),
		operations,
		operations.machineName,
		2223,
		backupDir,
	); err != nil {
		t.Fatal(err)
	}

	snapshotIndex := callIndex(operations.calls, "snapshot:pre-restore:false")
	candidateCreateIndex := callIndex(operations.calls, "create:test-box-restore-")
	candidateApplyIndex := callIndex(operations.calls, "apply:test-box-restore-")
	primaryRestoreIndex := callIndex(operations.calls, "restore-primary:"+backupDir)
	if snapshotIndex < 0 || candidateCreateIndex < 0 || candidateApplyIndex < 0 || primaryRestoreIndex < 0 {
		t.Fatalf("existing restore did not run both phases:\n%s", strings.Join(operations.calls, "\n"))
	}
	if !(snapshotIndex < candidateCreateIndex &&
		candidateCreateIndex < candidateApplyIndex &&
		candidateApplyIndex < primaryRestoreIndex) {
		t.Fatalf("existing restore phase order is wrong:\n%s", strings.Join(operations.calls, "\n"))
	}
	if got := countCalls(operations.calls, "restore-primary:"); got != 1 {
		t.Fatalf("primary restore count = %d, want 1", got)
	}
}

func TestRestoreVerifiedBackupBoundsCutoverInspectionAndDelete(t *testing.T) {
	operations := newRestoreRecorder("test-box")
	operations.existing[operations.machineName] = true
	operations.running[operations.machineName] = true

	if err := restoreVerifiedBackup(
		context.Background(),
		operations,
		operations.machineName,
		2223,
		"/backups/replacement.hermesbox",
	); err != nil {
		t.Fatal(err)
	}
	var cutoverInspection, primaryDelete context.Context
	for index, name := range operations.existsNames {
		if name == operations.machineName {
			cutoverInspection = operations.existsContexts[index]
		}
	}
	for index, name := range operations.deleteNames {
		if name == operations.machineName {
			primaryDelete = operations.deleteContexts[index]
		}
	}
	for label, operationCtx := range map[string]context.Context{
		"cutover inspection": cutoverInspection,
		"primary delete":     primaryDelete,
	} {
		if operationCtx == nil {
			t.Fatalf("missing %s context", label)
		}
		deadline, ok := operationCtx.Deadline()
		if !ok {
			t.Fatalf("%s context is unbounded", label)
		}
		if remaining := time.Until(deadline); remaining > restoreCleanupTimeout+time.Second {
			t.Fatalf("%s deadline is too long: %s", label, remaining)
		}
	}
}

func TestRestoreVerifiedBackupCandidateFailureRestartsRunningPrimary(t *testing.T) {
	operations := newRestoreRecorder("test-box")
	operations.existing[operations.machineName] = true
	operations.running[operations.machineName] = true
	operations.candidateApplyError = errors.New("injected candidate failure")

	err := restoreVerifiedBackup(
		context.Background(),
		operations,
		operations.machineName,
		2223,
		"/backups/replacement.hermesbox",
	)
	if err == nil || !strings.Contains(err.Error(), "candidate validation failed") {
		t.Fatalf("candidate failure error = %v", err)
	}
	if callIndex(operations.calls, "start:test-box:2223") < 0 {
		t.Fatalf("running primary was not restarted:\n%s", strings.Join(operations.calls, "\n"))
	}
	for _, call := range operations.calls {
		if call == "delete:test-box" {
			t.Fatalf("candidate failure deleted the primary:\n%s", strings.Join(operations.calls, "\n"))
		}
	}
	if callIndex(operations.calls, "delete:test-box-restore-") < 0 {
		t.Fatalf("candidate failure leaked the candidate:\n%s", strings.Join(operations.calls, "\n"))
	}
	stopIndex := callIndex(operations.calls, "stop:test-box-restore-")
	restartIndex := callIndex(operations.calls, "start:test-box:2223")
	clearIndex := callIndex(operations.calls, "clear-known-host:")
	deleteIndex := callIndex(operations.calls, "delete:test-box-restore-")
	if !(stopIndex >= 0 && stopIndex < clearIndex && clearIndex < deleteIndex && deleteIndex < restartIndex) {
		t.Fatalf("candidate cleanup did not precede original restart:\n%s", strings.Join(operations.calls, "\n"))
	}
	for _, cleanupCtx := range append(
		append([]context.Context{}, operations.stopContexts...),
		append(operations.clearContexts, operations.deleteContexts...)...,
	) {
		if _, ok := cleanupCtx.Deadline(); !ok {
			t.Fatal("candidate cleanup used an unbounded context")
		}
	}
	if len(operations.startContexts) != 1 {
		t.Fatalf("restart context count = %d, want 1", len(operations.startContexts))
	}
	if _, ok := operations.startContexts[0].Deadline(); !ok {
		t.Fatal("original restart used an unbounded context")
	}
}

func TestRestoreVerifiedBackupCandidateStopFailureDeletesBeforePrimaryRestart(t *testing.T) {
	operations := newRestoreRecorder("test-box")
	operations.existing[operations.machineName] = true
	operations.running[operations.machineName] = true
	operations.candidateApplyError = errors.New("injected candidate failure")
	operations.candidateStopError = errors.New("injected candidate stop timeout")
	operations.failPrimaryStartWhileCandidateRunning = true

	err := restoreVerifiedBackup(
		context.Background(),
		operations,
		operations.machineName,
		2223,
		"/backups/replacement.hermesbox",
	)
	if err == nil || !strings.Contains(err.Error(), "candidate validation failed") ||
		!strings.Contains(err.Error(), "candidate stop timeout") {
		t.Fatalf("candidate failure error = %v", err)
	}
	if !operations.running[operations.machineName] {
		t.Fatalf("primary did not restart after candidate deletion:\n%s", strings.Join(operations.calls, "\n"))
	}
	stopIndex := callIndex(operations.calls, "stop:test-box-restore-")
	clearIndex := callIndex(operations.calls, "clear-known-host:")
	deleteIndex := callIndex(operations.calls, "delete:test-box-restore-")
	restartIndex := callIndex(operations.calls, "start:test-box:2223")
	if !(stopIndex >= 0 && stopIndex < clearIndex && clearIndex < deleteIndex && deleteIndex < restartIndex) {
		t.Fatalf("candidate was not deleted before primary restart:\n%s", strings.Join(operations.calls, "\n"))
	}
}

func TestRestoreVerifiedBackupCandidateCreateFailureDoesNotDeleteUnownedName(t *testing.T) {
	operations := newRestoreRecorder("test-box")
	operations.existing[operations.machineName] = true
	operations.running[operations.machineName] = true
	operations.candidateCreateError = errors.New("injected candidate creation failure")

	err := restoreVerifiedBackup(
		context.Background(),
		operations,
		operations.machineName,
		2223,
		"/backups/replacement.hermesbox",
	)
	if err == nil || !strings.Contains(err.Error(), "candidate creation failure") {
		t.Fatalf("candidate creation error = %v", err)
	}
	for _, prefix := range []string{"stop:test-box-restore-", "clear-known-host:", "delete:test-box-restore-"} {
		if callIndex(operations.calls, prefix) >= 0 {
			t.Fatalf("candidate create failure cleaned an unowned name:\n%s", strings.Join(operations.calls, "\n"))
		}
	}
	if callIndex(operations.calls, "start:test-box:2223") < 0 {
		t.Fatalf("running primary was not restarted:\n%s", strings.Join(operations.calls, "\n"))
	}
}

func TestRestoreVerifiedBackupPortFailureLeavesOriginalUntouched(t *testing.T) {
	operations := newRestoreRecorder("test-box")
	operations.existing[operations.machineName] = true
	operations.running[operations.machineName] = true
	operations.candidatePortError = errors.New("injected port failure")

	err := restoreVerifiedBackup(
		context.Background(),
		operations,
		operations.machineName,
		2223,
		"/backups/replacement.hermesbox",
	)
	if err == nil || !strings.Contains(err.Error(), "injected port failure") {
		t.Fatalf("port allocation error = %v", err)
	}
	if callIndex(operations.calls, "snapshot:") >= 0 || callIndex(operations.calls, "stop:") >= 0 {
		t.Fatalf("port failure touched the original machine:\n%s", strings.Join(operations.calls, "\n"))
	}
	if !operations.running[operations.machineName] {
		t.Fatal("port failure left original stopped")
	}
}

func TestRestoreVerifiedBackupPrimaryHostKeyFailureRollsBack(t *testing.T) {
	operations := newRestoreRecorder("test-box")
	operations.existing[operations.machineName] = true
	operations.running[operations.machineName] = true
	operations.clearKnownHostErrors = []error{nil, errors.New("injected host-key failure")}

	err := restoreVerifiedBackup(
		context.Background(),
		operations,
		operations.machineName,
		2223,
		"/backups/replacement.hermesbox",
	)
	if err == nil || !strings.Contains(err.Error(), "injected host-key failure") ||
		!strings.Contains(err.Error(), "original machine was recreated") {
		t.Fatalf("host-key rollback error = %v", err)
	}
	if got := countCalls(operations.calls, "restore-primary:/backups/safety.hermesbox"); got != 1 {
		t.Fatalf("safety restore count = %d, want 1\n%s", got, strings.Join(operations.calls, "\n"))
	}
}

func TestRestoreVerifiedBackupAmbiguousPrimaryDeleteRollsBackWhenMachineIsGone(t *testing.T) {
	operations := newRestoreRecorder("test-box")
	operations.existing[operations.machineName] = true
	operations.running[operations.machineName] = true
	operations.primaryDeleteError = errors.New("injected delete timeout")
	operations.primaryDeleteRemoves = true

	err := restoreVerifiedBackup(
		context.Background(),
		operations,
		operations.machineName,
		2223,
		"/backups/replacement.hermesbox",
	)
	if err == nil || !strings.Contains(err.Error(), "injected delete timeout") ||
		!strings.Contains(err.Error(), "original machine was recreated") {
		t.Fatalf("ambiguous delete rollback error = %v", err)
	}
	if got := countCalls(operations.calls, "restore-primary:/backups/safety.hermesbox"); got != 1 {
		t.Fatalf("safety restore count = %d, want 1\n%s", got, strings.Join(operations.calls, "\n"))
	}
	if !operations.existing[operations.machineName] || !operations.running[operations.machineName] {
		t.Fatal("ambiguous delete did not recreate the original machine")
	}
}

func TestRestoreVerifiedBackupFailedPrimaryDeletePreservesExistingMachine(t *testing.T) {
	operations := newRestoreRecorder("test-box")
	operations.existing[operations.machineName] = true
	operations.running[operations.machineName] = true
	operations.primaryDeleteError = errors.New("injected delete rejection")

	err := restoreVerifiedBackup(
		context.Background(),
		operations,
		operations.machineName,
		2223,
		"/backups/replacement.hermesbox",
	)
	if err == nil || !strings.Contains(err.Error(), "preserved and restarted") {
		t.Fatalf("definite delete failure error = %v", err)
	}
	if got := countCalls(operations.calls, "restore-primary:"); got != 0 {
		t.Fatalf("preserved original was unnecessarily recreated: %s", strings.Join(operations.calls, "\n"))
	}
	if !operations.existing[operations.machineName] || !operations.running[operations.machineName] {
		t.Fatal("failed delete did not preserve and restart the original machine")
	}
}

func TestRestoreVerifiedBackupFailedPrimaryDeleteRollsBackWhenRestartIsUnhealthy(t *testing.T) {
	operations := newRestoreRecorder("test-box")
	operations.existing[operations.machineName] = true
	operations.running[operations.machineName] = true
	operations.primaryDeleteError = errors.New("injected delete rejection")
	operations.startErrors[operations.machineName] = []error{errors.New("injected health failure")}

	err := restoreVerifiedBackup(
		context.Background(),
		operations,
		operations.machineName,
		2223,
		"/backups/replacement.hermesbox",
	)
	if err == nil || !strings.Contains(err.Error(), "injected health failure") ||
		!strings.Contains(err.Error(), "original machine was recreated") {
		t.Fatalf("unhealthy preserved-machine rollback error = %v", err)
	}
	if got := countCalls(operations.calls, "restore-primary:/backups/safety.hermesbox"); got != 1 {
		t.Fatalf("safety restore count = %d, want 1\n%s", got, strings.Join(operations.calls, "\n"))
	}
	if !operations.existing[operations.machineName] || !operations.running[operations.machineName] {
		t.Fatal("unhealthy listed machine was not replaced from the safety backup")
	}
}

func TestRestoreVerifiedBackupPrimaryCollisionPreservesInterloper(t *testing.T) {
	operations := newRestoreRecorder("test-box")
	operations.existing[operations.machineName] = true
	operations.running[operations.machineName] = true
	operations.interloperOnRestorePrimary = true

	err := restoreVerifiedBackup(
		context.Background(),
		operations,
		operations.machineName,
		2223,
		"/backups/replacement.hermesbox",
	)
	if err == nil || !strings.Contains(err.Error(), "rollback also failed") ||
		!strings.Contains(err.Error(), "machine already exists") {
		t.Fatalf("primary collision error = %v", err)
	}
	if !operations.existing[operations.machineName] || !operations.running[operations.machineName] {
		t.Fatal("restore collision deleted or stopped the unowned interloper")
	}
	if got := countExactCalls(operations.calls, "delete:"+operations.machineName); got != 1 {
		t.Fatalf(
			"primary delete count = %d, want only the original cutover delete\n%s",
			got,
			strings.Join(operations.calls, "\n"),
		)
	}
}

func TestRestoreVerifiedBackupRollbackIgnoresCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	operations := newRestoreRecorder("test-box")
	operations.existing[operations.machineName] = true
	operations.running[operations.machineName] = true
	operations.cancelOnCandidateDelete = cancel
	operations.restorePrimaryErrors = []error{errors.New("target validation failed"), nil}

	err := restoreVerifiedBackup(
		ctx,
		operations,
		operations.machineName,
		2223,
		"/backups/replacement.hermesbox",
	)
	if err == nil || !strings.Contains(err.Error(), "target validation failed") ||
		!strings.Contains(err.Error(), "original machine was recreated") {
		t.Fatalf("rollback error = %v", err)
	}
	if strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("rollback reused canceled context: %v", err)
	}
	if got := countCalls(operations.calls, "restore-primary:"); got != 2 {
		t.Fatalf("restore attempts = %d, want target plus rollback", got)
	}
	if len(operations.restoreContexts) != 2 || operations.restoreContexts[0] == operations.restoreContexts[1] {
		t.Fatal("rollback reused the target restore context")
	}
}

func createTestBackup(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	rootfs := filepath.Join(directory, "rootfs.tar.gz")
	workspace := filepath.Join(directory, "workspace.tar.gz")
	writeArchive(t, rootfs, []string{"./", "./etc/", "./etc/hosts"})
	writeArchive(t, workspace, []string{
		"./",
		"./hermes-home/",
		"./hermes-home/config.yaml",
		"./codex-home/",
		"./codex-home/config.toml",
		"./work/",
		"./work/example.txt",
	})

	names, err := archiveFileNames(rootfs)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeLines(filepath.Join(directory, "rootfs-files.txt"), names); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "snapshot-warnings.log"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(directory, "manifest.txt"),
		[]byte("format=hermes-box-v2\ncreated=2026-06-17T00:00:00Z\nmachine=test\nssh_key_fingerprint=SHA256:test\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := writeChecksums(directory); err != nil {
		t.Fatal(err)
	}
	return directory
}

func writeArchive(t *testing.T, path string, names []string) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	compressed := gzip.NewWriter(file)
	archive := tar.NewWriter(compressed)
	for _, name := range names {
		header := &tar.Header{
			Name: name,
			Mode: 0o644,
			Size: 0,
		}
		if name[len(name)-1] == '/' {
			header.Typeflag = tar.TypeDir
			header.Mode = 0o755
		}
		if err := archive.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

type failingSnapshotRunner struct{}

type cancelAfterRead struct {
	cancel context.CancelFunc
	reads  int
}

func (r *cancelAfterRead) Read(buffer []byte) (int, error) {
	r.reads++
	buffer[0] = 'x'
	r.cancel()
	return 1, nil
}

type partialStopSnapshotRunner struct {
	runs []process.Spec
}

type ambiguousStopSnapshotRunner struct {
	t                   *testing.T
	port                int
	running             bool
	stopFailed          bool
	cancelAfterTransfer context.CancelFunc
	remoteCleanupError  error
	remoteCleanupCalls  int
	failMachineStop     bool
	runs                []process.Spec
	runContexts         []context.Context
}

type restorePreflightRunner struct {
	wrongSmolVMVersion bool
	specs              []process.Spec
}

func (r *restorePreflightRunner) Run(_ context.Context, spec process.Spec) error {
	r.specs = append(r.specs, spec)
	return nil
}

func (r *restorePreflightRunner) Output(_ context.Context, spec process.Spec) ([]byte, error) {
	r.specs = append(r.specs, spec)
	if spec.Name == "go" {
		return []byte("go version go1.24.0 darwin/arm64\n"), nil
	}
	if spec.Name == "smolvm" {
		if r.wrongSmolVMVersion {
			return []byte("smolvm 1.0.5\n"), nil
		}
		return []byte("smolvm 1.0.4\n"), nil
	}
	return nil, fmt.Errorf("unexpected output command: %s", spec.Name)
}

func (failingSnapshotRunner) Run(_ context.Context, spec process.Spec) error {
	if spec.Name == "smolvm" &&
		len(spec.Args) >= 2 &&
		spec.Args[0] == "machine" &&
		spec.Args[1] == "cp" {
		return errors.New("injected transfer failure")
	}
	return nil
}

func (failingSnapshotRunner) Output(_ context.Context, spec process.Spec) ([]byte, error) {
	if spec.Name == "ssh-keygen" && containsArgument(spec.Args, "-y") {
		return []byte("ssh-ed25519 AAAATEST\n"), nil
	}
	if spec.Name == "ssh-keygen" && containsArgument(spec.Args, "-lf") {
		return []byte("256 SHA256:test test (ED25519)\n"), nil
	}
	if spec.Name == "smolvm" &&
		len(spec.Args) >= 2 &&
		spec.Args[0] == "machine" &&
		spec.Args[1] == "list" {
		return []byte(`[{"name":"test-box"}]`), nil
	}
	if spec.Name == "smolvm" &&
		len(spec.Args) >= 2 &&
		spec.Args[0] == "machine" &&
		spec.Args[1] == "status" {
		return []byte(`{"state":"running"}`), nil
	}
	return nil, errors.New("unexpected output command")
}

func (r *partialStopSnapshotRunner) Run(_ context.Context, spec process.Spec) error {
	r.runs = append(r.runs, spec)
	if commandMatches(spec, "supervisorctl", "stop", "all") {
		return errors.New("injected partial service stop")
	}
	return nil
}

func (r *partialStopSnapshotRunner) Output(_ context.Context, spec process.Spec) ([]byte, error) {
	r.runs = append(r.runs, spec)
	if spec.Name == "ssh-keygen" && containsArgument(spec.Args, "-y") {
		return []byte("ssh-ed25519 AAAATEST\n"), nil
	}
	if spec.Name == "ssh-keygen" && containsArgument(spec.Args, "-lf") {
		return []byte("256 SHA256:test test (ED25519)\n"), nil
	}
	if spec.Name == "smolvm" && containsArgument(spec.Args, "list") {
		return []byte(`[{"name":"test-box"}]`), nil
	}
	if spec.Name == "smolvm" && containsArgument(spec.Args, "status") {
		return []byte(`{"state":"running"}`), nil
	}
	return nil, errors.New("unexpected output command")
}

func (r *ambiguousStopSnapshotRunner) Run(ctx context.Context, spec process.Spec) error {
	r.runs = append(r.runs, spec)
	r.runContexts = append(r.runContexts, ctx)
	if commandMatches(
		spec,
		"rm", "-f",
		"/tmp/hermes-box-snapshot.sh",
		"/workspace/.hermes-box-rootfs.tar.gz",
		"/tmp/hermes-box-workspace.tar.gz",
		"/tmp/hermes-box-snapshot-warnings.log",
	) {
		r.remoteCleanupCalls++
		if r.remoteCleanupError != nil {
			return r.remoteCleanupError
		}
	}
	if spec.Name != "smolvm" {
		return nil
	}
	if commandMatches(spec, "machine", "stop", "--name", "test-box") {
		r.running = false
		if r.failMachineStop && !r.stopFailed {
			r.stopFailed = true
			return errors.New("injected machine stop timeout")
		}
		return nil
	}
	if commandMatches(spec, "machine", "start", "--name", "test-box") {
		r.running = true
		return nil
	}
	if len(spec.Args) >= 4 &&
		spec.Args[0] == "machine" &&
		spec.Args[1] == "cp" &&
		!strings.Contains(spec.Args[3], ":") {
		source := spec.Args[2]
		destination := spec.Args[3]
		switch {
		case strings.HasSuffix(source, "rootfs.tar.gz"):
			writeArchive(r.t, destination, []string{"./", "./etc/", "./etc/hosts"})
		case strings.HasSuffix(source, "workspace.tar.gz"):
			writeArchive(r.t, destination, []string{
				"./",
				"./hermes-home/",
				"./hermes-home/config.yaml",
				"./codex-home/",
				"./codex-home/config.toml",
				"./work/",
				"./work/example",
			})
		case strings.HasSuffix(source, "snapshot-warnings.log"):
			if err := os.WriteFile(destination, nil, 0o600); err != nil {
				return err
			}
			if r.cancelAfterTransfer != nil {
				r.cancelAfterTransfer()
				r.cancelAfterTransfer = nil
			}
			return nil
		}
	}
	return nil
}

func (r *ambiguousStopSnapshotRunner) Output(ctx context.Context, spec process.Spec) ([]byte, error) {
	r.runs = append(r.runs, spec)
	r.runContexts = append(r.runContexts, ctx)
	if spec.Name == "ssh-keygen" && containsArgument(spec.Args, "-y") {
		return []byte("ssh-ed25519 AAAATEST\n"), nil
	}
	if spec.Name == "ssh-keygen" && containsArgument(spec.Args, "-lf") {
		return []byte("256 SHA256:test test (ED25519)\n"), nil
	}
	if spec.Name == "lsof" {
		return []byte(fmt.Sprintf("n127.0.0.1:%d\n", r.port)), nil
	}
	if spec.Name == "smolvm" && containsArgument(spec.Args, "list") {
		return []byte(`[{"name":"test-box"}]`), nil
	}
	if spec.Name == "smolvm" && containsArgument(spec.Args, "status") {
		if r.running {
			return []byte(`{"state":"running"}`), nil
		}
		return []byte(`{"state":"stopped"}`), nil
	}
	if spec.Name == "smolvm" && len(spec.Args) == 1 && spec.Args[0] == "--version" {
		return []byte("smolvm 1.0.4\n"), nil
	}
	return nil, errors.New("unexpected output command")
}

type restartFailingSnapshotRunner struct {
	t                 *testing.T
	invalidWorkspace  bool
	rootOnlyWorkspace bool
}

func (r restartFailingSnapshotRunner) Run(_ context.Context, spec process.Spec) error {
	if spec.Name != "smolvm" {
		return nil
	}
	if len(spec.Args) >= 3 &&
		spec.Args[0] == "machine" &&
		spec.Args[1] == "start" {
		return errors.New("injected restart failure")
	}
	if len(spec.Args) >= 4 &&
		spec.Args[0] == "machine" &&
		spec.Args[1] == "cp" &&
		!strings.Contains(spec.Args[3], ":") {
		source := spec.Args[2]
		destination := spec.Args[3]
		switch {
		case strings.HasSuffix(source, "rootfs.tar.gz"):
			writeArchive(r.t, destination, []string{"./", "./etc/", "./etc/hosts"})
		case strings.HasSuffix(source, "workspace.tar.gz"):
			if r.invalidWorkspace {
				return os.WriteFile(destination, []byte("invalid archive"), 0o600)
			}
			if r.rootOnlyWorkspace {
				writeArchive(r.t, destination, []string{"./"})
				break
			}
			writeArchive(r.t, destination, []string{
				"./",
				"./hermes-home/",
				"./hermes-home/config.yaml",
				"./codex-home/",
				"./codex-home/config.toml",
				"./work/",
				"./work/example",
			})
		case strings.HasSuffix(source, "snapshot-warnings.log"):
			return os.WriteFile(destination, nil, 0o600)
		}
	}
	return nil
}

func (restartFailingSnapshotRunner) Output(_ context.Context, spec process.Spec) ([]byte, error) {
	if spec.Name == "ssh-keygen" && containsArgument(spec.Args, "-y") {
		return []byte("ssh-ed25519 AAAATEST\n"), nil
	}
	if spec.Name == "ssh-keygen" && containsArgument(spec.Args, "-lf") {
		return []byte("256 SHA256:test test (ED25519)\n"), nil
	}
	if spec.Name == "smolvm" &&
		len(spec.Args) >= 2 &&
		spec.Args[0] == "machine" &&
		spec.Args[1] == "list" {
		return []byte(`[{"name":"test-box"}]`), nil
	}
	if spec.Name == "smolvm" &&
		len(spec.Args) >= 2 &&
		spec.Args[0] == "machine" &&
		spec.Args[1] == "status" {
		return []byte(`{"state":"running"}`), nil
	}
	if spec.Name == "smolvm" && len(spec.Args) == 1 && spec.Args[0] == "--version" {
		return []byte("smolvm 1.0.4\n"), nil
	}
	return nil, errors.New("unexpected output command")
}

type restoreRecorder struct {
	machineName                           string
	existing                              map[string]bool
	running                               map[string]bool
	createErrors                          map[string]error
	applyErrors                           map[string]error
	startErrors                           map[string][]error
	candidateCreateError                  error
	candidateApplyError                   error
	candidateStopError                    error
	candidatePortError                    error
	restorePrimaryErrors                  []error
	clearKnownHostErrors                  []error
	primaryDeleteError                    error
	primaryDeleteRemoves                  bool
	primaryDeleteErrorUsed                bool
	interloperOnRestorePrimary            bool
	failPrimaryStartWhileCandidateRunning bool
	restoreContexts                       []context.Context
	existsContexts                        []context.Context
	existsNames                           []string
	startContexts                         []context.Context
	stopContexts                          []context.Context
	deleteContexts                        []context.Context
	deleteNames                           []string
	clearContexts                         []context.Context
	cancelOnCandidateDelete               context.CancelFunc
	calls                                 []string
}

func newRestoreRecorder(machineName string) *restoreRecorder {
	return &restoreRecorder{
		machineName:  machineName,
		existing:     make(map[string]bool),
		running:      make(map[string]bool),
		createErrors: make(map[string]error),
		applyErrors:  make(map[string]error),
		startErrors:  make(map[string][]error),
	}
}

func (r *restoreRecorder) machineExists(ctx context.Context, name string) (bool, error) {
	r.calls = append(r.calls, "exists:"+name)
	r.existsContexts = append(r.existsContexts, ctx)
	r.existsNames = append(r.existsNames, name)
	return r.existing[name], nil
}

func (r *restoreRecorder) isRunning(_ context.Context, name string) (bool, error) {
	r.calls = append(r.calls, "running:"+name)
	return r.running[name], nil
}

func (r *restoreRecorder) findRestoreCandidatePort() (int, error) {
	r.calls = append(r.calls, "find-candidate-port")
	if r.candidatePortError != nil {
		return 0, r.candidatePortError
	}
	return 2244, nil
}

func (r *restoreRecorder) snapshotInternal(
	_ context.Context,
	label string,
	restartAfter bool,
) (string, error) {
	r.calls = append(r.calls, fmt.Sprintf("snapshot:%s:%t", label, restartAfter))
	r.running[r.machineName] = false
	return "/backups/safety.hermesbox", nil
}

func (r *restoreRecorder) createBlankMachine(_ context.Context, name string, port int) error {
	r.calls = append(r.calls, fmt.Sprintf("create:%s:%d", name, port))
	r.existing[name] = true
	if strings.Contains(name, "-restore-") && r.candidateCreateError != nil {
		return r.candidateCreateError
	}
	return r.createErrors[name]
}

func (r *restoreRecorder) applyBackup(
	_ context.Context,
	name string,
	backupDir string,
	port int,
) error {
	r.calls = append(r.calls, fmt.Sprintf("apply:%s:%d:%s", name, port, backupDir))
	r.running[name] = true
	if strings.Contains(name, "-restore-") && r.candidateApplyError != nil {
		return r.candidateApplyError
	}
	return r.applyErrors[name]
}

func (r *restoreRecorder) startNamedMachine(ctx context.Context, name string, port int) error {
	r.calls = append(r.calls, fmt.Sprintf("start:%s:%d", name, port))
	r.startContexts = append(r.startContexts, ctx)
	if name == r.machineName && r.failPrimaryStartWhileCandidateRunning {
		for candidate, running := range r.running {
			if strings.Contains(candidate, "-restore-") && running {
				return errors.New("candidate still owns the shared Executor port")
			}
		}
	}
	if len(r.startErrors[name]) > 0 {
		err := r.startErrors[name][0]
		r.startErrors[name] = r.startErrors[name][1:]
		r.running[name] = false
		return err
	}
	r.existing[name] = true
	r.running[name] = true
	return nil
}

func (r *restoreRecorder) stopNamedMachine(ctx context.Context, name string) error {
	r.calls = append(r.calls, "stop:"+name)
	r.stopContexts = append(r.stopContexts, ctx)
	if strings.Contains(name, "-restore-") && r.candidateStopError != nil {
		return r.candidateStopError
	}
	r.running[name] = false
	return nil
}

func (r *restoreRecorder) deleteNamedMachine(ctx context.Context, name string) error {
	r.calls = append(r.calls, "delete:"+name)
	r.deleteContexts = append(r.deleteContexts, ctx)
	r.deleteNames = append(r.deleteNames, name)
	if name == r.machineName && r.primaryDeleteError != nil && !r.primaryDeleteErrorUsed {
		r.primaryDeleteErrorUsed = true
		if !r.primaryDeleteRemoves {
			return r.primaryDeleteError
		}
		err := r.primaryDeleteError
		delete(r.existing, name)
		delete(r.running, name)
		return err
	}
	delete(r.existing, name)
	delete(r.running, name)
	if strings.Contains(name, "-restore-") && r.cancelOnCandidateDelete != nil {
		r.cancelOnCandidateDelete()
		r.cancelOnCandidateDelete = nil
	}
	return nil
}

func (r *restoreRecorder) clearKnownHost(ctx context.Context, port int) error {
	r.calls = append(r.calls, fmt.Sprintf("clear-known-host:%d", port))
	r.clearContexts = append(r.clearContexts, ctx)
	if len(r.clearKnownHostErrors) > 0 {
		err := r.clearKnownHostErrors[0]
		r.clearKnownHostErrors = r.clearKnownHostErrors[1:]
		return err
	}
	return nil
}

func (r *restoreRecorder) restorePrimary(ctx context.Context, backupDir string) (bool, error) {
	r.calls = append(r.calls, "restore-primary:"+backupDir)
	r.restoreContexts = append(r.restoreContexts, ctx)
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if r.interloperOnRestorePrimary {
		r.interloperOnRestorePrimary = false
		r.existing[r.machineName] = true
		r.running[r.machineName] = true
	}
	if r.existing[r.machineName] {
		return false, errors.New("machine already exists: unowned interloper")
	}
	r.existing[r.machineName] = true
	if len(r.restorePrimaryErrors) > 0 {
		err := r.restorePrimaryErrors[0]
		r.restorePrimaryErrors = r.restorePrimaryErrors[1:]
		if err != nil {
			return true, err
		}
	}
	r.running[r.machineName] = true
	return true, nil
}

func (*restoreRecorder) log(string, ...any) {}

func callIndex(calls []string, prefix string) int {
	for index, call := range calls {
		if strings.HasPrefix(call, prefix) {
			return index
		}
	}
	return -1
}

func countCalls(calls []string, prefix string) int {
	count := 0
	for _, call := range calls {
		if strings.HasPrefix(call, prefix) {
			count++
		}
	}
	return count
}

func countExactCalls(calls []string, expected string) int {
	count := 0
	for _, call := range calls {
		if call == expected {
			count++
		}
	}
	return count
}

func commandIndex(specs []process.Spec, arguments ...string) int {
	for index, spec := range specs {
		if commandMatches(spec, arguments...) {
			return index
		}
	}
	return -1
}

func commandMatches(spec process.Spec, arguments ...string) bool {
	if len(spec.Args) < len(arguments) {
		return false
	}
	for offset := 0; offset <= len(spec.Args)-len(arguments); offset++ {
		matched := true
		for index, argument := range arguments {
			if spec.Args[offset+index] != argument {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}
