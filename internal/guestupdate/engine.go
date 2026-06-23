package guestupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/davis7dotsh/hermes-box/internal/component"
	"golang.org/x/sys/unix"
)

type Engine struct {
	Root   string
	Runner Runner
	Now    func() time.Time
	Stdout io.Writer
	Stderr io.Writer
	// writeComponentState is replaceable only by package tests to inject a
	// crash at the state publication boundary.
	writeComponentState func(string, any, os.FileMode) error
	writeJSON           func(string, any, os.FileMode) error
	writeFile           func(string, []byte, os.FileMode) error
	syncDirectoryHook   func(string) error
	// durabilityCheckpointHook is package-test-only crash injection. A panic from
	// the hook models process loss after the named durability boundary.
	durabilityCheckpointHook func(string)
}

const (
	validationTimeout = 30 * time.Second
	cleanupTimeout    = 10 * time.Second
)

func New(root string) *Engine {
	return &Engine{
		Root:   root,
		Runner: OSRunner{},
		Now:    time.Now,
		Stdout: os.Stderr,
		Stderr: os.Stderr,
	}
}

func (e *Engine) Apply(ctx context.Context, specs []component.Spec, initial, snapshotReady bool, reviewedLock string) (Status, error) {
	if err := validateReviewedLock(reviewedLock); err != nil {
		return Status{}, err
	}
	paths, unlock, err := e.lock()
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	if err := e.recoverPathRestore(paths); err != nil {
		return Status{}, err
	}
	if err := ensureNoPendingJournal(paths); err != nil {
		return Status{}, err
	}
	ordered, err := component.Sort(specs)
	if err != nil {
		return Status{}, withClass(errInvalidInput, err)
	}
	if initial {
		names := component.Names()
		if len(ordered) != len(names) {
			return Status{}, withClass(errInvalidInput, errors.New("initial apply requires all six components"))
		}
		for index := range names {
			if ordered[index].Name != names[index] {
				return Status{}, withClass(errInvalidInput, errors.New("initial apply requires all six components"))
			}
		}
	} else if len(ordered) != 1 {
		return Status{}, withClass(errInvalidInput, errors.New("component updates must be applied one at a time"))
	}
	applied, releases, err := loadState(paths)
	if err != nil {
		return Status{}, err
	}
	for index, spec := range ordered {
		if !initial {
			if err := e.checkInteractiveBusy(ctx); err != nil {
				return Status{}, err
			}
		}
		current := releases.Components[spec.Name]
		if current.Current == spec.Pin {
			if err := e.verifyInstalledIdentity(paths, spec); err != nil {
				return Status{}, err
			}
			continue
		}
		if current.Current != "" && !snapshotReady {
			return Status{}, withClass(errInvalidInput, fmt.Errorf("%s update requires a verified durable-state snapshot", spec.Name))
		}
		if current.Current == "" && !initial && len(applied.Components) > 0 {
			return Status{}, withClass(errInvalidInput, fmt.Errorf("installing new component %s requires initial apply", spec.Name))
		}
		publishLock := ""
		if index == len(ordered)-1 {
			publishLock = reviewedLock
		}
		if err := e.applyOne(ctx, paths, spec, &applied, &releases, publishLock, initial); err != nil {
			return Status{}, err
		}
	}
	if initial {
		if _, err := e.Runner.Run(ctx, []string{"systemctl", "enable", "executor.service", "hermes.service"}, RunOptions{Stderr: e.Stderr}); err != nil {
			return Status{}, err
		}
		for _, service := range []string{"executor.service", "hermes.service"} {
			if _, err := e.Runner.Run(ctx, []string{"systemctl", "start", service}, RunOptions{Stderr: e.Stderr}); err != nil {
				return Status{}, err
			}
			if _, err := e.Runner.Run(ctx, []string{"systemctl", "is-active", "--quiet", service}, RunOptions{Stderr: e.Stderr}); err != nil {
				return Status{}, fmt.Errorf("initial service health %s: %w", service, err)
			}
		}
		if err := e.runtimeHealth(ctx, paths, component.Hermes); err != nil {
			return Status{}, err
		}
		if err := e.runtimeHealth(ctx, paths, component.Executor); err != nil {
			return Status{}, err
		}
	}
	return e.status(paths)
}

