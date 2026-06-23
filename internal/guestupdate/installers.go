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
	wheelManifest, err := readHermesWheelManifest(wheels, spec.Pin, spec.UVLockSHA256)
	if err != nil {
		return withClass(errIntegrity, fmt.Errorf("Hermes wheel manifest: %w", err))
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
	venvPython := filepath.Join(venv, "bin", "python")
	environment := map[string]string{
		"UV_NO_INDEX": "1", "UV_OFFLINE": "1", "UV_FIND_LINKS": wheels,
		"UV_PYTHON": venvPython, "HOME": filepath.Join(staging, ".home"),
	}
	if _, err := e.Runner.Run(ctx, []string{
		uv, "pip", "install", "--python", venvPython, "--offline", "--no-index",
		"--find-links", wheels, "--require-hashes", "--requirement", wheelManifest.Requirements,
	}, RunOptions{Environment: environment, Stderr: e.Stderr}); err != nil {
		return err
	}
	if _, err := e.Runner.Run(ctx, []string{
		uv, "pip", "install", "--python", venvPython, "--offline", "--no-index",
		"--find-links", wheels, "--no-deps", wheelManifest.ProjectWheel,
	}, RunOptions{Environment: environment, Stderr: e.Stderr}); err != nil {
		return err
	}
	if _, err := e.Runner.Run(ctx, []string{venvPython, "-m", "compileall", "-q", source}, RunOptions{Environment: environment, Stderr: e.Stderr}); err != nil {
		return err
	}
	approvalTest := filepath.Join(source, "hermes_box_release", "test_gated_approval.py")
	approvalPatcher := filepath.Join(source, "hermes_box_release", "patch-hermes-gated-approval.py")
	if !fileExists(approvalTest) || !fileExists(approvalPatcher) {
		return errors.New("Hermes source is missing its approval regression suite")
	}
	approvalEnvironment := make(map[string]string, len(environment)+3)
	for key, value := range environment {
		approvalEnvironment[key] = value
	}
	approvalEnvironment["HERMES_GATED_APPROVAL_MODULE"] = filepath.Join(source, "tools", "gated_approval.py")
	approvalEnvironment["HERMES_GATED_APPROVAL_PATCHER"] = approvalPatcher
	approvalEnvironment["HERMES_GATED_APPROVAL_SOURCE"] = source
	if _, err := e.Runner.Run(ctx, []string{venvPython, approvalTest}, RunOptions{Environment: approvalEnvironment, Stderr: e.Stderr}); err != nil {
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

type hermesWheelManifest struct {
	Requirements string
	ProjectWheel string
}

func readHermesWheelManifest(root, expectedCommit, expectedUVLock string) (hermesWheelManifest, error) {
	manifestPath := filepath.Join(root, "manifest.txt")
	info, err := os.Lstat(manifestPath)
	if err != nil || !info.Mode().IsRegular() {
		return hermesWheelManifest{}, errors.New("manifest is not a regular file")
	}
	contents, err := os.ReadFile(manifestPath)
	if err != nil {
		return hermesWheelManifest{}, err
	}
	values := map[string]string{}
	wheelHashes := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(contents)), "\n") {
		key, value, found := strings.Cut(line, "=")
		if !found || key == "" || value == "" {
			return hermesWheelManifest{}, fmt.Errorf("invalid line %q", line)
		}
		if key == "wheel" {
			name, digest, err := parseManifestFile(value)
			if err != nil {
				return hermesWheelManifest{}, fmt.Errorf("wheel: %w", err)
			}
			if _, duplicate := wheelHashes[name]; duplicate {
				return hermesWheelManifest{}, fmt.Errorf("duplicate wheel %q", name)
			}
			wheelHashes[name] = digest
			continue
		}
		switch key {
		case "schema", "platform", "python", "uv", "hermes_commit", "uv_lock_sha256", "requirements", "project_wheel":
		default:
			return hermesWheelManifest{}, fmt.Errorf("unknown field %q", key)
		}
		if _, duplicate := values[key]; duplicate {
			return hermesWheelManifest{}, fmt.Errorf("duplicate field %q", key)
		}
		values[key] = value
	}
	if values["schema"] != "2" || values["platform"] != "linux-arm64" {
		return hermesWheelManifest{}, errors.New("unsupported schema or platform")
	}
	if values["python"] == "" || values["uv"] == "" || values["hermes_commit"] != expectedCommit || values["uv_lock_sha256"] != expectedUVLock {
		return hermesWheelManifest{}, errors.New("pinned identities do not match the reviewed component")
	}
	requirementsName, requirementsHash, err := parseManifestFile(values["requirements"])
	if err != nil || requirementsName != "requirements-linux-arm64.txt" {
		return hermesWheelManifest{}, errors.New("invalid Linux ARM64 requirements identity")
	}
	projectName, projectHash, err := parseManifestFile(values["project_wheel"])
	if err != nil || !strings.HasPrefix(projectName, "hermes_agent-") || !strings.HasSuffix(projectName, "-py3-none-any.whl") {
		return hermesWheelManifest{}, errors.New("invalid Hermes project wheel identity")
	}
	if wheelHashes[projectName] != projectHash {
		return hermesWheelManifest{}, errors.New("project wheel identity is not present in the wheel closure")
	}
	for name, expected := range wheelHashes {
		if err := verifyManifestFile(filepath.Join(root, name), expected); err != nil {
			return hermesWheelManifest{}, fmt.Errorf("wheel %s sha256 does not match manifest", name)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return hermesWheelManifest{}, err
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".whl") {
			if _, listed := wheelHashes[entry.Name()]; !listed {
				return hermesWheelManifest{}, fmt.Errorf("unlisted wheel %q", entry.Name())
			}
		}
	}
	if err := verifyManifestFile(filepath.Join(root, requirementsName), requirementsHash); err != nil {
		return hermesWheelManifest{}, errors.New("requirements sha256 does not match manifest")
	}
	return hermesWheelManifest{
		Requirements: filepath.Join(root, requirementsName),
		ProjectWheel: filepath.Join(root, projectName),
	}, nil
}

func verifyManifestFile(path, expected string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("artifact is not a regular file")
	}
	actual, err := fileSHA256(path)
	if err != nil {
		return err
	}
	if actual != expected {
		return errors.New("artifact sha256 mismatch")
	}
	return nil
}

func parseManifestFile(value string) (string, string, error) {
	name, field, found := strings.Cut(value, "\t")
	if !found || filepath.Base(name) != name || name == "." || strings.ContainsRune(name, '\x00') {
		return "", "", errors.New("artifact path is invalid")
	}
	digest, found := strings.CutPrefix(field, "sha256=")
	if !found || len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
		return "", "", errors.New("artifact sha256 is invalid")
	}
	return name, digest, nil
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
