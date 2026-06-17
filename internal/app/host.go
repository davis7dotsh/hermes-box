package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/davis7dotsh/hermes-box/internal/process"
)

type machineStatus struct {
	State string `json:"state"`
}

func (a *App) machineExists(ctx context.Context, name string) bool {
	err := a.runner.Run(ctx, process.Spec{
		Name:   "smolvm",
		Args:   []string{"machine", "status", "--name", name},
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	return err == nil
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

func waitForPort(ctx context.Context, port, attempts int) error {
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	dialer := net.Dialer{Timeout: 250 * time.Millisecond}
	for range attempts {
		connection, err := dialer.DialContext(ctx, "tcp", address)
		if err == nil {
			connection.Close()
			return nil
		}
		if err := sleep(ctx, 500*time.Millisecond); err != nil {
			return err
		}
	}
	return fmt.Errorf("port %d did not become ready", port)
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
		return fmt.Errorf("no listener found on SSH port %d", port)
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
			return fmt.Errorf("could not parse SSH listener %q", endpoint)
		}
		if endpointPort != strconv.Itoa(port) || host != "127.0.0.1" && host != "::1" {
			return fmt.Errorf("unsafe listener detected: %s", endpoint)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read listener data: %w", err)
	}
	if !found {
		return fmt.Errorf("no listener found on SSH port %d", port)
	}
	return nil
}

func (a *App) sshArgs(port int, remoteArgs ...string) []string {
	args := []string{
		"-i", a.sshKey,
		"-p", strconv.Itoa(port),
		"-o", "IdentitiesOnly=yes",
		"-o", "ForwardAgent=no",
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
test "$(stat -c %U /workspace/hermes-home)" = hermes
supervisorctl status sshd | grep -q RUNNING
supervisorctl status hermes | grep -q RUNNING
sudo -iu hermes env HERMES_HOME=/workspace/hermes-home hermes --version
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

func (a *App) waitForGuest(ctx context.Context, name string, port, attempts int) error {
	var lastErr error
	for range attempts {
		if err := a.verifyGuest(ctx, name); err == nil {
			if err := a.verifySSH(ctx, port); err == nil {
				return nil
			} else {
				lastErr = err
			}
		} else {
			lastErr = err
		}
		if err := sleep(ctx, 500*time.Millisecond); err != nil {
			return err
		}
	}
	if lastErr == nil {
		lastErr = errors.New("guest did not become healthy")
	}
	return fmt.Errorf("guest %s did not become healthy: %w", name, lastErr)
}

func (a *App) startNamedMachine(ctx context.Context, name string, port int) error {
	if err := a.run(ctx, "smolvm", "machine", "start", "--name", name); err != nil {
		return err
	}
	stopOnFailure := true
	defer func() {
		if stopOnFailure {
			_ = a.runQuiet(context.Background(), "smolvm", "machine", "stop", "--name", name)
		}
	}()

	if err := waitForPort(ctx, port, 60); err != nil {
		return err
	}
	if err := a.verifyLoopbackListener(ctx, port); err != nil {
		return err
	}
	if err := a.waitForGuest(ctx, name, port, 240); err != nil {
		return err
	}
	stopOnFailure = false
	return nil
}

func (a *App) stopNamedMachine(ctx context.Context, name string) error {
	if !a.machineExists(ctx, name) {
		return nil
	}
	running, err := a.isRunning(ctx, name)
	if err != nil {
		return err
	}
	if !running {
		return nil
	}
	_ = a.runQuiet(ctx, "smolvm", "machine", "exec", "--name", name, "--", "supervisorctl", "stop", "hermes")
	return a.run(ctx, "smolvm", "machine", "stop", "--name", name)
}

func (a *App) ensureKey(ctx context.Context) error {
	if _, err := os.Stat(a.sshKey); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect SSH key: %w", err)
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
	return os.Chmod(a.sshKey, 0o600)
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
		"--net",
	}
	secrets, err := readSecretMappings(a.secretEnvFile)
	if err != nil {
		return nil, err
	}
	for _, secret := range secrets {
		args = append(args, "--secret-env", secret)
	}
	return args, nil
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

func (a *App) createFromArtifact(ctx context.Context, name, artifact string, port int) error {
	args, err := a.runtimeCreateArgs(name, artifact, port)
	if err != nil {
		return err
	}
	if err := a.run(ctx, "smolvm", args...); err != nil {
		return err
	}
	return a.run(ctx, "smolvm", "machine", "update", "--name", name, "--net")
}

func (a *App) createBlankMachine(ctx context.Context, name string, port int) error {
	if _, err := os.Stat(a.baseArtifact); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("restore requires the local base artifact: %s", a.baseArtifact)
		}
		return err
	}
	return a.createFromArtifact(ctx, name, a.baseArtifact, port)
}

func readSecretMappings(path string) ([]string, error) {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open secret mappings: %w", err)
	}
	defer file.Close()

	var mappings []string
	scanner := bufio.NewScanner(file)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(strings.SplitN(scanner.Text(), "#", 2)[0])
		if line == "" {
			continue
		}
		guest, host, ok := strings.Cut(line, "=")
		if !ok || !validEnvironmentName(strings.TrimSpace(guest)) || !validEnvironmentName(strings.TrimSpace(host)) {
			return nil, fmt.Errorf("%s:%d: expected GUEST_VARIABLE=HOST_VARIABLE", path, lineNumber)
		}
		mappings = append(mappings, strings.TrimSpace(guest)+"="+strings.TrimSpace(host))
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read secret mappings: %w", err)
	}
	return mappings, nil
}

func validEnvironmentName(value string) bool {
	if value == "" || !(value[0] == '_' || value[0] >= 'A' && value[0] <= 'Z' || value[0] >= 'a' && value[0] <= 'z') {
		return false
	}
	for _, character := range value[1:] {
		if character != '_' &&
			(character < 'A' || character > 'Z') &&
			(character < 'a' || character > 'z') &&
			(character < '0' || character > '9') {
			return false
		}
	}
	return true
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
