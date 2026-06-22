package app

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/davis7dotsh/hermes-box/internal/config"
	"github.com/davis7dotsh/hermes-box/internal/process"
)

func TestCreatePortablePackageIncludesRequiredArtifacts(t *testing.T) {
	const executorImage = "ghcr.io/rhyssullivan/executor-selfhost:v1.5.16@sha256:5d763c718b6567d56e0168d3c205065a37355da1468290f2030f7f6c792f02b6"

	root := t.TempDir()
	dataRoot := t.TempDir()
	sshKey := filepath.Join(root, "credentials", "stable-key")
	writePortableProjectFixture(t, root)
	if err := os.MkdirAll(filepath.Dir(sshKey), 0o700); err != nil {
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
	if err := os.WriteFile(
		filepath.Join(root, "scratch-secret.txt"),
		[]byte("unrelated untracked secret"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "secret-env.txt"),
		[]byte("GUEST_TOKEN=HOST_TOKEN\n"),
		0o600,
	); err != nil {
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
	if err := os.WriteFile(
		filepath.Join(backupDir, "unrelated-private-key"),
		[]byte("must never be packaged"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	archivePath, err := application.createPortablePackage(backupDir, "ready")
	if err != nil {
		t.Fatal(err)
	}
	entries := readPortableArchive(t, archivePath)
	for _, relative := range portableProjectFiles {
		name := filepath.ToSlash(filepath.Join("hermes-box", filepath.FromSlash(relative)))
		if _, ok := entries[name]; !ok {
			t.Errorf("portable archive is missing allowlisted runtime file %s", name)
		}
	}
	for _, name := range []string{
		"hermes-box/AGENTS.md",
		"hermes-box/bin/hermes-box",
		"hermes-box/cmd/hermes-box/main.go",
		"hermes-box/internal/app/app.go",
		"hermes-box/internal/app/portable_agents.md",
		"hermes-box/internal/process/process.go",
		"hermes-box/guest/start.sh",
		"hermes-box/tests/hermes-gated-approval.py",
		"hermes-box/go.mod",
		"hermes-box/Smolfile",
		"hermes-box/README.md",
		"hermes-box/PORTABLE_RESTORE.md",
		"hermes-box/EXECUTOR_CONNECTIONS.md",
		"hermes-box/hermes-box.conf.example",
		"hermes-box/secret-env.txt",
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
		"hermes-box/scratch-secret.txt",
		"hermes-box/internal/app/app_test.go",
		"hermes-box/internal/app/credentials.go",
		"hermes-box/internal/testdata/private-token.txt",
		"hermes-box/guest/test_probe.sh",
		"hermes-box/guest/debug.sh",
		"hermes-box/guest/scratch-secret.txt",
		"hermes-box/another-box/hermes-box.conf",
		"hermes-box/another-box/state/hermes-box-ed25519",
		"hermes-box/state/hermes-box-ed25519",
		"hermes-box/state/hermes-box-ed25519.pub",
		"hermes-box/backups/portable.hermesbox/unrelated-private-key",
	} {
		if _, ok := entries[forbidden]; ok {
			t.Errorf("portable archive contains excluded project data: %s", forbidden)
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
	for _, path := range []string{archivePath, checksumPath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %o, want 600", filepath.Base(path), info.Mode().Perm())
		}
	}
}

func TestCreatePortablePackageDirectCallVerifiesBackup(t *testing.T) {
	root := t.TempDir()
	application := New(root, config.Config{}, process.OSRunner{}, io.Discard, io.Discard)
	backupDir := createTestBackup(t)
	if err := os.WriteFile(
		filepath.Join(backupDir, "manifest.txt"),
		[]byte("format=hermes-box-v2\nchanged=true\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := application.createPortablePackage(backupDir, "invalid"); err == nil {
		t.Fatal("createPortablePackage accepted an unverified backup")
	}
}

func TestPortablePackageUsesFrozenBackupAndOriginalLogicalName(t *testing.T) {
	root := t.TempDir()
	writePortableProjectFixture(t, root)
	application := New(root, config.Config{}, process.OSRunner{}, io.Discard, io.Discard)
	if err := application.prepareDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(application.baseArtifact, []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(application.backupsDir, "original.hermesbox")
	if err := copyTestBackup(t, source); err != nil {
		t.Fatal(err)
	}
	original := make(map[string][]byte, len(backupFiles)+1)
	for _, name := range append(append([]string{}, backupFiles...), "SHA256SUMS") {
		content, err := os.ReadFile(filepath.Join(source, name))
		if err != nil {
			t.Fatal(err)
		}
		original[name] = content
	}
	staged, err := application.stageVerifiedBackupContext(context.Background(), source)
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

	archivePath, err := application.createPortablePackageFromVerifiedBackupContext(
		context.Background(),
		staged,
		filepath.Base(source),
		"frozen",
	)
	if err != nil {
		t.Fatal(err)
	}
	entries := readPortableArchive(t, archivePath)
	for name, want := range original {
		entry := filepath.ToSlash(filepath.Join("hermes-box", "backups", filepath.Base(source), name))
		if got := entries[entry]; !bytes.Equal(got, want) {
			t.Fatalf("embedded frozen backup %s changed", name)
		}
	}
	for name := range entries {
		if strings.Contains(name, ".restore-backup-") {
			t.Fatalf("portable archive exposed staging path: %s", name)
		}
	}
}

func TestCanceledPortablePackagePublishesNothingAndCleansStage(t *testing.T) {
	root := t.TempDir()
	writePortableProjectFixture(t, root)
	application := New(root, config.Config{}, process.OSRunner{}, io.Discard, io.Discard)
	if err := application.prepareDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(application.baseArtifact, []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	backupDir := filepath.Join(application.backupsDir, "canceled.hermesbox")
	if err := copyTestBackup(t, backupDir); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := application.createPortablePackageContext(ctx, backupDir, "canceled"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled package error = %v", err)
	}
	stateEntries, err := os.ReadDir(application.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range stateEntries {
		if strings.HasPrefix(entry.Name(), ".restore-backup-") {
			t.Fatalf("canceled package retained staged backup: %s", entry.Name())
		}
	}
	backupEntries, err := os.ReadDir(application.backupsDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range backupEntries {
		if strings.HasPrefix(entry.Name(), ".hermes-box-portable-") ||
			strings.HasPrefix(entry.Name(), "hermes-box-portable-") {
			t.Fatalf("canceled package published or retained temp output: %s", entry.Name())
		}
	}
}

func TestPortableProjectAllowlistCoversRunnableSources(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate portable test source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
	allowlisted := make(map[string]bool, len(portableProjectFiles))
	for _, relative := range portableProjectFiles {
		allowlisted[filepath.ToSlash(relative)] = true
	}
	checkTree := func(relative string, include func(string) bool) {
		t.Helper()
		start := filepath.Join(root, filepath.FromSlash(relative))
		if err := filepath.WalkDir(start, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			projectRelative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			projectRelative = filepath.ToSlash(projectRelative)
			if include(projectRelative) && !allowlisted[projectRelative] {
				t.Errorf("runnable source is missing from portable allowlist: %s", projectRelative)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	checkTree("cmd", func(relative string) bool {
		return strings.HasSuffix(relative, ".go") && !strings.HasSuffix(relative, "_test.go")
	})
	checkTree("internal", func(relative string) bool {
		return strings.HasSuffix(relative, ".go") && !strings.HasSuffix(relative, "_test.go")
	})
	checkTree("guest", func(string) bool { return true })
	for _, relative := range []string{
		"Smolfile",
		"internal/app/portable_agents.md",
		"tests/hermes-gated-approval.py",
	} {
		if !allowlisted[relative] {
			t.Errorf("required runtime input is missing from portable allowlist: %s", relative)
		}
	}
}

func TestPortableOutputPathsAvoidExistingArchiveOrChecksum(t *testing.T) {
	directory := t.TempDir()
	first, firstChecksum := portableOutputPaths(directory, "20260620-120000", "ready", 1)
	second, secondChecksum := portableOutputPaths(directory, "20260620-120000", "ready", 2)
	if second != filepath.Join(directory, "hermes-box-portable-20260620-120000-ready-2.tar") ||
		secondChecksum != second+".sha256" {
		t.Fatalf("second output pair = %s, %s", second, secondChecksum)
	}
	for _, collision := range []string{first, firstChecksum} {
		if err := os.WriteFile(collision, []byte("existing"), 0o600); err != nil {
			t.Fatal(err)
		}
		conflict, err := portableOutputConflict(first, firstChecksum)
		if err != nil {
			t.Fatal(err)
		}
		if !conflict {
			t.Fatalf("portableOutputConflict missed %s", collision)
		}
		if err := os.Remove(collision); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPublishPortableFileNoReplacePreservesInterloper(t *testing.T) {
	directory := t.TempDir()
	temporary, err := os.CreateTemp(directory, portableArchiveTempPattern)
	if err != nil {
		t.Fatal(err)
	}
	temporaryPath := temporary.Name()
	if _, err := temporary.WriteString("new"); err != nil {
		t.Fatal(err)
	}
	if err := temporary.Close(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(filepath.Base(temporaryPath), ".") {
		t.Fatalf("temporary output is visible: %s", temporaryPath)
	}
	secondTemporary, err := os.CreateTemp(directory, portableArchiveTempPattern)
	if err != nil {
		t.Fatal(err)
	}
	secondTemporaryPath := secondTemporary.Name()
	if err := secondTemporary.Close(); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(secondTemporaryPath)
	if temporaryPath == secondTemporaryPath || !strings.HasPrefix(filepath.Base(secondTemporaryPath), ".") {
		t.Fatalf("temporary outputs are not hidden and unique: %s, %s", temporaryPath, secondTemporaryPath)
	}
	destination := filepath.Join(directory, "portable.tar")
	if err := os.WriteFile(destination, []byte("interloper"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := publishPortableFileNoReplace(temporaryPath, destination); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("publish error = %v, want destination-exists", err)
	}
	content, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "interloper" {
		t.Fatalf("concurrent destination changed to %q", content)
	}
	if content, err := os.ReadFile(temporaryPath); err != nil || string(content) != "new" {
		t.Fatalf("owned temporary changed: %q, %v", content, err)
	}
}

func TestPublishPortableFileNoReplaceRejectsEveryExistingPathType(t *testing.T) {
	for _, kind := range []string{"file", "directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			directory := t.TempDir()
			temporary := filepath.Join(directory, ".owned.tmp")
			if err := os.WriteFile(temporary, []byte("new"), 0o600); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(directory, "portable.tar")
			switch kind {
			case "file":
				if err := os.WriteFile(destination, []byte("existing"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(destination, 0o700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := filepath.Join(directory, "target")
				if err := os.WriteFile(target, []byte("existing"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, destination); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Lstat(destination)
			if err != nil {
				t.Fatal(err)
			}
			if err := publishPortableFileNoReplace(temporary, destination); !errors.Is(err, fs.ErrExist) {
				t.Fatalf("publish error = %v, want destination-exists", err)
			}
			after, err := os.Lstat(destination)
			if err != nil {
				t.Fatal(err)
			}
			if before.Mode() != after.Mode() {
				t.Fatalf("destination mode changed from %v to %v", before.Mode(), after.Mode())
			}
			if content, err := os.ReadFile(temporary); err != nil || string(content) != "new" {
				t.Fatalf("owned temporary changed: %q, %v", content, err)
			}
		})
	}
}

func TestPortableSecondPublishFailureRetainsBothOwnedAndInterloperFiles(t *testing.T) {
	directory := t.TempDir()
	archiveTemp := filepath.Join(directory, ".archive.tmp")
	checksumTemp := filepath.Join(directory, ".checksum.tmp")
	archivePath := filepath.Join(directory, "portable.tar")
	checksumPath := archivePath + ".sha256"
	for path, content := range map[string]string{
		archiveTemp:  "archive",
		checksumTemp: "checksum",
		checksumPath: "interloper",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := publishPortableFileNoReplace(archiveTemp, archivePath); err != nil {
		t.Fatal(err)
	}
	if err := publishPortableFileNoReplace(checksumTemp, checksumPath); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("checksum publish error = %v, want destination-exists", err)
	}
	for path, want := range map[string]string{
		archivePath:  "archive",
		checksumPath: "interloper",
		checksumTemp: "checksum",
	} {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != want {
			t.Fatalf("%s = %q, want %q", path, content, want)
		}
	}
}

func TestConcurrentPortablePackagesUseDistinctOutputs(t *testing.T) {
	root := t.TempDir()
	writePortableProjectFixture(t, root)
	application := New(root, config.Config{}, process.OSRunner{}, io.Discard, io.Discard)
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

	var wait sync.WaitGroup
	results := make(chan string, 2)
	errors := make(chan error, 2)
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			path, err := application.createPortablePackage(backupDir, "concurrent")
			if err != nil {
				errors <- err
				return
			}
			results <- path
		}()
	}
	wait.Wait()
	close(results)
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	var paths []string
	for path := range results {
		paths = append(paths, path)
	}
	if len(paths) != 2 || paths[0] == paths[1] {
		t.Fatalf("portable paths = %v, want two distinct outputs", paths)
	}
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path + ".sha256"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAddBackupToTarRejectsRequiredSymlink(t *testing.T) {
	backupDir := createTestBackup(t)
	manifest := filepath.Join(backupDir, "manifest.txt")
	target := filepath.Join(t.TempDir(), "manifest.txt")
	if err := os.WriteFile(target, []byte("format=hermes-box-v2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(manifest); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, manifest); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	archive := tar.NewWriter(&output)
	defer archive.Close()
	if err := addBackupToTar(archive, backupDir, "backup"); err == nil {
		t.Fatal("addBackupToTar accepted a symlinked required file")
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

func writePortableProjectFixture(t *testing.T, root string) {
	t.Helper()
	for _, relative := range portableProjectDirectories {
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, relative := range portableProjectFiles {
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if relative == "bin/hermes-box" || strings.HasSuffix(relative, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(path, []byte("fixture\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	for relative, content := range map[string]string{
		"internal/app/app_test.go":            "package app\n",
		"internal/app/credentials.go":         "package app\n",
		"internal/testdata/private-token.txt": "must stay out\n",
		"guest/test_probe.sh":                 "#!/usr/bin/env bash\n",
		"guest/debug.sh":                      "#!/usr/bin/env bash\n",
		"guest/scratch-secret.txt":            "must stay out\n",
	} {
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
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
