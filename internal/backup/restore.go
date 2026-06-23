package backup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"filippo.io/age"
)

// Restore verifies the complete encrypted bundle before creating destination.
// It stages verification beside the destination and atomically publishes it.
// Callers consume destination/data, destination/applied.lock,
// and destination/artifacts as separate inputs to VM reconstruction.
func Restore(ctx context.Context, archivePath, envelopePath string, identity age.Identity, validateClosure ClosureValidator, destination string) (Manifest, error) {
	return restore(ctx, archivePath, envelopePath, identity, validateClosure, destination, restoreFileOps{
		syncTree:         syncTree,
		publishDirectory: publishDirectoryExclusive,
		syncDirectory:    syncDirectory,
		removeAll:        os.RemoveAll,
	})
}

type restoreFileOps struct {
	syncTree         func(string) error
	publishDirectory func(string, string) error
	syncDirectory    func(string) error
	removeAll        func(string) error
}

func restore(ctx context.Context, archivePath, envelopePath string, identity age.Identity, validateClosure ClosureValidator, destination string, operations restoreFileOps) (Manifest, error) {
	if destination == "" {
		return Manifest{}, errors.New("restore destination is required")
	}
	if _, err := os.Lstat(destination); err == nil {
		return Manifest{}, fmt.Errorf("restore destination already exists: %s", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Manifest{}, err
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return Manifest{}, err
	}
	bundle, err := verifyAt(ctx, archivePath, envelopePath, identity, validateClosure, parent, true)
	if err != nil {
		return Manifest{}, err
	}
	defer bundle.Cleanup()
	if err := operations.syncTree(bundle.Root); err != nil {
		return Manifest{}, err
	}
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	if _, err := os.Lstat(destination); err == nil {
		return Manifest{}, fmt.Errorf("restore destination appeared during verification: %s", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Manifest{}, err
	}
	if err := operations.publishDirectory(bundle.Root, destination); err != nil {
		return Manifest{}, err
	}
	bundle.Root = ""
	if err := operations.syncDirectory(parent); err != nil {
		cleanupErr := operations.removeAll(destination)
		resyncErr := operations.syncDirectory(parent)
		return Manifest{}, errors.Join(
			fmt.Errorf("sync published restore destination: %w", err),
			wrapRestoreCleanupError(destination, cleanupErr),
			wrapRestoreResyncError(parent, resyncErr),
		)
	}
	return bundle.Manifest, nil
}

func wrapRestoreCleanupError(destination string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("remove restore destination %s after failed publication sync: %w", destination, err)
}

func wrapRestoreResyncError(parent string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("sync restore parent %s after cleanup: %w", parent, err)
}

func syncTree(root string) error {
	var directories []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			directories = append(directories, path)
			return nil
		case info.Mode().IsRegular():
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			syncErr := file.Sync()
			closeErr := file.Close()
			if syncErr != nil {
				return syncErr
			}
			return closeErr
		case info.Mode()&os.ModeSymlink != 0:
			return nil
		default:
			return fmt.Errorf("verified bundle contains special file %q", path)
		}
	})
	if err != nil {
		return err
	}
	for i := len(directories) - 1; i >= 0; i-- {
		if err := syncDirectory(directories[i]); err != nil {
			return err
		}
	}
	return nil
}
