package guestupdate

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/davis7dotsh/hermes-box/internal/component"
)

type fakeRunner struct {
	commands [][]string
	active   map[string]bool
	fail     func([]string) error
	stdout   string
}

func (runner *fakeRunner) Run(ctx context.Context, argv []string, options RunOptions) (int, error) {
	if err := ctx.Err(); err != nil {
		return 1, err
	}
	copyArgv := append([]string(nil), argv...)
	runner.commands = append(runner.commands, copyArgv)
	isDigestInspection := len(argv) >= 3 && reflect.DeepEqual(argv[:3], []string{"podman", "image", "inspect"})
	if options.Stdout != nil && runner.stdout != "" && !isDigestInspection {
		_, _ = io.WriteString(options.Stdout, runner.stdout)
	}
	if len(argv) > 0 && argv[0] == "/usr/bin/pgrep" {
		process := argv[len(argv)-1]
		if strings.Contains(process, "claude") {
			process = "claude"
		}
		if runner.active["process:"+process] {
			return 0, nil
		}
		return 1, errors.New("no matching process")
	}
	if len(argv) >= 4 && reflect.DeepEqual(argv[:3], []string{"systemctl", "is-active", "--quiet"}) {
		if runner.active[argv[3]] {
			return 0, nil
		}
		return 3, errors.New("inactive")
	}
	if len(argv) == 3 && argv[0] == "systemctl" {
		if argv[1] == "stop" {
			runner.active[argv[2]] = false
		}
		if argv[1] == "start" {
			runner.active[argv[2]] = true
		}
	}
	if len(argv) > 0 && argv[0] == "/usr/bin/systemd-run" {
		for index, value := range argv {
			if value == "--unit" && index+1 < len(argv) {
				runner.active[argv[index+1]] = true
			}
		}
	}
	if runner.fail != nil {
		if err := runner.fail(argv); err != nil {
			return 1, err
		}
	}
	if options.Stdout != nil && len(argv) > 0 && argv[0] == "printf-output" {
		_, _ = io.WriteString(options.Stdout, strings.Join(argv[1:], "|"))
	}
	if options.Stdout != nil && isDigestInspection {
		_, digest, _ := strings.Cut(argv[len(argv)-1], "@")
		_, _ = io.WriteString(options.Stdout, digest+"\n")
	}
	return 0, nil
}

func newTestEngine(t *testing.T) (*Engine, *fakeRunner) {
	t.Helper()
	runner := &fakeRunner{active: map[string]bool{"hermes.service": true, "executor.service": true}}
	engine := New(t.TempDir())
	engine.Runner = runner
	engine.Stdout = io.Discard
	engine.Stderr = io.Discard
	return engine, runner
}

