package app

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/davis7dotsh/hermes-box/internal/config"
	"github.com/davis7dotsh/hermes-box/internal/process"
)

func TestCreatePortablePackageIncludesRequiredArtifacts(t *testing.T) {
	const executorImage = "ghcr.io/rhyssullivan/executor-selfhost:v1.5.12@sha256:e40b2179c005b3124e794e9a8505341db46d0a9a1631e7f3fdcd023462ecf70b"

	root := t.TempDir()
	dataRoot := t.TempDir()
	sshKey := filepath.Join(root, "credentials", "stable-key")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(sshKey), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "bin", "hermes-box"),
		[]byte("#!/usr/bin/env bash\n"),
		0o755,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "hermes-box.conf"),
		[]byte("HERMES_BOX_DATA_DIR=/old/host/path\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "AGENTS.md"),
		[]byte("source contributor instructions\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sshKey, []byte("external private key"), 0o600); err != nil {
		t.Fatal(err)
	}
	nestedDataRoot := filepath.Join(root, "another-box")
	for _, directory := range []string{"images", "backups", "state"} {
		if err := os.MkdirAll(filepath.Join(nestedDataRoot, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(
		filepath.Join(nestedDataRoot, "hermes-box.conf"),
		[]byte("HERMES_BOX_MACHINE_NAME=another-box\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(nestedDataRoot, "state", "hermes-box-ed25519"),
		[]byte("unrelated private key"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{
		MachineName:     "portable-box",
		BuilderName:     "portable-builder",
		SSHPort:         2444,
		CPUs:            2,
		MemoryMiB:       4096,
		StorageGB:       12,
		OverlayGB:       5,
		NetworkMode:     "full",
		ExecutorEnabled: true,
		ExecutorPort:    4789,
		ExecutorImage:   executorImage,
		DataDir:         dataRoot,
		SSHKey:          sshKey,
	}
	application := New(root, cfg, process.OSRunner{}, io.Discard, io.Discard)
	if err := application.prepareDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(application.baseArtifact, []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	backupDir := filepath.Join(application.backupsDir, "portable.hermesbox")
	if err := copyTestBackup(t, backupDir); err != nil {
		t.Fatal(err)
	}

	archivePath, err := application.createPortablePackage(backupDir, "ready")
	if err != nil {
		t.Fatal(err)
	}
	entries := readPortableArchive(t, archivePath)
	for _, name := range []string{
		"hermes-box/AGENTS.md",
		"hermes-box/bin/hermes-box",
		"hermes-box/hermes-box.conf",
		"hermes-box/images/hermes-base.smolmachine",
		"hermes-box/backups/portable.hermesbox/manifest.txt",
	} {
		if _, ok := entries[name]; !ok {
			t.Errorf("portable archive is missing %s", name)
		}
	}
	portableAgentsFile := string(entries["hermes-box/AGENTS.md"])
	for _, expected := range []string{
		"shasum -a 256 -c hermes-box-portable-*.tar.sha256",
		"tar -xpf hermes-box-portable-*.tar",
		"./bin/hermes-box restore backups/*.hermesbox",
		"./bin/hermes-box start",
		"./bin/hermes-box status",
		"./bin/hermes-box ssh",
		"https://github.com/davis7dotsh/hermes-box",
	} {
		if !strings.Contains(portableAgentsFile, expected) {
			t.Errorf("portable AGENTS.md is missing %q", expected)
		}
	}
	if strings.Contains(portableAgentsFile, "source contributor instructions") {
		t.Fatal("portable AGENTS.md retained source contributor instructions")
	}
	for _, forbidden := range []string{
		"hermes-box/credentials/stable-key",
		"hermes-box/another-box/hermes-box.conf",
		"hermes-box/another-box/state/hermes-box-ed25519",
		"hermes-box/state/hermes-box-ed25519",
		"hermes-box/state/hermes-box-ed25519.pub",
	} {
		if _, ok := entries[forbidden]; ok {
			t.Errorf("portable archive contains SSH key material: %s", forbidden)
		}
	}
	portableConfig := string(entries["hermes-box/hermes-box.conf"])
	if !strings.Contains(portableConfig, "HERMES_BOX_DATA_DIR=\n") {
		t.Fatalf("portable config retained a host data directory:\n%s", portableConfig)
	}
	if strings.Contains(portableConfig, "/old/host/path") {
		t.Fatalf("portable config contains the old host path:\n%s", portableConfig)
	}
	if !strings.Contains(portableConfig, "HERMES_BOX_SSH_KEY=\n") {
		t.Fatalf("portable config is missing the external SSH key handoff:\n%s", portableConfig)
	}
	if strings.Contains(portableConfig, sshKey) {
		t.Fatalf("portable config retained the source SSH key path:\n%s", portableConfig)
	}
	for _, expected := range []string{
		"HERMES_BOX_EXECUTOR_ENABLED=true\n",
		"HERMES_BOX_EXECUTOR_PORT=4789\n",
		"HERMES_BOX_EXECUTOR_IMAGE=" + strconv.Quote(executorImage) + "\n",
	} {
		if !strings.Contains(portableConfig, expected) {
			t.Fatalf("portable config is missing %q:\n%s", expected, portableConfig)
		}
	}
	checksumPath := archivePath + ".sha256"
	checksum, err := os.ReadFile(checksumPath)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := fileChecksum(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(checksum) != sum+"  "+filepath.Base(archivePath)+"\n" {
		t.Fatalf("unexpected checksum file: %q", checksum)
	}
}

func TestRequireRegularFileRejectsSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	link := filepath.Join(directory, "link")
	if err := os.WriteFile(target, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := requireRegularFile(link); err == nil {
		t.Fatal("requireRegularFile accepted a symlink")
	}
}

func copyTestBackup(t *testing.T, destination string) error {
	t.Helper()
	source := createTestBackup(t)
	if err := os.Mkdir(destination, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		content, err := os.ReadFile(filepath.Join(source, entry.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(destination, entry.Name()), content, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func readPortableArchive(t *testing.T, path string) map[string][]byte {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	entries := make(map[string][]byte)
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.FileInfo().Mode().IsRegular() {
			content, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			entries[header.Name] = content
		}
	}
	return entries
}
