package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/davis7dotsh/hermes-box/internal/config"
	"github.com/davis7dotsh/hermes-box/internal/process"
)

func (a *App) cmdInit(ctx context.Context, args []string) (resultErr error) {
	if err := requireNoArgs("init", args); err != nil {
		return err
	}
	if err := a.preflightInit(ctx, exec.LookPath, runtime.GOOS, runtime.GOARCH); err != nil {
		return fmt.Errorf("init preflight: %w", err)
	}
	if err := a.ensureKey(ctx); err != nil {
		return err
	}

	builderCreated := false
	defer func() {
		resultErr = a.finishInitBuilderCleanup(resultErr, builderCreated)
	}()

	a.log("creating temporary builder with unrestricted build-time networking")
	if err := a.run(
		ctx,
		"smolvm",
		"machine", "create",
		"--name", a.config.BuilderName,
		"--image", "ubuntu:24.04",
		"--cpus", strconv.Itoa(a.config.CPUs),
		"--mem", strconv.Itoa(a.config.MemoryMiB),
		"--storage", strconv.Itoa(a.config.StorageGB),
		"--overlay", strconv.Itoa(a.config.OverlayGB),
		"--net",
		"--net-backend", "virtio-net",
	); err != nil {
		return err
	}
	builderCreated = true
	// smolvm 1.0.4 can record network=true from machine create --net while
	// booting the guest without a NIC. Reapply the setting before the first
	// boot, as the runtime creation path already does.
	if err := a.run(ctx, "smolvm", "machine", "update", "--name", a.config.BuilderName, "--net"); err != nil {
		return err
	}
	if err := a.run(ctx, "smolvm", "machine", "start", "--name", a.config.BuilderName); err != nil {
		return err
	}
	if err := a.ensureBuilderNetwork(ctx, a.config.BuilderName); err != nil {
		return err
	}

	files := [][2]string{
		{filepath.Join(a.root, "guest", "bootstrap.sh"), "/tmp/hermes-box-bootstrap.sh"},
		{filepath.Join(a.root, "guest", "install-node.sh"), "/tmp/hermes-box-install-node.sh"},
		{filepath.Join(a.root, "guest", "start.sh"), "/tmp/hermes-box-start.sh"},
		{filepath.Join(a.root, "guest", "entrypoint.sh"), "/tmp/hermes-box-entrypoint.sh"},
		{filepath.Join(a.root, "guest", "executor.sh"), "/tmp/hermes-box-executor.sh"},
		{filepath.Join(a.root, "guest", "extract-executor.py"), "/tmp/hermes-box-extract-executor.py"},
		{filepath.Join(a.root, "guest", "hermes_gated_approval.py"), "/tmp/hermes-box-hermes-gated-approval.py"},
		{filepath.Join(a.root, "guest", "patch-hermes-gated-approval.py"), "/tmp/hermes-box-patch-hermes-gated-approval.py"},
		{filepath.Join(a.root, "tests", "hermes-gated-approval.py"), "/tmp/hermes-box-test-hermes-gated-approval.py"},
		{filepath.Join(a.root, "guest", "workspace-seed.sh"), "/tmp/hermes-box-workspace-seed.sh"},
		{filepath.Join(a.root, "guest", "boxadmin.bash_profile"), "/tmp/hermes-box-boxadmin.bash_profile"},
		{filepath.Join(a.root, "guest", "hermes-box.sudoers"), "/tmp/hermes-box-sudoers"},
		{filepath.Join(a.root, "guest", "supervisord.conf"), "/tmp/hermes-box-supervisord.conf"},
		{a.sshPublicKey, "/tmp/hermes-box-authorized-key.pub"},
	}
	for _, file := range files {
		if err := a.run(ctx, "smolvm", "machine", "cp", file[0], a.config.BuilderName+":"+file[1]); err != nil {
			return err
		}
	}

	execArgs := []string{
		"machine", "exec",
		"--stream",
		"--name", a.config.BuilderName,
	}
	if a.config.HermesCommit != "" {
		execArgs = append(execArgs, "-e", "HERMES_INSTALL_COMMIT="+a.config.HermesCommit)
	}
	execArgs = append(
		execArgs,
		"-e", "HERMES_BOX_EXECUTOR_ENABLED="+strconv.FormatBool(a.config.ExecutorEnabled),
	)
	execArgs = append(execArgs, "--", "bash", "/tmp/hermes-box-bootstrap.sh", "/tmp/hermes-box-authorized-key.pub")
	if err := a.run(ctx, "smolvm", execArgs...); err != nil {
		return err
	}
	if err := a.run(ctx, "smolvm", "machine", "stop", "--name", a.config.BuilderName); err != nil {
		return err
	}

	tempOutput := temporaryPath(a.baseOutput)
	tempArtifact := tempOutput + ".smolmachine"
	cleanPath(tempOutput)
	cleanPath(tempArtifact)
	defer cleanPath(tempOutput)
	defer cleanPath(tempArtifact)

	if err := a.run(
		ctx,
		"smolvm",
		"pack", "create",
		"--from-vm", a.config.BuilderName,
		"--smolfile", filepath.Join(a.root, "Smolfile"),
		"--output", tempOutput,
	); err != nil {
		return err
	}
	cleanPath(tempOutput)
	if err := ensurePrivateFile(tempArtifact); err != nil {
		return err
	}
	if err := os.Rename(tempArtifact, a.baseArtifact); err != nil {
		return fmt.Errorf("install base artifact: %w", err)
	}

	if err := a.createFromArtifact(ctx, a.config.MachineName, a.baseArtifact, a.config.SSHPort, false); err != nil {
		return err
	}
	cleanupRuntime := true
	defer func() {
		if cleanupRuntime {
			_ = a.deleteMachineForCleanup(a.config.MachineName)
		}
	}()
	keepRuntime, err := a.startFirstRuntime(ctx)
	if keepRuntime {
		cleanupRuntime = false
	}
	if err != nil {
		return err
	}
	if err := a.deleteMachineForCleanup(a.config.BuilderName); err != nil {
		a.log("warning: runtime is healthy, but the disposable builder cleanup failed; retrying cleanup: %v", err)
	} else {
		builderCreated = false
	}

	a.printInitHandoff()
	return nil
}