func treeSpec(t *testing.T, name component.Name, pin string) component.Spec {
	t.Helper()
	artifact := t.TempDir()
	if err := os.MkdirAll(filepath.Join(artifact, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifact, "bin", string(name)), []byte("payload"), 0o755); err != nil {
		t.Fatal(err)
	}
	spec := component.Spec{
		Name: name, Pin: pin, Kind: "tree", Artifact: artifact, SHA256: strings.Repeat("a", 64),
	}
	if name == component.Hermes {
		spec.Inputs = map[string]component.Input{}
		for _, inputName := range []string{"python", "wheels"} {
			path := filepath.Join(t.TempDir(), inputName)
			payload := []byte(inputName)
			if err := os.WriteFile(path, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(payload)
			spec.Inputs[inputName] = component.Input{Path: path, SHA256: hex.EncodeToString(digest[:])}
		}
		spec.UVLockSHA256 = strings.Repeat("c", 64)
	}
	return spec
}

func initialSpecs(t *testing.T) []component.Spec {
	t.Helper()
	specs := []component.Spec{
		treeSpec(t, component.Node, "1"), treeSpec(t, component.UV, "1"),
		treeSpec(t, component.Claude, "1"), treeSpec(t, component.Codex, "1"),
		treeSpec(t, component.Hermes, "1"),
	}
	specs = append(specs, executorSpec(t, "1", "b"))
	return specs
}

func executorSpec(t *testing.T, pin, digestCharacter string) component.Spec {
	t.Helper()
	artifact := filepath.Join(t.TempDir(), "executor.tar")
	payload := []byte("oci-" + pin)
	if err := os.WriteFile(artifact, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	return component.Spec{
		Name: component.Executor, Pin: pin, Kind: "container", Artifact: artifact,
		SHA256: hex.EncodeToString(digest[:]), Image: "example/executor@sha256:" + strings.Repeat(digestCharacter, 64),
		ImageIndexDigest: "sha256:" + strings.Repeat("a", 64), ImageChildDigest: "sha256:" + strings.Repeat(digestCharacter, 64),
	}
}

func initialize(t *testing.T, engine *Engine) {
	t.Helper()
	if _, err := engine.Apply(context.Background(), initialSpecs(t), true, false, "schema: 1\ninitial: true\n"); err != nil {
		t.Fatal(err)
	}
}

func TestInitialApplyStartsDisabledServices(t *testing.T) {
	t.Parallel()
	engine, runner := newTestEngine(t)
	runner.active = map[string]bool{}
	if _, err := engine.Apply(context.Background(), initialSpecs(t), true, false, "schema: 1\ninitial: true\n"); err != nil {
		t.Fatal(err)
	}
	if !runner.active["executor.service"] || !runner.active["hermes.service"] {
		t.Fatalf("initial services were not started: %#v", runner.active)
	}
	startExecutor, healthExecutor := -1, -1
	for index, argv := range runner.commands {
		if reflect.DeepEqual(argv, []string{"systemctl", "start", "executor.service"}) && startExecutor < 0 {
			startExecutor = index
		}
		if len(argv) > 0 && argv[0] == "/usr/bin/curl" && healthExecutor < 0 {
			healthExecutor = index
		}
	}
	if startExecutor < 0 || healthExecutor < 0 || startExecutor > healthExecutor {
		t.Fatalf("Executor health ran before service start: %#v", runner.commands)
	}
}

func TestApplyActivatesAtomicallyAndRetainsOnePrevious(t *testing.T) {
	t.Parallel()
	engine, runner := newTestEngine(t)
	initialize(t, engine)
	var loaded, pulled, inspected bool
	for _, argv := range runner.commands {
		loaded = loaded || (len(argv) >= 2 && argv[0] == "podman" && argv[1] == "load")
		pulled = pulled || (len(argv) >= 2 && argv[0] == "podman" && argv[1] == "pull")
		inspected = inspected || (len(argv) >= 3 && reflect.DeepEqual(argv[:3], []string{"podman", "image", "inspect"}))
	}
	if !loaded || pulled || !inspected {
		t.Fatalf("Executor OCI stage commands = %#v", runner.commands)
	}
	second := treeSpec(t, component.Codex, "2.0.0")
	status, err := engine.Apply(context.Background(), []component.Spec{second}, false, true, "schema: 1\ncodex: 2.0.0\n")
	if err != nil {
		t.Fatal(err)
	}
	metadata := status.Releases.Components[component.Codex]
	if metadata.Current != "2.0.0" || metadata.Previous != "1" {
		t.Fatalf("unexpected metadata: %#v", metadata)
	}
	if status.AppliedLock != "schema: 1\ncodex: 2.0.0\n" {
		t.Fatalf("applied lock = %q", status.AppliedLock)
	}
	link := filepath.Join(engine.Root, "opt/hermes-box/current/codex")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(target, "/2.0.0") {
		t.Fatalf("active target = %q", target)
	}
	if _, err := os.Stat(filepath.Join(engine.Root, "var/lib/hermes-box/update.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal remains after commit: %v", err)
	}
}

func TestComponentStatePublicationFailureLeavesCompletePriorDocument(t *testing.T) {
	t.Parallel()
	engine, _ := newTestEngine(t)
	initialize(t, engine)
	paths, err := newPaths(engine.Root)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(paths.componentState)
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected crash before atomic state publication")
	engine.writeComponentState = func(string, any, os.FileMode) error { return injected }
	_, err = engine.Apply(context.Background(), []component.Spec{treeSpec(t, component.Codex, "2")}, false, true, "schema: 1\ncodex: 2\n")
	if !errors.Is(err, injected) {
		t.Fatalf("apply error = %v, want injected crash", err)
	}
	after, err := os.ReadFile(paths.componentState)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("failed atomic publication exposed partial component state")
	}
	var persisted persistedState
	if err := json.Unmarshal(after, &persisted); err != nil {
		t.Fatalf("persisted state is not one complete document: %v", err)
	}
	if persisted.Applied.Components[component.Codex] != "1" || persisted.Releases.Components[component.Codex].Current != "1" {
		t.Fatalf("persisted state mixes generations: %#v", persisted)
	}
	engine.writeComponentState = nil
	status, err := engine.Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Applied.Components[component.Codex] != "1" || status.Releases.Components[component.Codex].Current != "1" {
		t.Fatalf("recovered state = %#v", status)
	}
}

func TestSamePinArtifactMutationIsRejected(t *testing.T) {
	t.Parallel()
	engine, _ := newTestEngine(t)
	initialize(t, engine)
	spec := treeSpec(t, component.Codex, "1")
	spec.SHA256 = strings.Repeat("f", 64)
	if _, err := engine.Apply(context.Background(), []component.Spec{spec}, false, true, "schema: 1\ncodex: 1\n"); err == nil || !strings.Contains(err.Error(), "different reviewed artifact identity") {
		t.Fatalf("same-pin mutation error = %v", err)
	}
}

func TestExecutorPrunesImagesBeyondPrevious(t *testing.T) {
	t.Parallel()
	engine, runner := newTestEngine(t)
	initialize(t, engine)
	if _, err := engine.Apply(context.Background(), []component.Spec{executorSpec(t, "2", "c")}, false, true, "schema: 1\nexecutor: 2\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(context.Background(), []component.Spec{executorSpec(t, "3", "d")}, false, true, "schema: 1\nexecutor: 3\n"); err != nil {
		t.Fatal(err)
	}
	want := "example/executor@sha256:" + strings.Repeat("b", 64)
	for _, argv := range runner.commands {
		if reflect.DeepEqual(argv, []string{"podman", "image", "rm", want}) {
			return
		}
	}
	t.Fatalf("old Executor image was not pruned: %#v", runner.commands)
}

func TestExecutorActivationRejectsTamperedStoredImage(t *testing.T) {
	t.Parallel()
	engine, _ := newTestEngine(t)
	paths, err := newPaths(engine.Root)
	if err != nil {
		t.Fatal(err)
	}
	release := releasePath(paths, component.Executor, "1")
	if err := os.MkdirAll(release, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(release, "image"), []byte("example/executor@sha256:"+strings.Repeat("b", 64)+"\nINJECTED=value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := engine.activate(paths, component.Executor, "1"); err == nil || !errors.Is(err, errIntegrity) {
		t.Fatalf("tampered Executor activation error = %v", err)
	}
	if _, err := os.Stat(paths.executor); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsafe EnvironmentFile was written: %v", err)
	}
}

func TestHermesCandidateRunsTemporaryGatewaySmoke(t *testing.T) {
	t.Parallel()
	engine, runner := newTestEngine(t)
	initialize(t, engine)
	var transient, status, cleanup bool
	for _, argv := range runner.commands {
		transient = transient || (len(argv) > 0 && argv[0] == "/usr/bin/systemd-run" && slices.Contains(argv, "--collect"))
		status = status || (len(argv) >= 2 && argv[len(argv)-2] == "gateway" && argv[len(argv)-1] == "status")
		cleanup = cleanup || (len(argv) == 3 && argv[0] == "systemctl" && argv[1] == "stop" && strings.HasPrefix(argv[2], "hermes-box-candidate-"))
	}
	if !transient || !status || !cleanup {
		t.Fatalf("Hermes temporary gateway smoke was incomplete: %#v", runner.commands)
	}
	paths, err := newPaths(engine.Root)
	if err != nil {
		t.Fatal(err)
	}
	var contract releaseContract
	if err := readJSON(filepath.Join(releasePath(paths, component.Claude, "1"), "contract.json"), &contract); err != nil {
		t.Fatal(err)
	}
	if contract.Env["DISABLE_AUTOUPDATER"] != "1" || contract.Env["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"] != "1" {
		t.Fatalf("Claude validation environment = %#v", contract.Env)
	}
}

func TestNodeUsesTrustedGuestArchiveInstaller(t *testing.T) {
	t.Parallel()
	engine, runner := newTestEngine(t)
	paths, err := newPaths(engine.Root)
	if err != nil {
		t.Fatal(err)
	}
	spec := component.Spec{Name: component.Node, Pin: "24", Artifact: "/verified/node.tar.xz", SHA256: strings.Repeat("a", 64)}
	if err := engine.installComponent(context.Background(), paths, spec, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	want := []string{"/usr/bin/tar", "--extract", "--file", spec.Artifact}
	if len(runner.commands) != 1 || len(runner.commands[0]) < len(want) || !reflect.DeepEqual(runner.commands[0][:len(want)], want) {
		t.Fatalf("installer argv = %#v", runner.commands)
	}
}

func TestFailedHealthRestoresPreviousActivation(t *testing.T) {
	t.Parallel()
	engine, runner := newTestEngine(t)
	initialize(t, engine)
	runner.fail = func(argv []string) error {
		if len(argv) > 1 && argv[1] == "doctor" && strings.Contains(argv[0], "/2/") {
			return errors.New("candidate unhealthy")
		}
		return nil
	}
	_, err := engine.Apply(context.Background(), []component.Spec{treeSpec(t, component.Claude, "2")}, false, true, "schema: 1\nclaude: 2\n")
	if err == nil {
		t.Fatal("expected health failure")
	}
	status, statusErr := engine.Status()
	if statusErr != nil {
		t.Fatal(statusErr)
	}
	if got := status.Applied.Components[component.Claude]; got != "1" {
		t.Fatalf("applied pin = %q, want 1", got)
	}
	if status.Pending == nil {
		t.Fatal("failed update cleared recovery journal")
	}
}

func TestApplyFailuresPreserveCommittedStateAtEachPrecommitPhase(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		candidate func(*testing.T) component.Spec
		fail      func([]string) error
		pending   bool
	}{
		{
			name:      "stage",
			candidate: func(t *testing.T) component.Spec { return executorSpec(t, "2", "c") },
			fail: func(argv []string) error {
				if len(argv) >= 2 && argv[0] == "podman" && argv[1] == "load" {
					return errors.New("stage failed")
				}
				return nil
			},
		},
		{
			name:      "smoke",
			candidate: func(t *testing.T) component.Spec { return treeSpec(t, component.Codex, "2") },
			fail: func(argv []string) error {
				if len(argv) > 1 && strings.Contains(argv[0], "/2/") && argv[1] == "--help" {
					return errors.New("smoke failed")
				}
				return nil
			},
		},
		{
			name:      "freeze",
			candidate: func(t *testing.T) component.Spec { return treeSpec(t, component.Codex, "2") },
			fail: func(argv []string) error {
				if reflect.DeepEqual(argv, []string{"systemctl", "stop", "hermes.service"}) {
					return errors.New("freeze failed")
				}
				return nil
			},
		},
		{
			name:      "runtime health",
			candidate: func(t *testing.T) component.Spec { return treeSpec(t, component.Hermes, "2") },
			fail: func() func([]string) error {
				gatewayChecks := 0
				return func(argv []string) error {
					if len(argv) >= 3 && argv[len(argv)-2] == "gateway" && argv[len(argv)-1] == "status" {
						gatewayChecks++
						if gatewayChecks > 1 {
							return errors.New("runtime health failed")
						}
					}
					return nil
				}
			}(),
			pending: true,
		},
		{
			name:      "dependent health",
			candidate: func(t *testing.T) component.Spec { return treeSpec(t, component.Node, "2") },
			fail: func(argv []string) error {
				if len(argv) > 1 && strings.Contains(argv[0], "/claude/") && argv[1] == "doctor" {
					return errors.New("dependent health failed")
				}
				return nil
			},
			pending: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			engine, runner := newTestEngine(t)
			initialize(t, engine)
			candidate := test.candidate(t)
			runner.fail = test.fail
			_, applyErr := engine.Apply(context.Background(), []component.Spec{candidate}, false, true, "schema: 1\ncandidate: 2\n")
			if applyErr == nil {
				t.Fatal("expected phase failure")
			}
			status, err := engine.Status()
			if err != nil {
				t.Fatal(err)
			}
			if got := status.Applied.Components[candidate.Name]; got != "1" {
				t.Fatalf("failed phase published applied pin %q", got)
			}
			if (status.Pending != nil) != test.pending {
				t.Fatalf("pending journal = %#v, want present %v", status.Pending, test.pending)
			}
			if !test.pending && (!runner.active["hermes.service"] || !runner.active["executor.service"]) {
				t.Fatalf("preactivation failure left services stopped: %#v", runner.active)
			}
		})
	}
}

