package app

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/davis7dotsh/hermes-box/internal/artifacts"
	"github.com/davis7dotsh/hermes-box/internal/component"
)

const maxClaudeTarballBytes int64 = 1 << 30

func materializeOne(ctx context.Context, home, name, url, sha256 string) (string, error) {
	store := artifacts.Store{Root: filepath.Join(home, "artifacts")}
	artifact, err := store.Fetch(ctx, artifacts.Reference{Name: name, URL: url, SHA256: sha256})
	if err != nil {
		return "", err
	}
	return artifact.Path, nil
}

func materializeLockClosure(ctx context.Context, def Definition) error {
	lock := def.Bundle.Lock
	for _, item := range []struct{ name, url, digest string }{
		{"ubuntu-image", lock.Ubuntu.Image, lock.Ubuntu.SHA256},
		{"provisioner", lock.Ubuntu.Provisioner, lock.Ubuntu.ProvisionerSHA256},
		{"node", lock.Tooling.Node.Archive, lock.Tooling.Node.SHA256},
		{"uv", lock.Tooling.UV.Archive, lock.Tooling.UV.SHA256},
		{"codex", lock.Codex.Archive, lock.Codex.SHA256},
		{"hermes", lock.Hermes.Archive, lock.Hermes.SHA256},
		{"hermes-python", lock.Hermes.PythonArchive, lock.Hermes.PythonSHA256},
		{"hermes-wheels", lock.Hermes.WheelsArchive, lock.Hermes.WheelsSHA256},
	} {
		if _, err := materializeOne(ctx, def.Home, item.name, item.url, item.digest); err != nil {
			return err
		}
	}
	if _, err := fetchSRI(ctx, filepath.Join(def.Home, "artifacts"), lock.Claude.Tarball, lock.Claude.Integrity); err != nil {
		return err
	}
	_, indexDigest, ok := strings.Cut(lock.Executor.Image, "@")
	if !ok {
		return errors.New("Executor image has no immutable index digest")
	}
	store := artifacts.Store{Root: filepath.Join(def.Home, "artifacts")}
	_, err := store.MaterializeOCI(ctx, lock.Executor.Image, indexDigest, lock.Executor.LinuxARM64Digest)
	return err
}

func (o *defaultOperations) prepareAndUpload(ctx context.Context, def Definition, target string) ([]component.Spec, func(), error) {
	lock := def.Bundle.Lock
	selected := func(name string) bool { return target == "all" || target == name }
	guestRoot := "/tmp/hermes-box-artifacts"
	cleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = o.guestShell(cleanupCtx, def, nil, "sudo", "rm", "-rf", guestRoot)
	}
	if err := o.guestShell(ctx, def, nil, "sudo", "rm", "-rf", guestRoot); err != nil {
		return nil, func() {}, err
	}
	if err := o.guestShell(ctx, def, nil, "mkdir", "-p", guestRoot); err != nil {
		return nil, func() {}, err
	}
	upload := func(name, source string) (string, error) {
		client, err := o.client(def)
		if err != nil {
			return "", err
		}
		target := filepath.Join(guestRoot, name)
		if err := client.Copy(ctx, false, []string{source}, def.Name+":"+target); err != nil {
			return "", err
		}
		return target, nil
	}
	var specs []component.Spec
	add := func(name component.Name, pin, url, digest string) error {
		host, err := materializeOne(ctx, def.Home, string(name), url, digest)
		if err != nil {
			return err
		}
		guest, err := upload(string(name), host)
		if err != nil {
			return err
		}
		spec := component.Spec{Name: name, Pin: pin, Artifact: guest, SHA256: digest}
		if err := spec.Validate(); err != nil {
			return err
		}
		specs = append(specs, spec)
		return nil
	}
	fail := func(err error) ([]component.Spec, func(), error) {
		cleanup()
		return nil, func() {}, err
	}
	if selected("node") {
		if err := add(component.Node, lock.Tooling.Node.Version, lock.Tooling.Node.Archive, lock.Tooling.Node.SHA256); err != nil {
			return fail(err)
		}
	}
	if selected("uv") {
		if err := add(component.UV, lock.Tooling.UV.Version, lock.Tooling.UV.Archive, lock.Tooling.UV.SHA256); err != nil {
			return fail(err)
		}
	}
	if selected("claude") {
		host, err := fetchSRI(ctx, filepath.Join(def.Home, "artifacts"), lock.Claude.Tarball, lock.Claude.Integrity)
		if err != nil {
			return fail(err)
		}
		digest, err := hashFile(host)
		if err != nil {
			return fail(err)
		}
		guest, err := upload("claude", host)
		if err != nil {
			return fail(err)
		}
		spec := component.Spec{Name: component.Claude, Pin: lock.Claude.Version, Artifact: guest, SHA256: digest}
		if err := spec.Validate(); err != nil {
			return fail(err)
		}
		specs = append(specs, spec)
	}
	if selected("codex") {
		if err := add(component.Codex, lock.Codex.Version, lock.Codex.Archive, lock.Codex.SHA256); err != nil {
			return fail(err)
		}
	}
	if selected("hermes") {
		source, err := materializeOne(ctx, def.Home, "hermes", lock.Hermes.Archive, lock.Hermes.SHA256)
		if err != nil {
			return fail(err)
		}
		python, err := materializeOne(ctx, def.Home, "hermes-python", lock.Hermes.PythonArchive, lock.Hermes.PythonSHA256)
		if err != nil {
			return fail(err)
		}
		wheels, err := materializeOne(ctx, def.Home, "hermes-wheels", lock.Hermes.WheelsArchive, lock.Hermes.WheelsSHA256)
		if err != nil {
			return fail(err)
		}
		sourceGuest, err := upload("hermes", source)
		if err != nil {
			return fail(err)
		}
		pythonGuest, err := upload("hermes-python", python)
		if err != nil {
			return fail(err)
		}
		wheelsGuest, err := upload("hermes-wheels", wheels)
		if err != nil {
			return fail(err)
		}
		spec := component.Spec{
			Name: component.Hermes, Pin: lock.Hermes.Commit, Artifact: sourceGuest, SHA256: lock.Hermes.SHA256,
			Inputs: map[string]component.Input{
				"python": {Path: pythonGuest, SHA256: lock.Hermes.PythonSHA256},
				"wheels": {Path: wheelsGuest, SHA256: lock.Hermes.WheelsSHA256},
			},
			UVLockSHA256: lock.Hermes.UVLockSHA256,
		}
		if err := spec.Validate(); err != nil {
			return fail(err)
		}
		specs = append(specs, spec)
	}
	if selected("executor") {
		store := artifacts.Store{Root: filepath.Join(def.Home, "artifacts")}
		imageName, indexDigest, _ := strings.Cut(lock.Executor.Image, "@")
		runtimeImage := imageName + "@" + lock.Executor.LinuxARM64Digest
		oci, err := store.MaterializeOCI(ctx, lock.Executor.Image, indexDigest, lock.Executor.LinuxARM64Digest)
		if err != nil {
			return fail(err)
		}
		guest, err := upload("executor.tar", oci.Path)
		if err != nil {
			return fail(err)
		}
		spec := component.Spec{
			Name: component.Executor, Pin: lock.Executor.LinuxARM64Digest, Kind: "container",
			Artifact: guest, SHA256: oci.ArchiveSHA256, Image: runtimeImage,
			ImageIndexDigest: indexDigest, ImageChildDigest: lock.Executor.LinuxARM64Digest,
		}
		if err := spec.Validate(); err != nil {
			return fail(err)
		}
		specs = append(specs, spec)
	}
	return specs, cleanup, nil
}

