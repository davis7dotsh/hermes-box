package app

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//go:embed portable_agents.md
var portableAgents string

const (
	portableArchiveTempPattern  = ".hermes-box-portable-*.tar.tmp"
	portableChecksumTempPattern = ".hermes-box-portable-*.sha256.tmp"
)

var portableProjectDirectories = []string{
	"bin",
	"cmd",
	"cmd/hermes-box",
	"guest",
	"internal",
	"internal/app",
	"internal/config",
	"internal/process",
	"tests",
}

var portableProjectFiles = []string{
	"bin/hermes-box",
	"cmd/hermes-box/main.go",
	"guest/bootstrap.sh",
	"guest/boxadmin.bash_profile",
	"guest/entrypoint.sh",
	"guest/executor.sh",
	"guest/extract-executor.py",
	"guest/hermes-box.sudoers",
	"guest/hermes_gated_approval.py",
	"guest/install-node.sh",
	"guest/patch-hermes-gated-approval.py",
	"guest/restore.sh",
	"guest/snapshot.sh",
	"guest/start.sh",
	"guest/supervisord.conf",
	"guest/tm",
	"guest/tmux.conf",
	"guest/workspace-seed.sh",
	"internal/app/app.go",
	"internal/app/backup.go",
	"internal/app/executor.go",
	"internal/app/host.go",
	"internal/app/lifecycle.go",
	"internal/app/portable.go",
	"internal/app/portable_publish_darwin.go",
	"internal/app/portable_publish_linux.go",
	"internal/app/portable_publish_unsupported.go",
	"internal/app/portable_agents.md",
	"internal/config/config.go",
	"internal/config/secrets.go",
	"internal/process/process.go",
	"tests/hermes-gated-approval.py",
	"go.mod",
	"Smolfile",
	"README.md",
	"PORTABLE_RESTORE.md",
	"EXECUTOR_CONNECTIONS.md",
	"hermes-box.conf.example",
}

func (a *App) cmdPackage(ctx context.Context, args []string) error {
	label := "portable"
	backupDir := ""
	freshSnapshot := false
	if len(args) > 0 && args[0] == "--snapshot" {
		if len(args) < 2 || len(args) > 3 {
			return errors.New("package --snapshot requires one .hermesbox directory and an optional label")
		}
		resolved, err := filepath.Abs(args[1])
		if err != nil {
			return fmt.Errorf("resolve snapshot path: %w", err)
		}
		backupDir = resolved
		if len(args) == 3 {
			label = args[2]
		}
	} else {
		if len(args) > 1 {
			return errors.New("package accepts at most one label")
		}
		if len(args) == 1 {
			label = args[0]
		}
		var err error
		backupDir, err = a.snapshotInternal(ctx, label, true)
		if err != nil {
			return err
		}
		freshSnapshot = true
	}
	var archivePath string
	var err error
	if freshSnapshot {
		// snapshotInternal just created every archive index and checksum. Avoid
		// rereading both large compressed archives before packaging them.
		archivePath, err = a.createPortablePackageFromVerifiedBackupContext(
			ctx,
			backupDir,
			filepath.Base(backupDir),
			label,
		)
	} else {
		archivePath, err = a.createPortablePackageContext(ctx, backupDir, label)
	}
	if err != nil {
		return fmt.Errorf("snapshot saved at %s, but portable packaging failed: %w", backupDir, err)
	}
	fmt.Fprintln(a.stdout, archivePath)
	return nil
}

func (a *App) createPortablePackage(backupDir, label string) (archivePath string, err error) {
	return a.createPortablePackageContext(context.Background(), backupDir, label)
}

func (a *App) createPortablePackageContext(
	ctx context.Context,
	backupDir,
	label string,
) (archivePath string, err error) {
	staged, err := a.stageVerifiedBackupContext(ctx, backupDir)
	if err != nil {
		return "", err
	}
	defer func() {
		cleanupErr := withRestoreCleanupContext(func(cleanupCtx context.Context) error {
			return cleanupStagedBackup(cleanupCtx, staged)
		})
		if cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("remove staged package backup: %w", cleanupErr))
		}
	}()
	return a.createPortablePackageFromVerifiedBackupContext(
		ctx,
		staged,
		filepath.Base(backupDir),
		label,
	)
}