func (e *Engine) Rollback(ctx context.Context, name component.Name, snapshotReady bool, reviewedLock string) (Status, error) {
	if err := validateReviewedLock(reviewedLock); err != nil {
		return Status{}, err
	}
	if !component.Known(name) {
		return Status{}, withClass(errInvalidInput, fmt.Errorf("unknown component %q", name))
	}
	if !snapshotReady {
		return Status{}, withClass(errInvalidInput, errors.New("rollback requires the retained durable-state snapshot"))
	}
	paths, unlock, err := e.lock()
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	if err := e.recoverPathRestore(paths); err != nil {
		return Status{}, err
	}
	if err := ensureNoPendingJournal(paths); err != nil {
		return Status{}, err
	}
	applied, releases, err := loadState(paths)
	if err != nil {
		return Status{}, err
	}
	metadata := releases.Components[name]
	if metadata.Previous == "" {
		return Status{}, withClass(errInvalidInput, fmt.Errorf("%s has no previous release", name))
	}
	services, err := e.freeze(ctx)
	if err != nil {
		return Status{}, err
	}
	activated := false
	committed := false
	defer func() {
		cleanup, cancel := cleanupContext()
		defer cancel()
		if !committed {
			if activated {
				_ = e.freezeOnly(cleanup)
			} else {
				_ = e.unfreeze(cleanup, services)
			}
		}
	}()
	journal := Journal{
		Schema: ProtocolSchema, Component: name, Previous: metadata.Current,
		Candidate: metadata.Previous, Phase: "prepared", Services: services, CreatedAt: e.Now().UTC(),
	}
	previousLock, err := os.ReadFile(paths.applied)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Status{}, err
	}
	journal.PreviousLock = string(previousLock)
	if err := e.atomicJSON(paths.journal, journal, 0o600); err != nil {
		return Status{}, err
	}
	if err := e.activate(paths, name, metadata.Previous); err != nil {
		return Status{}, err
	}
	activated = true
	e.durabilityCheckpoint("activation-durable:" + string(name))
	journal.Phase = "activated"
	if err := e.atomicJSON(paths.journal, journal, 0o600); err != nil {
		return Status{}, err
	}
	deauthorize, err := e.authorizeServiceStart(ctx, paths)
	if err != nil {
		return Status{}, err
	}
	defer deauthorize()
	if err := e.unfreeze(ctx, services); err != nil {
		return Status{}, err
	}
	candidate := metadata.Previous
	current := metadata.Current
	if err := e.healthRelease(ctx, paths, name, candidate); err != nil {
		_ = e.freezeOnly(ctx)
		return Status{}, withClass(errHealth, fmt.Errorf("rollback health %s: %w; host must restore the pre-rollback snapshot and call recover", name, err))
	}
	metadata.Current, metadata.Previous = candidate, current
	metadata.Retained = uniquePins(metadata.Current, metadata.Previous)
	releases.Components[name] = metadata
	applied.Components[name] = metadata.Current
	applied.UpdatedAt = e.Now().UTC()
	if err := e.saveState(paths, applied, releases); err != nil {
		return Status{}, err
	}
	if err := e.atomicFile(paths.applied, []byte(reviewedLock), 0o600); err != nil {
		return Status{}, err
	}
	journal.Phase = "committed"
	if err := e.atomicJSON(paths.journal, journal, 0o600); err != nil {
		return Status{}, err
	}
	if err := durableRemove(paths.journal, false); err != nil {
		return Status{}, err
	}
	committed = true
	return e.status(paths)
}

func (e *Engine) Recover(ctx context.Context) (Status, error) {
	paths, unlock, err := e.lock()
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	if err := e.recoverPathRestore(paths); err != nil {
		return Status{}, err
	}
	return e.recover(ctx, paths)
}

func (e *Engine) Status() (Status, error) {
	paths, err := newPaths(e.Root)
	if err != nil {
		return Status{}, err
	}
	return e.status(paths)
}

func (e *Engine) applyOne(ctx context.Context, paths paths, spec component.Spec, applied *AppliedState, releases *ReleasesState, publishLock string, initial bool) error {
	release, err := e.stage(ctx, paths, spec)
	if err != nil {
		return fmt.Errorf("stage %s: %w", spec.Name, err)
	}
	if err := e.validate(ctx, paths, spec.Name, trustedSmoke(spec.Name, spec.Image), release, trustedEnvironment(spec.Name)); err != nil {
		return withClass(errHealth, fmt.Errorf("smoke %s: %w", spec.Name, err))
	}
	if spec.Name == component.Hermes {
		if err := e.smokeHermesGateway(ctx, paths, release); err != nil {
			return withClass(errHealth, fmt.Errorf("smoke %s temporary gateway: %w", spec.Name, err))
		}
	}
	services, err := e.freeze(ctx)
	if err != nil {
		return err
	}
	activated := false
	committed := false
	defer func() {
		cleanup, cancel := cleanupContext()
		defer cancel()
		if !committed {
			if activated {
				_ = e.freezeOnly(cleanup)
			} else {
				_ = e.unfreeze(cleanup, services)
			}
		}
	}()
	metadata := releases.Components[spec.Name]
	journal := Journal{
		Schema: ProtocolSchema, Component: spec.Name, Previous: metadata.Current,
		Candidate: spec.Pin, Phase: "prepared", Services: services, CreatedAt: e.Now().UTC(),
	}
	previousLock, err := os.ReadFile(paths.applied)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	journal.PreviousLock = string(previousLock)
	if err := e.atomicJSON(paths.journal, journal, 0o600); err != nil {
		return err
	}
	if err := e.activate(paths, spec.Name, spec.Pin); err != nil {
		return err
	}
	activated = true
	e.durabilityCheckpoint("activation-durable:" + string(spec.Name))
	journal.Phase = "activated"
	if err := e.atomicJSON(paths.journal, journal, 0o600); err != nil {
		return err
	}
	deauthorize, err := e.authorizeServiceStart(ctx, paths)
	if err != nil {
		return err
	}
	defer deauthorize()
	if err := e.unfreeze(ctx, services); err != nil {
		_ = e.restoreActivation(paths, spec.Name, metadata.Current)
		return err
	}
	if initial && spec.Name == component.Executor && !slices.Contains(services, "executor.service") {
		if _, err := e.Runner.Run(ctx, []string{"systemctl", "start", "executor.service"}, RunOptions{Stderr: e.Stderr}); err != nil {
			return err
		}
	}
	if err := e.validate(ctx, paths, spec.Name, trustedHealth(spec.Name), release, trustedEnvironment(spec.Name)); err != nil {
		_ = e.freezeOnly(ctx)
		return withClass(errHealth, fmt.Errorf("health %s: %w; host must restore the transaction snapshot and call recover", spec.Name, err))
	}
	if !initial {
		if err := e.runtimeHealth(ctx, paths, spec.Name); err != nil {
			_ = e.freezeOnly(ctx)
			return withClass(errHealth, fmt.Errorf("runtime health %s: %w; host must restore the transaction snapshot and call recover", spec.Name, err))
		}
	}
	if err := e.validateDependents(ctx, paths, spec.Name); err != nil {
		_ = e.freezeOnly(ctx)
		return withClass(errHealth, fmt.Errorf("%w; host must restore the transaction snapshot and call recover", err))
	}
	metadata.Previous = metadata.Current
	metadata.Current = spec.Pin
	metadata.Retained = uniquePins(metadata.Current, metadata.Previous)
	releases.Components[spec.Name] = metadata
	applied.Components[spec.Name] = spec.Pin
	applied.UpdatedAt = e.Now().UTC()
	if err := e.saveState(paths, *applied, *releases); err != nil {
		return err
	}
	if publishLock != "" {
		if err := e.atomicFile(paths.applied, []byte(publishLock), 0o600); err != nil {
			return err
		}
	}
	journal.Phase = "committed"
	if err := e.atomicJSON(paths.journal, journal, 0o600); err != nil {
		return err
	}
	if err := durableRemove(paths.journal, false); err != nil {
		return err
	}
	committed = true
	if err := e.prune(ctx, paths, spec.Name, metadata.Retained); err != nil {
		fmt.Fprintf(e.Stderr, "hermes-box-guest: prune %s: %v\n", spec.Name, err)
	}
	return nil
}

