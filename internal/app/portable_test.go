package app

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/davis7dotsh/hermes-box/internal/config"
	"github.com/davis7dotsh/hermes-box/internal/process"
)

func TestCreatePortablePackageIncludesRequiredArtifacts(t *testing.T) {
	root := t.TempDir()
	dataRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
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

	cfg := config.Config{
		MachineName: "portable-box",
		BuilderName: "portable-builder",
		SSHPort:     2444,
		CPUs:        2,
		MemoryMiB:   4096,
		StorageGB:   12,
		OverlayGB:   5,
		NetworkMode: "full",
		DataDir:     dataRoot,
	}
	application := New(root, cfg, process.OSRunner{}, io.Discard, io.Discard)
	if err := application.prepareDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(application.baseArtifact, []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(application.sshKey, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(application.sshKey+".pub", []byte("public"), 0o600); err != nil {
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
		"hermes-box/bin/hermes-box",
		"hermes-box/hermes-box.conf",
		"hermes-box/images/hermes-base.smolmachine",
		"hermes-box/backups/portable.hermesbox/manifest.txt",
		"hermes-box/state/hermes-box-ed25519",
		"hermes-box/state/hermes-box-ed25519.pub",
	} {
		if _, ok := entries[name]; !ok {
			t.Errorf("portable archive is missing %s", name)
		}
	}
	portableConfig := string(entries["hermes-box/hermes-box.conf"])
	if !strings.Contains(portableConfig, "HERMES_BOX_DATA_DIR=\n") {
		t.Fatalf("portable config retained a host data directory:\n%s", portableConfig)
	}
	if strings.Contains(portableConfig, "/old/host/path") {
		t.Fatalf("portable config contains the old host path:\n%s", portableConfig)
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
