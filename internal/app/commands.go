package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/davis7dotsh/hermes-box/internal/box"
)

var components = map[string]bool{
	"claude": true, "codex": true, "hermes": true, "executor": true, "node": true, "uv": true,
}

func (c *CLI) dispatch(ctx context.Context, def Definition, inv invocation) (any, int, error) {
	switch inv.command {
	case "create":
		return c.create(ctx, def, inv)
	case "start":
		return c.start(ctx, def, inv)
	case "stop":
		return c.stop(ctx, def, inv)
	case "ssh":
		return c.ssh(ctx, def, inv)
	case "exec":
		return c.exec(ctx, def, inv)
	case "status":
		return c.status(ctx, def, inv)
	case "logs":
		return c.logs(ctx, def, inv)
	case "open":
		return c.open(ctx, def, inv)
	case "setup":
		return c.setup(ctx, def, inv)
	case "update":
		return c.update(ctx, def, inv)
	case "rollback":
		return c.rollback(ctx, def, inv)
	case "backup":
		return c.backup(ctx, def, inv)
	case "restore":
		return c.restore(ctx, def, inv)
	case "rebuild":
		return c.rebuild(ctx, def, inv)
	case "doctor":
		return c.doctor(ctx, def, inv)
	case "key":
		return c.key(ctx, def, inv)
	case "destroy":
		return c.destroy(ctx, def, inv)
	case "version":
		return c.version(ctx, def, inv)
	default:
		return nil, -1, apiError("invalid_input", "unknown command: "+inv.command, 2, nil)
	}
}

func (c *CLI) mutate(ctx context.Context, def Definition, command string, fn func() (any, error)) (result any, status int, err error) {
	release, err := c.deps.Locker.Acquire(ctx, def, command)
	if err != nil {
		var busy *box.BusyError
		if errors.As(err, &busy) {
			return nil, -1, apiError("busy", err.Error(), 1, err)
		}
		return nil, -1, err
	}
	defer func() {
		if releaseErr := release(); releaseErr != nil {
			err = errors.Join(err, fmt.Errorf("release operation lock: %w", releaseErr))
		}
	}()
	if err := c.deps.Operations.ResumeInterruptedMutation(ctx, def); err != nil {
		return nil, -1, fmt.Errorf("recover interrupted host operation: %w", err)
	}
	result, err = fn()
	return result, -1, err
}

func (c *CLI) owned(ctx context.Context, def Definition, requireExists bool) (Ownership, error) {
	state, err := c.deps.Operations.Ownership(ctx, def)
	if err != nil {
		return Ownership{}, err
	}
	if state.Exists && !state.Owned {
		err := apiError("already_exists", "refusing to operate on a Lima resource owned by another configuration directory", 1, nil)
		err.Recovery = "choose another name in hermes-box.yaml"
		return Ownership{}, err
	}
	if requireExists && !state.Exists {
		return Ownership{}, apiError("not_found", "the configured box does not exist", 1, nil)
	}
	return state, nil
}

func (c *CLI) preflight(ctx context.Context, def Definition, command string) error {
	if err := c.deps.Operations.Preflight(ctx, def, command); err != nil {
		return apiError("preflight_failed", err.Error(), 1, err)
	}
	return nil
}

func (c *CLI) create(ctx context.Context, def Definition, inv invocation) (any, int, error) {
	if err := requireArgs("create", inv.args, 0, 0); err != nil {
		return nil, -1, err
	}
	return c.mutate(ctx, def, "create", func() (_ any, resultErr error) {
		if err := c.preflight(ctx, def, "create"); err != nil {
			return nil, err
		}
		state, err := c.owned(ctx, def, false)
		if err != nil {
			return nil, err
		}
		if state.Exists {
			return nil, apiError("already_exists", "the configured box already exists", 1, nil)
		}
		created := false
		defer func() {
			if resultErr != nil && created {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				resultErr = errors.Join(resultErr, c.deps.Operations.CleanupCreate(cleanupCtx, def))
			}
		}()
		created = true
		if err := c.deps.Operations.CreateInfrastructure(ctx, def); err != nil {
			return nil, err
		}
		if _, err := c.deps.Operations.Apply(ctx, def, "all"); err != nil {
			return nil, err
		}
		if err := c.deps.Operations.StartServices(ctx, def); err != nil {
			return nil, err
		}
		health, err := c.deps.Operations.Health(ctx, def)
		if err != nil || !health.Healthy {
			if err == nil {
				err = errors.New("initial health check failed")
			}
			return nil, apiError("health_failed", err.Error(), 1, err)
		}
		backup, err := c.deps.Backups.Create(ctx, def, "initial")
		if err != nil {
			return nil, err
		}
		// A verified initial backup is the destructive-cleanup boundary. If only
		// final journal cleanup fails, leave the healthy recoverable box intact;
		// the next mutating invocation will complete that journal.
		created = false
		if err := c.deps.Operations.CompleteCreate(ctx, def); err != nil {
			return nil, err
		}
		if !inv.global.json && !inv.global.quiet {
			fmt.Fprint(c.stdout, firstRunHandoff)
		}
		return map[string]any{
			"state": "running", "healthy": true, "setup_required": health.SetupRequired,
			"components": health.Components, "ports": health.Ports, "backup": backup,
		}, nil
	})
}

