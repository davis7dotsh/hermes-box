package app

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
mkdir -p "$directory/root/etc" "$directory/workspace/work"
printf legacy-root >"$directory/root/etc/example"
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

func TestArchiveFileNamesRejectsTraversal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unsafe.tar.gz")
	writeArchive(t, path, []string{"../host-file"})
	if _, err := archiveFileNames(path); err == nil {
		t.Fatal("archiveFileNames accepted path traversal")
	}
}

func TestSanitizeLabel(t *testing.T) {
	if got := sanitizeLabel(" ready / now! "); got != "-ready--now-" {
		t.Fatalf("sanitizeLabel() = %q", got)
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

func createTestBackup(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	rootfs := filepath.Join(directory, "rootfs.tar.gz")
	workspace := filepath.Join(directory, "workspace.tar.gz")
	writeArchive(t, rootfs, []string{"./", "./etc/", "./etc/hosts"})
	writeArchive(t, workspace, []string{"./", "./work/", "./work/example.txt"})

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
		[]byte("format=hermes-box-v2\ncreated=2026-06-17T00:00:00Z\nmachine=test\n"),
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
		spec.Args[1] == "status" {
		return []byte(`{"state":"running"}`), nil
	}
	return nil, errors.New("unexpected output command")
}

type restartFailingSnapshotRunner struct {
	t *testing.T
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
			writeArchive(r.t, destination, []string{"./", "./work/", "./work/example"})
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
		spec.Args[1] == "status" {
		return []byte(`{"state":"running"}`), nil
	}
	if spec.Name == "smolvm" && len(spec.Args) == 1 && spec.Args[0] == "--version" {
		return []byte("smolvm 1.0.4\n"), nil
	}
	return nil, errors.New("unexpected output command")
}
