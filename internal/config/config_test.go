package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadPrecedenceAndQuotedValues(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "hermes-box.conf")
	content := `
# Comments and export syntax are supported.
export HERMES_BOX_MACHINE_NAME="configured box"
HERMES_BOX_SSH_PORT=2999
HERMES_BOX_CPUS='8'
HERMES_BOX_NETWORK_MODE=full # required by smolvm 1.0.4
HERMES_BOX_EXECUTOR_ENABLED=true
HERMES_BOX_EXECUTOR_PORT=4888
`
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(root, []string{
		"HERMES_BOX_MACHINE_NAME=environment-box",
		"HERMES_BOX_SSH_PORT=2444",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MachineName != "environment-box" {
		t.Fatalf("MachineName = %q", cfg.MachineName)
	}
	if cfg.SSHPort != 2444 {
		t.Fatalf("SSHPort = %d", cfg.SSHPort)
	}
	if cfg.CPUs != 8 {
		t.Fatalf("CPUs = %d", cfg.CPUs)
	}
	if cfg.NetworkMode != "full" {
		t.Fatalf("NetworkMode = %q", cfg.NetworkMode)
	}
	if !cfg.ExecutorEnabled {
		t.Fatal("ExecutorEnabled = false")
	}
	if cfg.ExecutorPort != 4888 {
		t.Fatalf("ExecutorPort = %d", cfg.ExecutorPort)
	}
}

func TestLoadExecutorDefaults(t *testing.T) {
	cfg, err := Load(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ExecutorEnabled {
		t.Fatal("ExecutorEnabled = true")
	}
	if cfg.ExecutorPort != defaultExecutorPort {
		t.Fatalf("ExecutorPort = %d", cfg.ExecutorPort)
	}
	if cfg.ExecutorImage != defaultExecutorImage {
		t.Fatalf("ExecutorImage = %q", cfg.ExecutorImage)
	}
	if cfg.HermesCommit != defaultHermesCommit {
		t.Fatalf("HermesCommit = %q", cfg.HermesCommit)
	}
	if cfg.NetworkMode != "full" {
		t.Fatalf("NetworkMode = %q, want literal default %q", cfg.NetworkMode, "full")
	}
}

func TestLoadRejectsUnpinnedExecutorImage(t *testing.T) {
	_, err := Load(t.TempDir(), []string{
		"HERMES_BOX_EXECUTOR_ENABLED=true",
		"HERMES_BOX_EXECUTOR_IMAGE=ghcr.io/rhyssullivan/executor-selfhost:latest",
	})
	if err == nil {
		t.Fatal("Load accepted an unpinned Executor image")
	}
}

func TestLoadIgnoresUnusedExecutorImageWhenDisabled(t *testing.T) {
	cfg, err := Load(t.TempDir(), []string{
		"HERMES_BOX_EXECUTOR_IMAGE=unused-placeholder",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ExecutorEnabled || cfg.ExecutorImage != "unused-placeholder" {
		t.Fatalf("Executor config = enabled %t, image %q", cfg.ExecutorEnabled, cfg.ExecutorImage)
	}
}

func TestValidateIgnoresUnusedExecutorPortWhenDisabled(t *testing.T) {
	cfg := Config{
		MachineName: "box", BuilderName: "builder", SSHPort: 2222,
		CPUs: 1, MemoryMiB: 1, StorageGB: 1, OverlayGB: 1,
		NetworkMode: "full",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRejectsExecutorPortCollision(t *testing.T) {
	_, err := Load(t.TempDir(), []string{
		"HERMES_BOX_EXECUTOR_ENABLED=true",
		"HERMES_BOX_EXECUTOR_PORT=2222",
	})
	if err == nil {
		t.Fatal("Load accepted colliding SSH and Executor ports")
	}
}

func TestLoadEmptyValuesUseDefaults(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "hermes-box.conf")
	if err := os.WriteFile(
		configPath,
		[]byte("HERMES_BOX_CPUS=\nHERMES_BOX_NETWORK_MODE=\nHERMES_BOX_STORAGE_GB=27\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(root, []string{
		"HERMES_BOX_CONFIG=",
		"HERMES_BOX_MACHINE_NAME=",
		"HERMES_BOX_SSH_PORT=",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MachineName != defaultMachineName {
		t.Fatalf("MachineName = %q", cfg.MachineName)
	}
	if cfg.SSHPort != defaultSSHPort {
		t.Fatalf("SSHPort = %d", cfg.SSHPort)
	}
	if cfg.CPUs != defaultCPUs {
		t.Fatalf("CPUs = %d", cfg.CPUs)
	}
	if cfg.NetworkMode != defaultNetworkMode {
		t.Fatalf("NetworkMode = %q", cfg.NetworkMode)
	}
	if cfg.StorageGB != 27 {
		t.Fatalf("StorageGB = %d; empty HERMES_BOX_CONFIG skipped %s", cfg.StorageGB, configPath)
	}
}

func TestLoadResolvesRelativeDataDirectory(t *testing.T) {
	root := t.TempDir()
	cfg, err := Load(root, []string{"HERMES_BOX_DATA_DIR=.test-data"})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, ".test-data")
	if cfg.DataDir != want {
		t.Fatalf("DataDir = %q, want %q", cfg.DataDir, want)
	}
}

func TestLoadResolvesExternalSSHKey(t *testing.T) {
	root := t.TempDir()
	cfg, err := Load(root, []string{"HERMES_BOX_SSH_KEY=../keys/miles-ed25519"})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Clean(filepath.Join(root, "../keys/miles-ed25519"))
	if cfg.SSHKey != want {
		t.Fatalf("SSHKey = %q, want %q", cfg.SSHKey, want)
	}
}

func TestLoadRejectsShellCode(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(root, "hermes-box.conf"),
		[]byte("touch /tmp/should-not-run\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(root, nil); err == nil {
		t.Fatal("Load succeeded with shell code")
	}
}

func TestLoadRejectsUnknownHermesBoxSetting(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(root, "hermes-box.conf"),
		[]byte("HERMES_BOX_NETWORK_MOD=none\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(root, nil); err == nil {
		t.Fatal("Load accepted an unknown HERMES_BOX_* setting")
	}
}

func TestLoadIgnoresUnrelatedConfigAssignments(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(root, "hermes-box.conf"),
		[]byte("EDITOR=vim\nHERMES_BOX_SSH_PORT=2444\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SSHPort != 2444 {
		t.Fatalf("SSHPort = %d", cfg.SSHPort)
	}
}

func TestLoadRejectsUnknownHermesBoxEnvironmentSetting(t *testing.T) {
	if _, err := Load(t.TempDir(), []string{
		"HERMES_BOX_NETWORK_MOD=none",
	}); err == nil {
		t.Fatal("Load accepted an unknown HERMES_BOX_* environment setting")
	}
}

func TestLoadAllowsControlAndUnrelatedEnvironmentSettings(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(root, "secret-env.txt"),
		[]byte("TELEGRAM_BOT_TOKEN=HERMES_BOX_TELEGRAM_BOT_TOKEN\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root, []string{
		"EDITOR=vim",
		"HERMES_BOX_CONFIG=",
		"HERMES_BOX_E2E=1",
		"HERMES_BOX_PROJECT_ROOT=/tmp/hermes-box",
		"HERMES_BOX_TELEGRAM_BOT_TOKEN=secret",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestValidateCommit(t *testing.T) {
	cfg := Config{
		MachineName:  "box",
		BuilderName:  "builder",
		SSHPort:      2222,
		CPUs:         1,
		MemoryMiB:    1,
		StorageGB:    1,
		OverlayGB:    1,
		NetworkMode:  "full",
		HermesCommit: "not-a-full-commit",
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate accepted an invalid commit")
	}
}
