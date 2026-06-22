package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/davis7dotsh/hermes-box/internal/config"
	"github.com/davis7dotsh/hermes-box/internal/process"
)

type machineStatus struct {
	State string `json:"state"`
}

type machineListEntry struct {
	Name string `json:"name"`
}

const (
	executorHermesRefreshMarker = "/run/hermes-box-executor-discovery-refreshed"
	machineExecReadyTimeout     = 2 * time.Minute
	machineExecProbeTimeout     = 5 * time.Second
)

func (a *App) machineExists(ctx context.Context, name string) (bool, error) {
	output, err := a.output(ctx, "smolvm", "machine", "list", "--json")
	if err != nil {
		return false, fmt.Errorf("list smolvm machines: %w", err)
	}
	var machines []machineListEntry
	if err := json.Unmarshal(output, &machines); err != nil {
		return false, fmt.Errorf("parse smolvm machine list: %w", err)
	}
	if machines == nil {
		return false, errors.New("parse smolvm machine list: expected a JSON array")
	}
	for _, machine := range machines {
		if machine.Name == name {
			return true, nil
		}
	}
	return false, nil
}

func (a *App) machineState(ctx context.Context, name string) (string, error) {
	output, err := a.output(ctx, "smolvm", "machine", "status", "--name", name, "--json")
	if err != nil {
		return "", fmt.Errorf("read machine status for %s: %w", name, err)
	}
	var status machineStatus
	if err := json.Unmarshal(output, &status); err != nil {
		return "", fmt.Errorf("parse machine status for %s: %w", name, err)
	}
	if status.State == "" {
		return "", fmt.Errorf("machine status for %s did not include state", name)
	}
	return status.State, nil
}

func (a *App) isRunning(ctx context.Context, name string) (bool, error) {
	state, err := a.machineState(ctx, name)
	if err != nil {
		return false, err
	}
	return state == "running", nil
}

func (a *App) clearKnownHost(ctx context.Context, port int) error {
	if _, err := os.Stat(a.knownHosts); os.IsNotExist(err) {
		return nil
	}
	err := a.runQuiet(
		ctx,
		"ssh-keygen",
		"-f", a.knownHosts,
		"-R", fmt.Sprintf("[127.0.0.1]:%d", port),
	)
	if err != nil {
		a.log("warning: could not remove stale SSH host key for port %d", port)
	}
	return nil
}

func waitForPort(ctx context.Context, port int) error {
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	dialer := net.Dialer{Timeout: 250 * time.Millisecond}
	for {
		connection, err := dialer.DialContext(ctx, "tcp", address)
		if err == nil {
			connection.Close()
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("port %d did not become ready: %w", port, ctx.Err())
		}
		if err := sleep(ctx, 500*time.Millisecond); err != nil {
			return fmt.Errorf("port %d did not become ready: %w", port, err)
		}
	}
}

func (a *App) verifyLoopbackListener(ctx context.Context, port int) error {
	output, err := a.runner.Output(ctx, process.Spec{
		Name: "lsof",
		Args: []string{
			"-nP",
			"-F", "n",
			fmt.Sprintf("-iTCP:%d", port),
			"-sTCP:LISTEN",
		},
		Stderr: io.Discard,
	})
	if err != nil {
		return fmt.Errorf("no listener found on port %d", port)
	}

	found := false
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "n") {
			continue
		}
		found = true
		endpoint := strings.TrimPrefix(line, "n")
		host, endpointPort, err := net.SplitHostPort(endpoint)
		if err != nil {
			return fmt.Errorf("could not parse listener %q", endpoint)
		}
		if endpointPort != strconv.Itoa(port) || host != "127.0.0.1" && host != "::1" {
			return fmt.Errorf("unsafe listener detected: %s", endpoint)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read listener data: %w", err)
	}
	if !found {
		return fmt.Errorf("no listener found on port %d", port)
	}
	return nil
}

