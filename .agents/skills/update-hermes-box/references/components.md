# Hermes Box component playbooks

Use only the sections for the requested component. Rediscover current versions
and paths on every run; the examples below describe stable seams, not permanent
version numbers.

## Contents

1. Hermes Agent
2. smolvm
3. uv
4. Ubuntu base image
5. Codex
6. Executor
7. Node.js
8. Go and CI actions
9. Suggested PR boundaries and estimates

## 1. Hermes Agent

Hermes is not a simple version-string bump. Hermes Box patches a reviewed
upstream commit to add the `gated` approval mode.

### Discovery

1. Resolve the latest stable Hermes release tag to its exact commit.
2. Compare the current configured commit to the candidate.
3. Verify the candidate `scripts/install.sh` SHA-256.
4. Confirm the required Python range and installer-managed Python version.
5. Confirm every extra installed by `guest/bootstrap.sh` still exists.

### Rehearsal

Clone the candidate into `/tmp`. Compare at least:

- `tools/approval.py`
- `gateway/run.py`
- `agent/auxiliary_client.py`
- `scripts/install.sh`
- `pyproject.toml`
- `uv.lock`

Run each current patch function against a disposable copy of the matching new
upstream file. Every anchor must match exactly once. Passing anchors are only a
starting point: review the surrounding new semantics for duplicated or
conflicting approval logic.

### Repository changes

Usually update:

- `guest/patch-hermes-gated-approval.py` expected commit and version.
- `guest/bootstrap.sh` supported commit, installer digest when changed,
  comments, extras, and post-install assertions.
- `internal/config/config.go` default commit.
- `hermes-box.conf.example`, README, and static/config tests.
- Gated-approval unit and patched-upstream integration tests when upstream
  control flow changed.

### Proof

- Compile all patched upstream modules.
- Run `tests/hermes-gated-approval.py` against the patched candidate source.
- Prove approve, deny, escalate, hardline precedence, context cleanup, and
  Responses API service-tier forwarding.
- Run a fresh isolated image build and verify `hermes --version`.
- Verify Discord/messaging imports and the Supervisor gateway.
- Exercise one real human-escalation path and one model-gated path when
  credentials are available and the user authorizes it.

Reject the candidate if patch semantics cannot be proven fail-closed.

## 2. smolvm

smolvm owns VM boot, OCI extraction, networking, persistent disks, packing,
and machine state. Treat even a minor release as a runtime migration.

### Discovery

1. Download the official Darwin ARM64 release and checksums to `/tmp`.
2. Verify the archive checksum before executing the candidate binary.
3. Compare `--version` and help for every command Hermes Box calls:
   `machine create`, `update`, `start`, `stop`, `exec`, `cp`, `status`,
   `data-dir`, `delete`, and `pack create`.
4. Compare upstream changes touching OCI extraction, ownership, whiteouts,
   networking, storage, overlay mounts, pruning, and pack caches.
5. Search the repo for every old-version string and workaround.

### Repository changes

Update the exact-version preflight, download guidance, docs, backup fixtures,
host tests, and any error messages. Reassess but do not automatically remove:

- Builder `machine update --net` reapplication.
- Network-mode fail-closed errors.
- Pack extraction-cache sizing and marker verification.
- Root ownership repair after packing.
- Workspace seed and persistent-disk workarounds.

### Proof matrix

Use only isolated resources. Prove:

- Fresh lean and Executor-enabled lifecycle runs.
- Ubuntu base pull, provisioning, pack, first boot, and restart.
- Root/file ownership and sudo integrity.
- OCI whiteouts and Executor extraction.
- Snapshot, candidate restore, rollback, and portable restore.
- Compatibility with machine state created by the old smolvm version using a
  disposable copy, never the primary machine.
- Pack-cache reuse and cleanup.
- Loopback-only SSH/Executor port publication.

