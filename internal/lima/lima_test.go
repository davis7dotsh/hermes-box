package lima

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/davis7dotsh/hermes-box/internal/config"
)

type fakeRunner struct {
	results []Result
	errors  []error
	calls   []Invocation
}

func (f *fakeRunner) Run(_ context.Context, invocation Invocation) (Result, error) {
	f.calls = append(f.calls, invocation)
	index := len(f.calls) - 1
	var result Result
	if index < len(f.results) {
		result = f.results[index]
	}
	if index < len(f.errors) {
		return result, f.errors[index]
	}
	return result, nil
}

func TestVersionAndDedicatedHome(t *testing.T) {
	runner := &fakeRunner{results: []Result{{Stdout: []byte("limactl version 2.1.3\n")}}}
	home := filepath.Join(t.TempDir(), "lima")
	client, err := New(home, runner)
	if err != nil {
		t.Fatal(err)
	}
	version, err := client.Version(context.Background())
	if err != nil || version != QualifiedVersion {
		t.Fatalf("version = %q, err = %v", version, err)
	}
	if !reflect.DeepEqual(runner.calls[0].Args, []string{"--version"}) {
		t.Fatalf("args = %#v", runner.calls[0].Args)
	}
	if !contains(runner.calls[0].Env, "LIMA_HOME="+home) {
		t.Fatalf("env = %#v", runner.calls[0].Env)
	}
}