func (a *App) finishInitBuilderCleanup(resultErr error, builderCreated bool) error {
	if !builderCreated {
		return resultErr
	}
	if cleanupErr := a.deleteMachineForCleanup(a.config.BuilderName); cleanupErr != nil {
		return errors.Join(
			resultErr,
			fmt.Errorf("clean up disposable builder %q: %w", a.config.BuilderName, cleanupErr),
		)
	}
	return resultErr
}

func (a *App) startFirstRuntime(ctx context.Context) (bool, error) {
	if err := a.clearKnownHost(ctx, a.config.SSHPort); err != nil {
		return false, err
	}
	if err := a.startNamedMachine(ctx, a.config.MachineName, a.config.SSHPort); err != nil {
		command := filepath.Join(a.root, "bin", "hermes-box")
		return true, fmt.Errorf(`first runtime start failed; preserved machine %q so completed Codex setup and verified Executor blobs are retained

Review the startup and service diagnostics above, then retry and inspect:
  %s start
  %s status
  %s logs

The preserved machine may contain injected secrets or partial private state.
To discard it and rebuild from scratch instead:
  %s destroy --force
  %s init

startup error: %w`,
			a.config.MachineName,
			command,
			command,
			command,
			command,
			command,
			err,
		)
	}
	return true, nil
}

func (a *App) preflightInit(
	ctx context.Context,
	lookPath func(string) (string, error),
	hostOS, hostArch string,
) error {
	if err := a.validateSupportedHost(ctx, lookPath, hostOS, hostArch); err != nil {
		return err
	}
	if err := a.validateNetworkMode(); err != nil {
		return err
	}
	runtimeExists, err := a.machineExists(ctx, a.config.MachineName)
	if err != nil {
		return err
	}
	if runtimeExists {
		return fmt.Errorf(
			"runtime machine %q already exists; choose another HERMES_BOX_MACHINE_NAME or manage the existing box",
			a.config.MachineName,
		)
	}
	builderExists, err := a.machineExists(ctx, a.config.BuilderName)
	if err != nil {
		return err
	}
	if builderExists {
		return fmt.Errorf(
			"builder machine %q already exists; choose another HERMES_BOX_BUILDER_NAME or remove it explicitly",
			a.config.BuilderName,
		)
	}
	secretMappings, err := config.ReadSecretMappings(a.secretEnvFile)
	if err != nil {
		return err
	}
	if err := config.ValidateSecretMappingEnvironment(secretMappings, os.LookupEnv); err != nil {
		return err
	}
	for _, directory := range []string{a.imagesDir, a.backupsDir, a.stateDir} {
		if err := verifyDirectoryWritable(directory); err != nil {
			return err
		}
	}
	if err := verifyPortAvailable(a.config.SSHPort); err != nil {
		return fmt.Errorf("SSH port: %w", err)
	}
	if a.config.ExecutorEnabled {
		if err := verifyPortAvailable(a.config.ExecutorPort); err != nil {
			return fmt.Errorf("Executor port: %w", err)
		}
	}
	return nil
}

