# Hermes Box v2 component playbooks

Read this reference completely, then apply only the selected component
sections. Rediscover all current values on each run; filenames below identify
stable seams, not permanent versions.

## Contents

1. Shared release and transaction contracts
2. Hermes Agent
3. Lima, Ubuntu, and the guest provisioner
4. uv, Node.js, and Hermes Python
5. Claude Code
6. Codex
7. Executor
8. Go and CI actions
9. PR boundaries

## 1. Shared release and transaction contracts

The reviewed update chain is:

```text
official immutable input
  -> release/pins.env
  -> qualification.lock.template
  -> reproducible repository-owned artifacts
  -> published immutable URLs and attestations
  -> generated candidate lock
  -> isolated Lima lifecycle proof
  -> reviewed root hermes-box.lock
  -> explicit update or rebuild
```

Runtime commands never discover, select, or write versions. Application drift
is applied with `hermes-box update COMPONENT|all`. Ubuntu, provisioner,
foundational package, and Lima-compatibility drift is applied by `rebuild`.

The host materializes exact lock inputs in its content-addressed cache and
uploads them. The static guest helper owns install, smoke, atomic activation,
health, rollback, and crash recovery. Preserve that boundary.

Before a noninitial update, the host creates an age-encrypted component snapshot
using the fixed scope in `internal/component/component.go`. Guest backup streaming
stops affected services, freezes the active `agent` user slice when taking a
full backup, freezes `/data`, streams the archive, then thaws the filesystem,
user slice, and previously running services on every exit path. Component
snapshots reject active Claude or Codex processes rather than freezing the
whole user slice. Do not replace this with destructive session termination.

Every full recovery backup embeds the applied lock plus the exact Ubuntu,
provisioner, Node, uv, Claude, Codex, gated Hermes source, Python, Hermes wheel,
and Executor OCI inputs. Any lock-schema or artifact change must update and test
that closure.

## 2. Hermes Agent

Hermes is a coupled four-input release:

- exact upstream commit and source checksum;
- deterministic gated source artifact owned by this repository;
- exact Python standalone archive;
- complete Linux ARM64 wheel closure for the reviewed `uv.lock`.

### Discovery

1. Resolve the stable tag to its exact commit and archive checksum.
2. Compare `pyproject.toml`, `uv.lock`, approval flow, gateway startup,
   messaging imports, and MCP configuration from the current commit.
3. Confirm the selected Python version satisfies the project range.
4. Verify every dependency has a compatible hashed Linux ARM64 wheel.
5. Review whether the local gated-approval contract is now upstream. Do not
   silently retain two competing implementations.

### Release construction

`release/build-hermes-source.sh` fetches the exact commit, verifies `uv.lock`,
applies the fail-closed patch in `release/hermes/`, runs the approval regression
suite, removes Git metadata, and creates a deterministic source archive.

`release/build-hermes-wheels.sh` exports locked `all` dependencies, downloads
only hashed binary wheels against the pinned standalone Python, creates a
manifest, and proves `uv sync --locked --offline --no-index` plus gated approval
and `hermes --help` before packaging the wheelhouse.

Provisioning never patches source. The guest trusts only the already reviewed
gated source artifact, verifies its archive and `uv.lock`, creates a relocatable
venv with the pinned uv and Python, performs an offline sync, compiles the
source, reruns approval tests, and only then exposes `bin/hermes`.

### Patch surface and proof

Usually update:

- `release/pins.env` and `release/qualification.lock.template`;
- `release/hermes/` patcher, module, and tests;
- deterministic artifact names in release scripts/workflow/validator;
- lock/config fixtures, guest installer expectations, backup closure, and docs.

Prove exact patch anchors, approve/deny/escalate/hardline precedence, context
cleanup, service-tier forwarding, offline install, messaging imports, temporary
gateway health, update rollback of `/data/home/agent/.hermes`, and rebuild from
the self-contained backup. Reject any ambiguous patch or incomplete wheel
closure.

## 3. Lima, Ubuntu, and the guest provisioner

Lima is a manually installed host prerequisite. The lock records the qualified
version; Hermes Box verifies it but never upgrades it. Ubuntu is an exact dated
official ARM64 cloud image. Platform changes are always `rebuild`, never
in-place mutation.

The separately named `<box>-data` Lima disk is durable. The VM and root disk are
replaceable. Preserve exact resource ownership and never discover destructive
targets by prefix.

### Discovery

For Lima, compare every used command and YAML contract: version, VM create,
start/stop/delete, shell/copy/inspect, disk create/attach/delete, Apple VZ,
mounts, and loopback forwarding. Verify the official Darwin ARM64 archive and
checksum.

For Ubuntu, verify the dated ARM64 cloud image checksum and boot compatibility.
Review OpenSSH, systemd, tmux/ncurses, Podman, util-linux/fsfreeze, sudo, archive
tools, certificates, and package renames.

For the provisioner, update `release/provisioner-packages.in`; resolve the exact
`.deb` closure only inside the pinned Ubuntu 26.04 ARM64 OCI child; and rebuild
the deterministic archive containing the static guest helper, bootstrap,
systemd units, sudoers, tmux assets, package manifest, and checksums.

### Proof