func TestVersionRejectsDrift(t *testing.T) {
	runner := &fakeRunner{results: []Result{{Stdout: []byte("limactl version 2.2.0\n")}}}
	client, err := New(filepath.Join(t.TempDir(), "lima"), runner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Version(context.Background()); err == nil {
		t.Fatal("unsupported Lima version accepted")
	}
}

func TestDetectVersionReportsUnqualifiedInstallationForDiagnostics(t *testing.T) {
	runner := &fakeRunner{results: []Result{{Stdout: []byte("limactl version 2.2.0\n")}}}
	client, err := New(filepath.Join(t.TempDir(), "lima"), runner)
	if err != nil {
		t.Fatal(err)
	}
	version, err := client.DetectVersion(context.Background())
	if err != nil || version != "2.2.0" {
		t.Fatalf("detected version = %q, err = %v", version, err)
	}
}

func TestNewRejectsPublicLimaHomeWithoutChangingIt(t *testing.T) {
	home := filepath.Join(t.TempDir(), "lima")
	if err := os.Mkdir(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := New(home, &fakeRunner{}); err == nil {
		t.Fatal("public LIMA_HOME accepted")
	}
	info, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("LIMA_HOME permissions changed to %o", info.Mode().Perm())
	}
}

func TestJSONLInspection(t *testing.T) {
	runner := &fakeRunner{results: []Result{
		{Stdout: []byte("{\"name\":\"main\",\"status\":\"Running\",\"arch\":\"aarch64\",\"vmType\":\"vz\"}\n{\"name\":\"other\",\"status\":\"Stopped\"}\n")},
		{Stdout: []byte("{\"name\":\"main-data\",\"status\":\"InUse\",\"size\":53687091200}\n")},
	}}
	client, err := New(filepath.Join(t.TempDir(), "lima"), runner)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := client.Inspect(context.Background())
	if err != nil || len(instances) != 2 || instances[0].VMType != "vz" {
		t.Fatalf("instances = %#v, err = %v", instances, err)
	}
	disks, err := client.InspectDisks(context.Background())
	if err != nil || len(disks) != 1 || disks[0].Name != "main-data" {
		t.Fatalf("disks = %#v, err = %v", disks, err)
	}
}

func TestExactJSONLInspection(t *testing.T) {
	runner := &fakeRunner{results: []Result{
		{Stdout: []byte("{\"name\":\"main\",\"status\":\"Stopped\"}\n")},
		{Stdout: []byte("{\"name\":\"main-data\",\"size\":1}\n")},
	}}
	client, err := New(filepath.Join(t.TempDir(), "lima"), runner)
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := client.InspectInstance(context.Background(), "main"); err != nil || !found {
		t.Fatalf("instance found = %t, err = %v", found, err)
	}
	if _, found, err := client.InspectDisk(context.Background(), "main-data"); err != nil || !found {
		t.Fatalf("disk found = %t, err = %v", found, err)
	}
	if !reflect.DeepEqual(runner.calls[0].Args, []string{"list", "--format", "json", "main"}) {
		t.Fatalf("instance args = %#v", runner.calls[0].Args)
	}
	if !reflect.DeepEqual(runner.calls[1].Args, []string{"disk", "list", "--json", "main-data"}) {
		t.Fatalf("disk args = %#v", runner.calls[1].Args)
	}
}

func TestJSONLRejectsArray(t *testing.T) {
	runner := &fakeRunner{results: []Result{{Stdout: []byte("[{\"name\":\"main\"}]\n")}}}
	client, err := New(filepath.Join(t.TempDir(), "lima"), runner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Inspect(context.Background()); err == nil {
		t.Fatal("JSON array accepted instead of JSONL")
	}
}

func TestCommandOperations(t *testing.T) {
	runner := &fakeRunner{}
	home := filepath.Join(t.TempDir(), "lima")
	client, err := New(home, runner)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	operations := []func() error{
		func() error { return client.CreateDisk(ctx, "main-data", "50GiB") },
		func() error { return client.Create(ctx, "main", []byte("vmType: vz\n")) },
		func() error { return client.Start(ctx, "main") },
		func() error { return client.Stop(ctx, "main", true) },
		func() error { return client.Copy(ctx, true, []string{"a"}, "main:/tmp") },
		func() error { _, err := client.Shell(ctx, "main", "/workspace", "true"); return err },
		func() error { return client.Delete(ctx, "main") },
		func() error { return client.DeleteDisk(ctx, "main-data") },
	}
	for _, operation := range operations {
		if err := operation(); err != nil {
			t.Fatal(err)
		}
	}
	wantPrefixes := [][]string{
		{"disk", "create", "main-data"}, {"create", "--name", "main"}, {"start", "main"},
		{"stop", "main"}, {"copy", "--backend=scp"},
		{"shell", "--workdir", "/workspace", "main", "--", "true"}, {"delete", "main"},
		{"disk", "delete", "main-data"},
	}
	for index, want := range wantPrefixes {
		got := runner.calls[index].Args
		if len(got) < len(want) || !reflect.DeepEqual(got[:len(want)], want) {
			t.Fatalf("call %d args = %#v, want prefix %#v", index, got, want)
		}
	}
	if !reflect.DeepEqual(runner.calls[0].Args, []string{"disk", "create", "main-data", "--size", "50GiB", "--format", "raw", "--tty=false"}) {
		t.Fatalf("disk create args = %#v", runner.calls[0].Args)
	}
	if _, err := os.Stat(filepath.Join(home, "_hermes-box-definitions", "main.yaml")); err != nil {
		t.Fatal(err)
	}
}

func TestGenerateYAMLHasNoHostMountsAndDenyCatchall(t *testing.T) {
	cfg := config.Config{
		Schema: 1, Name: "main",
		VM:    config.VMConfig{CPUs: 4, Memory: "8GiB", RootDisk: "30GiB", DataDisk: "50GiB"},
		Ports: config.PortsConfig{Executor: 4788}, Backup: config.BackupConfig{Keep: 5},
	}
	lock := validLock()
	data, err := GenerateYAML(cfg, lock)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, required := range []string{
		"minimumLimaVersion: 2.1.3", "vmType: vz", "arch: aarch64", "mounts: []", "system: false", "user: false",
		"loadDotSSHPubKeys: false", "forwardAgent: false", "name: agent", "home: /home/agent", "uid: 1000",
		"upgradePackages: false", "propagateProxyEnv: false", "display: none",
		"name: main-data", "guestPort: 4788", "hostIP: 127.0.0.1", "guestIP: 0.0.0.0", "proto: any", "ignore: true",
		UbuntuImageURL, "sha256:" + UbuntuImageSHA256,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("definition missing %q:\n%s", required, text)
		}
	}
	if strings.Index(text, "guestPort: 4788") > strings.Index(text, "ignore: true") {
		t.Fatal("deny catchall appears before explicit Executor forward")
	}
}

func TestExecutorConfigPortChangesOnlyHostForward(t *testing.T) {
	cfg := config.Config{
		Schema: 1, Name: "main",
		VM:    config.VMConfig{CPUs: 4, Memory: "8GiB", RootDisk: "30GiB", DataDisk: "50GiB"},
		Ports: config.PortsConfig{Executor: 60478}, Backup: config.BackupConfig{Keep: 5},
	}
	data, err := GenerateYAML(cfg, validLock())
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "guestPort: 4788") || !strings.Contains(text, "hostPort: 60478") {
		t.Fatalf("Executor forward did not keep fixed guest port:\n%s", text)
	}
}