func (a *App) sshArgs(port int, remoteArgs ...string) []string {
	args := []string{
		"-i", a.sshKey,
		"-p", strconv.Itoa(port),
		"-o", "IdentitiesOnly=yes",
		"-o", "ForwardAgent=no",
		"-o", "SendEnv=COLORTERM",
		"-o", "SendEnv=TERM_PROGRAM",
		"-o", "SendEnv=TERM_PROGRAM_VERSION",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=" + a.knownHosts,
		"boxadmin@127.0.0.1",
	}
	return append(args, remoteArgs...)
}

func (a *App) verifySSH(ctx context.Context, port int) error {
	if err := a.runQuiet(ctx, "ssh", a.sshArgs(port, "true")...); err != nil {
		return fmt.Errorf("SSH authentication failed on loopback port %d: %w", port, err)
	}
	return nil
}

func (a *App) verifyGuest(ctx context.Context, name string) error {
	script := `
set -e
test -d /workspace/hermes-home
test -w /workspace/hermes-home
test -f /workspace/hermes-home/config.yaml
test -d /workspace/codex-home
test -w /workspace/codex-home
test -f /workspace/codex-home/config.toml
test -x /usr/local/bin/tm
test -f /etc/tmux.conf
test "$(stat -c %U /workspace/hermes-home)" = hermes
test "$(stat -c %U /workspace/codex-home)" = hermes
supervisorctl status sshd | grep -q RUNNING
supervisorctl status hermes | grep -q RUNNING
sudo -iu hermes env HERMES_HOME=/workspace/hermes-home hermes --version
sudo -iu hermes /usr/local/lib/hermes-agent/venv/bin/python -c 'import discord'
sudo -iu hermes node --version
sudo -iu hermes npm --version
sudo -iu hermes tmux -V
infocmp -x tmux-256color >/dev/null
infocmp -x xterm-256color >/dev/null
sudo -iu hermes codex --strict-config --version
`
	if a.config.ExecutorEnabled {
		script += `
supervisorctl status executor | grep -q RUNNING
if test -L /workspace/.hermes-box-runtime/executor/current; then
  test "$(cat /workspace/.hermes-box-runtime/executor/current/.image-reference)" = "$HERMES_BOX_EXECUTOR_IMAGE"
  test -s /workspace/.hermes-box-runtime/executor/current/.manifest-digest
  executor_pid=$(sudo -u hermes sudo supervisorctl pid executor)
  sudo -u hermes sh -c 'tr "\0" "\n" <"/proc/$1/environ"' sh "$executor_pid" |
    grep -qx 'BUN_FEATURE_FLAG_DISABLE_IPV6=1'
else
  test -d /storage/executor-runtime
fi
curl --connect-timeout 2 --max-time 5 -fsS http://127.0.0.1:4788/api/health >/dev/null
`
	}
	return a.runQuiet(
		ctx,
		"smolvm",
		"machine", "exec",
		"--name", name,
		"--",
		"bash", "-lc", script,
	)
}

func (a *App) verifyExecutorHTTP(ctx context.Context) error {
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/api/health", a.config.ExecutorPort),
		nil,
	)
	if err != nil {
		return err
	}
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		return fmt.Errorf("Executor health check failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Executor health check returned %s", response.Status)
	}
	return nil
}

func (a *App) verifyBuilderNetwork(ctx context.Context, name string) error {
	script := `
set -e
awk '$2 == "00000000" && $4 ~ /3/ { found = 1 } END { exit !found }' /proc/net/route
getent ahostsv4 ports.ubuntu.com >/dev/null
`
	return a.runQuiet(
		ctx,
		"smolvm",
		"machine", "exec",
		"--name", name,
		"--",
		"bash", "-lc", script,
	)
}