func (c *CLI) start(ctx context.Context, def Definition, inv invocation) (any, int, error) {
	if err := requireArgs("start", inv.args, 0, 0); err != nil {
		return nil, -1, err
	}
	return c.mutate(ctx, def, "start", func() (any, error) {
		if err := c.preflight(ctx, def, "start"); err != nil {
			return nil, err
		}
		if _, err := c.owned(ctx, def, true); err != nil {
			return nil, err
		}
		if err := c.deps.Operations.StartVM(ctx, def); err != nil {
			return nil, err
		}
		// Recover only an interrupted root-local activation. Start deliberately
		// never calls Apply and therefore never reconciles the repository lock.
		if err := c.deps.Operations.Recover(ctx, def); err != nil {
			return nil, err
		}
		if err := c.deps.Operations.StartServices(ctx, def); err != nil {
			return nil, err
		}
		health, err := c.deps.Operations.Health(ctx, def)
		if err != nil || !health.Healthy {
			if err == nil {
				err = errors.New("box is degraded")
			}
			return nil, apiError("health_failed", err.Error(), 1, err)
		}
		return map[string]any{
			"state": "running", "healthy": true, "setup_required": health.SetupRequired,
			"components": health.Components, "ports": health.Ports,
		}, nil
	})
}

func (c *CLI) stop(ctx context.Context, def Definition, inv invocation) (any, int, error) {
	if err := requireArgs("stop", inv.args, 0, 0); err != nil {
		return nil, -1, err
	}
	return c.mutate(ctx, def, "stop", func() (any, error) {
		state, err := c.owned(ctx, def, true)
		if err != nil {
			return nil, err
		}
		if !state.Running {
			return map[string]any{"state": "stopped"}, nil
		}
		if err := c.deps.Operations.Recover(ctx, def); err != nil {
			return nil, err
		}
		if err := c.deps.Operations.StopServices(ctx, def); err != nil {
			return nil, err
		}
		if err := c.deps.Operations.SyncData(ctx, def); err != nil {
			return nil, err
		}
		if err := c.deps.Operations.StopVM(ctx, def); err != nil {
			return nil, err
		}
		return map[string]any{"state": "stopped"}, nil
	})
}

func (c *CLI) ssh(ctx context.Context, def Definition, inv invocation) (any, int, error) {
	if err := requireArgs("ssh", inv.args, 0, 0); err != nil {
		return nil, -1, err
	}
	if _, err := c.owned(ctx, def, true); err != nil {
		return nil, -1, err
	}
	status, err := c.deps.Operations.SSH(ctx, def, c.stdin, c.stdout, c.stderr)
	return nil, status, err
}

func (c *CLI) exec(ctx context.Context, def Definition, inv invocation) (any, int, error) {
	args := inv.args
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		return nil, -1, apiError("invalid_input", "exec requires -- COMMAND [ARGS...]", 2, nil)
	}
	if _, err := c.owned(ctx, def, true); err != nil {
		return nil, -1, err
	}
	status, err := c.deps.Operations.Exec(ctx, def, args, c.stdin, c.stdout, c.stderr)
	return nil, status, err
}

func (c *CLI) status(ctx context.Context, def Definition, inv invocation) (any, int, error) {
	check := false
	for _, arg := range inv.args {
		if arg != "--check" || check {
			return nil, -1, apiError("invalid_input", "usage: hermes-box status [--check]", 2, nil)
		}
		check = true
	}
	state, err := c.deps.Operations.Status(ctx, def, check)
	return state, -1, err
}

