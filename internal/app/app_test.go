package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/davis7dotsh/hermes-box/internal/config"
	"github.com/davis7dotsh/hermes-box/internal/process"
)

type initCleanupFailureRunner struct {
	cleanupErr error
	deleted    bool
}

func (r *initCleanupFailureRunner) Run(_ context.Context, spec process.Spec) error {
	if spec.Name == "smolvm" &&
		strings.Join(spec.Args, " ") == "machine delete --name test-builder --force" {
		r.deleted = true
		return r.cleanupErr
	}
	return nil
}

func (*initCleanupFailureRunner) Output(_ context.Context, _ process.Spec) ([]byte, error) {
	return nil, errors.New("unexpected output call")
}

func TestWithLockReusesUnlockedLockFile(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{DataDir: root}
	application := New(root, cfg, process.OSRunner{}, io.Discard, io.Discard)
	if err := application.prepareDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "state", "operation.lock"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	called := false
	if err := application.withLock(func() error {
		called = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("locked function was not called")
	}
}

func TestWithLockReapsOnlyAfterExclusiveAcquisition(t *testing.T) {
	root := t.TempDir()
	application := New(root, config.Config{DataDir: root}, process.OSRunner{}, io.Discard, io.Discard)
	contender := New(root, config.Config{DataDir: root}, process.OSRunner{}, io.Discard, io.Discard)
	if err := application.prepareDirs(); err != nil {
		t.Fatal(err)
	}

	var staged, temporary string
	if err := application.withLock(func() error {
		var err error
		staged, err = os.MkdirTemp(application.stateDir, ".restore-backup-")
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(staged, "manifest.txt"), []byte("stale"), 0o400); err != nil {
			return err
		}
		if err := os.Chmod(staged, 0o500); err != nil {
			return err
		}
		file, err := os.CreateTemp(application.backupsDir, portableArchiveTempPattern)
		if err != nil {
			return err
		}
		temporary = file.Name()
		if err := file.Close(); err != nil {
			return err
		}
		called := false
		err = contender.withLock(func() error {
			called = true
			return nil
		})
		if err == nil || called {
			t.Fatalf("contending operation acquired the active lock: called=%t error=%v", called, err)
		}
		for _, path := range []string{staged, temporary} {
			if _, err := os.Lstat(path); err != nil {
				t.Fatalf("contender reaped active artifact %s: %v", path, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	var current string
	if err := contender.withLock(func() error {
		for _, path := range []string{staged, temporary} {
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatalf("stale artifact was not reaped before callback: %s: %v", path, err)
			}
		}
		file, err := os.CreateTemp(contender.backupsDir, portableChecksumTempPattern)
		if err != nil {
			return err
		}
		current = file.Name()
		return file.Close()
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(current); err != nil {
		t.Fatalf("artifact created by callback did not survive its operation: %v", err)
	}
	if err := application.withLock(func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(current); !os.IsNotExist(err) {
		t.Fatalf("artifact was not reaped by the next operation: %v", err)
	}
}

func TestStaleArtifactReaperSkipsSuspiciousAndVisiblePaths(t *testing.T) {
	root := t.TempDir()
	application := New(root, config.Config{DataDir: root}, process.OSRunner{}, io.Discard, io.Discard)
	if err := application.prepareDirs(); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(root, "external-sentinel")
	if err := os.WriteFile(external, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}

	suspicious := filepath.Join(application.stateDir, ".restore-backup-suspicious")
	if err := os.Mkdir(suspicious, 0o700); err != nil {
		t.Fatal(err)
	}
	userStage := filepath.Join(application.stateDir, ".restore-backup-keep")
	if err := os.Mkdir(userStage, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userStage, "manifest.txt"), []byte("user file"), 0o600); err != nil {
		t.Fatal(err)
	}
	outOfRangeStage := filepath.Join(application.stateDir, ".restore-backup-9999999999")
	if err := os.Mkdir(outOfRangeStage, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outOfRangeStage, "manifest.txt"), []byte("user file"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"manifest.txt": "recognized",
		"unexpected":   "preserve everything",
	} {
		if err := os.WriteFile(filepath.Join(suspicious, name), []byte(content), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	childLinkStage := filepath.Join(application.stateDir, ".restore-backup-child-link")
	if err := os.Mkdir(childLinkStage, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(childLinkStage, "manifest.txt")); err != nil {
		t.Fatal(err)
	}
	rootLink := filepath.Join(application.stateDir, ".restore-backup-root-link")
	if err := os.Symlink(filepath.Dir(external), rootLink); err != nil {
		t.Fatal(err)
	}

	portableLink := filepath.Join(application.backupsDir, ".hermes-box-portable-link.tar.tmp")
	if err := os.Symlink(external, portableLink); err != nil {
		t.Fatal(err)
	}
	portableDirectory := filepath.Join(application.backupsDir, ".hermes-box-portable-directory.sha256.tmp")
	if err := os.Mkdir(portableDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	visibleArchive := filepath.Join(application.backupsDir, "hermes-box-portable-final.tar")
	nearMiss := filepath.Join(application.backupsDir, ".hermes-box-portable-.tar.tmp")
	userPortable := filepath.Join(application.backupsDir, ".hermes-box-portable-notes.tar.tmp")
	outOfRangePortable := filepath.Join(application.backupsDir, ".hermes-box-portable-9999999999.tar.tmp")
	for _, path := range []string{visibleArchive, nearMiss, userPortable, outOfRangePortable} {
		if err := os.WriteFile(path, []byte("preserve"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stale, err := os.CreateTemp(application.backupsDir, portableArchiveTempPattern)
	if err != nil {
		t.Fatal(err)
	}
	stalePath := stale.Name()
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}

	if err := application.withLock(func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stalePath); !os.IsNotExist(err) {
		t.Fatalf("recognized portable temp was not removed: %v", err)
	}
	for _, path := range []string{
		suspicious,
		filepath.Join(suspicious, "manifest.txt"),
		userStage,
		filepath.Join(userStage, "manifest.txt"),
		outOfRangeStage,
		filepath.Join(outOfRangeStage, "manifest.txt"),
		childLinkStage,
		rootLink,
		portableLink,
		portableDirectory,
		visibleArchive,
		nearMiss,
		userPortable,
		outOfRangePortable,
		external,
	} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("safe-skip path changed %s: %v", path, err)
		}
	}
	content, err := os.ReadFile(external)
	if err != nil || string(content) != "preserve" {
		t.Fatalf("external sentinel changed: %q, %v", content, err)
	}
}

func TestFinishInitBuilderCleanupReportsPrimaryAndCleanupFailures(t *testing.T) {
	primaryErr := errors.New("builder provisioning failed")
	cleanupErr := errors.New("builder delete failed")
	runner := &initCleanupFailureRunner{cleanupErr: cleanupErr}
	application := New(
		t.TempDir(),
		config.Config{BuilderName: "test-builder"},
		runner,
		io.Discard,
		io.Discard,
	)

	err := application.finishInitBuilderCleanup(primaryErr, true)
	if !errors.Is(err, primaryErr) || !errors.Is(err, cleanupErr) {
		t.Fatalf("cleanup result must preserve both errors, got %v", err)
	}
	if !runner.deleted {
		t.Fatal("disposable builder cleanup was not attempted")
	}
	if !strings.Contains(err.Error(), `clean up disposable builder "test-builder"`) {
		t.Fatalf("cleanup error does not identify the builder: %v", err)
	}

	runner.deleted = false
	err = application.finishInitBuilderCleanup(nil, true)
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("cleanup failure after otherwise successful init was discarded: %v", err)
	}
	if !runner.deleted {
		t.Fatal("successful init path did not retry disposable builder cleanup")
	}
}

func TestPrintInitHandoffWithoutExecutor(t *testing.T) {
	root := t.TempDir()
	var stderr bytes.Buffer
	application := New(
		root,
		config.Config{
			MachineName:     "test-box",
			SSHPort:         2223,
			ExecutorEnabled: false,
		},
		process.OSRunner{},
		io.Discard,
		&stderr,
	)

	application.printInitHandoff()
	output := stderr.String()
	for _, expected := range []string{
		"machine:  test-box",
		"boxadmin@127.0.0.1:2223",
		"Guest phase",
		"Host phase",
		"hermes model",
		"hermes auth add openai-codex --type oauth",
		"codex login --device-auth",
		"tmux new -As codex",
		"package configured-agent",
		".tar archive",
		".sha256",
		"encrypted",
		"off-host",
		application.sshKey,
		"hermes-box stop",
		"hermes-box start",
		"hermes-box status",
		"hermes-box logs -f",
		"hermes-box shell",
		"Adding it requires recreating this machine",
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("handoff does not contain %q:\n%s", expected, output)
		}
	}
	authIndex := strings.Index(output, "hermes auth add openai-codex --type oauth")
	modelIndex := strings.Index(output, "hermes model")
	if authIndex < 0 || modelIndex < 0 || authIndex >= modelIndex {
		t.Fatalf("handoff must authenticate Codex OAuth before model selection:\n%s", output)
	}
	if strings.Contains(output, "executor open") || strings.Contains(output, "mcp-test") {
		t.Fatalf("disabled Executor handoff contains Executor setup commands:\n%s", output)
	}
}

func TestPrintInitHandoffWithExecutor(t *testing.T) {
	root := t.TempDir()
	var stderr bytes.Buffer
	application := New(
		root,
		config.Config{
			MachineName:     "test-box",
			SSHPort:         2223,
			ExecutorEnabled: true,
			ExecutorPort:    4789,
		},
		process.OSRunner{},
		io.Discard,
		&stderr,
	)

	application.printInitHandoff()
	output := stderr.String()
	for _, expected := range []string{
		"http://127.0.0.1:4789",
		"executor open",
		"create the admin account",
		"configure provider integrations/OAuth",
		"destination-local API key",
		"executor auth set",
		"executor connect-hermes",
		"executor status",
		filepath.Join(root, "EXECUTOR_CONNECTIONS.md"),
		"/var/log/hermes-box-startup.log",
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("handoff does not contain %q:\n%s", expected, output)
		}
	}
	if strings.Contains(output, "mcp-test") || strings.Contains(output, "was not installed") {
		t.Fatalf("enabled Executor handoff has stale or disabled guidance:\n%s", output)
	}
}
