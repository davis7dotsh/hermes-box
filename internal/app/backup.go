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

const backupFormat = "hermes-box-v2"

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
	if !a.machineExists(ctx, name) {
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
		if wasRunning {
			if machineStopped {
				_ = a.startNamedMachine(context.Background(), name, a.config.SSHPort)
			} else if servicesStopped {
				_ = a.runQuiet(context.Background(), "smolvm", "machine", "exec", "--name", name, "--", "supervisorctl", "start", "all")
			}
		} else if !machineStopped {
			_ = a.stopNamedMachine(context.Background(), name)
		}
	}()

	stamp := time.Now().Format("20060102-150405")
	safeLabel := sanitizeLabel(label)
	if safeLabel == "" {
		safeLabel = "snapshot"
	}
	backupDir = filepath.Join(a.backupsDir, fmt.Sprintf("%s-%s-%s.hermesbox", name, stamp, safeLabel))
	if err := os.Mkdir(backupDir, 0o700); err != nil {
		return "", fmt.Errorf("create backup directory: %w", err)
	}
	createdBackupDir = backupDir

	a.log("quiescing Hermes and archiving the merged root filesystem")
	remoteScript := "/tmp/hermes-box-snapshot.sh"
	if err := a.run(ctx, "smolvm", "machine", "cp", filepath.Join(a.root, "guest", "snapshot.sh"), name+":"+remoteScript); err != nil {
		return "", err
	}
	defer a.runQuiet(context.Background(), "smolvm", "machine", "exec", "--name", name, "--", "rm", "-f", remoteScript)

	if err := a.run(ctx, "smolvm", "machine", "exec", "--name", name, "--", "supervisorctl", "stop", "all"); err != nil {
		return "", fmt.Errorf("stop guest services: %w", err)
	}
	servicesStopped = true
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
	rootfsFiles, err := archiveFileNames(filepath.Join(backupDir, "rootfs.tar.gz"))
	if err != nil {
		return "", fmt.Errorf("inspect rootfs archive: %w", err)
	}
	if err := writeLines(filepath.Join(backupDir, "rootfs-files.txt"), rootfsFiles); err != nil {
		return "", err
	}

	_ = a.runQuiet(
		ctx,
		"smolvm",
		"machine", "exec",
		"--name", name,
		"--",
		"rm", "-f",
		"/workspace/.hermes-box-rootfs.tar.gz",
		"/tmp/hermes-box-workspace.tar.gz",
		"/tmp/hermes-box-snapshot-warnings.log",
	)
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
	if err := writeChecksums(backupDir); err != nil {
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

func (a *App) cmdRestore(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("restore requires one .hermesbox directory")
	}
	if err := a.validateNetworkMode(); err != nil {
		return err
	}
	backupDir, err := filepath.Abs(args[0])
	if err != nil {
		return fmt.Errorf("resolve backup path: %w", err)
	}
	if err := verifyBackup(backupDir); err != nil {
		return err
	}
	if err := requireRegularFile(a.baseArtifact); err != nil {
		return fmt.Errorf("restore requires the local base artifact: %s", a.baseArtifact)
	}
	if err := a.requireSSHKey(ctx); err != nil {
		return err
	}
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

	safetyBackup := ""
	if a.machineExists(ctx, a.config.MachineName) {
		safetyBackup, err = a.snapshotInternal(ctx, "pre-restore", false)
		if err != nil {
			return err
		}
		a.log("safety snapshot: %s", safetyBackup)
	}

	candidateName := fmt.Sprintf("%s-restore-%d", a.config.MachineName, os.Getpid())
	candidatePort, err := findFreePort()
	if err != nil {
		return err
	}
	a.log("verifying restore candidate %s on port %d", candidateName, candidatePort)
	if err := a.createBlankMachine(ctx, candidateName, candidatePort); err != nil {
		return err
	}
	candidateExists := true
	defer func() {
		if candidateExists {
			_ = a.stopNamedMachine(context.Background(), candidateName)
			_ = a.runQuiet(context.Background(), "smolvm", "machine", "delete", "--name", candidateName, "--force")
		}
	}()
	if err := a.applyBackup(ctx, candidateName, backupDir, candidatePort); err != nil {
		return fmt.Errorf("restore candidate validation failed: %w", err)
	}
	if err := a.stopNamedMachine(ctx, candidateName); err != nil {
		return err
	}
	if err := a.run(ctx, "smolvm", "machine", "delete", "--name", candidateName, "--force"); err != nil {
		return err
	}
	candidateExists = false

	if a.machineExists(ctx, a.config.MachineName) {
		if err := a.run(ctx, "smolvm", "machine", "delete", "--name", a.config.MachineName, "--force"); err != nil {
			return err
		}
	}
	if err := a.clearKnownHost(ctx, a.config.SSHPort); err != nil {
		return err
	}
	if err := a.restorePrimary(ctx, backupDir); err == nil {
		a.log("restore completed")
		return nil
	} else {
		a.log("final restore validation failed: %v", err)
	}

	if safetyBackup == "" {
		return errors.New("restore failed and no original machine existed")
	}
	_ = a.stopNamedMachine(ctx, a.config.MachineName)
	if a.machineExists(ctx, a.config.MachineName) {
		_ = a.runQuiet(ctx, "smolvm", "machine", "delete", "--name", a.config.MachineName, "--force")
	}
	_ = a.clearKnownHost(ctx, a.config.SSHPort)
	if err := a.restorePrimary(ctx, safetyBackup); err != nil {
		return fmt.Errorf("restore failed and original-machine rollback also failed: %w", err)
	}
	return fmt.Errorf("restore failed; original machine was recreated from %s", safetyBackup)
}