func TestPruneFailureCannotRollbackCommittedHealthyUpdate(t *testing.T) {
	t.Parallel()
	engine, runner := newTestEngine(t)
	initialize(t, engine)
	if _, err := engine.Apply(context.Background(), []component.Spec{executorSpec(t, "2", "c")}, false, true, "schema: 1\nexecutor: 2\n"); err != nil {
		t.Fatal(err)
	}
	durable := filepath.Join(engine.Root, "data/executor/candidate-data")
	if err := os.MkdirAll(filepath.Dir(durable), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(durable, []byte("candidate"), 0o600); err != nil {
		t.Fatal(err)
	}
	var diagnostics bytes.Buffer
	engine.Stderr = &diagnostics
	runner.fail = func(argv []string) error {
		if len(argv) >= 3 && reflect.DeepEqual(argv[:3], []string{"podman", "image", "rm"}) {
			return errors.New("prune failed")
		}
		return nil
	}
	status, err := engine.Apply(context.Background(), []component.Spec{executorSpec(t, "3", "d")}, false, true, "schema: 1\nexecutor: 3\n")
	if err != nil {
		t.Fatalf("best-effort prune failed the committed update: %v", err)
	}
	if status.Applied.Components[component.Executor] != "3" || status.Releases.Components[component.Executor].Current != "3" {
		t.Fatalf("prune failure rolled back candidate state: %#v", status)
	}
	if status.Pending != nil || status.AppliedLock != "schema: 1\nexecutor: 3\n" {
		t.Fatalf("commit point was not durable before prune: %#v", status)
	}
	if contents, err := os.ReadFile(durable); err != nil || string(contents) != "candidate" {
		t.Fatalf("prune failure changed durable candidate data: %q, %v", contents, err)
	}
	if !strings.Contains(diagnostics.String(), "prune executor: prune failed") {
		t.Fatalf("missing best-effort prune diagnostic: %q", diagnostics.String())
	}
}

func TestRecoverRollsBackInterruptedActivation(t *testing.T) {
	t.Parallel()
	engine, _ := newTestEngine(t)
	initialize(t, engine)
	paths, err := newPaths(engine.Root)
	if err != nil {
		t.Fatal(err)
	}
	candidate := treeSpec(t, component.Codex, "2")
	if _, err := engine.stage(context.Background(), paths, candidate); err != nil {
		t.Fatal(err)
	}
	if err := engine.activate(paths, component.Codex, "2"); err != nil {
		t.Fatal(err)
	}
	journal := Journal{Schema: 1, Component: component.Codex, Previous: "1", PreviousLock: "schema: 1\ninitial: true\n", Candidate: "2", Phase: "activated"}
	if err := atomicJSON(paths.journal, journal, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := atomicFile(paths.applied, []byte("schema: 1\ncodex: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := engine.Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Applied.Components[component.Codex] != "1" || status.Pending != nil {
		t.Fatalf("unexpected recovered status: %#v", status)
	}
	if status.AppliedLock != journal.PreviousLock {
		t.Fatalf("recovered applied lock = %q", status.AppliedLock)
	}
}

func TestRecoverFinishesCommittedUpdateWithoutRollingItBack(t *testing.T) {
	t.Parallel()
	engine, _ := newTestEngine(t)
	initialize(t, engine)
	if _, err := engine.Apply(context.Background(), []component.Spec{treeSpec(t, component.Codex, "2")}, false, true, "schema: 1\ncodex: 2\n"); err != nil {
		t.Fatal(err)
	}
	paths, err := newPaths(engine.Root)
	if err != nil {
		t.Fatal(err)
	}
	journal := Journal{
		Schema: 1, Component: component.Codex, Previous: "1", Candidate: "2", Phase: "committed",
		PreviousLock: "schema: 1\ninitial: true\n",
	}
	if err := atomicJSON(paths.journal, journal, 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := engine.Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Applied.Components[component.Codex] != "2" || status.Releases.Components[component.Codex].Current != "2" {
		t.Fatalf("committed recovery rolled back candidate: %#v", status)
	}
	if status.Pending != nil {
		t.Fatalf("committed recovery did not clear journal: %#v", status.Pending)
	}
}

func TestApplyRefusesPendingHostRecovery(t *testing.T) {
	t.Parallel()
	engine, _ := newTestEngine(t)
	paths, err := newPaths(engine.Root)
	if err != nil {
		t.Fatal(err)
	}
	journal := Journal{Schema: 1, Component: component.Codex, Previous: "1", Candidate: "2", Phase: "activated"}
	if err := atomicJSON(paths.journal, journal, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = engine.Apply(context.Background(), []component.Spec{treeSpec(t, component.Codex, "2")}, false, true, "schema: 1\ncodex: 2\n")
	if err == nil || !strings.Contains(err.Error(), "host snapshot restoration") {
		t.Fatalf("apply error = %v", err)
	}
}

func TestProductionCommandsDropToAgentWithoutShell(t *testing.T) {
	t.Parallel()
	engine := New("")
	argv := []string{"printf", "%s", "a; echo injected"}
	want := []string{"/usr/bin/setpriv", "--reuid=1000", "--regid=1000", "--init-groups", "--", "printf", "%s", "a; echo injected"}
	if got := engine.agentArgv(argv); !reflect.DeepEqual(got, want) {
		t.Fatalf("agent argv = %#v", got)
	}
}

func TestManagedPathExecutesRealEnvShebangsForNodeAndClaude(t *testing.T) {
	t.Parallel()
	engine := New(t.TempDir())
	engine.Runner = OSRunner{}
	engine.Stdout = io.Discard
	engine.Stderr = io.Discard
	paths, err := newPaths(engine.Root)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(engine.Root, "executed")
	nodeRelease := releasePath(paths, component.Node, "fixture")
	if err := os.MkdirAll(filepath.Join(nodeRelease, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	nodeScript := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"${1:-direct}\" >> %q\n", marker)
	if err := os.WriteFile(filepath.Join(nodeRelease, "bin/node"), []byte(nodeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"npm", "npx"} {
		if err := os.WriteFile(filepath.Join(nodeRelease, "bin", name), []byte("#!/usr/bin/env node\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := engine.activate(paths, component.Node, "fixture"); err != nil {
		t.Fatal(err)
	}
	claudeRelease := releasePath(paths, component.Claude, "fixture")
	if err := os.MkdirAll(filepath.Join(claudeRelease, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeRelease, "bin/claude"), []byte("#!/usr/bin/env node\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := engine.validate(context.Background(), paths, component.Node, trustedSmoke(component.Node, ""), nodeRelease, nil); err != nil {
		t.Fatal(err)
	}
	if err := engine.validate(context.Background(), paths, component.Claude, [][]string{{"{release}/bin/claude", "--version"}}, claudeRelease, trustedEnvironment(component.Claude)); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	text := string(contents)
	if !strings.Contains(text, "/bin/npm") || !strings.Contains(text, "/bin/claude") {
		t.Fatalf("env shebangs did not resolve the managed Node runtime: %q", text)
	}
}

func TestPostActivationFailuresNeverRestartUncommittedCandidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		inject func(*Engine, *fakeRunner, paths, error)
	}{
		{
			name: "activated journal",
			inject: func(engine *Engine, _ *fakeRunner, paths paths, injected error) {
				engine.writeJSON = func(path string, value any, mode os.FileMode) error {
					journal, ok := value.(Journal)
					if path == paths.journal && ok && journal.Phase == "activated" {
						return injected
					}
					return atomicJSON(path, value, mode)
				}
			},
		},
		{
			name: "cancelled health",
			inject: func(_ *Engine, runner *fakeRunner, _ paths, _ error) {
				runner.fail = func(argv []string) error {
					if len(argv) > 1 && argv[1] == "doctor" && strings.Contains(argv[0], "/2/") {
						return context.Canceled
					}
					return nil
				}
			},
		},
		{
			name: "state publication",
			inject: func(engine *Engine, _ *fakeRunner, _ paths, injected error) {
				engine.writeComponentState = func(string, any, os.FileMode) error { return injected }
			},
		},
		{
			name: "applied lock publication",
			inject: func(engine *Engine, _ *fakeRunner, paths paths, injected error) {
				engine.writeFile = func(path string, value []byte, mode os.FileMode) error {
					if path == paths.applied {
						return injected
					}
					return atomicFile(path, value, mode)
				}
			},
		},
		{
			name: "commit journal publication",
			inject: func(engine *Engine, _ *fakeRunner, paths paths, injected error) {
				engine.writeJSON = func(path string, value any, mode os.FileMode) error {
					journal, ok := value.(Journal)
					if path == paths.journal && ok && journal.Phase == "committed" {
						return injected
					}
					return atomicJSON(path, value, mode)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			engine, runner := newTestEngine(t)
			initialize(t, engine)
			paths, err := newPaths(engine.Root)
			if err != nil {
				t.Fatal(err)
			}
			injected := errors.New("injected boundary failure")
			test.inject(engine, runner, paths, injected)
			_, err = engine.Apply(context.Background(), []component.Spec{treeSpec(t, component.Claude, "2")}, false, true, "schema: 1\nclaude: 2\n")
			if err == nil {
				t.Fatal("expected injected failure")
			}
			if runner.active["hermes.service"] || runner.active["executor.service"] {
				t.Fatalf("uncommitted candidate was restarted after failure: %#v", runner.active)
			}
			status, statusErr := engine.Status()
			if statusErr != nil {
				t.Fatal(statusErr)
			}
			if status.Pending == nil {
				t.Fatal("post-activation failure did not retain its recovery journal")
			}
		})
	}
}

func TestRollbackHealthChecksPreviousBeforeCommitting(t *testing.T) {
	t.Parallel()
	engine, runner := newTestEngine(t)
	initialize(t, engine)
	if _, err := engine.Apply(context.Background(), []component.Spec{treeSpec(t, component.Codex, "2")}, false, true, "schema: 1\ncodex: 2\n"); err != nil {
		t.Fatal(err)
	}
	runner.fail = func(argv []string) error {
		if len(argv) > 1 && argv[1] == "doctor" && strings.Contains(argv[0], "/1/") {
			return errors.New("old release no longer healthy")
		}
		return nil
	}
	if _, err := engine.Rollback(context.Background(), component.Codex, true, "schema: 1\ncodex: 1\n"); err == nil {
		t.Fatal("expected rollback health failure")
	}
	status, err := engine.Status()
	if err != nil {
		t.Fatal(err)
	}
	if status.Applied.Components[component.Codex] != "2" {
		t.Fatalf("failed rollback changed applied release: %#v", status)
	}
}

func TestBackupFailureRestartsPreviouslyRunningServices(t *testing.T) {
	t.Parallel()
	engine, runner := newTestEngine(t)
	runner.active["user-1000.slice"] = true
	data := filepath.Join(engine.Root, "data/home/agent")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(data, "escape")); err != nil {
		t.Fatal(err)
	}
	err := engine.BackupStream(context.Background(), io.Discard, nil)
	if err == nil {
		t.Fatal("expected escaping symlink to fail")
	}
	if !runner.active["hermes.service"] || !runner.active["executor.service"] {
		t.Fatalf("services were not restored: %#v", runner.active)
	}
	if !runner.active["user-1000.slice"] {
		t.Fatal("agent user slice was not restored")
	}
	var froze, unfroze bool
	var userFroze, userThawed bool
	for _, argv := range runner.commands {
		froze = froze || reflect.DeepEqual(argv, []string{"/usr/sbin/fsfreeze", "--freeze", filepath.Join(engine.Root, "data")})
		unfroze = unfroze || reflect.DeepEqual(argv, []string{"/usr/sbin/fsfreeze", "--unfreeze", filepath.Join(engine.Root, "data")})
		userFroze = userFroze || reflect.DeepEqual(argv, []string{"systemctl", "freeze", "user-1000.slice"})
		userThawed = userThawed || reflect.DeepEqual(argv, []string{"systemctl", "thaw", "user-1000.slice"})
	}
	if !froze || !unfroze || !userFroze || !userThawed {
		t.Fatalf("filesystem freeze was not safely unwound: %#v", runner.commands)
	}
}

func TestScopedSnapshotFailsBusyAndDoesNotKillUserSlice(t *testing.T) {
	t.Parallel()
	engine, runner := newTestEngine(t)
	runner.active["user-1000.slice"] = true
	runner.active["process:codex"] = true
	if err := engine.SnapshotStream(context.Background(), io.Discard, component.Codex); err == nil || !strings.Contains(err.Error(), "codex process is busy") {
		t.Fatalf("busy snapshot error = %v", err)
	}
	if !runner.active["user-1000.slice"] {
		t.Fatal("busy component snapshot killed the user slice")
	}
	delete(runner.active, "process:codex")
	var archive bytes.Buffer
	if err := engine.SnapshotStream(context.Background(), &archive, component.Codex); err != nil {
		t.Fatal(err)
	}
	if !runner.active["user-1000.slice"] {
		t.Fatal("component snapshot killed unrelated agent sessions")
	}
	header, err := tar.NewReader(&archive).Next()
	if err != nil || header.Name != "data/home/agent/.codex" {
		t.Fatalf("missing-state snapshot header = %#v, err %v", header, err)
	}
	if header.PAXRecords["HERMESBOX.absent"] != "1" {
		t.Fatalf("missing-state marker = %#v", header.PAXRecords)
	}
}

func TestRestoreRejectsTraversalWithoutWritingData(t *testing.T) {
	t.Parallel()
	engine, _ := newTestEngine(t)
	var payload bytes.Buffer
	archive := tar.NewWriter(&payload)
	if err := archive.WriteHeader(&tar.Header{Name: "data/../../escape", Mode: 0o600, Size: 1, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := engine.RestoreStream(&payload, false); err == nil {
		t.Fatal("expected traversal rejection")
	}
	if _, err := os.Stat(filepath.Join(engine.Root, "escape")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("escape path exists: %v", err)
	}
}

func TestRestoreReplacesOnlyEmptyBootstrapSkeleton(t *testing.T) {
	t.Parallel()
	engine, _ := newTestEngine(t)
	for _, path := range []string{"data/home/agent/workspace", "data/executor", "data/cache"} {
		if err := os.MkdirAll(filepath.Join(engine.Root, path), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var payload bytes.Buffer
	archive := tar.NewWriter(&payload)
	entries := []tar.Header{
		{Name: "data", Typeflag: tar.TypeDir, Mode: 0o750},
		{Name: "data/home", Typeflag: tar.TypeDir, Mode: 0o700},
		{Name: "data/home/agent", Typeflag: tar.TypeDir, Mode: 0o700, Uid: 1000, Gid: 1000},
		{Name: "data/home/agent/restored", Typeflag: tar.TypeReg, Mode: 0o600, Size: 2, Uid: 1000, Gid: 1000},
		{Name: "data/executor", Typeflag: tar.TypeDir, Mode: 0o700},
	}
	for index := range entries {
		if err := archive.WriteHeader(&entries[index]); err != nil {
			t.Fatal(err)
		}
		if entries[index].Size > 0 {
			if _, err := archive.Write([]byte("ok")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	dataBefore, err := os.Stat(filepath.Join(engine.Root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.RestoreStream(&payload, false); err != nil {
		t.Fatal(err)
	}
	dataAfter, err := os.Stat(filepath.Join(engine.Root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(dataBefore, dataAfter) {
		t.Fatal("restore replaced the data mountpoint")
	}
	if got := dataAfter.Mode().Perm(); got != 0o750 {
		t.Fatalf("restored data root mode = %o", got)
	}
	contents, err := os.ReadFile(filepath.Join(engine.Root, "data/home/agent/restored"))
	if err != nil || string(contents) != "ok" {
		t.Fatalf("restored file = %q, err %v", contents, err)
	}
	if _, err := os.Stat(filepath.Join(engine.Root, "data/cache")); err != nil {
		t.Fatalf("restore removed cache mount skeleton: %v", err)
	}
}

func TestBackupRestorePreservesDataRootAndLinuxModeBits(t *testing.T) {
	t.Parallel()
	source, _ := newTestEngine(t)
	data := filepath.Join(source.Root, "data")
	directory := filepath.Join(data, "home/agent/workspace/private")
	file := filepath.Join(directory, "tool")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(data, "executor"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("tool"), 0o600); err != nil {
		t.Fatal(err)
	}
	for path, mode := range map[string]fs.FileMode{
		data:      fs.ModeSticky | 0o750,
		directory: fs.ModeSetgid | fs.ModeSticky | 0o770,
		file:      fs.ModeSetuid | fs.ModeSetgid | 0o751,
	} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	var payload bytes.Buffer
	if err := source.BackupStream(context.Background(), &payload, nil); err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(bytes.NewReader(payload.Bytes()))
	header, err := reader.Next()
	if err != nil {
		t.Fatal(err)
	}
	if header.Name != "data" || header.Typeflag != tar.TypeDir || header.Mode != 0o1750 {
		t.Fatalf("data root header = %#v", header)
	}

	destination, _ := newTestEngine(t)
	for _, path := range []string{"data/home/agent/workspace", "data/executor", "data/cache"} {
		if err := os.MkdirAll(filepath.Join(destination.Root, path), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := destination.RestoreStream(bytes.NewReader(payload.Bytes()), false); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]fs.FileMode{
		filepath.Join(destination.Root, "data"):                                   fs.ModeSticky | 0o750,
		filepath.Join(destination.Root, "data/home/agent/workspace/private"):      fs.ModeSetgid | fs.ModeSticky | 0o770,
		filepath.Join(destination.Root, "data/home/agent/workspace/private/tool"): fs.ModeSetuid | fs.ModeSetgid | 0o751,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky); got != want {
			t.Fatalf("restored mode for %s = %v, want %v", path, got, want)
		}
	}
}

func TestRestoreRecoversUncommittedRootMetadataBeforeRetry(t *testing.T) {
	t.Parallel()
	engine, _ := newTestEngine(t)
	paths, err := newPaths(engine.Root)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"data/home/agent/workspace", "data/executor"} {
		if err := os.MkdirAll(filepath.Join(engine.Root, path), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(paths.data, 0o700); err != nil {
		t.Fatal(err)
	}
	journal := pathRestoreJournal{
		Schema: ProtocolSchema, Phase: "publishing",
		Staging: filepath.Join(paths.data, ".restore-"+strings.Repeat("0", 32)),
		DataRoot: &pathRestoreRoot{
			Before: archiveMetadata{Mode: 0o700, UID: os.Getuid(), GID: os.Getgid()},
			After:  archiveMetadata{Mode: 0o1755, UID: os.Getuid(), GID: os.Getgid()},
		},
	}
	for index, scope := range []string{"home", "executor"} {
		destination := filepath.Join(paths.data, scope)
		old := destination + fmt.Sprintf(".hermes-box-old-%d-%d", os.Getpid(), index)
		if err := os.Rename(destination, old); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(destination, 0o700); err != nil {
			t.Fatal(err)
		}
		journal.Entries = append(journal.Entries, pathRestoreEntry{
			Scope: scope, Destination: destination, Old: old, HadOriginal: true, Installed: true,
		})
	}
	if err := os.Chmod(paths.data, fs.ModeSticky|0o755); err != nil {
		t.Fatal(err)
	}
	if err := atomicJSON(paths.restoreJournal, journal, 0o600); err != nil {
		t.Fatal(err)
	}

	var payload bytes.Buffer
	archive := tar.NewWriter(&payload)
	entries := []tar.Header{
		{Name: "data", Typeflag: tar.TypeDir, Mode: 0o1750, Uid: os.Getuid(), Gid: os.Getgid()},
		{Name: "data/home", Typeflag: tar.TypeDir, Mode: 0o700, Uid: os.Getuid(), Gid: os.Getgid()},
		{Name: "data/home/agent", Typeflag: tar.TypeDir, Mode: 0o700, Uid: os.Getuid(), Gid: os.Getgid()},
		{Name: "data/home/agent/restored", Typeflag: tar.TypeReg, Mode: 0o600, Size: 2, Uid: os.Getuid(), Gid: os.Getgid()},
		{Name: "data/executor", Typeflag: tar.TypeDir, Mode: 0o700, Uid: os.Getuid(), Gid: os.Getgid()},
	}
	for index := range entries {
		if err := archive.WriteHeader(&entries[index]); err != nil {
			t.Fatal(err)
		}
		if entries[index].Size > 0 {
			if _, err := archive.Write([]byte("ok")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := engine.RestoreStream(&payload, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths.restoreJournal); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restore journal remains after retry: %v", err)
	}
	info, err := os.Stat(paths.data)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode() & (fs.ModePerm | fs.ModeSticky); got != fs.ModeSticky|0o750 {
		t.Fatalf("data root mode after retry = %v", got)
	}
	if contents, err := os.ReadFile(filepath.Join(paths.data, "home/agent/restored")); err != nil || string(contents) != "ok" {
		t.Fatalf("retry contents = %q, err %v", contents, err)
	}
}

func TestRestoreRecoveryReappliesCommittedRootMetadata(t *testing.T) {
	t.Parallel()
	engine, _ := newTestEngine(t)
	paths, err := newPaths(engine.Root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(paths.data, "home"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(paths.data, "executor"), 0o700); err != nil {
		t.Fatal(err)
	}
	journal := pathRestoreJournal{
		Schema: ProtocolSchema, Phase: "publishing", Committed: true,
		Staging: filepath.Join(paths.data, ".restore-"+strings.Repeat("0", 32)),
		DataRoot: &pathRestoreRoot{
			Before: archiveMetadata{Mode: 0o700, UID: os.Getuid(), GID: os.Getgid()},
			After:  archiveMetadata{Mode: 0o1750, UID: os.Getuid(), GID: os.Getgid()},
		},
	}
	for index, scope := range []string{"home", "executor"} {
		destination := filepath.Join(paths.data, scope)
		journal.Entries = append(journal.Entries, pathRestoreEntry{
			Scope: scope, Destination: destination,
			Old: destination + fmt.Sprintf(".hermes-box-old-%d-%d", os.Getpid(), index), Installed: true,
		})
	}
	if err := atomicJSON(paths.restoreJournal, journal, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(paths.data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := engine.recoverPathRestore(paths); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(paths.data)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode() & (fs.ModePerm | fs.ModeSticky); got != fs.ModeSticky|0o750 {
		t.Fatalf("committed data root mode = %v", got)
	}
	if _, err := os.Stat(paths.restoreJournal); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("committed restore journal remains: %v", err)
	}
}

func TestRestoreRejectsDuplicateDataRootHeader(t *testing.T) {
	t.Parallel()
	engine, _ := newTestEngine(t)
	var payload bytes.Buffer
	archive := tar.NewWriter(&payload)
	for range 2 {
		if err := archive.WriteHeader(&tar.Header{Name: "data", Typeflag: tar.TypeDir, Mode: 0o700}); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := engine.RestoreStream(&payload, false); err == nil || !strings.Contains(err.Error(), "exactly one normalized data root") {
		t.Fatalf("duplicate data root error = %v", err)
	}
}

func TestRestorePathsAtomicallyReplacesOnlyComponentScope(t *testing.T) {
	t.Parallel()
	engine, _ := newTestEngine(t)
	destination := filepath.Join(engine.Root, "data/home/agent/.codex")
	if err := os.MkdirAll(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "old"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	var payload bytes.Buffer
	archive := tar.NewWriter(&payload)
	if err := archive.WriteHeader(&tar.Header{Name: "data/home/agent/.codex", Typeflag: tar.TypeDir, Mode: 0o700, Uid: 1000, Gid: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := archive.WriteHeader(&tar.Header{Name: "data/home/agent/.codex/auth.json", Typeflag: tar.TypeReg, Mode: 0o600, Size: 3, Uid: 1000, Gid: 1000}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := engine.RestorePaths(context.Background(), component.Codex, &payload); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(destination, "auth.json"))
	if err != nil || string(contents) != "new" {
		t.Fatalf("restored contents = %q, err %v", contents, err)
	}
	if _, err := os.Stat(filepath.Join(destination, "old")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old component state remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(engine.Root, "var/lib/hermes-box/restore-paths.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restore journal remains: %v", err)
	}
}

func TestRestorePathsRejectsCrossComponentEntry(t *testing.T) {
	t.Parallel()
	engine, _ := newTestEngine(t)
	var payload bytes.Buffer
	archive := tar.NewWriter(&payload)
	if err := archive.WriteHeader(&tar.Header{Name: "data/executor/secret", Typeflag: tar.TypeReg, Mode: 0o600, Size: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := engine.RestorePaths(context.Background(), component.Codex, &payload); err == nil {
		t.Fatal("cross-component snapshot entry was accepted")
	}
}

func TestRestorePathsRejectsEntriesBeneathSymlinkAncestors(t *testing.T) {
	t.Parallel()
	engine, _ := newTestEngine(t)
	if err := os.MkdirAll(filepath.Join(engine.Root, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	var payload bytes.Buffer
	archive := tar.NewWriter(&payload)
	entries := []tar.Header{
		{Name: "data/home/agent/.codex", Typeflag: tar.TypeDir, Mode: 0o700, Uid: 1000, Gid: 1000},
		{Name: "data/home/agent/.codex/link", Typeflag: tar.TypeSymlink, Linkname: ".", Mode: 0o777, Uid: 1000, Gid: 1000},
		{Name: "data/home/agent/.codex/link/secret", Typeflag: tar.TypeReg, Mode: 0o600, Size: 1, Uid: 1000, Gid: 1000},
	}
	for index := range entries {
		if err := archive.WriteHeader(&entries[index]); err != nil {
			t.Fatal(err)
		}
		if entries[index].Size > 0 {
			if _, err := archive.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := engine.RestorePaths(context.Background(), component.Codex, &payload); err == nil || !strings.Contains(err.Error(), "symlink ancestor") {
		t.Fatalf("symlink ancestor error = %v", err)
	}
}

func TestRestorePathsRejectsSymlinkTargetOutsideComponentScope(t *testing.T) {
	t.Parallel()
	engine, _ := newTestEngine(t)
	if err := os.MkdirAll(filepath.Join(engine.Root, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	var payload bytes.Buffer
	archive := tar.NewWriter(&payload)
	if err := archive.WriteHeader(&tar.Header{Name: "data/home/agent/.codex", Typeflag: tar.TypeDir, Mode: 0o700, Uid: 1000, Gid: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := archive.WriteHeader(&tar.Header{Name: "data/home/agent/.codex/escape", Typeflag: tar.TypeSymlink, Linkname: "../.claude", Mode: 0o777, Uid: 1000, Gid: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := engine.RestorePaths(context.Background(), component.Codex, &payload); err == nil || !strings.Contains(err.Error(), "escapes component scope") {
		t.Fatalf("cross-scope symlink error = %v", err)
	}
}

func TestRestorePathsPublishesHostAbsentMarker(t *testing.T) {
	t.Parallel()
	engine, _ := newTestEngine(t)
	destination := filepath.Join(engine.Root, "data/home/agent/.codex")
	if err := os.MkdirAll(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "candidate-state"), []byte("remove me"), 0o600); err != nil {
		t.Fatal(err)
	}
	var payload bytes.Buffer
	archive := tar.NewWriter(&payload)
	header := &tar.Header{
		Name: "data/home/agent/.codex", Typeflag: tar.TypeDir, Mode: 0o700,
		Uid: 1000, Gid: 1000, PAXRecords: map[string]string{"HERMESBOX.absent": "1"},
	}
	if err := archive.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := engine.RestorePaths(context.Background(), component.Codex, &payload); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absent snapshot left component state behind: %v", err)
	}
}

func TestPendingPathRestoreIsReportedAndRecoveredBeforeMutation(t *testing.T) {
	t.Parallel()
	engine, _ := newTestEngine(t)
	initialize(t, engine)
	paths, err := newPaths(engine.Root)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(paths.data, "home/agent/.codex")
	if err := os.MkdirAll(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "original"), []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := destination + ".hermes-box-old-123-0"
	if err := os.Rename(destination, old); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "partial"), []byte("candidate"), 0o600); err != nil {
		t.Fatal(err)
	}
	journal := pathRestoreJournal{
		Schema: ProtocolSchema, Component: component.Codex, Phase: "publishing",
		Staging: filepath.Join(paths.data, ".restore-paths-"+strings.Repeat("0", 32)),
		Entries: []pathRestoreEntry{{
			Scope: "home/agent/.codex", Destination: destination, Old: old,
			HadOriginal: true, Installed: true,
		}},
	}
	if err := atomicJSON(paths.restoreJournal, journal, 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := engine.Status()
	if err != nil {
		t.Fatal(err)
	}
	if status.RestorePending == nil || status.RestorePending.Component != component.Codex {
		t.Fatalf("pending path restore was not surfaced: %#v", status.RestorePending)
	}
	if _, err := engine.Apply(context.Background(), []component.Spec{treeSpec(t, component.Codex, "2")}, false, true, "schema: 1\ncodex: 2\n"); err != nil {
		t.Fatal(err)
	}
	if contents, err := os.ReadFile(filepath.Join(destination, "original")); err != nil || string(contents) != "safe" {
		t.Fatalf("mutation did not recover prior durable state first: %q, %v", contents, err)
	}
	if _, err := os.Stat(filepath.Join(destination, "partial")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial restore state survived recovery: %v", err)
	}
	if _, err := os.Stat(paths.restoreJournal); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restore journal remains after mutation: %v", err)
	}
}

func fullRestorePayload(t *testing.T, marker string) []byte {
	t.Helper()
	var payload bytes.Buffer
	archive := tar.NewWriter(&payload)
	entries := []struct {
		header tar.Header
		body   string
	}{
		{header: tar.Header{Name: "data", Typeflag: tar.TypeDir, Mode: 0o755, Uid: 1000, Gid: 1000}},
		{header: tar.Header{Name: "data/home", Typeflag: tar.TypeDir, Mode: 0o700, Uid: 1000, Gid: 1000}},
		{header: tar.Header{Name: "data/home/agent", Typeflag: tar.TypeDir, Mode: 0o700, Uid: 1000, Gid: 1000}},
		{header: tar.Header{Name: "data/home/agent/workspace", Typeflag: tar.TypeDir, Mode: 0o700, Uid: 1000, Gid: 1000}},
		{header: tar.Header{Name: "data/home/agent/workspace/marker", Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(marker)), Uid: 1000, Gid: 1000}, body: marker},
		{header: tar.Header{Name: "data/executor", Typeflag: tar.TypeDir, Mode: 0o700, Uid: 1000, Gid: 1000}},
		{header: tar.Header{Name: "data/executor/marker", Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(marker)), Uid: 1000, Gid: 1000}, body: marker},
	}
	for index := range entries {
		if err := archive.WriteHeader(&entries[index].header); err != nil {
			t.Fatal(err)
		}
		if entries[index].body != "" {
			if _, err := io.WriteString(archive, entries[index].body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return payload.Bytes()
}

func codexSnapshotPayload(t *testing.T, marker string) []byte {
	t.Helper()
	var payload bytes.Buffer
	archive := tar.NewWriter(&payload)
	directory := tar.Header{Name: "data/home/agent/.codex", Typeflag: tar.TypeDir, Mode: 0o700, Uid: 1000, Gid: 1000}
	if err := archive.WriteHeader(&directory); err != nil {
		t.Fatal(err)
	}
	file := tar.Header{Name: "data/home/agent/.codex/marker", Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(marker)), Uid: 1000, Gid: 1000}
	if err := archive.WriteHeader(&file); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(archive, marker); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return payload.Bytes()
}

func seedDurableMarkers(t *testing.T, root, marker string) {
	t.Helper()
	for _, relative := range []string{"data/home/agent/workspace/marker", "data/executor/marker"} {
		path := filepath.Join(root, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(marker), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func assertDurableMarkers(t *testing.T, root, marker string) {
	t.Helper()
	for _, relative := range []string{"data/home/agent/workspace/marker", "data/executor/marker"} {
		contents, err := os.ReadFile(filepath.Join(root, relative))
		if err != nil || string(contents) != marker {
			t.Fatalf("%s = %q, %v; want %q", relative, contents, err, marker)
		}
	}
}

func TestRestoreReplaceModeIsExplicitAndPreservesNormalSafety(t *testing.T) {
	t.Parallel()
	engine, _ := newTestEngine(t)
	seedDurableMarkers(t, engine.Root, "original")
	payload := fullRestorePayload(t, "replacement")
	if err := engine.RestoreStream(bytes.NewReader(payload), false); err == nil || !strings.Contains(err.Error(), "already contains durable state") {
		t.Fatalf("normal restore into nonempty data error = %v", err)
	}
	assertDurableMarkers(t, engine.Root, "original")
	if err := engine.RestoreStream(bytes.NewReader(payload), true); err != nil {
		t.Fatal(err)
	}
	assertDurableMarkers(t, engine.Root, "replacement")
	paths, err := newPaths(engine.Root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths.restoreJournal); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful replacement left journal: %v", err)
	}
}

func TestReplaceRestorePowerLossRollsBackFromDurableDataJournal(t *testing.T) {
	t.Parallel()
	for _, boundary := range []string{"old-durable:home", "candidate-durable:home", "candidate-durable:executor"} {
		t.Run(boundary, func(t *testing.T) {
			engine, runner := newTestEngine(t)
			seedDurableMarkers(t, engine.Root, "original")
			engine.durabilityCheckpointHook = func(name string) {
				if name == boundary {
					panic("simulated power loss")
				}
			}
			crashed := false
			func() {
				defer func() { crashed = recover() != nil }()
				_ = engine.RestoreStream(bytes.NewReader(fullRestorePayload(t, "candidate")), true)
			}()
			if !crashed {
				t.Fatal("restore did not reach injected power-loss boundary")
			}
			paths, err := newPaths(engine.Root)
			if err != nil {
				t.Fatal(err)
			}
			if !fileExists(paths.restoreJournal) {
				t.Fatal("power loss did not leave the durable data journal")
			}
			restarted := New(engine.Root)
			restarted.Runner = runner
			restarted.Stdout = io.Discard
			restarted.Stderr = io.Discard
			if _, err := restarted.Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertDurableMarkers(t, engine.Root, "original")
			if _, err := os.Stat(paths.restoreJournal); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("recovery left data journal: %v", err)
			}
		})
	}
}

func TestReplaceRestorePowerLossAfterCommitKeepsCandidate(t *testing.T) {
	t.Parallel()
	engine, runner := newTestEngine(t)
	seedDurableMarkers(t, engine.Root, "original")
	engine.durabilityCheckpointHook = func(name string) {
		if name == "commit-durable" {
			panic("simulated power loss")
		}
	}
	func() {
		defer func() { _ = recover() }()
		_ = engine.RestoreStream(bytes.NewReader(fullRestorePayload(t, "candidate")), true)
	}()
	restarted := New(engine.Root)
	restarted.Runner = runner
	restarted.Stdout = io.Discard
	restarted.Stderr = io.Discard
	if _, err := restarted.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertDurableMarkers(t, engine.Root, "candidate")
	matches, err := filepath.Glob(filepath.Join(engine.Root, "data/*.hermes-box-old-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("committed recovery retained rollback copies: %#v, %v", matches, err)
	}
}

func TestActivationPowerLossRecoversFsyncedPreviousSymlink(t *testing.T) {
	t.Parallel()
	engine, _ := newTestEngine(t)
	initialize(t, engine)
	engine.durabilityCheckpointHook = func(name string) {
		if name == "activation-durable:codex" {
			panic("simulated power loss")
		}
	}
	func() {
		defer func() { _ = recover() }()
		_, _ = engine.Apply(context.Background(), []component.Spec{treeSpec(t, component.Codex, "2")}, false, true, "schema: 1\ncodex: 2\n")
	}()
	engine.durabilityCheckpointHook = nil
	status, err := engine.Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Applied.Components[component.Codex] != "1" {
		t.Fatalf("recovered codex pin = %q", status.Applied.Components[component.Codex])
	}
	paths, err := newPaths(engine.Root)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyActivation(paths, component.Codex, "1"); err != nil {
		t.Fatal(err)
	}
}

func TestCommittedRecoveryRejectsTamperedActivationSymlink(t *testing.T) {
	t.Parallel()
	engine, _ := newTestEngine(t)
	initialize(t, engine)
	paths, err := newPaths(engine.Root)
	if err != nil {
		t.Fatal(err)
	}
	journal := Journal{Schema: 1, Component: component.Codex, Previous: "0", Candidate: "1", Phase: "committed"}
	if err := atomicJSON(paths.journal, journal, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(activationPath(paths, component.Codex)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../releases/codex/tampered", activationPath(paths, component.Codex)); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Recover(context.Background()); err == nil || !errors.Is(err, errIntegrity) {
		t.Fatalf("tampered committed activation error = %v", err)
	}
	if _, err := os.Stat(paths.journal); err != nil {
		t.Fatalf("tampered recovery cleared forensic journal: %v", err)
	}
}

func TestRestoreCrashAfterJournalBeforeStagingRetriesExactly(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"full", "scoped"} {
		t.Run(mode, func(t *testing.T) {
			engine, _ := newTestEngine(t)
			unrelated := filepath.Join(engine.Root, "data/.restore-unrelated")
			if err := os.MkdirAll(unrelated, 0o700); err != nil {
				t.Fatal(err)
			}
			engine.durabilityCheckpointHook = func(name string) {
				if name == "extraction-journal-durable" {
					panic("simulated crash after journal and before staging")
				}
			}
			crashed := false
			func() {
				defer func() { crashed = recover() != nil }()
				if mode == "full" {
					_ = engine.RestoreStream(bytes.NewReader(fullRestorePayload(t, "replacement")), false)
				} else {
					_ = engine.RestorePaths(context.Background(), component.Codex, bytes.NewReader(codexSnapshotPayload(t, "replacement")))
				}
			}()
			if !crashed {
				t.Fatal("restore did not reach pre-staging crash boundary")
			}
			paths, err := newPaths(engine.Root)
			if err != nil {
				t.Fatal(err)
			}
			var journal pathRestoreJournal
			if err := readJSON(paths.restoreJournal, &journal); err != nil {
				t.Fatal(err)
			}
			if journal.Phase != "extracting" {
				t.Fatalf("crash journal = %#v", journal)
			}
			if _, err := os.Stat(journal.Staging); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("staging existed before its durable creation: %v", err)
			}
			engine.durabilityCheckpointHook = nil
			if mode == "full" {
				if err := engine.RestoreStream(bytes.NewReader(fullRestorePayload(t, "replacement")), false); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := engine.RestorePaths(context.Background(), component.Codex, bytes.NewReader(codexSnapshotPayload(t, "replacement"))); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := os.Stat(unrelated); err != nil {
				t.Fatalf("retry broad-deleted unrelated restore-like path: %v", err)
			}
		})
	}
}

func TestRestoreJournalParentSyncPrecedesStagingCreation(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"full", "scoped"} {
		t.Run(mode, func(t *testing.T) {
			engine, _ := newTestEngine(t)
			paths, err := newPaths(engine.Root)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(paths.data, 0o755); err != nil {
				t.Fatal(err)
			}
			injected := errors.New("injected /data journal-parent sync failure")
			journalDurable := false
			engine.durabilityCheckpointHook = func(name string) {
				if name == "extraction-journal-durable" {
					journalDurable = true
				}
			}
			engine.syncDirectoryHook = func(path string) error {
				if path == paths.data {
					return injected
				}
				return syncDirectory(path)
			}
			if mode == "full" {
				err = engine.RestoreStream(bytes.NewReader(fullRestorePayload(t, "replacement")), false)
			} else {
				err = engine.RestorePaths(context.Background(), component.Codex, bytes.NewReader(codexSnapshotPayload(t, "replacement")))
			}
			if !errors.Is(err, injected) {
				t.Fatalf("parent sync error = %v", err)
			}
			if journalDurable {
				t.Fatal("journal was declared durable before /data was synced")
			}
			entries, err := os.ReadDir(paths.data)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".restore-") {
					t.Fatalf("staging %q was created before journal parent sync", entry.Name())
				}
			}
			if _, err := os.Stat(paths.restoreJournal); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed parent sync left journal: %v", err)
			}
		})
	}
}

func TestRestoreExtractionSIGKILLHelper(t *testing.T) {
	root := os.Getenv("HERMES_BOX_SIGKILL_ROOT")
	if root == "" {
		return
	}
	payload, err := os.ReadFile(os.Getenv("HERMES_BOX_SIGKILL_PAYLOAD"))
	if err != nil {
		t.Fatal(err)
	}
	engine := New(root)
	engine.Runner = &fakeRunner{active: map[string]bool{}}
	engine.Stdout = io.Discard
	engine.Stderr = io.Discard
	ready := os.Getenv("HERMES_BOX_SIGKILL_READY")
	engine.durabilityCheckpointHook = func(name string) {
		if strings.HasPrefix(name, "extraction-entry:") {
			if err := os.WriteFile(ready, []byte(name), 0o600); err != nil {
				panic(err)
			}
			for {
				time.Sleep(time.Hour)
			}
		}
	}
	if os.Getenv("HERMES_BOX_SIGKILL_MODE") == "full" {
		err = engine.RestoreStream(bytes.NewReader(payload), false)
	} else {
		err = engine.RestorePaths(context.Background(), component.Codex, bytes.NewReader(payload))
	}
	t.Fatalf("restore returned before SIGKILL: %v", err)
}

func TestRestoreExtractionSIGKILLRetryCleansOnlyJournalOwnedStaging(t *testing.T) {
	for _, mode := range []string{"full", "scoped"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			unrelated := filepath.Join(root, "data/.restore-unrelated")
			if err := os.MkdirAll(unrelated, 0o700); err != nil {
				t.Fatal(err)
			}
			payload := fullRestorePayload(t, "replacement")
			if mode == "scoped" {
				payload = codexSnapshotPayload(t, "replacement")
				seedDurableMarkers(t, root, "original")
			}
			payloadPath := filepath.Join(root, "payload.tar")
			if err := os.WriteFile(payloadPath, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			ready := filepath.Join(root, "ready")
			command := exec.Command(os.Args[0], "-test.run=^TestRestoreExtractionSIGKILLHelper$")
			command.Env = append(os.Environ(),
				"HERMES_BOX_SIGKILL_ROOT="+root,
				"HERMES_BOX_SIGKILL_PAYLOAD="+payloadPath,
				"HERMES_BOX_SIGKILL_READY="+ready,
				"HERMES_BOX_SIGKILL_MODE="+mode,
			)
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(10 * time.Second)
			for {
				if _, err := os.Stat(ready); err == nil {
					break
				} else if !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				if time.Now().After(deadline) {
					_ = command.Process.Kill()
					_, _ = command.Process.Wait()
					t.Fatal("restore helper did not reach extraction before timeout")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err := command.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := command.Wait(); err == nil {
				t.Fatal("SIGKILL helper exited successfully")
			}
			paths, err := newPaths(root)
			if err != nil {
				t.Fatal(err)
			}
			var journal pathRestoreJournal
			if err := readJSON(paths.restoreJournal, &journal); err != nil {
				t.Fatal(err)
			}
			if journal.Phase != "extracting" || journal.Staging == "" {
				t.Fatalf("SIGKILL journal = %#v", journal)
			}
			if _, err := os.Stat(journal.Staging); err != nil {
				t.Fatalf("SIGKILL did not leave owned staging for retry cleanup: %v", err)
			}
			engine := New(root)
			engine.Runner = &fakeRunner{active: map[string]bool{}}
			engine.Stdout = io.Discard
			engine.Stderr = io.Discard
			if mode == "full" {
				if err := engine.RestoreStream(bytes.NewReader(payload), false); err != nil {
					t.Fatal(err)
				}
				assertDurableMarkers(t, root, "replacement")
			} else {
				if err := engine.RestorePaths(context.Background(), component.Codex, bytes.NewReader(payload)); err != nil {
					t.Fatal(err)
				}
				contents, err := os.ReadFile(filepath.Join(root, "data/home/agent/.codex/marker"))
				if err != nil || string(contents) != "replacement" {
					t.Fatalf("scoped retry marker = %q, %v", contents, err)
				}
			}
			if _, err := os.Stat(journal.Staging); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("retry left SIGKILL staging: %v", err)
			}
			if _, err := os.Stat(unrelated); err != nil {
				t.Fatalf("retry broad-deleted unrelated restore-like path: %v", err)
			}
			if _, err := os.Stat(paths.restoreJournal); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("retry left extraction journal: %v", err)
			}
		})
	}
}

func TestProtocolExecPreservesArgvBoundaries(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HERMES_BOX_GUEST_TEST_ROOT", root)
	request := Request{Schema: 1, Operation: "exec", Root: root, Directory: root, Argv: []string{"printf", "%s", "a; echo injected"}}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := Serve(context.Background(), bytes.NewReader(append(encoded, '\n')), &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	var response struct {
		OK     bool `json:"ok"`
		Result struct {
			Stdout string `json:"stdout"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.OK || response.Result.Stdout != "a; echo injected" {
		t.Fatalf("unexpected response: %s", output.String())
	}
}

func TestProtocolRequestLimitIsAppliedBeforeUnboundedRead(t *testing.T) {
	t.Parallel()
	payload := bytes.Repeat([]byte{'x'}, maximumRequestBytes+1024)
	counted := &countingReader{Reader: bytes.NewReader(payload)}
	var output bytes.Buffer
	err := Serve(context.Background(), counted, &output, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "request exceeds 4 MiB") {
		t.Fatalf("oversized request error = %v", err)
	}
	if counted.read > maximumRequestBytes+1 {
		t.Fatalf("request reader consumed %d bytes before rejecting limit", counted.read)
	}
}

func TestProtocolRejectsFieldsOutsideOperationSchema(t *testing.T) {
	t.Parallel()
	for _, request := range []Request{
		{Schema: 1, Operation: "status", Argv: []string{"unexpected"}},
		{Schema: 1, Operation: "status", ReplaceExisting: true},
		{Schema: 1, Operation: "exec", Argv: []string{"true"}, Environment: map[string]string{"PATH": "/tmp"}},
		{Schema: 1, Operation: "restore-paths", Component: "unknown"},
	} {
		encoded, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		if err := Serve(context.Background(), bytes.NewReader(append(encoded, '\n')), &output, io.Discard); err == nil {
			t.Fatalf("invalid request was accepted: %#v", request)
		}
		var response Response
		if err := json.Unmarshal(output.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.OK || response.Error == nil || response.Error.Code != "invalid_input" {
			t.Fatalf("invalid request response = %#v", response)
		}
	}
}

func TestProtocolRestoreReplaceModeReportsExactCommittedMode(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HERMES_BOX_GUEST_TEST_ROOT", root)
	seedDurableMarkers(t, root, "original")
	request := Request{Schema: 1, Operation: "restore-stream", Root: root, ReplaceExisting: true}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	input := append(append(encoded, '\n'), fullRestorePayload(t, "replacement")...)
	var output bytes.Buffer
	runner := &fakeRunner{active: map[string]bool{}}
	if err := serve(context.Background(), bytes.NewReader(input), &output, io.Discard, runner); err != nil {
		t.Fatal(err)
	}
	var response struct {
		OK     bool `json:"ok"`
		Result struct {
			Restored        bool `json:"restored"`
			ReplaceExisting bool `json:"replace_existing"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.OK || !response.Result.Restored || !response.Result.ReplaceExisting {
		t.Fatalf("replace restore response = %s", output.String())
	}
	assertDurableMarkers(t, root, "replacement")
}

func TestProtocolResponseFrameIsBounded(t *testing.T) {
	t.Parallel()
	response := Response{Schema: 1, OK: true, Result: strings.Repeat("x", maximumResponseBytes)}
	if err := encodeResponse(io.Discard, response); err == nil || !strings.Contains(err.Error(), "response exceeds") {
		t.Fatalf("oversized response error = %v", err)
	}
}

type countingReader struct {
	io.Reader
	read int
}

func (reader *countingReader) Read(value []byte) (int, error) {
	count, err := reader.Reader.Read(value)
	reader.read += count
	return count, err
}

func TestReadRequestLinePreservesDirectStreamBody(t *testing.T) {
	t.Parallel()
	line, stream, err := readRequestLine(strings.NewReader("{\"schema\":1}\nraw-stream"))
	if err != nil {
		t.Fatal(err)
	}
	if string(line) != "{\"schema\":1}\n" {
		t.Fatalf("request line = %q", line)
	}
	body, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "raw-stream" {
		t.Fatalf("stream body = %q", body)
	}
}

func TestClassifyUsesTypedErrorsWithSpecificPrecedence(t *testing.T) {
	t.Parallel()
	if got := classify(errors.New("busy invalid sha256 health")); got != "external_failed" {
		t.Fatalf("message text classified as %q", got)
	}
	integrityInsideInvalid := withClass(errInvalidInput, withClass(errIntegrity, errors.New("bad artifact")))
	if got := classify(integrityInsideInvalid); got != "integrity_failed" {
		t.Fatalf("integrity precedence classified as %q", got)
	}
	busyInsideHealth := withClass(errHealth, withClass(errBusy, errors.New("active process")))
	if got := classify(busyInsideHealth); got != "busy" {
		t.Fatalf("busy precedence classified as %q", got)
	}
}

func TestServeReservesStdoutForOneJSONResponse(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HERMES_BOX_GUEST_TEST_ROOT", root)
	runner := &fakeRunner{active: map[string]bool{}, stdout: "child stdout\n"}
	request := Request{
		Schema: 1, Operation: "apply", Root: root, Initial: true,
		Components: initialSpecs(t), ReviewedLock: "schema: 1\ninitial: true\n",
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var output, diagnostics bytes.Buffer
	if err := serve(context.Background(), bytes.NewReader(append(encoded, '\n')), &output, &diagnostics, runner); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&output)
	var response Response
	if err := decoder.Decode(&response); err != nil {
		t.Fatal(err)
	}
	if !response.OK {
		t.Fatalf("response was not successful: %#v", response)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("protocol stdout contains more than one JSON value: %v", err)
	}
	if !strings.Contains(diagnostics.String(), "child stdout") {
		t.Fatalf("child stdout was not routed to diagnostics: %q", diagnostics.String())
	}
}

func TestServeBackupStreamWritesHeaderThenTarOnly(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HERMES_BOX_GUEST_TEST_ROOT", root)
	for path, contents := range map[string]string{
		"data/home/agent/workspace/readme.txt": "workspace",
		"data/executor/database":               "executor",
	} {
		absolute := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(absolute, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runner := &fakeRunner{
		active: map[string]bool{"hermes.service": true, "executor.service": true},
		stdout: "child stdout\n",
	}
	request := Request{Schema: 1, Operation: "backup-stream", Root: root}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var output, diagnostics bytes.Buffer
	if err := serve(context.Background(), bytes.NewReader(append(encoded, '\n')), &output, &diagnostics, runner); err != nil {
		t.Fatal(err)
	}
	stream := bufio.NewReader(&output)
	headerLine, err := stream.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var header Response
	if err := json.Unmarshal(headerLine, &header); err != nil {
		t.Fatalf("decode stream header: %v; line %q", err, headerLine)
	}
	result, ok := header.Result.(map[string]any)
	if !header.OK || !ok || result["stream"] != "tar" || result["framing"] != "direct" || result["owner"] != "guest" {
		t.Fatalf("unexpected stream header: %#v", header)
	}
	archive := tar.NewReader(stream)
	entries := map[string]string{}
	for {
		entry, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if entry.Typeflag == tar.TypeReg {
			contents, err := io.ReadAll(archive)
			if err != nil {
				t.Fatal(err)
			}
			entries[entry.Name] = string(contents)
		}
	}
	if entries["data/home/agent/workspace/readme.txt"] != "workspace" || entries["data/executor/database"] != "executor" {
		t.Fatalf("unexpected tar entries: %#v", entries)
	}
	if !strings.Contains(diagnostics.String(), "child stdout") {
		t.Fatalf("child stdout was not routed to diagnostics: %q", diagnostics.String())
	}
}

type gatedStreamWriter struct {
	mu          sync.Mutex
	header      bytes.Buffer
	headerReady chan struct{}
	release     chan struct{}
	streamBytes int64
	once        sync.Once
}

func (writer *gatedStreamWriter) Write(value []byte) (int, error) {
	writer.mu.Lock()
	if writer.header.Len() == 0 {
		_, _ = writer.header.Write(value)
		writer.mu.Unlock()
		writer.once.Do(func() { close(writer.headerReady) })
		return len(value), nil
	}
	writer.mu.Unlock()
	<-writer.release
	writer.mu.Lock()
	writer.streamBytes += int64(len(value))
	writer.mu.Unlock()
	return len(value), nil
}

func TestServeBackupStreamsLargeArchiveImmediatelyWithoutRootSpool(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HERMES_BOX_GUEST_TEST_ROOT", root)
	for _, directory := range []string{"data/home/agent/workspace", "data/executor"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	large := filepath.Join(root, "data/home/agent/workspace/large.bin")
	file, err := os.Create(large)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(32 << 20); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	request := Request{Schema: 1, Operation: "backup-stream", Root: root}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	writer := &gatedStreamWriter{headerReady: make(chan struct{}), release: make(chan struct{})}
	runner := &fakeRunner{active: map[string]bool{"hermes.service": true, "executor.service": true}}
	done := make(chan error, 1)
	go func() {
		done <- serve(context.Background(), bytes.NewReader(append(encoded, '\n')), writer, io.Discard, runner)
	}()
	<-writer.headerReady
	writer.mu.Lock()
	header := append([]byte(nil), writer.header.Bytes()...)
	writer.mu.Unlock()
	var response Response
	if err := json.Unmarshal(bytes.TrimSpace(header), &response); err != nil || !response.OK {
		t.Fatalf("direct stream header = %q, err %v", header, err)
	}
	matches, err := filepath.Glob(filepath.Join(root, "var/lib/hermes-box/backup-*.tar"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("backup used disposable-root spool: %#v, err %v", matches, err)
	}
	close(writer.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	writer.mu.Lock()
	streamBytes := writer.streamBytes
	writer.mu.Unlock()
	if streamBytes < 32<<20 {
		t.Fatalf("stream bytes = %d, want at least large file payload", streamBytes)
	}
}

type cancelStreamWriter struct {
	cancel context.CancelFunc
	writes int
}

func (writer *cancelStreamWriter) Write(value []byte) (int, error) {
	writer.writes++
	if writer.writes == 1 {
		return len(value), nil
	}
	writer.cancel()
	return 0, context.Canceled
}

func TestServeBackupDisconnectThawsAndRestarts(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HERMES_BOX_GUEST_TEST_ROOT", root)
	for _, path := range []string{"data/home/agent/workspace", "data/executor"} {
		if err := os.MkdirAll(filepath.Join(root, path), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	runner := &fakeRunner{active: map[string]bool{
		"hermes.service": true, "executor.service": true, "user-1000.slice": true,
	}}
	request := Request{Schema: 1, Operation: "backup-stream", Root: root}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	writer := &cancelStreamWriter{cancel: cancel}
	err = serve(ctx, bytes.NewReader(append(encoded, '\n')), writer, io.Discard, runner)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("disconnect error = %v", err)
	}
	if !runner.active["hermes.service"] || !runner.active["executor.service"] || !runner.active["user-1000.slice"] {
		t.Fatalf("disconnect did not fully resume guest: %#v", runner.active)
	}
	var unfroze, thawed bool
	for _, argv := range runner.commands {
		unfroze = unfroze || reflect.DeepEqual(argv, []string{"/usr/sbin/fsfreeze", "--unfreeze", filepath.Join(root, "data")})
		thawed = thawed || reflect.DeepEqual(argv, []string{"systemctl", "thaw", "user-1000.slice"})
	}
	if !unfroze || !thawed {
		t.Fatalf("disconnect cleanup commands = %#v", runner.commands)
	}
}

func TestGuestAssetsPreserveTerminalAndRecoveryContracts(t *testing.T) {
	t.Parallel()
	checks := map[string][]string{
		"../../guest/tmux.conf": {
			"set -g mouse on", "bg=#006400,fg=white", "allow-passthrough on",
			"extended-keys always", "extended-keys-format csi-u",
		},
		"../../guest/xterm-ghostty.terminfo": {"xterm-ghostty", "use=xterm-256color"},
		"../../guest/bootstrap.sh":           {"tic -x", "infocmp -x xterm-ghostty"},
		"../../guest/tm":                     {"session=main", "workdir=/home/agent/workspace", "attach-session"},
		"../../guest/hermes.service": {
			"Requires=hermes-box-recover.service",
		},
		"../../guest/executor.service": {
			"/usr/bin/podman run", "--volume /data/executor:/data:Z", "127.0.0.1:4788:4788",
		},
		"../../guest/hermes-box-recover": {
			"/var/lib/hermes-box/update.json", "/data/.hermes-box/restore-paths.json",
		},
	}
	for path, needles := range checks {
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, needle := range needles {
			if !bytes.Contains(contents, []byte(needle)) {
				t.Errorf("%s does not contain %q", path, needle)
			}
		}
	}
}