func (a *App) validateCurrentHost(ctx context.Context) error {
	return a.validateSupportedHost(ctx, exec.LookPath, runtime.GOOS, runtime.GOARCH)
}

func (a *App) validateSupportedHost(
	ctx context.Context,
	lookPath func(string) (string, error),
	hostOS, hostArch string,
) error {
	if err := validateInitHost(hostOS, hostArch); err != nil {
		return err
	}
	if err := validateHostCommands(lookPath); err != nil {
		return err
	}
	return a.validateHostVersions(ctx)
}

func validateHostCommands(lookPath func(string) (string, error)) error {
	for _, command := range []string{"go", "smolvm", "ssh", "ssh-keygen", "lsof"} {
		if _, err := lookPath(command); err != nil {
			switch command {
			case "go":
				return fmt.Errorf(
					"required host command %q was not found in PATH; install Go 1.24 or newer from https://go.dev/dl/ and rerun the same init command",
					command,
				)
			case "smolvm":
				return fmt.Errorf(
					"required host command %q was not found in PATH; install the official smolvm v1.0.4 Darwin ARM64 release from https://github.com/smol-machines/smolvm/releases/tag/v1.0.4 and rerun the same init command",
					command,
				)
			default:
				return fmt.Errorf(
					"required host command %q was not found in PATH; install it and rerun the same init command",
					command,
				)
			}
		}
	}
	return nil
}

func (a *App) validateHostVersions(ctx context.Context) error {
	goCtx, goCancel := context.WithTimeout(ctx, 10*time.Second)
	goOutput, err := a.output(goCtx, "go", "version")
	goCancel()
	if err != nil {
		return fmt.Errorf("read Go version: %w", err)
	}
	if err := validateGoVersion(string(goOutput)); err != nil {
		return err
	}

	smolVMCtx, smolVMCancel := context.WithTimeout(ctx, 10*time.Second)
	smolVMOutput, err := a.output(smolVMCtx, "smolvm", "--version")
	smolVMCancel()
	if err != nil {
		return fmt.Errorf("read smolvm version: %w", err)
	}
	return validateSmolVMVersion(string(smolVMOutput))
}

func validateGoVersion(output string) error {
	fields := strings.Fields(output)
	if len(fields) < 3 || fields[0] != "go" || fields[1] != "version" ||
		!strings.HasPrefix(fields[2], "go") {
		return fmt.Errorf("could not parse Go version output %q", strings.TrimSpace(output))
	}
	parts := strings.Split(strings.TrimPrefix(fields[2], "go"), ".")
	if len(parts) < 2 {
		return fmt.Errorf("could not parse Go version output %q", strings.TrimSpace(output))
	}
	major, majorErr := strconv.Atoi(parts[0])
	minor, minorErr := strconv.Atoi(parts[1])
	if majorErr != nil || minorErr != nil {
		return fmt.Errorf("could not parse Go version output %q", strings.TrimSpace(output))
	}
	if major < 1 || major == 1 && minor < 24 {
		return fmt.Errorf(
			"Hermes Box requires Go 1.24 or newer; found %s; install a supported release from https://go.dev/dl/ and rerun the same init command",
			fields[2],
		)
	}
	return nil
}

func validateSmolVMVersion(output string) error {
	version := strings.TrimSpace(output)
	if version != "smolvm 1.0.4" {
		return fmt.Errorf(
			"Hermes Box requires exactly smolvm 1.0.4; found %q; install the official v1.0.4 Darwin ARM64 release from https://github.com/smol-machines/smolvm/releases/tag/v1.0.4 and rerun the same init command",
			version,
		)
	}
	return nil
}

func validateInitHost(hostOS, hostArch string) error {
	if hostOS != "darwin" || hostArch != "arm64" {
		return fmt.Errorf("Hermes Box requires macOS ARM64; found %s/%s", hostOS, hostArch)
	}
	return nil
}