func (a *App) ensureBuilderNetwork(ctx context.Context, name string) error {
	const (
		bootAttempts  = 3
		probeAttempts = 15
	)
	networkCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	var lastErr error
	for bootAttempt := 1; bootAttempt <= bootAttempts; bootAttempt++ {
		for range probeAttempts {
			probeCtx, probeCancel := context.WithTimeout(networkCtx, 5*time.Second)
			err := a.verifyBuilderNetwork(probeCtx, name)
			probeCancel()
			if err == nil {
				return nil
			}
			lastErr = err
			if err := sleep(networkCtx, time.Second); err != nil {
				return err
			}
		}
		if bootAttempt == bootAttempts {
			break
		}
		a.log(
			"builder network unavailable after boot %d/%d; restarting disposable builder",
			bootAttempt,
			bootAttempts,
		)
		operationCtx, operationCancel := context.WithTimeout(networkCtx, 30*time.Second)
		err := a.run(operationCtx, "smolvm", "machine", "stop", "--name", name)
		operationCancel()
		if err != nil {
			return err
		}
		operationCtx, operationCancel = context.WithTimeout(networkCtx, 30*time.Second)
		err = a.run(operationCtx, "smolvm", "machine", "update", "--name", name, "--net")
		operationCancel()
		if err != nil {
			return err
		}
		operationCtx, operationCancel = context.WithTimeout(networkCtx, 30*time.Second)
		err = a.run(operationCtx, "smolvm", "machine", "start", "--name", name)
		operationCancel()
		if err != nil {
			return err
		}
	}
	return fmt.Errorf(
		"builder %s has no usable IPv4 route or DNS after %d boots: %w",
		name,
		bootAttempts,
		lastErr,
	)
}

func (a *App) verifyStartupReady(ctx context.Context, name string) error {
	script := `
set -e
test -f /workspace/hermes-home/config.yaml
test -f /workspace/codex-home/config.toml
supervisorctl status sshd | grep -q RUNNING
supervisorctl status hermes | grep -q RUNNING
`
	if a.config.ExecutorEnabled {
		script += `
supervisorctl status executor | grep -q RUNNING
curl --connect-timeout 1 --max-time 2 -fsS http://127.0.0.1:4788/api/health >/dev/null
`
	}
	return a.runQuiet(
		ctx,
		"smolvm",
		"machine", "exec",
		"--name", name,
		"--",
		"bash", "-lc", script,
	)
}

func (a *App) waitForStartupReady(ctx context.Context, name string, port int) error {
	var lastErr error
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := a.verifyStartupReady(probeCtx, name); err == nil {
			if err := a.verifySSH(probeCtx, port); err == nil {
				cancel()
				return nil
			} else {
				lastErr = err
			}
		} else {
			lastErr = err
		}
		cancel()
		if ctx.Err() != nil {
			break
		}
		if err := sleep(ctx, 500*time.Millisecond); err != nil {
			break
		}
	}
	if lastErr == nil {
		lastErr = errors.New("guest did not become healthy")
	}
	return fmt.Errorf("guest %s did not become ready before the startup deadline: %w", name, lastErr)
}

func (a *App) waitForMachineExec(ctx context.Context, name string, attempts int) error {
	return a.waitForMachineExecWithin(
		ctx,
		name,
		attempts,
		machineExecReadyTimeout,
		machineExecProbeTimeout,
	)
}

func (a *App) waitForMachineExecWithin(
	ctx context.Context,
	name string,
	attempts int,
	readyTimeout time.Duration,
	probeTimeout time.Duration,
) error {
	readyCtx, cancelReady := context.WithTimeout(ctx, readyTimeout)
	defer cancelReady()
	var lastErr error
	for attempt := range attempts {
		probeCtx, cancel := context.WithTimeout(readyCtx, probeTimeout)
		if err := a.runQuiet(
			probeCtx,
			"smolvm",
			"machine", "exec",
			"--name", name,
			"--",
			"true",
		); err == nil {
			cancel()
			return nil
		} else {
			lastErr = err
		}
		cancel()
		if attempt+1 == attempts {
			break
		}
		if err := sleep(readyCtx, 500*time.Millisecond); err != nil {
			return err
		}
	}
	return fmt.Errorf("guest agent for %s did not become ready: %w", name, lastErr)
}

