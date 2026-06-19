package app

import (
	"archive/tar"
	"context"
	_ "embed"
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

func (a *App) cmdPackage(ctx context.Context, args []string) error {
	label := "portable"
	backupDir := ""
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
	}
	archivePath, err := a.createPortablePackage(backupDir, label)
	if err != nil {
		return fmt.Errorf("snapshot saved at %s, but portable packaging failed: %w", backupDir, err)
	}
	fmt.Fprintln(a.stdout, archivePath)
	return nil
}

func (a *App) createPortablePackage(backupDir, label string) (archivePath string, err error) {
	if err := verifyBackup(backupDir); err != nil {
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
	archivePath = filepath.Join(
		a.backupsDir,
		fmt.Sprintf("hermes-box-portable-%s-%s.tar", stamp, safeLabel),
	)
	temporaryArchive := temporaryPath(archivePath)
	checksumPath := archivePath + ".sha256"
	defer func() {
		if err != nil {
			_ = os.Remove(temporaryArchive)
			_ = os.Remove(archivePath)
			_ = os.Remove(checksumPath)
		}
	}()

	file, err := os.OpenFile(temporaryArchive, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("create portable archive: %w", err)
	}
	archive := tar.NewWriter(file)
	closeArchive := func() error {
		if err := archive.Close(); err != nil {
			_ = file.Close()
			return err
		}
		return file.Close()
	}

	if err := writeTarDirectory(archive, "hermes-box", 0o700); err != nil {
		_ = closeArchive()
		return "", err
	}
	if err := a.addProjectFiles(archive); err != nil {
		_ = closeArchive()
		return "", err
	}
	if err := writeTarFile(
		archive,
		"hermes-box/AGENTS.md",
		[]byte(portableAgents),
		0o600,
	); err != nil {
		_ = closeArchive()
		return "", err
	}
	if err := writeTarFile(
		archive,
		"hermes-box/hermes-box.conf",
		[]byte(a.portableConfig()),
		0o600,
	); err != nil {
		_ = closeArchive()
		return "", err
	}
	for _, directory := range []string{
		"hermes-box/images",
		"hermes-box/backups",
		"hermes-box/state",
	} {
		if err := writeTarDirectory(archive, directory, 0o700); err != nil {
			_ = closeArchive()
			return "", err
		}
	}
	if err := addPathToTar(
		archive,
		a.baseArtifact,
		"hermes-box/images/hermes-base.smolmachine",
	); err != nil {
		_ = closeArchive()
		return "", err
	}
	if err := addTreeToTar(
		archive,
		backupDir,
		filepath.ToSlash(filepath.Join("hermes-box", "backups", filepath.Base(backupDir))),
	); err != nil {
		_ = closeArchive()
		return "", err
	}
	if err := closeArchive(); err != nil {
		return "", fmt.Errorf("finish portable archive: %w", err)
	}
	if err := os.Rename(temporaryArchive, archivePath); err != nil {
		return "", fmt.Errorf("install portable archive: %w", err)
	}
	if err := ensurePrivateFile(archivePath); err != nil {
		return "", err
	}

	sum, err := fileChecksum(archivePath)
	if err != nil {
		return "", err
	}
	checksum := fmt.Sprintf("%s  %s\n", sum, filepath.Base(archivePath))
	if err := os.WriteFile(checksumPath, []byte(checksum), 0o600); err != nil {
		return "", fmt.Errorf("write portable checksum: %w", err)
	}
	a.log("portable archive: %s", archivePath)
	a.log("checksum: %s", checksumPath)
	return archivePath, nil
}

func (a *App) addProjectFiles(archive *tar.Writer) error {
	dataRoot := filepath.Dir(a.imagesDir)
	dataRelative := ""
	if relative, err := filepath.Rel(a.root, dataRoot); err == nil &&
		relative != "." &&
		relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		dataRelative = filepath.Clean(relative)
	}

	return filepath.WalkDir(a.root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(a.root, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		if entry.IsDir() && isHermesDataRoot(path) {
			return filepath.SkipDir
		}
		if filepath.Clean(path) == filepath.Clean(a.sshKey) {
			return nil
		}
		if shouldSkipPortableProjectPath(relative, dataRelative) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.ToSlash(filepath.Join("hermes-box", relative))
		return addPathToTar(archive, path, target)
	})
}

func isHermesDataRoot(path string) bool {
	for _, relative := range []string{"hermes-box.conf", "images", "backups", "state"} {
		info, err := os.Stat(filepath.Join(path, relative))
		if err != nil {
			return false
		}
		if relative == "hermes-box.conf" && !info.Mode().IsRegular() {
			return false
		}
		if relative != "hermes-box.conf" && !info.IsDir() {
			return false
		}
	}
	return true
}

func shouldSkipPortableProjectPath(relative, dataRelative string) bool {
	relative = filepath.Clean(relative)
	first, _, _ := strings.Cut(relative, string(filepath.Separator))
	switch first {
	case ".git", "backups", "dist", "images", "state":
		return true
	}
	if relative == "hermes-box.conf" {
		return true
	}
	if relative == "AGENTS.md" {
		return true
	}
	return dataRelative != "" &&
		(relative == dataRelative ||
			strings.HasPrefix(relative, dataRelative+string(filepath.Separator)))
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
	if _, err := io.Copy(archive, file); err != nil {
		return fmt.Errorf("archive contents of %s: %w", source, err)
	}
	return nil
}

func addTreeToTar(archive *tar.Writer, source, target string) error {
	return filepath.WalkDir(source, func(path string, _ fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		entryTarget := target
		if relative != "." {
			entryTarget = filepath.ToSlash(filepath.Join(target, relative))
		}
		return addPathToTar(archive, path, entryTarget)
	})
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
