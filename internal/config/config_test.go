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