func (e *Engine) verifyInstalledIdentity(paths paths, spec component.Spec) error {
	fingerprint, err := specFingerprint(spec)
	if err != nil {
		return err
	}
	var contract releaseContract
	if err := readJSON(filepath.Join(releasePath(paths, spec.Name, spec.Pin), "contract.json"), &contract); err != nil {
		return err
	}
	if contract.Fingerprint != fingerprint {
		return withClass(errIntegrity, fmt.Errorf("%s pin %s has different reviewed artifact identity; choose a new pin", spec.Name, spec.Pin))
	}
	return nil
}

func (e *Engine) stage(ctx context.Context, paths paths, spec component.Spec) (string, error) {
	release := releasePath(paths, spec.Name, spec.Pin)
	fingerprint, err := specFingerprint(spec)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(release); err == nil {
		var contract releaseContract
		contractErr := readJSON(filepath.Join(release, "contract.json"), &contract)
		if contractErr == nil && contract.Fingerprint == fingerprint && len(contract.Health) > 0 && (spec.Kind != "container" || fileExists(filepath.Join(release, "image"))) {
			return release, nil
		}
		if err := os.RemoveAll(release); err != nil {
			return "", fmt.Errorf("remove incomplete release: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	staging := release + fmt.Sprintf(".staging-%d", os.Getpid())
	if err := os.RemoveAll(staging); err != nil {
		return "", err
	}
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return "", err
	}
	if spec.Kind != "tree" {
		actual, err := fileSHA256(spec.Artifact)
		if err != nil {
			return "", err
		}
		if actual != spec.SHA256 {
			return "", withClass(errIntegrity, fmt.Errorf("artifact sha256 %s does not match %s", actual, spec.SHA256))
		}
	}
	for label, input := range spec.Inputs {
		actual, err := fileSHA256(input.Path)
		if err != nil {
			return "", fmt.Errorf("verify %s input: %w", label, err)
		}
		if actual != input.SHA256 {
			return "", withClass(errIntegrity, fmt.Errorf("%s input sha256 %s does not match %s", label, actual, input.SHA256))
		}
	}
	if spec.Kind == "container" {
		if _, err := e.Runner.Run(ctx, []string{"podman", "load", "--input", spec.Artifact}, RunOptions{Stdout: e.Stdout, Stderr: e.Stderr}); err != nil {
			return "", err
		}
		if _, err := e.Runner.Run(ctx, []string{"podman", "image", "exists", spec.Image}, RunOptions{Stderr: e.Stderr}); err != nil {
			return "", err
		}
		var inspect strings.Builder
		if _, err := e.Runner.Run(ctx, []string{"podman", "image", "inspect", "--format", "{{.Digest}}", spec.Image}, RunOptions{Stdout: &inspect, Stderr: e.Stderr}); err != nil {
			return "", err
		}
		if strings.TrimSpace(inspect.String()) != spec.ImageChildDigest {
			return "", withClass(errIntegrity, errors.New("loaded Executor child digest does not match reviewed linux/arm64 digest"))
		}
		if err := atomicFile(filepath.Join(staging, "image"), []byte(spec.Image+"\n"), 0o644); err != nil {
			return "", err
		}
		if err := atomicJSON(filepath.Join(staging, "contract.json"), releaseContract{Fingerprint: fingerprint, Health: trustedHealth(spec.Name), Env: trustedEnvironment(spec.Name)}, 0o600); err != nil {
			return "", err
		}
		if err := os.MkdirAll(filepath.Dir(release), 0o755); err != nil {
			return "", err
		}
		if err := os.Rename(staging, release); err != nil {
			return "", err
		}
		return release, nil
	}
	if spec.Kind == "tree" && e.Root != "" && e.Root != "/" {
		if err := copyTree(spec.Artifact, staging); err != nil {
			os.RemoveAll(staging)
			return "", err
		}
	} else {
		if err := e.installComponent(ctx, paths, spec, staging); err != nil {
			os.RemoveAll(staging)
			return "", err
		}
	}
	if err := atomicJSON(filepath.Join(staging, "contract.json"), releaseContract{Fingerprint: fingerprint, Health: trustedHealth(spec.Name), Env: trustedEnvironment(spec.Name)}, 0o600); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(release), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(staging, release); err != nil {
		return "", err
	}
	return release, nil
}

func specFingerprint(spec component.Spec) (string, error) {
	spec.Artifact = ""
	inputs := make(map[string]component.Input, len(spec.Inputs))
	for name, input := range spec.Inputs {
		input.Path = ""
		inputs[name] = input
	}
	spec.Inputs = inputs
	encoded, err := json.Marshal(spec)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func trustedSmoke(name component.Name, image string) [][]string {
	switch name {
	case component.Node:
		return [][]string{{"{release}/bin/node", "--version"}, {"{release}/bin/npm", "--version"}}
	case component.UV:
		return [][]string{{"{release}/bin/uv", "--version"}}
	case component.Claude:
		return [][]string{{"{release}/bin/claude", "--version"}}
	case component.Codex:
		return [][]string{{"{release}/bin/codex", "--strict-config", "--version"}, {"{release}/bin/codex", "--help"}}
	case component.Hermes:
		return [][]string{{"{release}/bin/hermes", "--version"}, {"{release}/venv/bin/python", "-c", "import hermes_cli; import hermes_cli.gateway.run"}}
	case component.Executor:
		return [][]string{{"podman", "image", "exists", image}}
	default:
		return nil
	}
}

func trustedHealth(name component.Name) [][]string {
	switch name {
	case component.Claude:
		return [][]string{{"{release}/bin/claude", "doctor"}}
	case component.Codex:
		return [][]string{{"{release}/bin/codex", "doctor"}, {"{release}/bin/codex", "--strict-config", "--version"}}
	case component.Executor:
		return [][]string{{"/usr/bin/curl", "--fail", "--silent", "http://127.0.0.1:4788/health"}}
	default:
		return trustedSmoke(name, "")
	}
}

func trustedEnvironment(name component.Name) map[string]string {
	if name != component.Claude {
		return nil
	}
	return map[string]string{
		"DISABLE_AUTOUPDATER":                      "1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
		"DISABLE_TELEMETRY":                        "1",
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func (e *Engine) validate(ctx context.Context, paths paths, name component.Name, commands [][]string, release string, environment map[string]string) error {
	node := activationPath(paths, component.Node)
	if name == component.Node {
		node = release
	}
	for _, argv := range commands {
		expanded := make([]string, len(argv))
		for index, value := range argv {
			expanded[index] = strings.ReplaceAll(value, "{release}", release)
		}
		commandContext, cancel := context.WithTimeout(ctx, validationTimeout)
		_, err := e.Runner.Run(commandContext, e.agentArgv(expanded), RunOptions{
			Environment: e.agentEnvironmentWithNode(node, environment), Stdout: e.Stdout, Stderr: e.Stderr,
		})
		cancel()
		if err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) smokeHermesGateway(ctx context.Context, paths paths, release string) error {
	home := filepath.Join(release, ".gateway-smoke-home")
	if err := os.RemoveAll(home); err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(home)
	if e.Root == "" || e.Root == "/" {
		if err := os.Chown(home, 1000, 1000); err != nil {
			return err
		}
	}
	unit := fmt.Sprintf("hermes-box-candidate-%d-%d.service", os.Getpid(), e.Now().UnixNano())
	environment := e.agentEnvironmentWithNode(activationPath(paths, component.Node), map[string]string{
		"HOME": home, "HERMES_HOME": home,
	})
	arguments := []string{
		"/usr/bin/systemd-run", "--quiet", "--collect", "--unit", unit,
		"--property=User=agent", "--property=RuntimeMaxSec=30s",
	}
	for _, key := range []string{"HOME", "HERMES_HOME", "PATH"} {
		arguments = append(arguments, "--setenv="+key+"="+environment[key])
	}
	arguments = append(arguments, filepath.Join(release, "bin/hermes"), "gateway", "run", "--quiet", "--force")
	startContext, startCancel := context.WithTimeout(ctx, validationTimeout)
	_, err := e.Runner.Run(startContext, arguments, RunOptions{Stderr: e.Stderr})
	startCancel()
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := cleanupContext()
		defer cancel()
		_, _ = e.Runner.Run(cleanup, []string{"systemctl", "stop", unit}, RunOptions{Stderr: e.Stderr})
		_, _ = e.Runner.Run(cleanup, []string{"systemctl", "reset-failed", unit}, RunOptions{Stderr: e.Stderr})
	}()
	var last error
	for range 5 {
		checkContext, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, activeErr := e.Runner.Run(checkContext, []string{"systemctl", "is-active", "--quiet", unit}, RunOptions{Stderr: e.Stderr})
		if activeErr == nil {
			_, last = e.Runner.Run(checkContext, e.agentArgv([]string{filepath.Join(release, "bin/hermes"), "gateway", "status"}), RunOptions{
				Environment: environment, Stdout: e.Stdout, Stderr: e.Stderr,
			})
		}
		cancel()
		if activeErr == nil && last == nil {
			return nil
		}
		if activeErr != nil {
			last = activeErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("temporary gateway did not become healthy: %w", last)
}

func (e *Engine) validateDependents(ctx context.Context, paths paths, name component.Name) error {
	var dependents []component.Name
	switch name {
	case component.Node:
		dependents = []component.Name{component.Claude, component.Hermes}
	case component.UV:
		dependents = []component.Name{component.Hermes}
	default:
		return nil
	}
	for _, dependent := range dependents {
		link := activationPath(paths, dependent)
		if _, err := os.Lstat(link); errors.Is(err, os.ErrNotExist) {
			continue
		}
		status, err := e.Status()
		if err != nil {
			return err
		}
		pin := status.Releases.Components[dependent].Current
		if err := e.healthRelease(ctx, paths, dependent, pin); err != nil {
			return fmt.Errorf("%s dependency health after %s: %w", dependent, name, err)
		}
	}
	return nil
}

func (e *Engine) checkInteractiveBusy(ctx context.Context) error {
	checks := [][]string{
		{"/usr/bin/pgrep", "--uid", "1000", "--full", "--", `/opt/hermes-box/(current|releases)/claude/.*/(claude|cli\.js)`},
		{"/usr/bin/pgrep", "--uid", "1000", "--exact", "codex"},
	}
	for index, argv := range checks {
		exit, _ := e.Runner.Run(ctx, argv, RunOptions{})
		if exit == 0 {
			return withClass(errBusy, fmt.Errorf("interactive %s process is busy", []string{"claude", "codex"}[index]))
		}
	}
	return nil
}

func (e *Engine) runtimeHealth(ctx context.Context, paths paths, name component.Name) error {
	switch name {
	case component.Hermes:
		if _, err := e.Runner.Run(ctx, []string{"systemctl", "is-active", "--quiet", "hermes.service"}, RunOptions{Stderr: e.Stderr}); err != nil {
			return err
		}
		release := activationPath(paths, component.Hermes)
		if err := e.validate(ctx, paths, component.Hermes, [][]string{{filepath.Join(release, "bin", "hermes"), "gateway", "status"}}, release, nil); err != nil {
			return err
		}
	case component.Executor:
		if _, err := e.Runner.Run(ctx, []string{"systemctl", "is-active", "--quiet", "executor.service"}, RunOptions{Stderr: e.Stderr}); err != nil {
			return err
		}
		configPath := filepath.Join(paths.data, "home/agent/.hermes/config.yaml")
		config, err := os.ReadFile(configPath)
		if err == nil && bytes.Contains(config, []byte("executor")) {
			hermes := filepath.Join(activationPath(paths, component.Hermes), "bin", "hermes")
			if err := e.validate(ctx, paths, component.Hermes, [][]string{{hermes, "mcp", "test", "executor"}}, filepath.Dir(filepath.Dir(hermes)), nil); err != nil {
				return fmt.Errorf("authenticated Executor MCP discovery: %w", err)
			}
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (e *Engine) agentArgv(argv []string) []string {
	if e.Root != "" && e.Root != "/" {
		return argv
	}
	wrapped := []string{"/usr/bin/setpriv", "--reuid=1000", "--regid=1000", "--init-groups", "--"}
	return append(wrapped, argv...)
}

func (e *Engine) agentEnvironment(overrides map[string]string) map[string]string {
	paths, err := newPaths(e.Root)
	if err != nil {
		return e.agentEnvironmentWithNode("/opt/hermes-box/tooling/current/node", overrides)
	}
	return e.agentEnvironmentWithNode(activationPath(paths, component.Node), overrides)
}

func (e *Engine) agentEnvironmentWithNode(node string, overrides map[string]string) map[string]string {
	environment := map[string]string{
		"HOME":        "/home/agent",
		"CODEX_HOME":  "/home/agent/.codex",
		"HERMES_HOME": "/home/agent/.hermes",
	}
	for key, value := range overrides {
		environment[key] = value
	}
	environment["PATH"] = filepath.Join(node, "bin") + ":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	return environment
}

func (e *Engine) healthRelease(ctx context.Context, paths paths, name component.Name, pin string) error {
	if pin == "" {
		return errors.New("release pin is empty")
	}
	release := releasePath(paths, name, pin)
	var contract releaseContract
	if err := readJSON(filepath.Join(release, "contract.json"), &contract); err != nil {
		return err
	}
	if len(contract.Health) == 0 {
		return fmt.Errorf("release %s/%s has no health contract", name, pin)
	}
	return e.validate(ctx, paths, name, contract.Health, release, contract.Env)
}

func (e *Engine) activate(paths paths, name component.Name, pin string) error {
	if name == component.Executor {
		image, err := os.ReadFile(filepath.Join(releasePath(paths, name, pin), "image"))
		if err != nil {
			return err
		}
		value := strings.TrimSpace(string(image))
		if !component.ValidExecutorImage(value) {
			return withClass(errIntegrity, errors.New("stored Executor image reference is invalid"))
		}
		return atomicFile(paths.executor, []byte("EXECUTOR_IMAGE="+value+"\n"), 0o644)
	}
	target := releasePath(paths, name, pin)
	link := activationPath(paths, name)
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return err
	}
	temporary := link + fmt.Sprintf(".new-%d", os.Getpid())
	_ = os.Remove(temporary)
	relative, err := filepath.Rel(filepath.Dir(link), target)
	if err != nil {
		return err
	}
	if err := os.Symlink(relative, temporary); err != nil {
		return err
	}
	defer os.Remove(temporary)
	if err := durableRename(temporary, link); err != nil {
		return err
	}
	return verifyActivation(paths, name, pin)
}

func (e *Engine) restoreActivation(paths paths, name component.Name, pin string) error {
	if pin == "" {
		if name == component.Executor {
			return durableRemove(paths.executor, false)
		}
		return durableRemove(activationPath(paths, name), false)
	}
	return e.activate(paths, name, pin)
}

func verifyActivation(paths paths, name component.Name, pin string) error {
	if name == component.Executor {
		contents, err := os.ReadFile(paths.executor)
		if err != nil {
			return err
		}
		expected, err := os.ReadFile(filepath.Join(releasePath(paths, name, pin), "image"))
		if err != nil {
			return err
		}
		if string(contents) != "EXECUTOR_IMAGE="+strings.TrimSpace(string(expected))+"\n" {
			return withClass(errIntegrity, errors.New("Executor activation does not match recovery journal"))
		}
		return nil
	}
	link := activationPath(paths, name)
	target, err := os.Readlink(link)
	if err != nil {
		return withClass(errIntegrity, fmt.Errorf("read active %s symlink: %w", name, err))
	}
	expected, err := filepath.Rel(filepath.Dir(link), releasePath(paths, name, pin))
	if err != nil {
		return err
	}
	if target != expected {
		return withClass(errIntegrity, fmt.Errorf("active %s symlink target %q does not match journal pin %q", name, target, pin))
	}
	return nil
}

func (e *Engine) recover(ctx context.Context, paths paths) (Status, error) {
	var journal Journal
	if err := readJSON(paths.journal, &journal); err != nil {
		return Status{}, err
	}
	if journal.Component == "" {
		return e.status(paths)
	}
	if err := validateJournal(journal); err != nil {
		return Status{}, err
	}
	if journal.Phase == "committed" {
		status, err := e.status(paths)
		if err != nil {
			return Status{}, err
		}
		if status.Applied.Components[journal.Component] != journal.Candidate || status.Releases.Components[journal.Component].Current != journal.Candidate {
			return Status{}, withClass(errIntegrity, errors.New("committed update journal disagrees with published component state"))
		}
		if err := verifyActivation(paths, journal.Component, journal.Candidate); err != nil {
			return Status{}, err
		}
		if err := durableRemove(paths.journal, false); err != nil {
			return Status{}, err
		}
		status.Pending = nil
		return status, nil
	}
	if err := e.restoreActivation(paths, journal.Component, journal.Previous); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Status{}, err
	}
	if journal.Previous != "" {
		if err := verifyActivation(paths, journal.Component, journal.Previous); err != nil {
			return Status{}, err
		}
	}
	applied, releases, err := loadState(paths)
	if err != nil {
		return Status{}, err
	}
	metadata := releases.Components[journal.Component]
	metadata.Current = journal.Previous
	if metadata.Previous == journal.Previous {
		metadata.Previous = ""
	}
	metadata.Retained = uniquePins(metadata.Current, metadata.Previous)
	releases.Components[journal.Component] = metadata
	if journal.Previous == "" {
		delete(applied.Components, journal.Component)
	} else {
		applied.Components[journal.Component] = journal.Previous
	}
	applied.UpdatedAt = e.Now().UTC()
	if err := e.saveState(paths, applied, releases); err != nil {
		return Status{}, err
	}
	if journal.PreviousLock == "" {
		if err := durableRemove(paths.applied, false); err != nil {
			return Status{}, err
		}
	} else if err := atomicFile(paths.applied, []byte(journal.PreviousLock), 0o600); err != nil {
		return Status{}, err
	}
	deauthorize, err := e.authorizeServiceStart(ctx, paths)
	if err != nil {
		return Status{}, err
	}
	defer deauthorize()
	if err := e.unfreeze(ctx, journal.Services); err != nil {
		cleanup, cancel := cleanupContext()
		_ = e.freezeOnly(cleanup)
		cancel()
		return Status{}, err
	}
	if journal.Previous != "" {
		if err := e.healthRelease(ctx, paths, journal.Component, journal.Previous); err != nil {
			cleanup, cancel := cleanupContext()
			_ = e.freezeOnly(cleanup)
			cancel()
			return Status{}, fmt.Errorf("recovered %s health: %w", journal.Component, err)
		}
	}
	if err := durableRemove(paths.journal, false); err != nil {
		return Status{}, err
	}
	return e.status(paths)
}

func (e *Engine) status(paths paths) (Status, error) {
	applied, releases, err := loadState(paths)
	if err != nil {
		return Status{}, err
	}
	var journal Journal
	if err := readJSON(paths.journal, &journal); err != nil {
		return Status{}, err
	}
	lock, err := os.ReadFile(paths.applied)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Status{}, err
	}
	status := Status{Applied: applied, AppliedLock: string(lock), Releases: releases}
	if len(applied.Components) > 0 && len(lock) == 0 {
		return Status{}, errors.New("applied.lock is missing for installed components")
	}
	if journal.Component != "" {
		if err := validateJournal(journal); err != nil {
			return Status{}, err
		}
		status.Pending = &journal
	}
	var restore pathRestoreJournal
	if err := readJSON(paths.restoreJournal, &restore); err != nil {
		return Status{}, err
	}
	if restore.Schema != 0 {
		if err := validatePathRestoreJournal(paths, restore); err != nil {
			return Status{}, err
		}
		status.RestorePending = &RestorePending{Component: restore.Component, Committed: restore.Committed}
	}
	return status, nil
}

func (e *Engine) freeze(ctx context.Context) ([]string, error) {
	services := make([]string, 0, 2)
	for _, service := range []string{"hermes.service", "executor.service"} {
		exit, _ := e.Runner.Run(ctx, []string{"systemctl", "is-active", "--quiet", service}, RunOptions{})
		if exit == 0 {
			services = append(services, service)
		}
	}
	for _, service := range services {
		if _, err := e.Runner.Run(ctx, []string{"systemctl", "stop", service}, RunOptions{Stderr: e.Stderr}); err != nil {
			cleanup, cancel := cleanupContext()
			_ = e.unfreeze(cleanup, services)
			cancel()
			return nil, fmt.Errorf("stop %s: %w", service, err)
		}
	}
	return services, nil
}

func cleanupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), cleanupTimeout)
}

func (e *Engine) freezeOnly(ctx context.Context) error {
	var failures []error
	for _, service := range []string{"hermes.service", "executor.service"} {
		if _, err := e.Runner.Run(ctx, []string{"systemctl", "stop", service}, RunOptions{Stderr: e.Stderr}); err != nil {
			failures = append(failures, fmt.Errorf("stop %s: %w", service, err))
		}
	}
	return errors.Join(failures...)
}

func (e *Engine) unfreeze(ctx context.Context, services []string) error {
	for index := len(services) - 1; index >= 0; index-- {
		if _, err := e.Runner.Run(ctx, []string{"systemctl", "start", services[index]}, RunOptions{Stderr: e.Stderr}); err != nil {
			return fmt.Errorf("start %s: %w", services[index], err)
		}
	}
	return nil
}

func (e *Engine) authorizeServiceStart(ctx context.Context, paths paths) (func(), error) {
	if err := atomicFile(paths.serviceAuthorization, []byte("host-coordinated transaction\n"), 0o600); err != nil {
		return func() {}, err
	}
	cleanup := func() { _ = os.Remove(paths.serviceAuthorization) }
	if _, err := e.Runner.Run(ctx, []string{"systemctl", "reset-failed", "hermes-box-recover.service"}, RunOptions{Stderr: e.Stderr}); err != nil {
		cleanup()
		return func() {}, err
	}
	if _, err := e.Runner.Run(ctx, []string{"systemctl", "start", "hermes-box-recover.service"}, RunOptions{Stderr: e.Stderr}); err != nil {
		cleanup()
		return func() {}, err
	}
	return cleanup, nil
}

func (e *Engine) lock() (paths, func(), error) {
	paths, err := newPaths(e.Root)
	if err != nil {
		return paths, func() {}, err
	}
	if err := os.MkdirAll(paths.state, 0o755); err != nil {
		return paths, func() {}, err
	}
	file, err := os.OpenFile(paths.lock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return paths, func() {}, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		return paths, func() {}, withClass(errBusy, errors.New("another guest mutation is active"))
	}
	return paths, func() {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
	}, nil
}

func loadState(paths paths) (AppliedState, ReleasesState, error) {
	applied := AppliedState{Schema: ProtocolSchema, Components: map[component.Name]string{}}
	releases := ReleasesState{Schema: ProtocolSchema, Components: map[component.Name]ReleaseMetadata{}}
	state := persistedState{Schema: ProtocolSchema, Applied: applied, Releases: releases}
	if err := readJSON(paths.componentState, &state); err != nil {
		return applied, releases, err
	}
	if state.Schema != ProtocolSchema {
		return applied, releases, errors.New("persisted component state has an unsupported schema")
	}
	applied, releases = state.Applied, state.Releases
	if applied.Components == nil {
		applied.Components = map[component.Name]string{}
	}
	if releases.Components == nil {
		releases.Components = map[component.Name]ReleaseMetadata{}
	}
	if applied.Schema != ProtocolSchema || releases.Schema != ProtocolSchema {
		return applied, releases, errors.New("persisted component state has an unsupported schema")
	}
	for name, pin := range applied.Components {
		if !component.Known(name) || !component.ValidPin(pin) {
			return applied, releases, fmt.Errorf("persisted applied state contains invalid component %q", name)
		}
		metadata, ok := releases.Components[name]
		if !ok || metadata.Current != pin {
			return applied, releases, fmt.Errorf("persisted component state is missing release metadata for %q", name)
		}
	}
	for name, metadata := range releases.Components {
		if !component.Known(name) || (metadata.Current != "" && !component.ValidPin(metadata.Current)) || (metadata.Previous != "" && !component.ValidPin(metadata.Previous)) {
			return applied, releases, fmt.Errorf("persisted release state contains invalid component %q", name)
		}
		for _, pin := range metadata.Retained {
			if !component.ValidPin(pin) {
				return applied, releases, fmt.Errorf("persisted release state contains invalid retained pin for %q", name)
			}
		}
		if applied.Components[name] != metadata.Current {
			return applied, releases, fmt.Errorf("persisted component state disagrees for %q", name)
		}
	}
	return applied, releases, nil
}

func ensureNoPendingJournal(paths paths) error {
	if _, err := os.Stat(paths.journal); err == nil {
		return withClass(errBusy, errors.New("pending update requires host snapshot restoration and explicit recover"))
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (e *Engine) saveState(paths paths, applied AppliedState, releases ReleasesState) error {
	write := e.writeComponentState
	if write == nil {
		write = atomicJSON
	}
	return write(paths.componentState, persistedState{
		Schema: ProtocolSchema, Applied: applied, Releases: releases,
	}, 0o600)
}

func (e *Engine) atomicJSON(path string, value any, mode os.FileMode) error {
	if e.writeJSON != nil {
		return e.writeJSON(path, value, mode)
	}
	return atomicJSON(path, value, mode)
}

func (e *Engine) atomicFile(path string, value []byte, mode os.FileMode) error {
	if e.writeFile != nil {
		return e.writeFile(path, value, mode)
	}
	return atomicFile(path, value, mode)
}

func (e *Engine) durabilityCheckpoint(name string) {
	if e.durabilityCheckpointHook != nil {
		e.durabilityCheckpointHook(name)
	}
}

func (e *Engine) syncDirectory(path string) error {
	if e.syncDirectoryHook != nil {
		return e.syncDirectoryHook(path)
	}
	return syncDirectory(path)
}

func validateReviewedLock(value string) error {
	if value == "" || len(value) > 2<<20 || strings.ContainsRune(value, '\x00') {
		return withClass(errInvalidInput, errors.New("reviewed_lock must contain the complete validated lock document"))
	}
	return nil
}

func validateJournal(journal Journal) error {
	if journal.Schema != ProtocolSchema || !component.Known(journal.Component) || !component.ValidPin(journal.Candidate) || (journal.Previous != "" && !component.ValidPin(journal.Previous)) {
		return errors.New("pending update journal is invalid")
	}
	if journal.Phase != "prepared" && journal.Phase != "activated" && journal.Phase != "committed" {
		return errors.New("pending update journal has an invalid phase")
	}
	for _, service := range journal.Services {
		if service != "hermes.service" && service != "executor.service" {
			return errors.New("pending update journal has an invalid service")
		}
	}
	if len(journal.PreviousLock) > 2<<20 || strings.ContainsRune(journal.PreviousLock, '\x00') {
		return errors.New("pending update journal has an invalid previous lock")
	}
	return nil
}

func releasePath(paths paths, name component.Name, pin string) string {
	if name == component.Node || name == component.UV {
		return filepath.Join(paths.tooling, string(name), pin)
	}
	return filepath.Join(paths.releases, string(name), pin)
}

func activationPath(paths paths, name component.Name) string {
	if name == component.Node || name == component.UV {
		return filepath.Join(paths.tooling, "current", string(name))
	}
	return filepath.Join(paths.current, string(name))
}

func uniquePins(pins ...string) []string {
	result := make([]string, 0, len(pins))
	for _, pin := range pins {
		if pin != "" && !slices.Contains(result, pin) {
			result = append(result, pin)
		}
	}
	return result
}

func (e *Engine) prune(ctx context.Context, paths paths, name component.Name, keep []string) error {
	parent := filepath.Dir(releasePath(paths, name, "placeholder"))
	entries, err := os.ReadDir(parent)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() && !slices.Contains(keep, entry.Name()) && !strings.Contains(entry.Name(), ".staging-") {
			image := ""
			if name == component.Executor {
				contents, err := os.ReadFile(filepath.Join(parent, entry.Name(), "image"))
				if err != nil {
					return err
				}
				image = strings.TrimSpace(string(contents))
			}
			if err := os.RemoveAll(filepath.Join(parent, entry.Name())); err != nil {
				return err
			}
			if image != "" {
				if _, err := e.Runner.Run(ctx, []string{"podman", "image", "rm", image}, RunOptions{Stderr: e.Stderr}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
