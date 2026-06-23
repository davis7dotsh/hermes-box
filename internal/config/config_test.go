package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validConfig = `schema: 1
name: main
vm:
  cpus: 4
  memory: 8GiB
  root_disk: 30GiB
  data_disk: 50GiB
ports:
  executor: 4788
backup:
  keep: 5
`

const validLock = `schema: 1
host:
  lima: 2.1.3
ubuntu:
  release: "26.04"
  image: https://cloud-images.ubuntu.com/releases/26.04/release-20260612/ubuntu-26.04-server-cloudimg-arm64.img
  sha256: 5e1c212ac29354dbf51c5b1926d8a359de57ca8c2d2bdacf17651129c29791cb
  provisioner: https://example.com/provisioner.tar.zst
  provisioner_sha256: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
tooling:
  node: {version: 24.0.0, archive: https://example.com/node.tar.xz, sha256: bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb}
  uv: {version: 0.1.0, archive: https://example.com/uv.tar.gz, sha256: cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc}
claude:
  version: 1.0.0
  package: "@anthropic-ai/claude-code"
  tarball: https://example.com/claude.tgz
  integrity: sha512-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA==
codex: {version: 1.0.0, archive: https://example.com/codex.tar.gz, sha256: dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd}
hermes:
  repository: https://github.com/example/hermes.git
  commit: eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee
  archive: https://example.com/hermes.tar.gz
  sha256: ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
  uv_lock_sha256: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  python_archive: https://example.com/python.tar.zst
  python_sha256: bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
  wheels_archive: https://example.com/wheels.tar.zst
  wheels_sha256: cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
executor:
  image: ghcr.io/example/executor:v1@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
  linux_arm64_digest: sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee
`

func TestLoadAndSelectionPrecedence(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "chosen.yaml"), validConfig)
	writeFixture(t, filepath.Join(dir, "hermes-box.lock"), validLock)
	bundle, err := Load("chosen.yaml", dir, []string{"HERMES_BOX_CONFIG=ignored.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Config.Name != "main" || bundle.ConfigPath != filepath.Join(dir, "chosen.yaml") {
		t.Fatalf("unexpected bundle: %#v", bundle)
	}
}

func TestResolvePathUsesEnvironmentThenDefault(t *testing.T) {
	dir := t.TempDir()
	got, err := ResolvePath("", dir, []string{"HERMES_BOX_CONFIG=other/config.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(dir, "other/config.yaml") {
		t.Fatalf("path = %q", got)
	}
	got, err = ResolvePath("", dir, nil)
	if err != nil || got != filepath.Join(dir, "hermes-box.yaml") {
		t.Fatalf("default path = %q, err = %v", got, err)
	}
}

func TestLoadRejectsUnknownHermesBoxEnvironment(t *testing.T) {
	if err := ValidateEnvironment([]string{"HERMES_BOX_CONFG=wrong"}); err == nil {
		t.Fatal("unknown Hermes Box environment setting was accepted")
	}
	if err := ValidateEnvironment([]string{"HERMES_BOX_HOME=/tmp/state", "NO_COLOR=1"}); err != nil {
		t.Fatal(err)
	}
}

func TestStrictYAMLRejectsUnknownDuplicateAndMultipleDocuments(t *testing.T) {
	for name, content := range map[string]string{
		"unknown":   validConfig + "surprise: true\n",
		"duplicate": strings.Replace(validConfig, "name: main", "name: main\nname: other", 1),
		"multiple":  validConfig + "---\n" + validConfig,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hermes-box.yaml")
			writeFixture(t, path, content)
			if _, err := LoadConfig(path); err == nil {
				t.Fatal("invalid YAML was accepted")
			}
		})
	}
}

func TestConfigValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hermes-box.yaml")
	writeFixture(t, path, strings.Replace(validConfig, "name: main", "name: Main!", 1))
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("invalid name was accepted")
	}
}

func TestLockValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hermes-box.lock")
	writeFixture(t, path, strings.Replace(validLock, "lima: 2.1.3", "lima: 2.2.0", 1))
	if _, err := LoadLock(path); err == nil {
		t.Fatal("unqualified Lima version was accepted")
	}
	writeFixture(t, path, strings.Replace(validLock, "release-20260612/", "release/", 1))
	if _, err := LoadLock(path); err == nil {
		t.Fatal("moving Ubuntu image was accepted")
	}
}

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
