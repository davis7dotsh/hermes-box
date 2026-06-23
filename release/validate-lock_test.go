package main

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/davis7dotsh/hermes-box/internal/config"
)

func TestValidateLifecyclePairRequiresEveryComponentChange(t *testing.T) {
	baseline, candidate := lifecycleLocks()
	if err := validateLifecyclePair(baseline, candidate); err != nil {
		t.Fatalf("validate complete lifecycle pair: %v", err)
	}

	candidate.Tooling.UV = baseline.Tooling.UV
	err := validateLifecyclePair(baseline, candidate)
	if err == nil || !strings.Contains(err.Error(), "does not change uv") {
		t.Fatalf("expected unchanged uv rejection, got %v", err)
	}
}

func TestStatusEnvelopeShape(t *testing.T) {
	status := statusEnvelope{Schema: 1, OK: true}
	status.Result.Components = map[string]struct {
		Desired  string  `json:"desired"`
		Applied  string  `json:"applied"`
		Previous *string `json:"previous"`
		State    string  `json:"state"`
	}{
		"node": {Desired: "candidate", Applied: "baseline", State: "drifted"},
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	var decoded statusEnvelope
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if got := decoded.Result.Components["node"].State; got != "drifted" {
		t.Fatalf("decoded state = %q", got)
	}
}

func TestComponentPinUsesExecutorChildDigest(t *testing.T) {
	lock := config.Lock{Executor: config.ExecutorLock{
		Image:            "registry.invalid/executor:tag@sha256:index",
		LinuxARM64Digest: "sha256:child",
	}}
	if got := componentPin(lock, "executor"); got != "sha256:child" {
		t.Fatalf("componentPin(executor) = %q", got)
	}
}

func TestAssertStatusRejectsOversizedInputBeforeTrailingJSON(t *testing.T) {
	// Keep a regression check on the bounded parser without invoking the
	// process-exiting assertion helper.
	decoder := json.NewDecoder(io.LimitReader(strings.NewReader(strings.Repeat(" ", 1<<20)+"{}"), 1<<20))
	var envelope statusEnvelope
	if err := decoder.Decode(&envelope); err == nil {
		t.Fatal("oversized status unexpectedly decoded")
	}
}

func TestValidateLifecyclePairRejectsPlatformDrift(t *testing.T) {
	baseline, candidate := lifecycleLocks()
	candidate.Ubuntu.Release = "different"
	err := validateLifecyclePair(baseline, candidate)
	if err == nil || !strings.Contains(err.Error(), "identical host and Ubuntu") {
		t.Fatalf("expected platform drift rejection, got %v", err)
	}
}

func lifecycleLocks() (config.Lock, config.Lock) {
	baseline := config.Lock{
		Host:     config.HostLock{Lima: "lima-baseline"},
		Ubuntu:   config.UbuntuLock{Release: "ubuntu-baseline"},
		Tooling:  config.ToolingLock{Node: config.ToolLock{Version: "node-baseline"}, UV: config.ToolLock{Version: "uv-baseline"}},
		Claude:   config.ClaudeLock{Version: "claude-baseline"},
		Codex:    config.CodexLock{Version: "codex-baseline"},
		Hermes:   config.HermesLock{Commit: "hermes-baseline"},
		Executor: config.ExecutorLock{Image: "executor-baseline"},
	}
	candidate := baseline
	candidate.Tooling.Node.Version = "node-candidate"
	candidate.Tooling.UV.Version = "uv-candidate"
	candidate.Claude.Version = "claude-candidate"
	candidate.Codex.Version = "codex-candidate"
	candidate.Hermes.Commit = "hermes-candidate"
	candidate.Executor.Image = "executor-candidate"
	return baseline, candidate
}