Re-run the direct-IP, unlisted-host, `--no-net`, localhost-only, and hostname
allowlist probes before changing any network containment claim. Keep modes
disabled if the candidate still bypasses them.

## 3. uv

The code change is small; qualification is the work. Hermes Box pins uv because
a newer candidate previously deadlocked while building Hermes in smolvm.

### Discovery and update

1. Read every release note from the current pin through the candidate.
2. Look specifically for resolver, editable install, Python selection,
   concurrency, filesystem, and lockfile changes.
3. Download the ARM64 GNU/Linux archive and `.sha256` from the official release.
4. Verify the checksum and artifact attestation when available.
5. Update `uv_version`, `uv_archive_sha256`, matching docs, and static tests.

### Bounded proof

Run the same Hermes installer stages and locked `uv sync` command used by
`guest/bootstrap.sh` under a hard timeout. Prove:

1. Cold builder with empty caches.
2. Second builder or retry with populated caches.
3. Lean install and messaging/`all` extras.
4. Correct managed Python and venv ownership.
5. No stuck uv/build processes after timeout or failure.

If the candidate hangs, capture process state and retain the known-good pin.
Release notes that do not mention the deadlock are not evidence of a fix.

## 4. Ubuntu base image

Update both the builder image in Go and the `Smolfile` image. Add a test that
prevents those values from drifting.

Before changing them:

- Confirm the official Linux ARM64 OCI manifest exists.
- Check required package availability and renamed utilities.
- Review default Python, OpenSSH, sudo, tmux/ncurses, coreutils, and service
  behavior.
- Run the full isolated lifecycle because local checks cannot execute guest
  provisioning.

Document that existing machines and archived root filesystems do not upgrade in
place. A restore preserves the snapshot's OS; creating a new box is the OS
migration path unless a separately designed migration exists.

## 5. Codex

Resolve the official release and ARM64 Linux artifact, verify its SHA-256, and
update the pinned version/digest together. Prove first-boot installation,
`codex --version`, device-auth persistence, `codex update` compatibility,
tmux/Shift+Enter behavior, and snapshot restore of `CODEX_HOME`.

## 6. Executor

Resolve the release tag to the immutable multi-platform OCI index digest and
verify its Linux ARM64 child. Update config defaults, tests, docs, and runtime
manifest expectations together. Prove extraction safety, OCI whiteouts,
Supervisor health, portal health, persistence under `/workspace/executor/data`,
MCP `execute`/`resume`, IPv4 fallback behavior, and restore without bundling the
repullable runtime.

## 7. Node.js

Decide whether the project intentionally follows `latest-v<major>.x` or pins a
patch release. Preserve checksum verification and safe archive ownership. Prove
`node`, `npm`, `npx`, and `corepack` links as both root and Hermes, plus
snapshot/restore behavior. Do not call a moving patch selector reproducible.

## 8. Go and CI actions

Separate the `go.mod` language/toolchain directive from the minimum supported
host security baseline. Update preflight validation, docs, CI, and tests
together when raising the host minimum.

Pin GitHub Actions to reviewed full commit SHAs, keep a human-readable release
comment, and set `persist-credentials: false` for checkout when the workflow
does not push.

## 9. Suggested PR boundaries and estimates

Use estimates only after inspecting the current candidate:

- **Pin/checksum-only component with stable runtime contract:** a few hours.
- **Hermes source upgrade with passing patch anchors:** roughly half to one day,
  dominated by semantic review and lifecycle proof.
- **Previously failing uv candidate:** roughly half a day if it passes; longer
  if the hang needs diagnosis.
- **smolvm runtime upgrade:** roughly one to two days because machine-state,
  networking, packing, ownership, and restore behavior all require proof.
- **Several coupled runtime upgrades in one PR:** usually two to three days and
  harder failure attribution. Prefer staged PRs unless a combined matrix is the
  explicit goal.

These are engineering ranges, not promises. Increase them when upstream has a
large semantic diff, a backup migration, or an unresolved lifecycle failure.
