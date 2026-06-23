package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/davis7dotsh/hermes-box/internal/component"
	"github.com/davis7dotsh/hermes-box/internal/config"
	"github.com/davis7dotsh/hermes-box/internal/guestupdate"
	"gopkg.in/yaml.v3"
)

func hostAppliedLockPath(def Definition) string {
	return filepath.Join(def.Home, "boxes", def.Name+".applied.lock")
}

func updateHostAppliedLock(def Definition, target string) error {
	lock, err := targetAppliedLock(def, target)
	if err != nil {
		return err
	}
	return writeLock(hostAppliedLockPath(def), lock)
}

func targetAppliedLock(def Definition, target string) (config.Lock, error) {
	desired := def.Bundle.Lock
	if target == "all" {
		return desired, nil
	}
	current, err := config.LoadLock(hostAppliedLockPath(def))
	if err != nil {
		return config.Lock{}, fmt.Errorf("read host applied lock: %w", err)
	}
	switch target {
	case "node":
		current.Tooling.Node = desired.Tooling.Node
	case "uv":
		current.Tooling.UV = desired.Tooling.UV
	case "claude":
		current.Claude = desired.Claude
	case "codex":
		current.Codex = desired.Codex
	case "hermes":
		current.Hermes = desired.Hermes
	case "executor":
		current.Executor = desired.Executor
	default:
		return config.Lock{}, fmt.Errorf("unknown applied-lock component %q", target)
	}
	return current, nil
}

type componentObservation struct {
	running string
	err     error
}

func componentStatus(def Definition, guest guestupdate.Status, observations map[component.Name]componentObservation) map[string]any {
	desired := def.Bundle.Lock
	result := make(map[string]any, 6)
	for _, name := range []string{"node", "uv", "claude", "codex", "hermes", "executor"} {
		componentName := component.Name(name)
		desiredPin := lockPin(desired, name)
		appliedPin := guest.Applied.Components[componentName]
		metadata := guest.Releases.Components[componentName]
		observation := observations[componentName]
		state := "healthy"
		if appliedPin == "" || metadata.Current != appliedPin || observation.err != nil {
			state = "failed"
		} else if desiredPin != appliedPin {
			state = "drifted"
		}
		result[name] = map[string]any{
			"desired": desiredPin, "applied": appliedPin, "running": observation.running,
			"previous": nullablePin(metadata.Previous), "state": state,
		}
	}
	return result
}

func nullablePin(pin string) any {
	if pin == "" {
		return nil
	}
	return pin
}

func lockPin(lock config.Lock, name string) string {
	switch name {
	case "node":
		return lock.Tooling.Node.Version
	case "uv":
		return lock.Tooling.UV.Version
	case "claude":
		return lock.Claude.Version
	case "codex":
		return lock.Codex.Version
	case "hermes":
		return lock.Hermes.Commit
	case "executor":
		return lock.Executor.LinuxARM64Digest
	default:
		return ""
	}
}

func componentLockEqual(first, second config.Lock, name string) bool {
	switch name {
	case "node":
		return reflect.DeepEqual(first.Tooling.Node, second.Tooling.Node)
	case "uv":
		return reflect.DeepEqual(first.Tooling.UV, second.Tooling.UV)
	case "claude":
		return reflect.DeepEqual(first.Claude, second.Claude)
	case "codex":
		return reflect.DeepEqual(first.Codex, second.Codex)
	case "hermes":
		return reflect.DeepEqual(first.Hermes, second.Hermes)
	case "executor":
		return reflect.DeepEqual(first.Executor, second.Executor)
	default:
		return false
	}
}

func writeLock(path string, lock config.Lock) error {
	if err := lock.Validate(); err != nil {
		return err
	}
	data, err := yaml.Marshal(lock)
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".applied-lock-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
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
	directoryHandle, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer directoryHandle.Close()
	return directoryHandle.Sync()
}

func encodeLock(lock config.Lock) (string, error) {
	if err := lock.Validate(); err != nil {
		return "", err
	}
	data, err := yaml.Marshal(lock)
	return string(data), err
}

func syncHostAppliedLock(def Definition, encoded string) error {
	if encoded == "" {
		return errors.New("guest status omitted applied lock")
	}
	var lock config.Lock
	decoder := yaml.NewDecoder(strings.NewReader(encoded))
	decoder.KnownFields(true)
	if err := decoder.Decode(&lock); err != nil {
		return fmt.Errorf("decode guest applied lock: %w", err)
	}
	return writeLock(hostAppliedLockPath(def), lock)
}

func copyReplace(source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".replace-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	_, copyErr := io.Copy(temporary, input)
	syncErr := temporary.Sync()
	closeErr := temporary.Close()
	if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(destination))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func copyRegularFile(source, destination string) error {
	if _, err := os.Lstat(destination); err == nil {
		return os.ErrExist
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return copyReplace(source, destination, 0o600)
}

func sanitizePin(pin string) string {
	return strings.NewReplacer("/", "-", ":", "-").Replace(pin)
}