func (a *App) createPortablePackageFromVerifiedBackupContext(
	ctx context.Context,
	backupDir,
	backupName,
	label string,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := requireRegularFile(a.baseArtifact); err != nil {
		return "", err
	}

	safeLabel := sanitizeLabel(label)
	if safeLabel == "" {
		safeLabel = "portable"
	}
	stamp := time.Now().Format("20060102-150405")
	file, err := os.CreateTemp(a.backupsDir, portableArchiveTempPattern)
	if err != nil {
		return "", fmt.Errorf("create portable archive: %w", err)
	}
	temporaryArchive := file.Name()
	archiveTempOwned := true
	defer func() {
		if archiveTempOwned {
			_ = os.Remove(temporaryArchive)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return "", fmt.Errorf("secure portable archive: %w", err)
	}
	hash := sha256.New()
	archive := tar.NewWriter(io.MultiWriter(file, hash))
	closeArchive := func() error {
		if err := archive.Close(); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return err
		}
		return file.Close()
	}

	if err := writeTarDirectory(archive, "hermes-box", 0o700); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := a.addProjectFiles(ctx, archive); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := writeTarFile(
		archive,
		"hermes-box/AGENTS.md",
		[]byte(portableAgents),
		0o600,
	); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := writeTarFile(
		archive,
		"hermes-box/hermes-box.conf",
		[]byte(a.portableConfig()),
		0o600,
	); err != nil {
		_ = file.Close()
		return "", err
	}
	for _, directory := range []string{
		"hermes-box/images",
		"hermes-box/backups",
		"hermes-box/state",
	} {
		if err := writeTarDirectory(archive, directory, 0o700); err != nil {
			_ = file.Close()
			return "", err
		}
	}
	if err := addPathToTarContext(
		ctx,
		archive,
		a.baseArtifact,
		"hermes-box/images/hermes-base.smolmachine",
	); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := addBackupToTarContext(
		ctx,
		archive,
		backupDir,
		filepath.ToSlash(filepath.Join("hermes-box", "backups", backupName)),
	); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := ctx.Err(); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := closeArchive(); err != nil {
		return "", fmt.Errorf("finish portable archive: %w", err)
	}

	var archivePath, checksumPath string
	for sequence := 1; ; sequence++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		archivePath, checksumPath = portableOutputPaths(a.backupsDir, stamp, safeLabel, sequence)
		conflict, err := portableOutputConflict(archivePath, checksumPath)
		if err != nil {
			return "", err
		}
		if conflict {
			continue
		}
		if err := publishPortableFileNoReplace(temporaryArchive, archivePath); err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return "", fmt.Errorf("install portable archive: %w", err)
		}
		archiveTempOwned = false
		break
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("portable archive retained at %s: %w", archivePath, err)
	}

	sum := hex.EncodeToString(hash.Sum(nil))
	checksum := fmt.Sprintf("%s  %s\n", sum, filepath.Base(archivePath))
	checksumFile, err := os.CreateTemp(a.backupsDir, portableChecksumTempPattern)
	if err != nil {
		return "", fmt.Errorf("create portable checksum; archive retained at %s: %w", archivePath, err)
	}
	temporaryChecksum := checksumFile.Name()
	checksumTempOwned := true
	defer func() {
		if checksumTempOwned {
			_ = os.Remove(temporaryChecksum)
		}
	}()
	if err := checksumFile.Chmod(0o600); err != nil {
		_ = checksumFile.Close()
		return "", fmt.Errorf("secure portable checksum; archive retained at %s: %w", archivePath, err)
	}
	if _, err := io.WriteString(checksumFile, checksum); err != nil {
		_ = checksumFile.Close()
		return "", fmt.Errorf("write portable checksum; archive retained at %s: %w", archivePath, err)
	}
	if err := checksumFile.Sync(); err != nil {
		_ = checksumFile.Close()
		return "", fmt.Errorf("sync portable checksum; archive retained at %s: %w", archivePath, err)
	}
	if err := checksumFile.Close(); err != nil {
		return "", fmt.Errorf("finish portable checksum; archive retained at %s: %w", archivePath, err)
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("portable archive retained at %s: %w", archivePath, err)
	}
	if err := publishPortableFileNoReplace(temporaryChecksum, checksumPath); err != nil {
		return "", fmt.Errorf("install portable checksum; archive retained at %s: %w", archivePath, err)
	}
	checksumTempOwned = false
	a.log("portable archive: %s", archivePath)
	a.log("checksum: %s", checksumPath)
	return archivePath, nil
}