func (c *CLI) logs(ctx context.Context, def Definition, inv invocation) (any, int, error) {
	target, lines, follow, err := parseLogs(inv.args)
	if err != nil {
		return nil, -1, err
	}
	if _, err := c.owned(ctx, def, true); err != nil {
		return nil, -1, err
	}
	if follow {
		return nil, -1, c.deps.Operations.Logs(ctx, def, target, lines, true, c.stdout, c.stderr)
	}
	var output strings.Builder
	if err := c.deps.Operations.Logs(ctx, def, target, lines, false, &output, c.stderr); err != nil {
		return nil, -1, err
	}
	if !inv.global.json {
		_, err := fmt.Fprint(c.stdout, output.String())
		return nil, -1, err
	}
	return map[string]any{"target": target, "lines": strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")}, -1, nil
}

func (c *CLI) open(ctx context.Context, def Definition, inv invocation) (any, int, error) {
	if err := exactTarget("open", inv.args, "executor"); err != nil {
		return nil, -1, err
	}
	if _, err := c.owned(ctx, def, true); err != nil {
		return nil, -1, err
	}
	url, err := c.deps.Operations.OpenExecutor(ctx, def)
	return map[string]any{"target": "executor", "url": url}, -1, err
}

func (c *CLI) setup(ctx context.Context, def Definition, inv invocation) (any, int, error) {
	tokenStdin := false
	if len(inv.args) == 2 && inv.args[0] == "executor" && inv.args[1] == "--token-stdin" {
		tokenStdin = true
	} else if err := exactTarget("setup", inv.args, "executor"); err != nil {
		return nil, -1, err
	}
	return c.mutate(ctx, def, "setup executor", func() (any, error) {
		if _, err := c.owned(ctx, def, true); err != nil {
			return nil, err
		}
		if err := c.deps.Operations.Recover(ctx, def); err != nil {
			return nil, err
		}
		tools, err := c.deps.Operations.SetupExecutor(ctx, def, c.stdin, tokenStdin)
		return map[string]any{"target": "executor", "connected": err == nil, "tools": tools}, err
	})
}

func (c *CLI) update(ctx context.Context, def Definition, inv invocation) (any, int, error) {
	if err := oneComponent("update", inv.args, true); err != nil {
		return nil, -1, err
	}
	component := inv.args[0]
	return c.mutate(ctx, def, "update "+component, func() (any, error) {
		if err := c.preflight(ctx, def, "update"); err != nil {
			return nil, err
		}
		if _, err := c.owned(ctx, def, true); err != nil {
			return nil, err
		}
		if err := c.deps.Operations.Recover(ctx, def); err != nil {
			return nil, err
		}
		result, err := c.deps.Operations.Apply(ctx, def, component)
		if err != nil {
			return nil, err
		}
		changed := stringSlice(result["changed"])
		components, _ := result["components"].(map[string]any)
		if components == nil {
			components = map[string]any{}
		}
		return map[string]any{"components": components, "changed": changed, "failed": nil}, nil
	})
}

func (c *CLI) rollback(ctx context.Context, def Definition, inv invocation) (any, int, error) {
	if err := oneComponent("rollback", inv.args, false); err != nil {
		return nil, -1, err
	}
	component := inv.args[0]
	return c.mutate(ctx, def, "rollback "+component, func() (any, error) {
		if _, err := c.owned(ctx, def, true); err != nil {
			return nil, err
		}
		if err := c.deps.Operations.Recover(ctx, def); err != nil {
			return nil, err
		}
		result, err := c.deps.Operations.Rollback(ctx, def, component)
		if err != nil {
			return nil, err
		}
		return result, nil
	})
}

func (c *CLI) backup(ctx context.Context, def Definition, inv invocation) (any, int, error) {
	if err := requireArgs("backup", inv.args, 0, 1); err != nil {
		return nil, -1, err
	}
	label := "manual"
	if len(inv.args) == 1 {
		label = inv.args[0]
	}
	return c.mutate(ctx, def, "backup", func() (any, error) {
		if _, err := c.owned(ctx, def, true); err != nil {
			return nil, err
		}
		if err := c.deps.Operations.Recover(ctx, def); err != nil {
			return nil, err
		}
		return c.deps.Backups.Create(ctx, def, label)
	})
}

func (c *CLI) restore(ctx context.Context, def Definition, inv invocation) (any, int, error) {
	backup, identity, lock, err := parseRestore(inv.args)
	if err != nil {
		return nil, -1, err
	}
	return c.mutate(ctx, def, "restore", func() (any, error) {
		if err := c.preflight(ctx, def, "restore"); err != nil {
			return nil, err
		}
		state, err := c.owned(ctx, def, false)
		if err != nil {
			return nil, err
		}
		if state.Exists {
			return nil, apiError("already_exists", "restore requires an absent destination box", 1, nil)
		}
		archiveSHA256, err := hashFile(backup)
		if err != nil {
			return nil, fmt.Errorf("hash restore source backup: %w", err)
		}
		if err := c.deps.Operations.Restore(ctx, def, backup, identity, lock); err != nil {
			return nil, err
		}
		backupResult := BackupResult{
			Archive: backup, Envelope: strings.TrimSuffix(backup, ".tar.zst.age") + ".envelope.json",
			ArchiveSHA256: archiveSHA256,
		}
		health, err := c.deps.Operations.Health(ctx, def)
		if err != nil || !health.Healthy {
			return nil, apiError("health_failed", "restored box failed health verification", 1, err)
		}
		return map[string]any{"state": "running", "healthy": true, "components": health.Components, "backup": backupResult}, nil
	})
}

func (c *CLI) rebuild(ctx context.Context, def Definition, inv invocation) (any, int, error) {
	if err := requireArgs("rebuild", inv.args, 0, 0); err != nil {
		return nil, -1, err
	}
	return c.mutate(ctx, def, "rebuild", func() (any, error) {
		if err := c.preflight(ctx, def, "rebuild"); err != nil {
			return nil, err
		}
		ownership, err := c.owned(ctx, def, true)
		if err != nil {
			return nil, err
		}
		recovery := RecoveryState{}
		backup := BackupResult{}
		hostOnlyRecovery := false
		if err := c.deps.Operations.Recover(ctx, def); err != nil {
			latest, latestErr := c.deps.Backups.LatestVerified(ctx, def)
			if latestErr != nil || latest == nil {
				failure := apiError("integrity_failed", "guest recovery is unavailable and no verified recovery backup exists; refusing to rebuild", 1, errors.Join(err, latestErr))
				failure.Recovery = "repair guest access or restore a verified backup before retrying rebuild"
				return nil, failure
			}
			backup = *latest
			hostOnlyRecovery = true
			if !inv.global.quiet {
				fmt.Fprintf(c.stderr, "[hermes-box] WARNING: guest recovery is unavailable; rebuild will use verified backup %s for rollback\n", latest.Archive)
			}
		} else {
			var err error
			recovery, err = c.deps.Operations.CaptureRecoveryState(ctx, def)
			if err != nil {
				latest, latestErr := c.deps.Backups.LatestVerified(ctx, def)
				if latestErr != nil || latest == nil {
					failure := apiError("integrity_failed", "current root recovery state could not be captured and no verified recovery backup exists; refusing to rebuild", 1, errors.Join(err, latestErr))
					failure.Recovery = "repair guest access or restore a verified backup before retrying rebuild"
					return nil, failure
				}
				backup = *latest
				hostOnlyRecovery = true
				if !inv.global.quiet {
					fmt.Fprintf(c.stderr, "[hermes-box] WARNING: current root state could not be captured; rebuild will use verified backup %s for rollback\n", latest.Archive)
				}
			}
		}
		if recovery.Temporary {
			defer os.Remove(recovery.AppliedLock)
		}
		if !hostOnlyRecovery {
			var err error
			backup, err = c.deps.Backups.Create(ctx, def, "pre-rebuild")
			if err != nil {
				return nil, fmt.Errorf("create fresh pre-rebuild recovery backup: %w", err)
			}
		}
		preparedRecovery, err := c.deps.Operations.PrepareRebuildRecovery(ctx, def, recovery, backup)
		if err != nil {
			return nil, fmt.Errorf("persist rebuild recovery state: %w", err)
		}
		recovery = preparedRecovery
		if !hostOnlyRecovery {
			if err := c.deps.Operations.StopServices(ctx, def); err != nil {
				return nil, err
			}
			if err := c.deps.Operations.SyncData(ctx, def); err != nil {
				return nil, err
			}
		}
		if ownership.Running {
			if err := c.deps.Operations.StopVM(ctx, def); err != nil {
				return nil, err
			}
		}
		if err := c.deps.Operations.RemoveVM(ctx, def, true); err != nil {
			return nil, err
		}
		failed := func(cause error) (any, error) {
			// Once the old root is gone, cancellation of the initiating request must
			// not cancel the safety rollback. Recovery gets its own bounded lifetime.
			recoveryCtx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()
			recoveryErr := c.deps.Operations.RecoverRebuild(recoveryCtx, def, recovery, backup)
			if recoveryErr == nil {
				recoveryErr = c.deps.Operations.CompleteRebuild(recoveryCtx, def)
			}
			return nil, errors.Join(cause, recoveryErr)
		}
		if err := c.deps.Operations.RecreateVM(ctx, def); err != nil {
			return failed(err)
		}
		if hostOnlyRecovery {
			if err := c.deps.Operations.RestoreRebuildData(ctx, def, backup); err != nil {
				return failed(fmt.Errorf("restore known-good data after unavailable guest recovery: %w", err))
			}
		}
		if _, err := c.deps.Operations.Apply(ctx, def, "all"); err != nil {
			return failed(err)
		}
		if err := c.deps.Operations.StartServices(ctx, def); err != nil {
			return failed(err)
		}
		health, err := c.deps.Operations.Health(ctx, def)
		if err != nil || !health.Healthy {
			if err == nil {
				err = errors.New("rebuilt box is unhealthy")
			}
			return failed(err)
		}
		if err := c.deps.Operations.CompleteRebuild(ctx, def); err != nil {
			return nil, err
		}
		return map[string]any{"state": "running", "healthy": true, "components": health.Components, "backup": backup}, nil
	})
}

func (c *CLI) doctor(ctx context.Context, def Definition, inv invocation) (any, int, error) {
	if err := requireArgs("doctor", inv.args, 0, 0); err != nil {
		return nil, -1, err
	}
	checks, err := c.deps.Operations.Doctor(ctx, def)
	result := map[string]any{"healthy": err == nil, "checks": checks}
	if err == nil {
		return result, -1, nil
	}
	// Doctor diagnostics are useful precisely when a check fails. Emit the
	// complete result while preserving a non-zero status for automation.
	if writeErr := c.writeSuccess(inv, def.Name, result); writeErr != nil {
		return nil, -1, writeErr
	}
	fmt.Fprintf(c.stderr, "[hermes-box] doctor found unhealthy checks: %v\n", err)
	return nil, 1, nil
}

func (c *CLI) key(ctx context.Context, def Definition, inv invocation) (any, int, error) {
	if len(inv.args) != 2 || inv.args[0] != "export" {
		return nil, -1, apiError("invalid_input", "usage: hermes-box key export PATH", 2, nil)
	}
	path := inv.args[1]
	return c.mutate(ctx, def, "key export", func() (any, error) {
		fingerprint, err := c.deps.Operations.ExportKey(ctx, def, path)
		return map[string]any{"path": path, "recipient_fingerprint": fingerprint}, err
	})
}

func (c *CLI) destroy(ctx context.Context, def Definition, inv invocation) (any, int, error) {
	force := false
	if len(inv.args) == 1 && inv.args[0] == "--force" {
		force = true
	} else if err := requireArgs("destroy", inv.args, 0, 0); err != nil {
		return nil, -1, err
	}
	return c.mutate(ctx, def, "destroy", func() (any, error) {
		state, err := c.owned(ctx, def, true)
		if err != nil {
			return nil, err
		}
		if state.Running && !force {
			if err := c.deps.Operations.Recover(ctx, def); err != nil {
				return nil, err
			}
		}
		var final *BackupResult
		warning := ""
		if !force {
			backup, err := c.deps.Backups.Create(ctx, def, "final")
			if err != nil {
				failure := apiError("integrity_failed", "final backup failed; refusing to destroy the box", 1, err)
				failure.Recovery = "repair backup creation or rerun with destroy --force"
				return nil, failure
			}
			final = &backup
		} else {
			latest, latestErr := c.deps.Backups.LatestVerified(ctx, def)
			final = latest
			if latest != nil {
				warning = fmt.Sprintf("forced removal will rely on latest verified backup %s", latest.Archive)
			} else {
				warning = "forced removal has no valid recovery backup"
				if latestErr != nil {
					warning += ": " + latestErr.Error()
				}
			}
			fmt.Fprintf(c.stderr, "[hermes-box] WARNING: %s\n", warning)
		}
		if err := c.deps.Operations.RemoveAll(ctx, def); err != nil {
			return nil, err
		}
		result := map[string]any{"backup": final, "removed": []string{"vm", "root_disk", "data_disk"}}
		if warning != "" {
			result["warning"] = warning
		}
		return result, nil
	})
}

func (c *CLI) version(ctx context.Context, def Definition, inv invocation) (any, int, error) {
	if err := requireArgs("version", inv.args, 0, 0); err != nil {
		return nil, -1, err
	}
	result, err := c.deps.Operations.Version(ctx, def)
	return result, -1, err
}

func parseLogs(args []string) (string, int, bool, error) {
	target, lines, follow, targetSet := "hermes", 100, false, false
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "hermes", "executor", "recovery":
			if targetSet {
				return "", 0, false, apiError("invalid_input", "logs accepts one target", 2, nil)
			}
			target = args[index]
			targetSet = true
		case "-f":
			if follow {
				return "", 0, false, apiError("invalid_input", "duplicate -f", 2, nil)
			}
			follow = true
		case "-n":
			index++
			if index >= len(args) {
				return "", 0, false, apiError("invalid_input", "-n requires a line count", 2, nil)
			}
			value, err := strconv.Atoi(args[index])
			if err != nil || value < 1 {
				return "", 0, false, apiError("invalid_input", "log line count must be positive", 2, err)
			}
			lines = value
		default:
			return "", 0, false, apiError("invalid_input", "usage: hermes-box logs [hermes|executor|recovery] [-f] [-n LINES]", 2, nil)
		}
	}
	return target, lines, follow, nil
}

