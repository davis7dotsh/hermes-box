package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/davis7dotsh/hermes-box/internal/config"
	"github.com/davis7dotsh/hermes-box/internal/process"
)

type recordingRunner struct {
	last process.Spec
}

type artifactRunner struct {
	dataDir   string
	outputErr error
	updateErr error
	runs      []process.Spec
}

type startupRunner struct {
	port                  int
	readinessFailures     int
	readinessCalls        int
	startErr              error
	runs                  []process.Spec
	recordEvent           func(string)
	executorRefreshMarked bool
}

type cleanupContextRunner struct {
	deleteContextErr error
}

type machineListRunner struct {
	output []byte
	err    error
}

type blockingMachineExecRunner struct {
	probes      int
	hadDeadline bool
}

type testListener struct {
	closed   bool
	closeErr error
}

func (*testListener) Accept() (net.Conn, error) {
	return nil, errors.New("test listener does not accept connections")
}

func (l *testListener) Close() error {
	l.closed = true
	return l.closeErr
}

func (*testListener) Addr() net.Addr {
	return &net.TCPAddr{}
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
	if r.updateErr != nil && containsArgument(spec.Args, "update") {
		return r.updateErr
	}
	return nil
}

func (r *artifactRunner) Output(_ context.Context, spec process.Spec) ([]byte, error) {
	r.runs = append(r.runs, spec)
	if r.outputErr != nil {
		return nil, r.outputErr
	}
	return []byte(r.dataDir + "\n"), nil
}

func (r *startupRunner) Run(_ context.Context, spec process.Spec) error {
	r.runs = append(r.runs, spec)
	if spec.Name == "smolvm" && len(spec.Args) > 0 &&
		strings.Contains(spec.Args[len(spec.Args)-1],
			"boot_id > "+executorHermesRefreshMarker) {
		r.executorRefreshMarked = true
	}
	if r.startErr != nil && spec.Name == "smolvm" && containsArgument(spec.Args, "start") {
		return r.startErr
	}
	if spec.Name == "smolvm" && len(spec.Args) > 0 {
		script := spec.Args[len(spec.Args)-1]
		if r.recordEvent != nil {
			switch {
			case containsArgument(spec.Args, "restart") && containsArgument(spec.Args, "hermes"):
				r.recordEvent("restart")
			case strings.Contains(script, "hermes --version"):
				r.recordEvent("deep")
			case strings.Contains(script, "supervisorctl status sshd"):
				r.recordEvent("readiness")
			}
		}
		if strings.Contains(script, "supervisorctl status sshd") &&
			!strings.Contains(script, "hermes --version") {
			r.readinessCalls++
			if r.readinessCalls <= r.readinessFailures {
				return fmt.Errorf("readiness probe %d is not ready", r.readinessCalls)
			}
		}
	}
	return nil
}

func (r *startupRunner) Output(_ context.Context, spec process.Spec) ([]byte, error) {
	r.runs = append(r.runs, spec)
	if spec.Name == "lsof" {
		return []byte(fmt.Sprintf("n127.0.0.1:%d\n", r.port)), nil
	}
	if spec.Name == "smolvm" && containsArgument(spec.Args, "list") {
		return []byte(`[{"name":"test-box"}]`), nil
	}
	if spec.Name == "smolvm" && containsArgument(spec.Args, "status") {
		return []byte(`{"state":"running"}`), nil
	}
	if spec.Name == "smolvm" && len(spec.Args) > 0 &&
		strings.Contains(spec.Args[len(spec.Args)-1], executorHermesRefreshMarker) {
		if r.executorRefreshMarked {
			return []byte("refreshed"), nil
		}
		return []byte("stale"), nil
	}
	return nil, nil
}

func (r *cleanupContextRunner) Run(ctx context.Context, spec process.Spec) error {
	if containsArgument(spec.Args, "stop") {
		<-ctx.Done()
		return ctx.Err()
	}
	if containsArgument(spec.Args, "delete") {
		r.deleteContextErr = ctx.Err()
	}
	return nil
}