func portableOutputPaths(directory, stamp, label string, sequence int) (string, string) {
	suffix := ""
	if sequence > 1 {
		suffix = fmt.Sprintf("-%d", sequence)
	}
	archivePath := filepath.Join(
		directory,
		fmt.Sprintf("hermes-box-portable-%s-%s%s.tar", stamp, label, suffix),
	)
	return archivePath, archivePath + ".sha256"
}

func portableOutputConflict(paths ...string) (bool, error) {
	for _, path := range paths {
		if _, err := os.Lstat(path); err == nil {
			return true, nil
		} else if !os.IsNotExist(err) {
			return false, fmt.Errorf("inspect portable output: %w", err)
		}
	}
	return false, nil
}

func publishPortableFileNoReplace(temporary, destination string) error {
	return renameNoReplace(temporary, destination)
}

func (a *App) addProjectFiles(ctx context.Context, archive *tar.Writer) error {
	for _, relative := range portableProjectDirectories {
		if err := ctx.Err(); err != nil {
			return err
		}
		source := filepath.Join(a.root, filepath.FromSlash(relative))
		target := filepath.ToSlash(filepath.Join("hermes-box", filepath.FromSlash(relative)))
		if err := requireDirectory(source); err != nil {
			return err
		}
		if err := addPathToTarContext(ctx, archive, source, target); err != nil {
			return err
		}
	}
	for _, relative := range portableProjectFiles {
		if err := ctx.Err(); err != nil {
			return err
		}
		source := filepath.Join(a.root, filepath.FromSlash(relative))
		target := filepath.ToSlash(filepath.Join("hermes-box", filepath.FromSlash(relative)))
		if err := requireRegularFile(source); err != nil {
			return err
		}
		if err := addPathToTarContext(ctx, archive, source, target); err != nil {
			return err
		}
	}

	secretMappings := filepath.Join(a.root, "secret-env.txt")
	info, err := os.Lstat(secretMappings)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect secret mappings: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("portable package requires secret-env.txt to be a regular file")
	}
	return addPathToTarContext(ctx, archive, secretMappings, "hermes-box/secret-env.txt")
}