func fetchSRI(ctx context.Context, root, url, integrity string) (string, error) {
	return fetchSRIWithClient(ctx, root, url, integrity, &http.Client{Timeout: 2 * time.Minute}, maxClaudeTarballBytes)
}

func fetchSRIWithClient(ctx context.Context, root, url, integrity string, client *http.Client, maxBytes int64) (string, error) {
	if client == nil || maxBytes < 1 {
		return "", errors.New("Claude artifact client and positive byte limit are required")
	}
	encoded, ok := strings.CutPrefix(integrity, "sha512-")
	if !ok {
		return "", errors.New("Claude integrity must use sha512 SRI")
	}
	expected, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(expected) != sha512.Size {
		return "", errors.New("Claude sha512 SRI is invalid")
	}
	digestName := fmt.Sprintf("%x", expected)
	directory := filepath.Join(root, "sha512", digestName[:2])
	destination := filepath.Join(directory, digestName)
	if info, statErr := os.Lstat(destination); statErr == nil {
		valid := info.Mode().IsRegular() && info.Size() <= maxBytes
		if valid {
			file, openErr := os.Open(destination)
			if openErr != nil {
				return "", openErr
			}
			hash := sha512.New()
			size, readErr := io.Copy(hash, io.LimitReader(file, maxBytes+1))
			closeErr := file.Close()
			valid = readErr == nil && closeErr == nil && size == info.Size() && size <= maxBytes && bytes.Equal(hash.Sum(nil), expected)
		}
		if valid {
			return destination, nil
		}
		if err := os.Remove(destination); err != nil {
			return "", fmt.Errorf("remove invalid Claude cache entry: %w", err)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return "", statErr
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download Claude tarball: %s", response.Status)
	}
	if response.ContentLength > maxBytes {
		return "", fmt.Errorf("Claude tarball Content-Length %d exceeds limit %d", response.ContentLength, maxBytes)
	}
	temporary, err := os.CreateTemp(directory, ".partial-")
	if err != nil {
		return "", err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	hash := sha512.New()
	size, copyErr := io.Copy(io.MultiWriter(temporary, hash), io.LimitReader(response.Body, maxBytes+1))
	syncErr := temporary.Sync()
	closeErr := temporary.Close()
	if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
		return "", err
	}
	if size > maxBytes {
		return "", fmt.Errorf("Claude tarball exceeds limit of %d bytes", maxBytes)
	}
	if string(hash.Sum(nil)) != string(expected) {
		return "", errors.New("Claude tarball SRI mismatch")
	}
	if err := os.Chmod(temporaryPath, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return "", err
	}
	dir, err := os.Open(directory)
	if err != nil {
		return "", err
	}
	syncErr = dir.Sync()
	closeErr = dir.Close()
	if err := errors.Join(syncErr, closeErr); err != nil {
		return "", err
	}
	return destination, nil
}