func (r *cleanupContextRunner) Output(_ context.Context, _ process.Spec) ([]byte, error) {
	return nil, nil
}

func (*machineListRunner) Run(_ context.Context, _ process.Spec) error {
	return nil
}

func (r *machineListRunner) Output(_ context.Context, _ process.Spec) ([]byte, error) {
	return r.output, r.err
}

func (r *blockingMachineExecRunner) Run(ctx context.Context, spec process.Spec) error {
	if spec.Name != "smolvm" || !containsArgument(spec.Args, "exec") {
		return nil
	}
	r.probes++
	_, r.hadDeadline = ctx.Deadline()
	<-ctx.Done()
	return ctx.Err()
}

func (*blockingMachineExecRunner) Output(_ context.Context, _ process.Spec) ([]byte, error) {
	return nil, nil
}

func TestMachineExistsUsesExactJSONListAndPropagatesErrors(t *testing.T) {
	runner := &machineListRunner{output: []byte(`[{"name":"box"},{"name":"box-old"}]`)}
	application := New(t.TempDir(), config.Config{}, runner, io.Discard, io.Discard)

	exists, err := application.machineExists(context.Background(), "box")
	if err != nil || !exists {
		t.Fatalf("machineExists(box) = %t, %v", exists, err)
	}
	exists, err = application.machineExists(context.Background(), "bo")
	if err != nil || exists {
		t.Fatalf("machineExists(bo) = %t, %v", exists, err)
	}

	runner.err = fmt.Errorf("smolvm unavailable")
	if _, err := application.machineExists(context.Background(), "box"); err == nil {
		t.Fatal("machineExists hid a smolvm list failure")
	}
	runner.err = nil
	for _, output := range []string{"not-json", "null"} {
		runner.output = []byte(output)
		if _, err := application.machineExists(context.Background(), "box"); err == nil {
			t.Fatalf("machineExists accepted invalid list JSON %q", output)
		}
	}
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
	mappings, err := config.ReadSecretMappings(path)
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

func TestSSHArgsForwardTerminalMetadataAndPreserveRemoteCommand(t *testing.T) {
	application := New(t.TempDir(), config.Config{}, process.OSRunner{}, io.Discard, io.Discard)
	args := application.sshArgs(2223, "printf", "remote command")
	for _, expected := range []string{
		"SendEnv=COLORTERM",
		"SendEnv=TERM_PROGRAM",
		"SendEnv=TERM_PROGRAM_VERSION",
	} {
		if !containsArgument(args, expected) {
			t.Fatalf("SSH args do not contain %q: %q", expected, args)
		}
	}
	if got := args[len(args)-2:]; got[0] != "printf" || got[1] != "remote command" {
		t.Fatalf("remote command changed: %q", got)
	}
}

func TestValidateSecretMappingEnvironmentRejectsMissingOrEmptyHostValue(t *testing.T) {
	mappings := []string{"OPENAI_API_KEY=HOST_OPENAI_API_KEY"}
	for _, lookup := range []func(string) (string, bool){
		func(string) (string, bool) { return "", false },
		func(string) (string, bool) { return "", true },
	} {
		if err := config.ValidateSecretMappingEnvironment(mappings, lookup); err == nil {
			t.Fatal("validateSecretMappingEnvironment accepted a missing host value")
		}
	}
	if err := config.ValidateSecretMappingEnvironment(
		mappings,
		func(string) (string, bool) { return "secret", true },
	); err != nil {
		t.Fatal(err)
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

func TestRuntimeCreateArgsRequireReferencedHostSecret(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(root, "secret-env.txt"),
		[]byte("OPENAI_API_KEY=HERMES_BOX_TEST_HOST_SECRET\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	application := New(root, config.Config{
		CPUs:        1,
		MemoryMiB:   1,
		StorageGB:   1,
		OverlayGB:   1,
		NetworkMode: "full",
	}, process.OSRunner{}, io.Discard, io.Discard)

	t.Setenv("HERMES_BOX_TEST_HOST_SECRET", "")
	if _, err := application.runtimeCreateArgs("test-box", "/tmp/base", 2223); err == nil {
		t.Fatal("runtimeCreateArgs accepted an empty mapped host secret")
	}
	t.Setenv("HERMES_BOX_TEST_HOST_SECRET", "secret")
	args, err := application.runtimeCreateArgs("test-box", "/tmp/base", 2223)
	if err != nil {
		t.Fatal(err)
	}
	if !containsArgument(args, "OPENAI_API_KEY=HERMES_BOX_TEST_HOST_SECRET") {
		t.Fatalf("runtime args do not contain the validated secret mapping: %q", args)
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

func TestVerifyGuestAcceptsCurrentAndLegacyExecutorLayouts(t *testing.T) {
	runner := &recordingRunner{}
	application := New(t.TempDir(), config.Config{ExecutorEnabled: true}, runner, io.Discard, io.Discard)
	if err := application.verifyGuest(context.Background(), "test-box"); err != nil {
		t.Fatal(err)
	}
	script := runner.last.Args[len(runner.last.Args)-1]
	for _, expected := range []string{
		"if test -L /workspace/.hermes-box-runtime/executor/current; then",
		"executor_pid=$(sudo -u hermes sudo supervisorctl pid executor)",
		"sudo -u hermes sh -c",
		`/proc/$1/environ`,
		"test -d /storage/executor-runtime",
		"curl --connect-timeout 2 --max-time 5",
	} {
		if !strings.Contains(script, expected) {
			t.Fatalf("Executor verification does not contain %q: %s", expected, script)
		}
	}
}

func TestVerifyStartupReadyUsesBoundedExecutorProbe(t *testing.T) {
	runner := &recordingRunner{}
	application := New(t.TempDir(), config.Config{ExecutorEnabled: true}, runner, io.Discard, io.Discard)
	if err := application.verifyStartupReady(context.Background(), "test-box"); err != nil {
		t.Fatal(err)
	}
	script := runner.last.Args[len(runner.last.Args)-1]
	for _, expected := range []string{
		"supervisorctl status sshd",
		"supervisorctl status hermes",
		"supervisorctl status executor",
		"curl --connect-timeout 1 --max-time 2",
	} {
		if !strings.Contains(script, expected) {
			t.Fatalf("startup readiness probe does not contain %q: %s", expected, script)
		}
	}
	if strings.Contains(script, "hermes --version") || strings.Contains(script, "codex --strict-config") {
		t.Fatalf("startup readiness probe performs full verification: %s", script)
	}
}

func TestStartupDeadlinePreservesLeanBoundAndAllowsSlowExecutorPull(t *testing.T) {
	for _, testCase := range []struct {
		name            string
		executorEnabled bool
		want            time.Duration
	}{
		{name: "lean box", want: 20 * time.Minute},
		{name: "Executor box", executorEnabled: true, want: 2 * time.Hour},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			application := New(t.TempDir(), config.Config{
				ExecutorEnabled: testCase.executorEnabled,
			}, process.OSRunner{}, io.Discard, io.Discard)
			if got := application.startupDeadline(); got != testCase.want {
				t.Fatalf("startupDeadline() = %s, want %s", got, testCase.want)
			}
		})
	}
}

func TestWaitForMachineExecBoundsProbeAndPreservesParentCancellation(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		context    func() (context.Context, context.CancelFunc)
		ready      time.Duration
		timeout    time.Duration
		wantErr    error
		wantProbes int
	}{
		{
			name: "probe timeout",
			context: func() (context.Context, context.CancelFunc) {
				return context.WithCancel(context.Background())
			},
			ready:      time.Minute,
			timeout:    time.Millisecond,
			wantErr:    context.DeadlineExceeded,
			wantProbes: 1,
		},
		{
			name: "aggregate timeout",
			context: func() (context.Context, context.CancelFunc) {
				return context.WithCancel(context.Background())
			},
			ready:      time.Millisecond,
			timeout:    time.Minute,
			wantErr:    context.DeadlineExceeded,
			wantProbes: 1,
		},
		{
			name: "parent cancellation",
			context: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				time.AfterFunc(time.Millisecond, cancel)
				return ctx, cancel
			},
			ready:      time.Minute,
			timeout:    time.Minute,
			wantErr:    context.Canceled,
			wantProbes: 1,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, cancel := testCase.context()
			defer cancel()
			runner := &blockingMachineExecRunner{}
			application := New(t.TempDir(), config.Config{}, runner, io.Discard, io.Discard)

			err := application.waitForMachineExecWithin(
				ctx,
				"test-box",
				testCase.wantProbes,
				testCase.ready,
				testCase.timeout,
			)
			if !errors.Is(err, testCase.wantErr) {
				t.Fatalf("waitForMachineExecWithin error = %v, want %v", err, testCase.wantErr)
			}
			if runner.probes != testCase.wantProbes {
				t.Fatalf("machine exec probes = %d, want %d", runner.probes, testCase.wantProbes)
			}
			if !runner.hadDeadline {
				t.Fatal("machine exec probe did not receive a bounded context")
			}
		})
	}
}