Prove fresh create, data-disk persistence, no host mounts/agent/socket
forwarding, loopback-only Executor, clean stop/start cycles, full backup, root
rebuild, automatic recovery from desired-root failure using the captured prior
applied lock and artifacts, and cleanup of only isolated resources.

Do not claim existing roots changed after a platform pin edit. The operator must
install a newly qualified Lima binary first when needed, then run `rebuild`.

## 4. uv, Node.js, and Hermes Python

Node and uv are versioned root tooling. Claude depends on Node; Hermes depends
on Node and uv. A tooling update is healthy only when its dependents pass.

### Node

Pin an exact patch release and official Linux ARM64 archive checksum. Do not use
`latest-v<major>.x`. Prove archive layout, `node`, `npm`, `npx`, and `corepack`,
offline Claude tarball installation, Hermes health, and rollback with snapshots
of both `.claude` and `.hermes`.

### uv

Pin the official `aarch64-unknown-linux-gnu` archive and checksum. Read all
release notes from the current pin, especially resolver, lock, offline,
relocatable-venv, Python selection, concurrency, and filesystem changes.

Reproduce the exact guest Hermes path under a hard timeout:

```text
uv venv --relocatable --python <pinned-python> <venv>
uv sync --project <gated-source> --extra all --locked --offline --no-managed-python
```

Run cold and cache/retry cases, verify no stuck processes, run approval tests
and gateway health, and prove rollback of `.hermes`. Retain the current pin if
the candidate hangs or mutates the lock.

### Python

Python is a Hermes input rather than a standalone update target. Verify the
exact python-build-standalone ARM64 archive, ABI compatibility, wheel closure,
relocatable venv, and complete offline replay. Update Python, wheel artifact
naming, release scripts, lock template, and Hermes proof together.

## 5. Claude Code

Claude is installed from an exact official npm tarball using its registry
SHA-512 SRI. The guest invokes the pinned Node/npm with offline mode,
`--ignore-scripts`, a versioned prefix, and update/audit/funding notifications
disabled. It must produce exactly the expected `bin/claude` surface.

Update the version, tarball URL, and SRI together. Prove:

- SRI verification and content-addressed caching;
- offline trusted installation and `claude --version`;
- `claude doctor` without clearing authentication;
- vendor self-update remains nonauthoritative;
- persisted `/home/agent/.claude` state;
- interactive-process busy rejection;
- encrypted `.claude` snapshot restoration on failed update and rollback;
- tmux extended-key and `Shift+Enter` behavior.

## 6. Codex

Pin the exact official Linux ARM64 musl bundle and SHA-256. The guest extracts
it into a versioned release, requires one Codex executable, normalizes it to
`bin/codex`, and activates it atomically.

Prove `codex --strict-config --version`, `codex --help`, and `codex doctor`;
device-auth persistence under `CODEX_HOME=/home/agent/.codex`; vendor
self-update nonauthority; interactive-process busy rejection; encrypted
`.codex` snapshot restoration; backup/restore; and tmux `Shift+Enter`.

If the upstream bundle layout or compression changes, update the trusted
installer and its adversarial archive tests rather than adding a shell fallback.

## 7. Executor

Pin both the official multi-platform OCI index digest and selected Linux ARM64
child digest. The host resolves that exact child, exports a verified OCI tar to
its content-addressed cache, and uploads it. The guest loads it into system
Podman and writes only the child-digest-qualified runtime reference to the
systemd environment. The Podman image store remains on disposable root.

Prove:

- index-to-child selection and archive digest validation;
- system Podman load with `--pull=never`;
- loopback-only `127.0.0.1:4788` publication;
- `/data/executor:/data` persistence and portal health;
- authenticated MCP exposes only reviewed `execute` and `resume` tools;
- Hermes discovery after `setup executor`;
- encrypted `/data/executor` snapshot before update;
- failed-update and explicit rollback restore the pre-update database before
  starting the previous image;
- backup/restore does not preserve the disposable Podman store but does embed
  the exact OCI input.

Never replace the child digest with a mutable tag or treat the index digest as
proof of the selected runtime bytes.

## 8. Go and CI actions

Keep the minimum supported host Go release separate from the exact release
builder toolchain in `release/pins.env`. A Go update may change both the host CLI
and static Linux ARM64 guest helper, so rerun deterministic provisioner builds,
protocol tests, and the isolated lifecycle.

Pin GitHub Actions to reviewed full commit SHAs with readable release comments.
Keep checkout `persist-credentials: false`. Qualification jobs build twice on
native Linux ARM64; publication jobs alone receive `contents: write`,
`id-token: write`, and attestation permissions.

## 9. PR boundaries

Prefer separate PRs for independently reversible boundaries:

- one application pin with stable artifact/install contracts;
- a Hermes commit plus gated source and complete offline wheel closure;
- Node, uv, or Python when it rebuilds and requalifies Hermes dependencies;
- Ubuntu, Lima, or provisioner changes requiring root rebuild proof;
- Go/protocol or recovery-format changes;
- CI action-only maintenance.

Combine components only when one cannot be qualified without the others. Every
PR must say whether operators run `update COMPONENT|all` or `rebuild`, which
encrypted durable scopes are protected, whether recovery format changed, and
which lifecycle paths were actually exercised.
