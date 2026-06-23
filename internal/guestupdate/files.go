package guestupdate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type paths struct {
	root                 string
	state                string
	releases             string
	current              string
	tooling              string
	executor             string
	data                 string
	applied              string
	componentState       string
	journal              string
	restoreJournal       string
	serviceAuthorization string
	lock                 string
}

func newPaths(root string) (paths, error) {
	if root == "" {
		root = "/"
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return paths{}, err
	}
	return paths{
		root:                 abs,
		state:                filepath.Join(abs, "var/lib/hermes-box"),
		releases:             filepath.Join(abs, "opt/hermes-box/releases"),
		current:              filepath.Join(abs, "opt/hermes-box/current"),
		tooling:              filepath.Join(abs, "opt/hermes-box/tooling"),
		executor:             filepath.Join(abs, "etc/hermes-box/executor.env"),
		data:                 filepath.Join(abs, "data"),
		applied:              filepath.Join(abs, "var/lib/hermes-box/applied.lock"),
		componentState:       filepath.Join(abs, "var/lib/hermes-box/components.json"),
		journal:              filepath.Join(abs, "var/lib/hermes-box/update.json"),
		restoreJournal:       filepath.Join(abs, "data/.hermes-box/restore-paths.json"),
		serviceAuthorization: filepath.Join(abs, "run/hermes-box-transaction-authorized"),
		lock:                 filepath.Join(abs, "var/lib/hermes-box/update.lock"),
	}, nil
}

func atomicJSON(path string, value any, mode fs.FileMode) error {
	contents, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	contents = append(contents, '\n')
	return atomicFile(path, contents, mode)
}

func atomicFile(path string, contents []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".hermes-box-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func durableRename(source, destination string) error {
	if err := os.Rename(source, destination); err != nil {
		return err
	}
	destinationParent := filepath.Dir(destination)
	if err := syncDirectory(destinationParent); err != nil {
		return err
	}
	sourceParent := filepath.Dir(source)
	if sourceParent != destinationParent {
		return syncDirectory(sourceParent)
	}
	return nil
}

func durableRemove(path string, recursive bool) error {
	var err error
	if recursive {
		err = os.RemoveAll(path)
	} else {
		err = os.Remove(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
	}
	if err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func syncTree(root string) error {
	var directories []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			directories = append(directories, path)
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("cannot sync unsupported restore entry %s", path)
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		syncErr := file.Sync()
		closeErr := file.Close()
		return errors.Join(syncErr, closeErr)
	})
	if err != nil {
		return err
	}
	for index := len(directories) - 1; index >= 0; index-- {
		if err := syncDirectory(directories[index]); err != nil {
			return err
		}
	}
	return nil
}

func readJSON(path string, target any) error {
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode %s: trailing JSON content", path)
	}
	return nil
}

func copyTree(source, destination string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("tree artifact %s is not a directory", source)
	}
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if filepath.IsAbs(link) || strings.HasPrefix(filepath.Clean(link), "..") {
				return fmt.Errorf("artifact symlink escapes release: %s", path)
			}
			return os.Symlink(link, target)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("unsupported artifact entry: %s", path)
		}
		return copyFile(path, target, info.Mode().Perm())
	})
}

func copyFile(source, destination string, mode fs.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		output.Close()
		return err
	}
	return output.Close()
}