func (a *App) startupDeadline() time.Duration {
	startupTimeout := 20 * time.Minute
	if a.config.ExecutorEnabled {
		// Executor's guest pull uses a 10-minute inactivity deadline instead of
		// discarding an actively downloading large layer at a shorter fixed cap.
		// Keep a generous outer safety bound for the complete startup.
		startupTimeout = 2 * time.Hour
	}
	return startupTimeout
}

func (a *App) completeStartupVerification(ctx context.Context, name string, port int) error {
	if err := a.waitForStartupReady(ctx, name, port); err != nil {
		return err
	}
	refreshedHermes := false
	if a.config.ExecutorEnabled {
		if err := a.verifyExecutorHTTP(ctx); err != nil {
			return err
		}
		refreshCurrent, err := a.executorHermesRefreshCurrent(ctx, name)
		if err != nil {
			return err
		}
		if !refreshCurrent {
			a.log("Executor is healthy; restarting Hermes to refresh MCP discovery")
			if err := a.runQuiet(
				ctx,
				"smolvm",
				"machine", "exec",
				"--name", name,
				"--",
				"supervisorctl", "restart", "hermes",
			); err != nil {
				return fmt.Errorf("restart Hermes after Executor became healthy: %w", err)
			}
			if err := a.waitForStartupReady(ctx, name, port); err != nil {
				return fmt.Errorf("Hermes did not become ready after Executor discovery refresh: %w", err)
			}
			refreshedHermes = true
		}
	}
	if err := a.verifyGuest(ctx, name); err != nil {
		return fmt.Errorf("full guest verification failed: %w", err)
	}
	if refreshedHermes {
		if err := a.runQuiet(
			ctx,
			"smolvm",
			"machine", "exec",
			"--name", name,
			"--",
			"sh", "-c",
			"umask 077; cat /proc/sys/kernel/random/boot_id > "+executorHermesRefreshMarker,
		); err != nil {
			return fmt.Errorf("record Executor discovery refresh: %w", err)
		}
	}
	return nil
}

func (a *App) executorHermesRefreshCurrent(ctx context.Context, name string) (bool, error) {
	output, err := a.output(
		ctx,
		"smolvm",
		"machine", "exec",
		"--name", name,
		"--",
		"sh", "-c",
		"boot_id=$(cat /proc/sys/kernel/random/boot_id); "+
			"if test -f "+executorHermesRefreshMarker+
			" && test \"$(cat "+executorHermesRefreshMarker+")\" = \"$boot_id\"; "+
			"then printf refreshed; else printf stale; fi",
	)
	if err != nil {
		return false, fmt.Errorf("check Executor discovery refresh: %w", err)
	}
	switch trimOutput(output) {
	case "refreshed":
		return true, nil
	case "stale":
		return false, nil
	default:
		return false, fmt.Errorf("check Executor discovery refresh returned %q", trimOutput(output))
	}
}

func (a *App) startNamedMachine(ctx context.Context, name string, port int) (resultErr error) {
	startupTimeout := a.startupDeadline()
	startupCtx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()

	stopOnFailure := true
	defer func() {
		if stopOnFailure {
			diagnosticCtx, diagnosticCancel := context.WithTimeout(context.Background(), 15*time.Second)
			a.captureStartupDiagnostics(diagnosticCtx, name)
			diagnosticCancel()
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer stopCancel()
			if stopErr := a.runQuiet(stopCtx, "smolvm", "machine", "stop", "--name", name); stopErr != nil {
				resultErr = fmt.Errorf(
					"%w; stopping failed machine %s also failed: %v",
					resultErr,
					name,
					stopErr,
				)
			}
		}
	}()
	if err := a.run(startupCtx, "smolvm", "machine", "start", "--name", name); err != nil {
		return err
	}

	if err := waitForPort(startupCtx, port); err != nil {
		return err
	}
	if err := a.verifyLoopbackListener(startupCtx, port); err != nil {
		return err
	}
	if a.config.ExecutorEnabled {
		if err := waitForPort(startupCtx, a.config.ExecutorPort); err != nil {
			return err
		}
		if err := a.verifyLoopbackListener(startupCtx, a.config.ExecutorPort); err != nil {
			return fmt.Errorf("Executor listener is not loopback-only: %w", err)
		}
	}
	if err := a.completeStartupVerification(startupCtx, name, port); err != nil {
		return err
	}
	stopOnFailure = false
	return nil
}

