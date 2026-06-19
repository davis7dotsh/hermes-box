package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/davis7dotsh/hermes-box/internal/config"
	"github.com/davis7dotsh/hermes-box/internal/process"
)

type App struct {
	root   string
	config config.Config
	runner process.Runner
	stdout io.Writer
	stderr io.Writer

	imagesDir     string
	backupsDir    string
	stateDir      string
	baseOutput    string
	baseArtifact  string
	sshKey        string
	sshPublicKey  string
	externalKey   bool
	knownHosts    string
	secretEnvFile string
	lockPath      string
}

func New(root string, cfg config.Config, runner process.Runner, stdout, stderr io.Writer) *App {
	dataRoot := root
	if cfg.DataDir != "" {
		dataRoot = cfg.DataDir
	}
	imagesDir := filepath.Join(dataRoot, "images")
	stateDir := filepath.Join(dataRoot, "state")
	sshKey := filepath.Join(stateDir, "hermes-box-ed25519")
	if cfg.SSHKey != "" {
		sshKey = cfg.SSHKey
	}
	return &App{
		root:          root,
		config:        cfg,
		runner:        runner,
		stdout:        stdout,
		stderr:        stderr,
		imagesDir:     imagesDir,
		backupsDir:    filepath.Join(dataRoot, "backups"),
		stateDir:      stateDir,
		baseOutput:    filepath.Join(imagesDir, "hermes-base"),
		baseArtifact:  filepath.Join(imagesDir, "hermes-base.smolmachine"),
		sshKey:        sshKey,
		sshPublicKey:  filepath.Join(stateDir, "hermes-box-ed25519.pub"),
		externalKey:   cfg.SSHKey != "",
		knownHosts:    filepath.Join(stateDir, "known_hosts"),
		secretEnvFile: filepath.Join(root, "secret-env.txt"),
		lockPath:      filepath.Join(stateDir, "operation.lock"),
	}
}

func FindProjectRoot() (string, error) {
	if root := os.Getenv("HERMES_BOX_PROJECT_ROOT"); root != "" {
		return filepath.EvalSymlinks(root)
	}
	if cwd, err := os.Getwd(); err == nil && projectRootLooksValid(cwd) {
		return filepath.EvalSymlinks(cwd)
	}
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("find executable: %w", err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return "", fmt.Errorf("resolve executable: %w", err)
	}
	root := filepath.Dir(filepath.Dir(executable))
	if !projectRootLooksValid(root) {
		return "", errors.New("could not locate Hermes Box project root")
	}
	return root, nil
}

func projectRootLooksValid(root string) bool {
	for _, name := range []string{"Smolfile", "guest", "images", "state"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			return false
		}
	}
	return true
}

func (a *App) Run(ctx context.Context, args []string) error {
	command := "help"
	if len(args) > 0 {
		command = args[0]
		args = args[1:]
	}

	switch command {
	case "init":
		return a.withLock(func() error { return a.cmdInit(ctx, args) })
	case "start":
		return a.withLock(func() error { return a.cmdStart(ctx, args) })
	case "stop":
		return a.withLock(func() error { return a.cmdStop(ctx, args) })
	case "restart":
		return a.withLock(func() error { return a.cmdRestart(ctx, args) })
	case "ssh":
		return a.cmdSSH(ctx, args)
	case "shell":
		return a.cmdShell(ctx, args)
	case "status":
		return a.cmdStatus(ctx, args)
	case "logs":
		return a.cmdLogs(ctx, args)
	case "executor":
		return a.cmdExecutor(ctx, args)
	case "snapshot":
		return a.withLock(func() error { return a.cmdSnapshot(ctx, args) })
	case "package":
		return a.withLock(func() error { return a.cmdPackage(ctx, args) })
	case "restore":
		return a.withLock(func() error { return a.cmdRestore(ctx, args) })
	case "destroy":
		return a.withLock(func() error { return a.cmdDestroy(ctx, args) })
	case "help", "-h", "--help":
		if len(args) != 0 {
			return errors.New("help takes no arguments")
		}
		a.usage()
		return nil
	default:
		return fmt.Errorf("unknown command: %s", command)
	}
}

func (a *App) log(format string, args ...any) {
	fmt.Fprintf(a.stderr, "[hermes-box] "+format+"\n", args...)
}

func (a *App) prepareDirs() error {
	for _, directory := range []string{a.imagesDir, a.backupsDir, a.stateDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", directory, err)
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return fmt.Errorf("secure %s: %w", directory, err)
		}
	}
	return nil
}

func (a *App) withLock(fn func() error) error {
	if err := a.prepareDirs(); err != nil {
		return err
	}
	lock, err := os.OpenFile(a.lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("acquire operation lock: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("another Hermes Box operation is in progress (%s)", a.lockPath)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	return fn()
}

func (a *App) run(ctx context.Context, name string, args ...string) error {
	return a.runner.Run(ctx, process.Spec{
		Name: name, Args: args, Stdout: a.stdout, Stderr: a.stderr,
	})
}

func (a *App) runQuiet(ctx context.Context, name string, args ...string) error {
	return a.runner.Run(ctx, process.Spec{
		Name: name, Args: args, Stdout: io.Discard, Stderr: io.Discard,
	})
}

func (a *App) output(ctx context.Context, name string, args ...string) ([]byte, error) {
	return a.runner.Output(ctx, process.Spec{Name: name, Args: args, Stderr: a.stderr})
}

func (a *App) usage() {
	fmt.Fprintf(a.stdout, `Usage: hermes-box COMMAND [ARGS]

Commands:
  init                     Build the base and create the runtime box
  start                    Start and verify the box
  stop                     Gracefully stop Hermes, then stop the box
  restart                  Stop and start the box
  ssh [COMMAND...]         SSH as boxadmin on 127.0.0.1:%d
  shell                    Open the out-of-band root console
  status                   Show VM, Supervisor, and workspace status
  logs [-f] [-n LINES]     Show or follow Hermes gateway logs
  executor SUBCOMMAND      Manage the local Executor service
  snapshot [LABEL]         Archive rootfs + workspace, checksum, then resume
  package [LABEL]          Snapshot and create a portable restore archive
  package --snapshot BACKUP [LABEL]
                           Package an existing verified snapshot
  restore BACKUP           Verify in a temporary box, then restore safely
  destroy --force          Delete VMs but retain keys, images, and backups
  help                     Show this help
`, a.config.SSHPort)
}

func requireNoArgs(command string, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("%s takes no arguments", command)
	}
	return nil
}

func goos() string {
	return runtime.GOOS
}

func trimOutput(output []byte) string {
	return strings.TrimSpace(string(output))
}