func TestValidateInitHost(t *testing.T) {
	if err := validateInitHost("darwin", "arm64"); err != nil {
		t.Fatal(err)
	}
	for _, unsupported := range [][2]string{{"linux", "arm64"}, {"darwin", "amd64"}} {
		if err := validateInitHost(unsupported[0], unsupported[1]); err == nil {
			t.Fatalf("validateInitHost accepted %s/%s", unsupported[0], unsupported[1])
		}
	}
}

func TestValidateHostCommandsReportsMissingCommand(t *testing.T) {
	err := validateHostCommands(func(command string) (string, error) {
		if command == "smolvm" {
			return "", fmt.Errorf("not found")
		}
		return "/usr/bin/" + command, nil
	})
	if err == nil || !strings.Contains(err.Error(), `required host command "smolvm"`) {
		t.Fatalf("validateHostCommands error = %v", err)
	}
}

func TestValidateSupportedToolVersions(t *testing.T) {
	for _, version := range []string{
		"go version go1.24.0 darwin/arm64",
		"go version go1.26.4 darwin/arm64",
		"go version go2.0.0 darwin/arm64",
	} {
		if err := validateGoVersion(version); err != nil {
			t.Fatalf("validateGoVersion(%q): %v", version, err)
		}
	}
	for _, version := range []string{
		"go version go1.23.9 darwin/arm64",
		"go version devel darwin/arm64",
	} {
		if err := validateGoVersion(version); err == nil {
			t.Fatalf("validateGoVersion accepted %q", version)
		}
	}
	if err := validateSmolVMVersion("smolvm 1.0.4\n"); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"smolvm 1.0.3", "smolvm 1.1.0", "garbage"} {
		if err := validateSmolVMVersion(version); err == nil {
			t.Fatalf("validateSmolVMVersion accepted %q", version)
		}
	}
}

