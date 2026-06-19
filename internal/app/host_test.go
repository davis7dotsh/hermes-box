package app

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/davis7dotsh/hermes-box/internal/config"
	"github.com/davis7dotsh/hermes-box/internal/process"
)

type recordingRunner struct {
	last process.Spec
}

type artifactRunner struct {
	dataDir string
	runs    []process.Spec
}

func (r *recordingRunner) Run(_ context.Context, spec process.Spec) error {
	r.last = spec
	return nil
}

func (r *recordingRunner) Output(_ context.Context, spec process.Spec) ([]byte, error) {
	r.last = spec
	return nil, nil
}

func (r *artifactRunner) Run(_ context.Context, spec process.Spec) error {
	r.runs = append(r.runs, spec)
	return nil
}

func (r *artifactRunner) Output(_ context.Context, spec process.Spec) ([]byte, error) {
	r.runs = append(r.runs, spec)
	return []byte(r.dataDir + "\n"), nil
}

func TestReadSecretMappings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret-env.txt")
	content := `
# Host values are referenced, not copied here.
OPENAI_API_KEY=OPENAI_API_KEY
TELEGRAM_BOT_TOKEN = HERMES_BOX_TELEGRAM_BOT_TOKEN # comment
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	mappings, err := readSecretMappings(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(mappings) != 2 {
		t.Fatalf("len(mappings) = %d", len(mappings))
	}
	if mappings[1] != "TELEGRAM_BOT_TOKEN=HERMES_BOX_TELEGRAM_BOT_TOKEN" {
		t.Fatalf("mapping = %q", mappings[1])
	}
}

func TestRuntimeCreateArgsIncludeExecutorPortAndEnvironment(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{
		MachineName:     "test-box",
		BuilderName:     "test-builder",
		SSHPort:         2223,
		CPUs:            1,
		MemoryMiB:       1,
		StorageGB:       1,
		OverlayGB:       1,
		NetworkMode:     "full",
		ExecutorEnabled: true,
		ExecutorPort:    4789,
		ExecutorImage:   "example.com/executor:v1@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	application := New(root, cfg, process.OSRunner{}, io.Discard, io.Discard)
	args, err := application.runtimeCreateArgs("test-box", "/tmp/base.smolmachine", 2223)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"2223:22",
		"4789:4788",
		"HERMES_BOX_EXECUTOR_ENABLED=true",
		"HERMES_BOX_EXECUTOR_IMAGE=" + cfg.ExecutorImage,
		"HERMES_BOX_EXECUTOR_HOST_PORT=4789",
	} {
		if !containsArgument(args, expected) {
			t.Fatalf("runtime args do not contain %q: %q", expected, args)
		}
	}
}

func TestVerifyBuilderNetworkChecksRouteAndDNS(t *testing.T) {
	runner := &recordingRunner{}
	application := New(t.TempDir(), config.Config{}, runner, io.Discard, io.Discard)
	if err := application.verifyBuilderNetwork(context.Background(), "test-builder"); err != nil {
		t.Fatal(err)
	}
	if runner.last.Name != "smolvm" || !containsArgument(runner.last.Args, "test-builder") {
		t.Fatalf("unexpected network probe: %#v", runner.last)
	}
	script := runner.last.Args[len(runner.last.Args)-1]
	for _, expected := range []string{"/proc/net/route", "getent ahostsv4 ports.ubuntu.com"} {
		if !strings.Contains(script, expected) {
			t.Fatalf("network probe does not contain %q: %s", expected, script)
		}
	}
}

func TestVerifyGuestReadsExecutorEnvironmentAsHermes(t *testing.T) {
	runner := &recordingRunner{}
	application := New(t.TempDir(), config.Config{ExecutorEnabled: true}, runner, io.Discard, io.Discard)
	if err := application.verifyGuest(context.Background(), "test-box"); err != nil {
		t.Fatal(err)
	}
	script := runner.last.Args[len(runner.last.Args)-1]
	for _, expected := range []string{
		"executor_pid=$(sudo -u hermes sudo supervisorctl pid executor)",
		"sudo -u hermes sh -c",
		`/proc/$1/environ`,
	} {
		if !strings.Contains(script, expected) {
			t.Fatalf("Executor verification does not contain %q: %s", expected, script)
		}
	}
}

func TestCreateFromArtifactPreservesPackedLayers(t *testing.T) {
	dataDir := t.TempDir()
	packDir := filepath.Join(dataDir, "pack")
	if err := os.MkdirAll(packDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packDir, ".smolvm-extracted"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &artifactRunner{dataDir: dataDir}
	application := New(t.TempDir(), config.Config{
		CPUs:        1,
		MemoryMiB:   1,
		StorageGB:   1,
		OverlayGB:   1,
		NetworkMode: "full",
	}, runner, io.Discard, io.Discard)
	if err := application.createFromArtifact(context.Background(), "test-box", "/tmp/base.smolmachine", 2223); err != nil {
		t.Fatal(err)
	}
	if runner.runs[0].Name != "env" ||
		!containsArgument(runner.runs[0].Args, "SMOLVM_PACK_CACHE_MAX_BYTES=17179869184") ||
		!containsArgument(runner.runs[0].Args, "smolvm") {
		t.Fatalf("unexpected artifact creation: %#v", runner.runs[0])
	}
}

func containsArgument(arguments []string, expected string) bool {
	for _, argument := range arguments {
		if argument == expected {
			return true
		}
	}
	return false
}

func TestReadSecretMappingsRejectsInvalidNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret-env.txt")
	if err := os.WriteFile(path, []byte("OPENAI_API_KEY=$(cat secret)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecretMappings(path); err == nil {
		t.Fatal("readSecretMappings accepted shell syntax")
	}
}