func (a *App) captureStartupDiagnostics(ctx context.Context, name string) {
	a.log("startup verification failed; collecting startup and service logs for %s", name)
	script := `
printf '%s\n' '--- /var/log/hermes-box-startup.log ---'
if test -f /var/log/hermes-box-startup.log; then
  tail -n 200 /var/log/hermes-box-startup.log
else
  printf '%s\n' 'startup log is unavailable'
fi
printf '%s\n' '--- supervisorctl status ---'
supervisorctl status 2>&1 || true
for service_log in \
  /workspace/hermes-home/logs/hermes-gateway.log \
  /workspace/hermes-home/logs/supervisord.log \
  /workspace/hermes-home/logs/sshd.log \
  /workspace/executor/executor.log; do
  printf '%s\n' "--- $service_log ---"
  if test -f "$service_log"; then
    tail -n 200 "$service_log"
  else
    printf '%s\n' 'log is unavailable'
  fi
done
`
	_ = a.runner.Run(ctx, process.Spec{
		Name: "smolvm",
		Args: []string{
			"machine", "exec",
			"--name", name,
			"--",
			"bash", "-lc", script,
		},
		Stdout: a.stderr,
		Stderr: a.stderr,
	})
}

func verifyDirectoryWritable(directory string) error {
	probe, err := os.CreateTemp(directory, ".hermes-box-write-test-*")
	if err != nil {
		return fmt.Errorf("directory is not writable: %s: %w", directory, err)
	}
	name := probe.Name()
	if err := probe.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("close write probe in %s: %w", directory, err)
	}
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("remove write probe in %s: %w", directory, err)
	}
	return nil
}

func verifyPortAvailable(port int) error {
	return verifyPortAvailableWith(port, net.Listen)
}

func verifyPortAvailableWith(
	port int,
	listen func(network, address string) (net.Listener, error),
) error {
	for _, target := range []struct {
		network string
		address string
	}{
		{network: "tcp4", address: net.JoinHostPort("127.0.0.1", strconv.Itoa(port))},
		{network: "tcp6", address: net.JoinHostPort("::1", strconv.Itoa(port))},
	} {
		listener, err := listen(target.network, target.address)
		if err != nil {
			if target.network == "tcp6" && ipv6LoopbackUnavailable(err) {
				continue
			}
			return fmt.Errorf("port %d is unavailable on host loopback %s: %w", port, target.address, err)
		}
		if err := listener.Close(); err != nil {
			return fmt.Errorf(
				"release port %d preflight listener on host loopback %s: %w",
				port,
				target.address,
				err,
			)
		}
	}
	return nil
}

func ipv6LoopbackUnavailable(err error) bool {
	return errors.Is(err, syscall.EAFNOSUPPORT) ||
		errors.Is(err, syscall.EPROTONOSUPPORT) ||
		errors.Is(err, syscall.EADDRNOTAVAIL)
}

func (a *App) stopNamedMachine(ctx context.Context, name string) error {
	exists, err := a.machineExists(ctx, name)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	running, err := a.isRunning(ctx, name)
	if err != nil {
		return err
	}
	if !running {
		return nil
	}
	if a.config.ExecutorEnabled {
		_ = a.runQuiet(ctx, "smolvm", "machine", "exec", "--name", name, "--", "supervisorctl", "stop", "hermes", "executor")
	} else {
		_ = a.runQuiet(ctx, "smolvm", "machine", "exec", "--name", name, "--", "supervisorctl", "stop", "hermes")
	}
	return a.run(ctx, "smolvm", "machine", "stop", "--name", name)
}

