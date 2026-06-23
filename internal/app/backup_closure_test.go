package app

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/davis7dotsh/hermes-box/internal/backup"
	"github.com/davis7dotsh/hermes-box/internal/config"
	"github.com/davis7dotsh/hermes-box/internal/lima"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

func TestValidateBackupClosureBindsExactInputsAndRejectsExtras(t *testing.T) {
	directory := t.TempDir()
	write := func(name, contents string) (string, string) {
		t.Helper()
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256([]byte(contents))
		return path, hex.EncodeToString(digest[:])
	}

	lock := validClosureLock()
	var closure []backup.Artifact
	standard := []struct {
		name   string
		assign func(string)
	}{
		{"ubuntu-image", func(value string) { lock.Ubuntu.SHA256 = value }},
		{"provisioner", func(value string) { lock.Ubuntu.ProvisionerSHA256 = value }},
		{"node", func(value string) { lock.Tooling.Node.SHA256 = value }},
		{"uv", func(value string) { lock.Tooling.UV.SHA256 = value }},
		{"codex", func(value string) { lock.Codex.SHA256 = value }},
		{"hermes", func(value string) { lock.Hermes.SHA256 = value }},
		{"hermes-python", func(value string) { lock.Hermes.PythonSHA256 = value }},
		{"hermes-wheels", func(value string) { lock.Hermes.WheelsSHA256 = value }},
	}
	for _, item := range standard {
		path, digest := write(item.name, "bytes-for-"+item.name)
		item.assign(digest)
		closure = append(closure, backup.Artifact{Name: item.name, SHA256: digest, Path: path})
	}
	claudePath, claudeSHA := write("claude", "claude-package")
	claudeSRI := sha512.Sum512([]byte("claude-package"))
	lock.Claude.Integrity = "sha512-" + base64.StdEncoding.EncodeToString(claudeSRI[:])
	closure = append(closure, backup.Artifact{Name: "claude", SHA256: claudeSHA, Path: claudePath})

	executorPath := filepath.Join(directory, "executor.tar")
	executorImage := empty.Image
	if err := tarball.WriteToFile(executorPath, name.MustParseReference("example.com/executor:v1"), executorImage); err != nil {
		t.Fatal(err)
	}
	executorDigest, err := executorImage.Digest()
	if err != nil {
		t.Fatal(err)
	}
	lock.Executor.LinuxARM64Digest = executorDigest.String()
	executorSHA, err := hashFile(executorPath)
	if err != nil {
		t.Fatal(err)
	}
	closure = append(closure, backup.Artifact{Name: "executor", SHA256: executorSHA, Path: executorPath})

	encoded, err := encodeLock(lock)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateBackupClosure([]byte(encoded), closure); err != nil {
		t.Fatalf("valid closure rejected: %v", err)
	}
	extraPath, extraSHA := write("extra", "extra")
	withExtra := append(append([]backup.Artifact(nil), closure...), backup.Artifact{Name: "extra", SHA256: extraSHA, Path: extraPath})
	if err := validateBackupClosure([]byte(encoded), withExtra); err == nil {
		t.Fatal("closure validator accepted an unreviewed extra artifact")
	}
	tampered := append([]backup.Artifact(nil), closure...)
	tampered[0].Path = extraPath
	if err := validateBackupClosure([]byte(encoded), tampered); err == nil {
		t.Fatal("closure validator accepted bytes selected by a trusted filename")
	}
}

func validClosureLock() config.Lock {
	zero := "0000000000000000000000000000000000000000000000000000000000000000"
	return config.Lock{
		Schema: 1,
		Host:   config.HostLock{Lima: lima.QualifiedVersion},
		Ubuntu: config.UbuntuLock{
			Release: "26.04", Image: "https://example.com/release-20260520/ubuntu.img", SHA256: zero,
			Provisioner: "https://example.com/provisioner.tar.zst", ProvisionerSHA256: zero,
		},
		Tooling: config.ToolingLock{
			Node: config.ToolLock{Version: "1", Archive: "https://example.com/node.tar.xz", SHA256: zero},
			UV:   config.ToolLock{Version: "1", Archive: "https://example.com/uv.tar.gz", SHA256: zero},
		},
		Claude: config.ClaudeLock{Version: "1", Package: "claude", Tarball: "https://example.com/claude.tgz", Integrity: "sha512-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="},
		Codex:  config.CodexLock{Version: "1", Archive: "https://example.com/codex.tar.gz", SHA256: zero},
		Hermes: config.HermesLock{
			Repository: "https://example.com/hermes.git", Commit: "0000000000000000000000000000000000000000",
			Archive: "https://example.com/hermes.tar.gz", SHA256: zero, UVLockSHA256: zero,
			PythonArchive: "https://example.com/python.tar.zst", PythonSHA256: zero,
			WheelsArchive: "https://example.com/wheels.tar.zst", WheelsSHA256: zero,
		},
		Executor: config.ExecutorLock{
			Image: "example.com/executor:v1@sha256:" + zero, LinuxARM64Digest: "sha256:" + zero,
		},
	}
}