func TestGenerateYAMLWithImageUsesAbsoluteRegularFile(t *testing.T) {
	imagePath := filepath.Join(t.TempDir(), "ubuntu 26.04.img")
	if err := os.WriteFile(imagePath, []byte("verified by caller"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Schema: 1, Name: "main",
		VM:    config.VMConfig{CPUs: 4, Memory: "8GiB", RootDisk: "30GiB", DataDisk: "50GiB"},
		Ports: config.PortsConfig{Executor: 4788}, Backup: config.BackupConfig{Keep: 5},
	}
	data, err := GenerateYAMLWithImage(cfg, validLock(), imagePath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "location: file://"+strings.ReplaceAll(imagePath, " ", "%20")) {
		t.Fatalf("definition does not use local image path:\n%s", text)
	}
	if !strings.Contains(text, "digest: sha256:"+UbuntuImageSHA256) {
		t.Fatalf("definition lost lock digest:\n%s", text)
	}
	if strings.Contains(text, UbuntuImageURL) {
		t.Fatalf("definition still uses remote image:\n%s", text)
	}
}

func TestGenerateYAMLWithImageRejectsUnsafePaths(t *testing.T) {
	cfg := config.Config{
		Schema: 1, Name: "main",
		VM:    config.VMConfig{CPUs: 4, Memory: "8GiB", RootDisk: "30GiB", DataDisk: "50GiB"},
		Ports: config.PortsConfig{Executor: 4788}, Backup: config.BackupConfig{Keep: 5},
	}
	lock := validLock()
	if _, err := GenerateYAMLWithImage(cfg, lock, "relative.img"); err == nil {
		t.Fatal("relative image path accepted")
	}
	directory := t.TempDir()
	if _, err := GenerateYAMLWithImage(cfg, lock, directory); err == nil {
		t.Fatal("directory image path accepted")
	}
	target := filepath.Join(directory, "target.img")
	if err := os.WriteFile(target, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(directory, "image.img")
	if err := os.Symlink(target, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := GenerateYAMLWithImage(cfg, lock, symlink); err == nil {
		t.Fatal("symlink image path accepted")
	}
}

func TestGeneratedYAMLValidatesWithQualifiedLima(t *testing.T) {
	binary := os.Getenv("LIMACTL_TEST_BINARY")
	if binary == "" {
		t.Skip("set LIMACTL_TEST_BINARY to run qualified Lima schema validation")
	}
	cfg := config.Config{
		Schema: 1, Name: "main",
		VM:    config.VMConfig{CPUs: 4, Memory: "8GiB", RootDisk: "30GiB", DataDisk: "50GiB"},
		Ports: config.PortsConfig{Executor: 4788}, Backup: config.BackupConfig{Keep: 5},
	}
	lock := validLock()
	data, err := GenerateYAML(cfg, lock)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "main.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "validate", "--tty=false", path)
	command.Env = append(os.Environ(), "LIMA_HOME="+filepath.Join(t.TempDir(), "lima"))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("limactl validate: %v\n%s\n%s", err, output, data)
	}
	imagePath := filepath.Join(t.TempDir(), "ubuntu.img")
	if err := os.WriteFile(imagePath, []byte("verified image fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err = GenerateYAMLWithImage(cfg, lock, imagePath)
	if err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(t.TempDir(), "main-local.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	command = exec.Command(binary, "validate", "--tty=false", path)
	command.Env = append(os.Environ(), "LIMA_HOME="+filepath.Join(t.TempDir(), "lima"))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("limactl validate local image: %v\n%s\n%s", err, output, data)
	}
}

func TestRunnerErrorIncludesStderr(t *testing.T) {
	runner := &fakeRunner{results: []Result{{Stderr: []byte("boom\n")}}, errors: []error{errors.New("exit 1")}}
	client, err := New(filepath.Join(t.TempDir(), "lima"), runner)
	if err != nil {
		t.Fatal(err)
	}
	err = client.Start(context.Background(), "main")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error = %v", err)
	}
}

func validLock() config.Lock {
	return config.Lock{
		Schema:   1,
		Host:     config.HostLock{Lima: QualifiedVersion},
		Ubuntu:   config.UbuntuLock{Release: "26.04", Image: UbuntuImageURL, SHA256: UbuntuImageSHA256, Provisioner: "https://example.com/p.tar.zst", ProvisionerSHA256: hex('a')},
		Tooling:  config.ToolingLock{Node: config.ToolLock{Version: "24", Archive: "https://example.com/n", SHA256: hex('b')}, UV: config.ToolLock{Version: "1", Archive: "https://example.com/u", SHA256: hex('c')}},
		Claude:   config.ClaudeLock{Version: "1", Package: "@anthropic-ai/claude-code", Tarball: "https://example.com/c", Integrity: "sha512-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="},
		Codex:    config.CodexLock{Version: "1", Archive: "https://example.com/x", SHA256: hex('d')},
		Hermes:   config.HermesLock{Repository: "https://github.com/example/h.git", Commit: strings.Repeat("e", 40), Archive: "https://example.com/h", SHA256: hex('f'), UVLockSHA256: hex('a'), PythonArchive: "https://example.com/p", PythonSHA256: hex('b'), WheelsArchive: "https://example.com/w", WheelsSHA256: hex('c')},
		Executor: config.ExecutorLock{Image: "ghcr.io/example/e:v1@sha256:" + hex('d'), LinuxARM64Digest: "sha256:" + hex('e')},
	}
}

func hex(character byte) string {
	return strings.Repeat(string(character), 64)
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
