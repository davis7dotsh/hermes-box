package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/davis7dotsh/hermes-box/internal/process"
)

func (a *App) cmdInit(ctx context.Context, args []string) error {
	if err := requireNoArgs("init", args); err != nil {
		return err
	}
	if err := a.validateNetworkMode(); err != nil {
		return err
	}
	if a.machineExists(ctx, a.config.MachineName) {
		return fmt.Errorf("runtime machine already exists: %s", a.config.MachineName)
	}
	if a.machineExists(ctx, a.config.BuilderName) {
		return fmt.Errorf("builder machine already exists: %s", a.config.BuilderName)
	}
	if err := a.ensureKey(ctx); err != nil {
		return err
	}

	builderCreated := false
	defer func() {
		if builderCreated {
			_ = a.stopNamedMachine(context.Background(), a.config.BuilderName)
			_ = a.runQuiet(context.Background(), "smolvm", "machine", "delete", "--name", a.config.BuilderName, "--force")
		}
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
	); err != nil {
		return err
	}
	builderCreated = true
	if err := a.run(ctx, "smolvm", "machine", "start", "--name", a.config.BuilderName); err != nil {
		return err
	}

	files := [][2]string{
		{filepath.Join(a.root, "guest", "bootstrap.sh"), "/tmp/hermes-box-bootstrap.sh"},
		{filepath.Join(a.root, "guest", "install-node.sh"), "/tmp/hermes-box-install-node.sh"},
		{filepath.Join(a.root, "guest", "start.sh"), "/tmp/hermes-box-start.sh"},
		{filepath.Join(a.root, "guest", "workspace-seed.sh"), "/tmp/hermes-box-workspace-seed.sh"},
		{filepath.Join(a.root, "guest", "boxadmin.bash_profile"), "/tmp/hermes-box-boxadmin.bash_profile"},
		{filepath.Join(a.root, "guest", "hermes-box.sudoers"), "/tmp/hermes-box-sudoers"},
		{filepath.Join(a.root, "guest", "supervisord.conf"), "/tmp/hermes-box-supervisord.conf"},
		{a.sshKey + ".pub", "/tmp/hermes-box-authorized-key.pub"},
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

	if err := a.createFromArtifact(ctx, a.config.MachineName, a.baseArtifact, a.config.SSHPort); err != nil {
		return err
	}
	if err := a.clearKnownHost(ctx, a.config.SSHPort); err != nil {
		return err
	}
	if err := a.startNamedMachine(ctx, a.config.MachineName, a.config.SSHPort); err != nil {
		return err
	}
	if err := a.run(ctx, "smolvm", "machine", "delete", "--name", a.config.BuilderName, "--force"); err != nil {
		return err
	}
	builderCreated = false

	a.log("initialized and running")
	a.log("next: %s ssh", filepath.Join(a.root, "bin", "hermes-box"))
	return nil
}

func (a *App) cmdStart(ctx context.Context, args []string) error {
	if err := requireNoArgs("start", args); err != nil {
		return err
	}
	if !a.machineExists(ctx, a.config.MachineName) {
		return errors.New("run init first")
	}
	running, err := a.isRunning(ctx, a.config.MachineName)
	if err != nil {
		return err
	}
	if running {
		if err := a.verifyLoopbackListener(ctx, a.config.SSHPort); err != nil {
			return fmt.Errorf("running machine does not have a safe SSH listener: %w", err)
		}
		if err := a.verifySSH(ctx, a.config.SSHPort); err != nil {
			return err
		}
		a.log("already running")
		return nil
	}
	return a.startNamedMachine(ctx, a.config.MachineName, a.config.SSHPort)
}

func (a *App) cmdStop(ctx context.Context, args []string) error {
	if err := requireNoArgs("stop", args); err != nil {
		return err
	}
	if !a.machineExists(ctx, a.config.MachineName) {
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
	if !a.machineExists(ctx, a.config.MachineName) {
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
	if !a.machineExists(ctx, a.config.MachineName) {
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
	if !a.machineExists(ctx, a.config.MachineName) {
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
	if !a.machineExists(ctx, a.config.MachineName) {
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
		if !a.machineExists(ctx, name) {
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