func (a *App) printInitHandoff() {
	command := filepath.Join(a.root, "bin", "hermes-box")
	fmt.Fprintf(a.stderr, `
[hermes-box] initialized and running

Box endpoints:
  machine:  %s
  SSH:      boxadmin@127.0.0.1:%d
`, a.config.MachineName, a.config.SSHPort)
	if a.config.ExecutorEnabled {
		fmt.Fprintf(a.stderr, "  Executor: http://127.0.0.1:%d\n", a.config.ExecutorPort)
	}
	fmt.Fprintf(a.stderr, `
Guest phase -- finish the human sign-ins:
  %s ssh
  hermes auth add openai-codex --type oauth     # reviewer + reusable Codex OAuth
  hermes model                                  # select inference; reuses Codex OAuth
  codex login --device-auth                     # separate Codex CLI account

Keep a Codex debugging session alive:
  tmux new -As codex
  codex
  # detach with Ctrl-b d; run tmux new -As codex to reattach
  exit                                          # return to the host
`, command)
	if a.config.ExecutorEnabled {
		fmt.Fprintf(a.stderr, `
Host phase -- finish Executor setup in this order:
  1. %s executor open
  2. In the portal, create the admin account, configure provider integrations/OAuth,
     and create a destination-local API key.
  3. %s executor auth set
  4. %s executor connect-hermes
  5. %s executor status
  Provider setup guide: %s
`, command, command, command, command, filepath.Join(a.root, "EXECUTOR_CONNECTIONS.md"))
	} else {
		fmt.Fprintln(a.stderr, "\nExecutor was intentionally omitted for this lean box. Adding it requires recreating this machine with HERMES_BOX_EXECUTOR_ENABLED=true before init.")
	}
	fmt.Fprintf(a.stderr, `
Host phase -- create the first portable backup after sign-in:
  %s package configured-agent
  Copy both the emitted .tar archive and its adjacent .sha256 file to encrypted,
  off-host storage. The archive contains credentials and private agent state.
  Portable packages exclude SSH keys. Store an encrypted external copy of exactly:
  %s
  Restores require this same private key.

Host operations and debugging:
  %s stop
  %s start
  %s status
  %s logs -f
  %s shell
  Guest startup log: /var/log/hermes-box-startup.log
`, command, a.sshKey, command, command, command, command, command)
}

func (a *App) cmdStart(ctx context.Context, args []string) error {
	if err := requireNoArgs("start", args); err != nil {
		return err
	}
	exists, err := a.machineExists(ctx, a.config.MachineName)
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("run init first")
	}
	running, err := a.isRunning(ctx, a.config.MachineName)
	if err != nil {
		return err
	}
	if running {
		verificationCtx, cancel := context.WithTimeout(ctx, a.startupDeadline())
		defer cancel()
		verified := false
		defer func() {
			if verified {
				return
			}
			diagnosticCtx, diagnosticCancel := context.WithTimeout(
				context.Background(),
				15*time.Second,
			)
			defer diagnosticCancel()
			a.captureStartupDiagnostics(diagnosticCtx, a.config.MachineName)
		}()
		if err := waitForPort(verificationCtx, a.config.SSHPort); err != nil {
			return err
		}
		if err := a.verifyLoopbackListener(verificationCtx, a.config.SSHPort); err != nil {
			return fmt.Errorf("running machine does not have a safe SSH listener: %w", err)
		}
		if a.config.ExecutorEnabled {
			if err := waitForPort(verificationCtx, a.config.ExecutorPort); err != nil {
				return err
			}
			if err := a.verifyLoopbackListener(verificationCtx, a.config.ExecutorPort); err != nil {
				return fmt.Errorf("running machine does not have a safe Executor listener: %w", err)
			}
		}
		if err := a.completeStartupVerification(
			verificationCtx,
			a.config.MachineName,
			a.config.SSHPort,
		); err != nil {
			return err
		}
		verified = true
		a.log("already running and fully verified")
		return nil
	}
	return a.startNamedMachine(ctx, a.config.MachineName, a.config.SSHPort)
}

func (a *App) cmdStop(ctx context.Context, args []string) error {
	if err := requireNoArgs("stop", args); err != nil {
		return err
	}
	exists, err := a.machineExists(ctx, a.config.MachineName)
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("machine does not exist")
	}
	return a.stopNamedMachine(ctx, a.config.MachineName)
}

func (a *App) cmdRestart(ctx context.Context, args []string) error {
	if err := requireNoArgs("restart", args); err != nil {
		return err
	}
	if err := a.cmdStop(ctx, nil); err != nil {
		return err
	}
	return a.cmdStart(ctx, nil)
}

func (a *App) cmdSSH(ctx context.Context, args []string) error {
	exists, err := a.machineExists(ctx, a.config.MachineName)
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("run init first")
	}
	running, err := a.isRunning(ctx, a.config.MachineName)
	if err != nil {
		return err
	}
	if !running {
		if err := a.withLock(func() error { return a.cmdStart(ctx, nil) }); err != nil {
			return err
		}
	}
	sshArgs := a.sshArgs(a.config.SSHPort, args...)
	for index := 0; index < len(sshArgs); index++ {
		if sshArgs[index] == "BatchMode=yes" {
			sshArgs = append(sshArgs[:index-1], sshArgs[index+1:]...)
			break
		}
	}
	return a.runner.Run(ctx, process.Spec{
		Name:   "ssh",
		Args:   sshArgs,
		Stdin:  os.Stdin,
		Stdout: a.stdout,
		Stderr: a.stderr,
	})
}