func TestVerifyPortAvailableRejectsOccupiedLoopbackPort(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	if err := verifyPortAvailable(port); err == nil {
		t.Fatalf("verifyPortAvailable accepted occupied port %d", port)
	}
}

func TestVerifyPortAvailableSkipsOnlyUnavailableIPv6(t *testing.T) {
	for _, ipv6Err := range []error{
		&net.OpError{Op: "listen", Net: "tcp6", Err: syscall.EAFNOSUPPORT},
		fmt.Errorf("wrapped protocol error: %w", syscall.EPROTONOSUPPORT),
		&net.OpError{Op: "listen", Net: "tcp6", Err: syscall.EADDRNOTAVAIL},
	} {
		t.Run(ipv6Err.Error(), func(t *testing.T) {
			ipv4 := &testListener{}
			var networks []string
			err := verifyPortAvailableWith(2223, func(network, _ string) (net.Listener, error) {
				networks = append(networks, network)
				if network == "tcp6" {
					return nil, ipv6Err
				}
				return ipv4, nil
			})
			if err != nil {
				t.Fatalf("verifyPortAvailableWith rejected unavailable IPv6: %v", err)
			}
			if !ipv4.closed {
				t.Fatal("IPv4 preflight listener was not closed")
			}
			if got := strings.Join(networks, ","); got != "tcp4,tcp6" {
				t.Fatalf("listen networks = %q", got)
			}
		})
	}
}

