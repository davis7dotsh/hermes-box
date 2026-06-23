package guestupdate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/davis7dotsh/hermes-box/internal/component"
)

func (e *Engine) installComponent(ctx context.Context, paths paths, spec component.Spec, staging string) error {
	switch spec.Name {
	case component.Node:
		return e.installArchive(ctx, spec.Artifact, staging, true)
	case component.UV:
		if err := e.installArchive(ctx, spec.Artifact, staging, true); err != nil {
			return err
		}
		return normalizeBinary(staging, "uv")
	case component.Claude:
		return e.installClaude(ctx, paths, spec, staging)
	case component.Codex:
		if err := e.installArchive(ctx, spec.Artifact, staging, false); err != nil {
			return err
		}
		return normalizeBinary(staging, "codex")
	case component.Hermes:
		return e.installHermes(ctx, paths, spec, staging)
	default:
		return fmt.Errorf("no trusted installer for %s", spec.Name)
	}
}

func (e *Engine) installArchive(ctx context.Context, archive, destination string, stripRoot bool) error {
	arguments := []string{"/usr/bin/tar", "--extract", "--file", archive, "--directory", destination, "--no-same-owner", "--no-same-permissions"}
	if stripRoot {
		arguments = append(arguments, "--strip-components=1")
	}
	_, err := e.Runner.Run(ctx, arguments, RunOptions{Stderr: e.Stderr})
	return err
}

func (e *Engine) installClaude(ctx context.Context, paths paths, spec component.Spec, staging string) error {
	npm := filepath.Join(activationPath(paths, component.Node), "bin", "npm")
	environment := map[string]string{
		"HOME": filepath.Join(staging, ".home"), "npm_config_update_notifier": "false",
		"npm_config_audit": "false", "npm_config_fund": "false", "npm_config_offline": "true",
		"DISABLE_AUTOUPDATER": "1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
	}
	_, err := e.Runner.Run(ctx, []string{npm, "install", "--global", "--prefix", staging, "--offline", "--ignore-scripts", spec.Artifact}, RunOptions{
		Environment: e.agentEnvironmentWithNode(activationPath(paths, component.Node), environment), Stderr: e.Stderr,
	})
	if err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(staging, ".home")); err != nil {
		return err
	}
	if !fileExists(filepath.Join(staging, "bin", "claude")) {
		return errors.New("Claude npm tarball did not install bin/claude")
	}
	return nil
}

func (e *Engine) installHermes(ctx context.Context, paths paths, spec component.Spec, staging string) error {
	source := filepath.Join(staging, "source")
	python := filepath.Join(staging, "python")
	wheels := filepath.Join(staging, "wheels")
	for _, directory := range []string{source, python, wheels} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return err
		}
	}
	if err := e.installArchive(ctx, spec.Artifact, source, true); err != nil {
		return fmt.Errorf("extract Hermes source: %w", err)
	}
	if err := e.installArchive(ctx, spec.Inputs["python"].Path, python, true); err != nil {
		return fmt.Errorf("extract Hermes Python: %w", err)
	}
	if err := e.installArchive(ctx, spec.Inputs["wheels"].Path, wheels, false); err != nil {
		return fmt.Errorf("extract Hermes wheels: %w", err)
	}
	lockPath := filepath.Join(source, "uv.lock")
	lockHash, err := fileSHA256(lockPath)
	if err != nil || lockHash != spec.UVLockSHA256 {
		return withClass(errIntegrity, errors.New("Hermes uv.lock does not match reviewed sha256"))
	}
	pythonBinary, err := findExactFile(python, "python3")
	if err != nil {
		pythonBinary, err = findExactFile(python, "python")
	}
	if err != nil {
		return err
	}
	uv := filepath.Join(activationPath(paths, component.UV), "bin", "uv")
	venv := filepath.Join(staging, "venv")
	if _, err := e.Runner.Run(ctx, []string{uv, "venv", "--relocatable", "--python", pythonBinary, venv}, RunOptions{Stderr: e.Stderr}); err != nil {
		return err
	}
	environment := map[string]string{
		"UV_NO_INDEX": "1", "UV_FIND_LINKS": wheels, "UV_PYTHON": filepath.Join(venv, "bin", "python"),
		"UV_PROJECT_ENVIRONMENT": venv, "HOME": filepath.Join(staging, ".home"),
	}
	if _, err := e.Runner.Run(ctx, []string{uv, "sync", "--project", source, "--extra", "all", "--locked", "--offline", "--no-managed-python"}, RunOptions{Environment: environment, Stderr: e.Stderr}); err != nil {
		return err
	}
	venvPython := filepath.Join(venv, "bin", "python")
	if _, err := e.Runner.Run(ctx, []string{venvPython, "-m", "compileall", "-q", source}, RunOptions{Environment: environment, Stderr: e.Stderr}); err != nil {
		return err
	}
	tests := filepath.Join(source, "tests")
	if info, err := os.Stat(tests); err != nil || !info.IsDir() {
		return errors.New("Hermes source is missing its approval regression suite")
	}
	if _, err := e.Runner.Run(ctx, []string{venvPython, "-m", "pytest", "-q", tests, "-k", "approval"}, RunOptions{Environment: environment, Stderr: e.Stderr}); err != nil {
		return fmt.Errorf("Hermes approval regression suite: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(staging, "bin"), 0o755); err != nil {
		return err
	}
	hermesBinary := filepath.Join(venv, "bin", "hermes")
	if !fileExists(hermesBinary) {
		return errors.New("Hermes offline install did not create venv/bin/hermes")
	}
	return os.Symlink("../venv/bin/hermes", filepath.Join(staging, "bin", "hermes"))
}

func normalizeBinary(root, name string) error {
	target := filepath.Join(root, "bin", name)
	if fileExists(target) {
		return os.Chmod(target, 0o755)
	}
	source, err := findNamedFile(root, name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if source == target {
		return os.Chmod(target, 0o755)
	}
	return os.Rename(source, target)
}

func findNamedFile(root, name string) (string, error) {
	var matches []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && (entry.Name() == name || strings.HasPrefix(entry.Name(), name+"-")) {
			matches = append(matches, path)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("archive must contain exactly one %s executable, found %d", name, len(matches))
	}
	return matches[0], nil
}

func findExactFile(root, name string) (string, error) {
	var matches []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && entry.Name() == name {
			matches = append(matches, path)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("archive must contain exactly one %s executable, found %d", name, len(matches))
	}
	return matches[0], nil
}