func (a *App) ensureKey(ctx context.Context) error {
	if _, err := os.Stat(a.sshKey); err == nil {
		return a.requireSSHKey(ctx)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect SSH key: %w", err)
	}
	if a.externalKey {
		return fmt.Errorf("configured SSH key not found: %s", a.sshKey)
	}
	a.log("generating dedicated SSH key: %s", a.sshKey)
	if err := a.run(
		ctx,
		"ssh-keygen",
		"-q",
		"-t", "ed25519",
		"-N", "",
		"-C", a.config.MachineName,
		"-f", a.sshKey,
	); err != nil {
		return err
	}
	return a.requireSSHKey(ctx)
}

func (a *App) requireSSHKey(ctx context.Context) error {
	info, err := os.Stat(a.sshKey)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("restore requires the stable external SSH key: %s", a.sshKey)
		}
		return fmt.Errorf("inspect SSH key: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("SSH key is not a regular file: %s", a.sshKey)
	}
	if err := os.Chmod(a.sshKey, 0o600); err != nil {
		return fmt.Errorf("secure SSH key: %w", err)
	}
	publicKey, err := a.output(ctx, "ssh-keygen", "-y", "-f", a.sshKey)
	if err != nil {
		return fmt.Errorf("derive SSH public key: %w", err)
	}
	publicKey = []byte(strings.TrimSpace(string(publicKey)) + "\n")
	if err := os.WriteFile(a.sshPublicKey, publicKey, 0o600); err != nil {
		return fmt.Errorf("write derived SSH public key: %w", err)
	}
	return nil
}

func (a *App) sshFingerprint(ctx context.Context) (string, error) {
	output, err := a.output(ctx, "ssh-keygen", "-lf", a.sshPublicKey, "-E", "sha256")
	if err != nil {
		return "", fmt.Errorf("fingerprint SSH key: %w", err)
	}
	fields := strings.Fields(string(output))
	if len(fields) < 2 || !strings.HasPrefix(fields[1], "SHA256:") {
		return "", errors.New("ssh-keygen returned an invalid fingerprint")
	}
	return fields[1], nil
}

func (a *App) runtimeCreateArgs(name, artifact string, port int) ([]string, error) {
	if err := a.validateNetworkMode(); err != nil {
		return nil, err
	}
	args := []string{
		"machine", "create",
		"--name", name,
		"--from", artifact,
		"--cpus", strconv.Itoa(a.config.CPUs),
		"--mem", strconv.Itoa(a.config.MemoryMiB),
		"--storage", strconv.Itoa(a.config.StorageGB),
		"--overlay", strconv.Itoa(a.config.OverlayGB),
		"-p", fmt.Sprintf("%d:22", port),
		"-e", "HERMES_BOX_EXECUTOR_ENABLED=" + strconv.FormatBool(a.config.ExecutorEnabled),
		"-e", "HERMES_BOX_EXECUTOR_IMAGE=" + a.config.ExecutorImage,
		"-e", "HERMES_BOX_EXECUTOR_HOST_PORT=" + strconv.Itoa(a.config.ExecutorPort),
		"--net",
	}
	if a.config.ExecutorEnabled {
		args = append(args, "-p", fmt.Sprintf("%d:4788", a.config.ExecutorPort))
	}
	secrets, err := a.readValidatedSecretMappings()
	if err != nil {
		return nil, err
	}
	for _, secret := range secrets {
		args = append(args, "--secret-env", secret)
	}
	return args, nil
}