func TestVerifyPortAvailableReportsIPv6ConflictsAndUnexpectedErrors(t *testing.T) {
	for _, testCase := range []struct {
		name string
		err  error
	}{
		{name: "address in use", err: syscall.EADDRINUSE},
		{name: "permission denied", err: syscall.EACCES},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ipv4 := &testListener{}
			err := verifyPortAvailableWith(2223, func(network, _ string) (net.Listener, error) {
				if network == "tcp6" {
					return nil, &net.OpError{Op: "listen", Net: network, Err: testCase.err}
				}
				return ipv4, nil
			})
			if !errors.Is(err, testCase.err) {
				t.Fatalf("verifyPortAvailableWith error = %v, want %v", err, testCase.err)
			}
			if !strings.Contains(err.Error(), "[::1]:2223") {
				t.Fatalf("IPv6 error does not identify the listener address: %v", err)
			}
			if !ipv4.closed {
				t.Fatal("IPv4 preflight listener was not closed")
			}
		})
	}
}

func TestVerifyPortAvailableReportsListenerCloseFailure(t *testing.T) {
	closeErr := errors.New("close failed")
	listener := &testListener{closeErr: closeErr}
	listenCalls := 0
	err := verifyPortAvailableWith(2223, func(_, _ string) (net.Listener, error) {
		listenCalls++
		return listener, nil
	})
	if !errors.Is(err, closeErr) {
		t.Fatalf("verifyPortAvailableWith error = %v, want %v", err, closeErr)
	}
	if !listener.closed {
		t.Fatal("preflight listener Close was not called")
	}
	if listenCalls != 1 {
		t.Fatalf("listen calls after Close failure = %d, want 1", listenCalls)
	}
	if !strings.Contains(err.Error(), "127.0.0.1:2223") {
		t.Fatalf("Close error does not identify the listener address: %v", err)
	}
}