func (a *App) cmdShell(ctx context.Context, args []string) error {
	if err := requireNoArgs("shell", args); err != nil {
		return err
	}
	exists, err := a.machineExists(ctx, a.config.MachineName)
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("run init first")
	}
	return a.runner.Run(ctx, process.Spec{
		Name:   "smolvm",
		Args:   []string{"machine", "shell", "--name", a.config.MachineName},
		Stdin:  os.Stdin,
		Stdout: a.stdout,
		Stderr: a.stderr,
	})
}

func (a *App) cmdStatus(ctx context.Context, args []string) error {
	if err := requireNoArgs("status", args); err != nil {
		return err
	}
	exists, err := a.machineExists(ctx, a.config.MachineName)
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("machine does not exist")
	}
	if err := a.run(ctx, "smolvm", "machine", "status", "--name", a.config.MachineName, "--json"); err != nil {
		return err
	}
	running, err := a.isRunning(ctx, a.config.MachineName)
	if err != nil {
		return err
	}
	if !running {
		return nil
	}
	if err := a.verifyLoopbackListener(ctx, a.config.SSHPort); err == nil {
		a.log("SSH listener is loopback-only on port %d", a.config.SSHPort)
	} else {
		return err
	}
	if a.config.ExecutorEnabled {
		if err := a.verifyLoopbackListener(ctx, a.config.ExecutorPort); err != nil {
			a.log("Executor listener is unavailable or unsafe on port %d: %v", a.config.ExecutorPort, err)
		} else if err := a.verifyExecutorHTTP(ctx); err != nil {
			a.log("Executor health check failed on http://localhost:%d: %v", a.config.ExecutorPort, err)
		} else {
			a.log("Executor is healthy on http://localhost:%d", a.config.ExecutorPort)
		}
	}
	_ = a.run(ctx, "smolvm", "machine", "exec", "--name", a.config.MachineName, "--", "supervisorctl", "status")
	_ = a.run(ctx, "smolvm", "machine", "exec", "--name", a.config.MachineName, "--", "df", "-h", "/workspace")
	return nil
}

func (a *App) cmdLogs(ctx context.Context, args []string) error {
	follow := false
	lines := 200
	for len(args) > 0 {
		switch args[0] {
		case "-f", "--follow":
			follow = true
			args = args[1:]
		case "-n", "--lines":
			if len(args) < 2 {
				return errors.New("--lines requires a line count")
			}
			value, err := strconv.Atoi(args[1])
			if err != nil || value < 1 {
				return errors.New("log line count must be a positive integer")
			}
			lines = value
			args = args[2:]
		default:
			return fmt.Errorf("unknown logs option: %s", args[0])
		}
	}
	exists, err := a.machineExists(ctx, a.config.MachineName)
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("machine does not exist")
	}
	running, err := a.isRunning(ctx, a.config.MachineName)
	if err != nil {
		return err
	}
	if !running {
		return errors.New("machine is stopped")
	}

	commandArgs := []string{"machine", "exec"}
	if follow {
		commandArgs = append(commandArgs, "--stream")
	}
	commandArgs = append(
		commandArgs,
		"--name", a.config.MachineName,
		"--",
		"tail", "-n", strconv.Itoa(lines),
	)
	if follow {
		commandArgs = append(commandArgs, "-F")
	}
	commandArgs = append(commandArgs, "/workspace/hermes-home/logs/hermes-gateway.log")
	return a.runner.Run(ctx, process.Spec{
		Name:   "smolvm",
		Args:   commandArgs,
		Stdout: a.stdout,
		Stderr: a.stderr,
	})
}

func (a *App) cmdDestroy(ctx context.Context, args []string) error {
	if len(args) != 1 || args[0] != "--force" {
		return errors.New("destroy requires --force; images and backups are retained")
	}
	for _, name := range []string{a.config.MachineName, a.config.BuilderName} {
		exists, err := a.machineExists(ctx, name)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		if err := a.stopNamedMachine(ctx, name); err != nil {
			return err
		}
		if err := a.run(ctx, "smolvm", "machine", "delete", "--name", name, "--force"); err != nil {
			return err
		}
	}
	if err := os.Remove(a.knownHosts); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove known hosts: %w", err)
	}
	a.log("machines destroyed; images, backups, and SSH key retained")
	return nil
}