func (a *App) readValidatedSecretMappings() ([]string, error) {
	mappings, err := config.ReadSecretMappings(a.secretEnvFile)
	if err != nil {
		return nil, err
	}
	if err := config.ValidateSecretMappingEnvironment(mappings, os.LookupEnv); err != nil {
		return nil, err
	}
	return mappings, nil
}

func (a *App) validateNetworkMode() error {
	switch a.config.NetworkMode {
	case "full":
		return nil
	case "none":
		return errors.New("no-egress mode is unavailable: smolvm 1.0.4 machine update --no-net and create --outbound-localhost-only still permit external traffic; explicitly select full networking to proceed")
	case "strict":
		return errors.New("strict mode is unavailable: smolvm 1.0.4 hostname allowlists permit direct-IP and unlisted-host egress; explicitly select full networking to proceed")
	default:
		return errors.New("HERMES_BOX_NETWORK_MODE must be strict, full, or none")
	}
}

func (a *App) createFromArtifact(
	ctx context.Context,
	name, artifact string,
	port int,
	restoreMode bool,
) (resultErr error) {
	args, err := a.runtimeCreateArgs(name, artifact, port)
	if err != nil {
		return err
	}
	createArgs := append(
		[]string{"SMOLVM_PACK_CACHE_MAX_BYTES=17179869184", "smolvm"},
		args...,
	)
	if restoreMode {
		createArgs = append(createArgs, "-e", "HERMES_BOX_RESTORE_MODE=true")
	}
	if err := a.runner.Run(ctx, process.Spec{
		Name:   "env",
		Args:   createArgs,
		Stdout: a.stdout,
		Stderr: a.stderr,
	}); err != nil {
		return err
	}
	cleanupCreated := true
	defer func() {
		if !cleanupCreated || resultErr == nil {
			return
		}
		if cleanupErr := a.deleteMachineForCleanup(name); cleanupErr != nil {
			resultErr = fmt.Errorf("%w; cleanup of newly created machine %s also failed: %v", resultErr, name, cleanupErr)
		}
	}()
	dataDir, err := a.output(ctx, "smolvm", "machine", "data-dir", "--name", name)
	if err != nil {
		return fmt.Errorf("locate machine data for %s: %w", name, err)
	}
	marker := filepath.Join(trimOutput(dataDir), "pack", ".smolvm-extracted")
	if _, err := os.Stat(marker); err != nil {
		return fmt.Errorf("verify packed layers for %s: %w", name, err)
	}
	if err := a.run(ctx, "smolvm", "machine", "update", "--name", name, "--net"); err != nil {
		return err
	}
	cleanupCreated = false
	return nil
}

func (a *App) deleteMachineForCleanup(name string) error {
	return a.deleteMachineForCleanupWithin(name, 30*time.Second, 30*time.Second)
}

func (a *App) deleteMachineForCleanupWithin(name string, stopTimeout, deleteTimeout time.Duration) error {
	stopCtx, stopCancel := context.WithTimeout(context.Background(), stopTimeout)
	_ = a.runQuiet(stopCtx, "smolvm", "machine", "stop", "--name", name)
	stopCancel()

	deleteCtx, deleteCancel := context.WithTimeout(context.Background(), deleteTimeout)
	defer deleteCancel()
	return a.runQuiet(deleteCtx, "smolvm", "machine", "delete", "--name", name, "--force")
}

func (a *App) createBlankMachine(ctx context.Context, name string, port int) error {
	if _, err := os.Stat(a.baseArtifact); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("restore requires the local base artifact: %s", a.baseArtifact)
		}
		return err
	}
	return a.createFromArtifact(
		ctx,
		name,
		a.baseArtifact,
		port,
		true,
	)
}

func findFreePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("find free port: %w", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func sleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func temporaryPath(base string) string {
	return fmt.Sprintf("%s.tmp-%d", base, os.Getpid())
}

func cleanPath(path string) {
	_ = os.Remove(path)
}

func ensurePrivateFile(path string) error {
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure %s: %w", filepath.Base(path), err)
	}
	return nil
}