func TestVerifyPortAvailableClosesBothSupportedListeners(t *testing.T) {
	ipv4 := &testListener{}
	ipv6 := &testListener{}
	err := verifyPortAvailableWith(2223, func(network, _ string) (net.Listener, error) {
		if network == "tcp6" {
			return ipv6, nil
		}
		return ipv4, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ipv4.closed || !ipv6.closed {
		t.Fatalf("closed listeners: IPv4=%t IPv6=%t", ipv4.closed, ipv6.closed)
	}
}

func TestVerifyPortAvailableReportsIPv6ListenerCloseFailure(t *testing.T) {
	closeErr := errors.New("IPv6 close failed")
	ipv4 := &testListener{}
	ipv6 := &testListener{closeErr: closeErr}
	err := verifyPortAvailableWith(2223, func(network, _ string) (net.Listener, error) {
		if network == "tcp6" {
			return ipv6, nil
		}
		return ipv4, nil
	})
	if !errors.Is(err, closeErr) {
		t.Fatalf("verifyPortAvailableWith error = %v, want %v", err, closeErr)
	}
	if !ipv4.closed || !ipv6.closed {
		t.Fatalf("closed listeners: IPv4=%t IPv6=%t", ipv4.closed, ipv6.closed)
	}
	if !strings.Contains(err.Error(), "[::1]:2223") {
		t.Fatalf("IPv6 Close error does not identify the listener address: %v", err)
	}
}

func TestVerifyPortAvailableDoesNotSkipUnavailableIPv4(t *testing.T) {
	listenCalls := 0
	err := verifyPortAvailableWith(2223, func(_, _ string) (net.Listener, error) {
		listenCalls++
		return nil, &net.OpError{Op: "listen", Net: "tcp4", Err: syscall.EADDRNOTAVAIL}
	})
	if !errors.Is(err, syscall.EADDRNOTAVAIL) {
		t.Fatalf("verifyPortAvailableWith error = %v, want %v", err, syscall.EADDRNOTAVAIL)
	}
	if listenCalls != 1 {
		t.Fatalf("listen calls after IPv4 failure = %d, want 1", listenCalls)
	}
	if !strings.Contains(err.Error(), "127.0.0.1:2223") {
		t.Fatalf("IPv4 error does not identify the listener address: %v", err)
	}
}

func TestStartNamedMachineRunsFullVerificationOnce(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	runner := &startupRunner{port: port, readinessFailures: 2}
	application := New(t.TempDir(), config.Config{}, runner, io.Discard, io.Discard)

	if err := application.startNamedMachine(context.Background(), "test-box", port); err != nil {
		t.Fatal(err)
	}
	fullVerifications := 0
	readinessProbes := 0
	for _, run := range runner.runs {
		if run.Name != "smolvm" || len(run.Args) == 0 {
			continue
		}
		script := run.Args[len(run.Args)-1]
		if strings.Contains(script, "hermes --version") {
			fullVerifications++
		}
		if strings.Contains(script, "supervisorctl status sshd") &&
			!strings.Contains(script, "hermes --version") {
			readinessProbes++
		}
	}
	if fullVerifications != 1 {
		t.Fatalf("full guest verification count = %d, want 1", fullVerifications)
	}
	if readinessProbes != 3 {
		t.Fatalf("lightweight readiness probe count = %d, want 3", readinessProbes)
	}
}

func TestExecutorStartupRestartsHermesThenRunsOneDeepVerification(t *testing.T) {
	var eventMutex sync.Mutex
	var events []string
	recordEvent := func(event string) {
		eventMutex.Lock()
		defer eventMutex.Unlock()
		events = append(events, event)
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		recordEvent("http")
		response.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	port := server.Listener.Addr().(*net.TCPAddr).Port
	runner := &startupRunner{port: port, recordEvent: recordEvent}
	application := New(t.TempDir(), config.Config{
		ExecutorEnabled: true,
		ExecutorPort:    port,
	}, runner, io.Discard, io.Discard)

	if err := application.startNamedMachine(context.Background(), "test-box", port); err != nil {
		t.Fatal(err)
	}
	fullVerifications := 0
	hermesRestarts := 0
	for _, run := range runner.runs {
		if run.Name != "smolvm" || len(run.Args) == 0 {
			continue
		}
		script := run.Args[len(run.Args)-1]
		if strings.Contains(script, "hermes --version") {
			fullVerifications++
		}
		if containsArgument(run.Args, "restart") && containsArgument(run.Args, "hermes") {
			hermesRestarts++
		}
	}
	if hermesRestarts != 1 {
		t.Fatalf("Hermes restart count = %d, want 1", hermesRestarts)
	}
	if fullVerifications != 1 {
		t.Fatalf("full guest verification count = %d, want 1", fullVerifications)
	}
	if runner.readinessCalls != 2 {
		t.Fatalf("readiness calls = %d, want pre/post restart probes", runner.readinessCalls)
	}
	eventMutex.Lock()
	gotEvents := strings.Join(events, ",")
	eventMutex.Unlock()
	if gotEvents != "readiness,http,restart,readiness,deep" {
		t.Fatalf("startup verification order = %q", gotEvents)
	}
	if !runner.executorRefreshMarked {
		t.Fatal("successful Executor startup did not record the per-boot Hermes refresh")
	}
}

func TestCmdStartAlreadyRunningWaitsAndRunsDeepVerification(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	runner := &startupRunner{port: port, readinessFailures: 1}
	application := New(t.TempDir(), config.Config{
		MachineName: "test-box",
		SSHPort:     port,
	}, runner, io.Discard, io.Discard)

	if err := application.cmdStart(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	fullVerifications := 0
	for _, run := range runner.runs {
		if run.Name == "smolvm" && len(run.Args) > 0 &&
			strings.Contains(run.Args[len(run.Args)-1], "hermes --version") {
			fullVerifications++
		}
	}
	if fullVerifications != 1 {
		t.Fatalf("full guest verification count = %d, want 1", fullVerifications)
	}
	if runner.readinessCalls != 2 {
		t.Fatalf("readiness calls = %d, want retry then success", runner.readinessCalls)
	}
}

func TestCmdStartAlreadyRunningExecutorSkipsCompletedPerBootRefresh(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	port := server.Listener.Addr().(*net.TCPAddr).Port
	runner := &startupRunner{
		port:                  port,
		executorRefreshMarked: true,
	}
	application := New(t.TempDir(), config.Config{
		MachineName:     "test-box",
		SSHPort:         port,
		ExecutorEnabled: true,
		ExecutorPort:    port,
	}, runner, io.Discard, io.Discard)

	if err := application.cmdStart(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	fullVerifications := 0
	hermesRestarts := 0
	for _, run := range runner.runs {
		if run.Name != "smolvm" || len(run.Args) == 0 {
			continue
		}
		if strings.Contains(run.Args[len(run.Args)-1], "hermes --version") {
			fullVerifications++
		}
		if containsArgument(run.Args, "restart") && containsArgument(run.Args, "hermes") {
			hermesRestarts++
		}
	}
	if hermesRestarts != 0 {
		t.Fatalf("Hermes restart count = %d, want 0 for refreshed boot", hermesRestarts)
	}
	if fullVerifications != 1 {
		t.Fatalf("full guest verification count = %d, want 1", fullVerifications)
	}
	if runner.readinessCalls != 1 {
		t.Fatalf("readiness calls = %d, want 1", runner.readinessCalls)
	}
}

func TestStartNamedMachineDiagnosesAndStopsAfterStartError(t *testing.T) {
	runner := &startupRunner{startErr: fmt.Errorf("start failed")}
	application := New(t.TempDir(), config.Config{}, runner, io.Discard, io.Discard)

	if err := application.startNamedMachine(context.Background(), "test-box", 2223); err == nil {
		t.Fatal("startNamedMachine succeeded despite injected start error")
	}
	foundDiagnostics := false
	foundStop := false
	for _, run := range runner.runs {
		if run.Name != "smolvm" || len(run.Args) == 0 {
			continue
		}
		if strings.Contains(run.Args[len(run.Args)-1], "/var/log/hermes-box-startup.log") {
			foundDiagnostics = true
		}
		if containsArgument(run.Args, "stop") {
			foundStop = true
		}
	}
	if !foundDiagnostics || !foundStop {
		t.Fatalf("start failure diagnostics=%t stop=%t runs=%#v", foundDiagnostics, foundStop, runner.runs)
	}
}

func TestStartFirstRuntimePreservesOwnedMachineForResume(t *testing.T) {
	runner := &startupRunner{startErr: fmt.Errorf("start failed")}
	root := t.TempDir()
	application := New(root, config.Config{
		MachineName: "resume-box",
		SSHPort:     2223,
	}, runner, io.Discard, io.Discard)

	keepRuntime, err := application.startFirstRuntime(context.Background())
	if err == nil {
		t.Fatal("startFirstRuntime succeeded despite injected start error")
	}
	if !keepRuntime {
		t.Fatal("startFirstRuntime marked the successfully created runtime for deletion")
	}
	for _, expected := range []string{
		"preserved machine \"resume-box\"",
		filepath.Join(root, "bin", "hermes-box") + " start",
		filepath.Join(root, "bin", "hermes-box") + " logs",
		filepath.Join(root, "bin", "hermes-box") + " destroy --force",
		"may contain injected secrets",
	} {
		if !strings.Contains(err.Error(), expected) {
			t.Fatalf("resume error does not contain %q:\n%s", expected, err)
		}
	}
	if recordedMachineDelete(runner.runs, "resume-box") {
		t.Fatalf("first-start failure deleted resumable runtime: %#v", runner.runs)
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
	if err := application.createFromArtifact(context.Background(), "test-box", "/tmp/base.smolmachine", 2223, false); err != nil {
		t.Fatal(err)
	}
	if runner.runs[0].Name != "env" ||
		!containsArgument(runner.runs[0].Args, "SMOLVM_PACK_CACHE_MAX_BYTES=17179869184") ||
		!containsArgument(runner.runs[0].Args, "smolvm") {
		t.Fatalf("unexpected artifact creation: %#v", runner.runs[0])
	}
}

func TestCreateFromArtifactCanEnableRestoreMode(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "pack"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "pack", ".smolvm-extracted"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &artifactRunner{dataDir: dataDir}
	application := New(t.TempDir(), config.Config{
		CPUs: 1, MemoryMiB: 1, StorageGB: 1, OverlayGB: 1, NetworkMode: "full",
	}, runner, io.Discard, io.Discard)
	if err := application.createFromArtifact(
		context.Background(),
		"restore-box",
		"/tmp/base.smolmachine",
		2223,
		true,
	); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"-e", "HERMES_BOX_RESTORE_MODE=true"} {
		if !containsArgument(runner.runs[0].Args, expected) {
			t.Fatalf("restore create args do not contain %q: %#v", expected, runner.runs[0].Args)
		}
	}
}

func TestCreateFromArtifactCleansUpAfterPostCreateFailures(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		outputErr error
		updateErr error
	}{
		{name: "data directory", outputErr: fmt.Errorf("data-dir failed")},
		{name: "network update", updateErr: fmt.Errorf("update failed")},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			dataDir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dataDir, "pack"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dataDir, "pack", ".smolvm-extracted"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			runner := &artifactRunner{
				dataDir:   dataDir,
				outputErr: testCase.outputErr,
				updateErr: testCase.updateErr,
			}
			application := New(t.TempDir(), config.Config{
				CPUs: 1, MemoryMiB: 1, StorageGB: 1, OverlayGB: 1, NetworkMode: "full",
			}, runner, io.Discard, io.Discard)
			if err := application.createFromArtifact(
				context.Background(),
				"cleanup-box",
				"/tmp/base.smolmachine",
				2223,
				false,
			); err == nil {
				t.Fatal("createFromArtifact succeeded despite injected post-create failure")
			}
			if !recordedMachineDelete(runner.runs, "cleanup-box") {
				t.Fatalf("newly created machine was not deleted: %#v", runner.runs)
			}
		})
	}
}

func TestDeleteMachineCleanupUsesFreshDeleteContextAfterStopTimeout(t *testing.T) {
	runner := &cleanupContextRunner{}
	application := New(t.TempDir(), config.Config{}, runner, io.Discard, io.Discard)
	if err := application.deleteMachineForCleanupWithin(
		"cleanup-box",
		time.Millisecond,
		time.Second,
	); err != nil {
		t.Fatal(err)
	}
	if runner.deleteContextErr != nil {
		t.Fatalf("delete received canceled context: %v", runner.deleteContextErr)
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

func recordedMachineDelete(runs []process.Spec, name string) bool {
	for _, run := range runs {
		if run.Name == "smolvm" &&
			containsArgument(run.Args, "delete") &&
			containsArgument(run.Args, name) {
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
	if _, err := config.ReadSecretMappings(path); err == nil {
		t.Fatal("readSecretMappings accepted shell syntax")
	}
}