func (a *App) restorePrimary(ctx context.Context, backupDir string) error {
	if err := a.createBlankMachine(ctx, a.config.MachineName, a.config.SSHPort); err != nil {
		return err
	}
	return a.applyBackup(ctx, a.config.MachineName, backupDir, a.config.SSHPort)
}

func (a *App) applyBackup(ctx context.Context, name, backupDir string, port int) (err error) {
	if err := a.run(ctx, "smolvm", "machine", "start", "--name", name); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = a.stopNamedMachine(context.Background(), name)
		}
	}()

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
	info, err := os.Stat(directory)
	if err != nil {
		return fmt.Errorf("backup directory not found: %s", directory)
	}
	if !info.IsDir() {
		return fmt.Errorf("backup path is not a directory: %s", directory)
	}
	for _, name := range append(append([]string{}, backupFiles...), "SHA256SUMS") {
		if _, err := os.Stat(filepath.Join(directory, name)); err != nil {
			return fmt.Errorf("backup file is missing: %s", name)
		}
	}
	manifest, err := readKeyValues(filepath.Join(directory, "manifest.txt"))
	if err != nil {
		return err
	}
	if manifest["format"] != backupFormat {
		return fmt.Errorf("unsupported backup format %q", manifest["format"])
	}
	if err := verifyChecksums(directory); err != nil {
		return err
	}
	rootfsNames, err := archiveFileNames(filepath.Join(directory, "rootfs.tar.gz"))
	if err != nil {
		return fmt.Errorf("inspect rootfs archive: %w", err)
	}
	workspaceNames, err := archiveFileNames(filepath.Join(directory, "workspace.tar.gz"))
	if err != nil {
		return fmt.Errorf("inspect workspace archive: %w", err)
	}
	if len(workspaceNames) == 0 {
		return errors.New("workspace archive is empty")
	}
	manifestNames, err := readLines(filepath.Join(directory, "rootfs-files.txt"))
	if err != nil {
		return err
	}
	if strings.Join(rootfsNames, "\n") != strings.Join(manifestNames, "\n") {
		return errors.New("rootfs archive does not match rootfs-files.txt")
	}
	return nil
}

func archiveFileNames(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
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
	var builder strings.Builder
	for _, name := range backupFiles {
		sum, err := fileChecksum(filepath.Join(directory, name))
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
	file, err := os.Open(filepath.Join(directory, "SHA256SUMS"))
	if err != nil {
		return fmt.Errorf("open backup checksums: %w", err)
	}
	defer file.Close()

	expectedFiles := make(map[string]bool, len(backupFiles))
	for _, name := range backupFiles {
		expectedFiles[name] = false
	}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			return errors.New("invalid SHA256SUMS entry")
		}
		expected, err := hex.DecodeString(fields[0])
		if err != nil || len(expected) != sha256.Size {
			return fmt.Errorf("invalid SHA-256 digest for %s", fields[1])
		}
		seen, known := expectedFiles[fields[1]]
		if !known {
			return fmt.Errorf("unexpected checksum entry: %s", fields[1])
		}
		if seen {
			return fmt.Errorf("duplicate checksum entry: %s", fields[1])
		}
		actual, err := fileChecksum(filepath.Join(directory, fields[1]))
		if err != nil {
			return err
		}
		if actual != strings.ToLower(fields[0]) {
			return fmt.Errorf("checksum mismatch: %s", fields[1])
		}
		expectedFiles[fields[1]] = true
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read backup checksums: %w", err)
	}
	for name, seen := range expectedFiles {
		if !seen {
			return fmt.Errorf("checksum entry is missing: %s", name)
		}
	}
	return nil
}

func fileChecksum(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", filepath.Base(path), err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
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
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", filepath.Base(path), err)
	}
	defer file.Close()

	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	return lines, nil
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