func (a *App) portableConfig() string {
	var builder strings.Builder
	builder.WriteString("# Generated by hermes-box package for portable restore.\n")
	fmt.Fprintf(&builder, "HERMES_BOX_MACHINE_NAME=%s\n", strconv.Quote(a.config.MachineName))
	fmt.Fprintf(&builder, "HERMES_BOX_BUILDER_NAME=%s\n", strconv.Quote(a.config.BuilderName))
	fmt.Fprintf(&builder, "HERMES_BOX_SSH_PORT=%d\n", a.config.SSHPort)
	fmt.Fprintf(&builder, "HERMES_BOX_CPUS=%d\n", a.config.CPUs)
	fmt.Fprintf(&builder, "HERMES_BOX_MEMORY_MIB=%d\n", a.config.MemoryMiB)
	fmt.Fprintf(&builder, "HERMES_BOX_STORAGE_GB=%d\n", a.config.StorageGB)
	fmt.Fprintf(&builder, "HERMES_BOX_OVERLAY_GB=%d\n", a.config.OverlayGB)
	fmt.Fprintf(&builder, "HERMES_BOX_NETWORK_MODE=%s\n", strconv.Quote(a.config.NetworkMode))
	if a.config.HermesCommit != "" {
		fmt.Fprintf(&builder, "HERMES_BOX_HERMES_COMMIT=%s\n", strconv.Quote(a.config.HermesCommit))
	}
	fmt.Fprintf(&builder, "HERMES_BOX_EXECUTOR_ENABLED=%t\n", a.config.ExecutorEnabled)
	fmt.Fprintf(&builder, "HERMES_BOX_EXECUTOR_PORT=%d\n", a.config.ExecutorPort)
	if a.config.ExecutorImage != "" {
		fmt.Fprintf(&builder, "HERMES_BOX_EXECUTOR_IMAGE=%s\n", strconv.Quote(a.config.ExecutorImage))
	}
	builder.WriteString("HERMES_BOX_DATA_DIR=\n")
	builder.WriteString("HERMES_BOX_SSH_KEY=\n")
	return builder.String()
}

func addPathToTar(archive *tar.Writer, source, target string) error {
	return addPathToTarContext(context.Background(), archive, source, target)
}

func addPathToTarContext(
	ctx context.Context,
	archive *tar.Writer,
	source,
	target string,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Lstat(source)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", source, err)
	}
	link := ""
	if info.Mode()&os.ModeSymlink != 0 {
		link, err = os.Readlink(source)
		if err != nil {
			return fmt.Errorf("read symlink %s: %w", source, err)
		}
	}
	header, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return fmt.Errorf("archive metadata for %s: %w", source, err)
	}
	header.Name = filepath.ToSlash(target)
	header.Uid = 0
	header.Gid = 0
	header.Uname = ""
	header.Gname = ""
	if info.IsDir() && !strings.HasSuffix(header.Name, "/") {
		header.Name += "/"
	}
	if err := archive.WriteHeader(header); err != nil {
		return fmt.Errorf("archive %s: %w", source, err)
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	file, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open %s: %w", source, err)
	}
	defer file.Close()
	if _, err := io.Copy(archive, contextReader{ctx: ctx, reader: file}); err != nil {
		return fmt.Errorf("archive contents of %s: %w", source, err)
	}
	return nil
}

func addBackupToTar(archive *tar.Writer, source, target string) error {
	return addBackupToTarContext(context.Background(), archive, source, target)
}

func addBackupToTarContext(
	ctx context.Context,
	archive *tar.Writer,
	source,
	target string,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := requireDirectory(source); err != nil {
		return err
	}
	if err := writeTarDirectory(archive, target, 0o700); err != nil {
		return err
	}
	for _, name := range append(append([]string{}, backupFiles...), "SHA256SUMS") {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := filepath.Join(source, name)
		if err := requireRegularFile(path); err != nil {
			return err
		}
		if err := addPathToTarContext(
			ctx,
			archive,
			path,
			filepath.ToSlash(filepath.Join(target, name)),
		); err != nil {
			return err
		}
	}
	return nil
}

func writeTarDirectory(archive *tar.Writer, name string, mode fs.FileMode) error {
	return archive.WriteHeader(&tar.Header{
		Name:     strings.TrimSuffix(filepath.ToSlash(name), "/") + "/",
		Mode:     int64(mode.Perm()),
		Typeflag: tar.TypeDir,
	})
}

func writeTarFile(archive *tar.Writer, name string, content []byte, mode fs.FileMode) error {
	if err := archive.WriteHeader(&tar.Header{
		Name: filepath.ToSlash(name),
		Mode: int64(mode.Perm()),
		Size: int64(len(content)),
	}); err != nil {
		return err
	}
	_, err := archive.Write(content)
	return err
}

func requireRegularFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("portable package requires %s", path)
		}
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("portable package requires a regular file: %s", path)
	}
	return nil
}

func requireDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("portable package requires a directory: %s", path)
	}
	return nil
}