func parseRestore(args []string) (backup, identity, lock string, err error) {
	if len(args) < 3 {
		return "", "", "", apiError("invalid_input", "usage: hermes-box restore BACKUP --identity PATH [--lock PATH]", 2, nil)
	}
	backup = args[0]
	for index := 1; index < len(args); index += 2 {
		if index+1 >= len(args) {
			return "", "", "", apiError("invalid_input", "restore option requires a path", 2, nil)
		}
		switch args[index] {
		case "--identity":
			if identity != "" {
				return "", "", "", apiError("invalid_input", "duplicate --identity", 2, nil)
			}
			identity = args[index+1]
		case "--lock":
			if lock != "" {
				return "", "", "", apiError("invalid_input", "duplicate --lock", 2, nil)
			}
			lock = args[index+1]
		default:
			return "", "", "", apiError("invalid_input", "unknown restore option: "+args[index], 2, nil)
		}
	}
	if identity == "" {
		return "", "", "", apiError("invalid_input", "restore requires --identity PATH", 2, nil)
	}
	return backup, identity, lock, nil
}

func requireArgs(command string, args []string, minimum, maximum int) error {
	if len(args) < minimum || len(args) > maximum {
		return apiError("invalid_input", fmt.Sprintf("%s received an invalid number of arguments", command), 2, nil)
	}
	return nil
}

func exactTarget(command string, args []string, target string) error {
	if len(args) != 1 || args[0] != target {
		return apiError("invalid_input", "usage: hermes-box "+command+" "+target, 2, nil)
	}
	return nil
}

func oneComponent(command string, args []string, all bool) error {
	if len(args) != 1 || !components[args[0]] && !(all && args[0] == "all") {
		values := "claude|codex|hermes|executor|node|uv"
		if all {
			values += "|all"
		}
		return apiError("invalid_input", "usage: hermes-box "+command+" "+values, 2, nil)
	}
	return nil
}

const firstRunHandoff = `Setup required:
1. hermes-box ssh
2. claude
3. codex login --device-auth
4. hermes auth add openai-codex --type oauth
5. hermes model
6. hermes-box open executor
7. Create the Executor admin account and integrations
8. hermes-box setup executor and enter the portal-provided token
9. hermes-box key export /secure/path/main-age-key.txt
10. hermes-box backup configured
`
