# Hermes Box v2: Architecture and CLI Specification

Status: implementation contract
Audience: implementers, operators, and reviewers
Primary design goal: the smallest reliable Ubuntu agent box that can be
rebuilt and updated without preserving a mutated operating-system image

## 1. Summary

Hermes Box v2 runs one Ubuntu 26.04 ARM64 virtual machine on an Apple Silicon
Mac. The box includes Claude Code, Codex, Hermes Agent, and Executor by
default.

The operating system is disposable. User work, authentication, application
state, and Executor data live on one persistent data disk. Installed software
is reconstructed from a reviewed lock file rather than restored from a root
filesystem snapshot.

The public product is one Go command named `hermes-box`. One configuration
directory owns exactly one box. The product has no host daemon, public HTTP
API, plugin system, multi-box registry, or compatibility layer for v1 machines
and backups. Automation uses the same CLI with `--json`.

## 2. Design principles

The implementation must follow these rules in priority order:

1. **The root disk is replaceable.** Never back it up or treat it as user
   state.
2. **One persistent disk owns all durable guest state.** A fresh VM must become
   the same box when that disk and the same lock file are attached.
3. **Desired versions are data.** Exact versions, commits, image digests, and
   checksums belong in `hermes-box.lock`.
4. **Updates are explicit and transactional per component.** Starting a box
   never installs or updates software.
5. **Rebuild instead of migrating the OS.** Ubuntu and VM-runtime changes use
   a new root disk.
6. **Fail closed on integrity and backup errors.** A partial install or archive
   never becomes current.
7. **Expose one honest security boundary.** The VM protects the host. Processes
   inside the VM are not presented as mutually hostile security domains.
8. **Prefer explicit downtime to a complicated cutover protocol.** This is one
   personal box, not a clustered service.
9. **Keep the command surface small.** New commands require a distinct user
   outcome that cannot fit an existing command.
10. **Human authentication remains human.** Device codes, browser grants, and
    first-admin creation are handed off clearly rather than automated around.

## 3. Explicit non-goals

v2 does not provide:

- Compatibility with smolvm machines, v1 rootfs snapshots, or v1 portable
  packages.
- In-place Ubuntu release upgrades.
- Multiple Linux distributions or non-ARM guests.
- Multiple active nodes, high availability, or live migration.
- Host-directory mounts, host SSH-agent forwarding, Docker-socket forwarding,
  or GPU passthrough.
- A claim that Executor credentials are hidden from an agent with guest sudo.
- Host or LAN network containment beyond NAT and loopback-only published ports.
- A web control plane for Hermes Box itself.
- Automatic Git commits, branches, or pull requests from operational update
  commands.

## 4. System architecture

### 4.1 Host components

The host contains:

- The `hermes-box` Go CLI.
- Lima using the Apple Virtualization framework.
- Repository-owned `hermes-box.yaml` and `hermes-box.lock` files.
- Per-box metadata, backups, and locks under `HERMES_BOX_HOME`.
- Per-box backup encryption identities in macOS Keychain.

The default host state root is:

```text
~/.hermes-box/
├── boxes/
│   └── main.json             Non-secret runtime metadata
├── backups/
│   └── main/                 Encrypted durable-state backups
├── artifacts/                Content-addressed verified install inputs
├── locks/
│   └── main.lock             Host operation lock
└── logs/
    └── main/                 Host orchestration logs
```

Lima owns the root VM and data-disk files in its normal state directory. Hermes
Box records only their logical names. It does not edit Lima's private files or
adopt resources created by another configuration directory.

The persistent disk is a separately named Lima disk, `<name>-data`, not an
extra volume hidden inside the VM definition. `create` creates and owns it,
`rebuild` detaches and reattaches it, and `destroy` removes it only after a
verified final backup. Host metadata records the exact VM and disk names so
cleanup never selects resources by a broad prefix.

`boxes/<name>.json` also records the canonical absolute configuration-directory
path as its ownership identity. Every mutating command verifies that identity;
the same box name from another checkout is a conflict, not an invitation to
adopt the VM.

### 4.2 VM components

The VM is created from a checksum-verified official Ubuntu 26.04 ARM64 cloud
image. Its root disk contains only reconstructable state:

```text
Ubuntu 26.04 root disk
├── systemd
├── OpenSSH client/server
├── tmux and terminfo
├── Git, curl, ca-certificates, jq, ripgrep, zstd
├── Podman
├── Node.js runtime
├── uv
├── /usr/local/libexec/hermes-box-guest
├── /opt/hermes-box/tooling/
│   ├── node/<version>/
│   ├── uv/<version>/
│   └── current/
│       ├── node -> ../node/<version>
│       └── uv   -> ../uv/<version>
├── /opt/hermes-box/releases/
│   ├── claude/<version>/
│   ├── codex/<version>/
│   └── hermes/<commit>/
├── /opt/hermes-box/current/
    ├── claude -> ../releases/claude/<version>
    ├── codex  -> ../releases/codex/<version>
    └── hermes -> ../releases/hermes/<commit>
└── /var/lib/hermes-box/
    ├── applied.lock          Root releases actually installed
    ├── applied.json          Applied component pins
    ├── releases.json         Activation and rollback metadata
    ├── update.json           Tiny activation journal; absent when idle
    └── restore-paths.json    Scoped-restore journal; absent when idle
```

There is no builder VM and no packed Hermes Box image. Cloud-init creates the
base user, mounts the data disk, installs the small OS package set, writes the
systemd units, and installs the guest transaction helper. The host resolves and
verifies locked artifacts into its content-addressed cache, uploads the exact
inputs, and invokes one guest transaction. All install, smoke, activation,
health, and rollback logic lives in the guest helper; the host never
reimplements component installers.

### 4.3 Persistent data disk

One ext4 disk is mounted at `/data`. It uses a fixed `agent` UID and GID of
`1000` on every VM.

```text
/data/
├── home/agent/
│   ├── .claude/              Claude settings, auth, and history
│   ├── .codex/               Codex settings, auth, sessions, and skills
│   ├── .hermes/              Hermes config, auth, sessions, memory, and skills
│   └── workspace/            Default working directory
├── executor/                 Executor database, keys, and application state
└── cache/                    Disposable downloads; excluded from backup
```

`/data/home/agent` is bind-mounted at `/home/agent`. `/workspace` is a stable
symlink to `/home/agent/workspace`.

The data disk never contains application executables, container images, or a
copy of the Ubuntu root. Root-applied metadata lives with the releases it
describes. A rebuild therefore begins with no applied state, installs every
locked release, and offers no component rollback until a later update creates a
new previous release.

The host artifact cache is persistent but reconstructable and is not guest
state. A verified backup embeds every exact lock-pinned guest install input so
restore does not depend on npm, GitHub, GHCR, or Ubuntu continuing to serve an
old artifact, nor on PyPI retaining a compatible Python distribution or wheel.

### 4.4 Guest users and privileges

The VM has one human and agent account:

- `agent`: interactive SSH user, owner of the persistent home and workspace,
  and runtime user for Hermes.

`agent` has passwordless sudo inside the VM. This is an intentional autonomy
choice. The VM boundary, not Unix privilege separation inside the guest,
protects the host.

Root owns systemd units, the guest transaction helper, and `/opt/hermes-box`.
Application state remains owned by `agent`.

### 4.5 Services

Ubuntu boots normal systemd. v2 does not use Supervisor.

The persistent services are:

- `hermes.service`: runs the pinned Hermes gateway as `agent`.
- `executor.service`: runs the digest-pinned Executor image through system
  Podman and mounts `/data/executor` at `/data` in the container. Podman's image
  store remains on the disposable root disk.

SSH is the standard `sshd.service`. Claude Code and Codex are interactive
commands, not services.

`hermes.service` orders itself after Executor but does not require it. Hermes
must remain usable when Executor is stopped or unhealthy. During `create` and
`rebuild`, neither application unit is enabled or started until the initial
guest transaction succeeds. On later boots, `hermes-box-recover.service` runs
before both units and resolves only an interrupted root-local activation; it
never applies repository-lock drift.

### 4.6 Networking

The guest has normal NAT egress. v2 exposes no `strict` or `none` networking
mode until the underlying runtime can prove those guarantees.

Rules:

- No host directory is mounted.
- No host SSH agent or container socket is forwarded.
- Executor port `4788` is forwarded to host loopback only.
- SSH is reached through Lima's managed SSH transport; no user-managed SSH
  port or long-lived project key is required.
- Guest services may listen only on guest loopback unless they are explicitly
  selected for host loopback forwarding.

## 5. Desired-state files

### 5.1 `hermes-box.yaml`

`hermes-box.yaml` is small, human-edited, and safe to commit.

```yaml
schema: 1
name: main

vm:
  cpus: 4
  memory: 8GiB
  root_disk: 30GiB
  data_disk: 50GiB

ports:
  executor: 4788

backup:
  keep: 5
```

Only settings that users are expected to choose belong here. Component
versions, source URLs, checksums, runtime-generated names, and credentials do
not.

Validation rules:

- `schema` must be exactly `1`.
- `name` must match `[a-z][a-z0-9-]{0,31}`.
- CPU, memory, and disk values must be positive and within Lima-supported
  limits.
- Host ports must be in `1024..65535` and free before creation or start.
- Unknown keys are errors.

### 5.2 `hermes-box.lock`

`hermes-box.lock` is a reviewable, input-only file that is safe to commit. It
is the complete application desired-state contract. An upgrade PR may generate
or edit it, but runtime commands never modify it. It pins the Ubuntu image and
guest provisioner bundle. That bundle contains the static guest helper, units,
configuration templates, and exact `.deb` inputs required after first boot, so
guest reconstruction does not depend on a later Ubuntu repository state.

```yaml
schema: 1

host:
  lima: "2.x-qualified-version"

ubuntu:
  release: "26.04"
  image: "https://cloud-images.ubuntu.com/...arm64.img"
  sha256: "..."
  provisioner: "...arm64.tar.zst"
  provisioner_sha256: "..."

tooling:
  node:
    version: "..."
    archive: "...linux-arm64.tar.xz"
    sha256: "..."
  uv:
    version: "..."
    archive: "...aarch64-unknown-linux-gnu.tar.gz"
    sha256: "..."

claude:
  version: "..."
  package: "@anthropic-ai/claude-code"
  tarball: "...claude-code-<version>.tgz"
  integrity: "sha512-..."

codex:
  version: "..."
  archive: "...linux-arm64..."
  sha256: "..."

hermes:
  repository: "https://github.com/<reviewed-upstream>/hermes-agent.git"
  commit: "..."
  archive: "...<commit>.tar.gz"
  sha256: "..."
  uv_lock_sha256: "..."
  python_archive: "...cpython-aarch64-linux.tar.zst"
  python_sha256: "..."
  wheels_archive: "...hermes-wheels-aarch64-linux.tar.zst"
  wheels_sha256: "..."

executor:
  image: "ghcr.io/...:<tag>@sha256:<index-digest>"
  linux_arm64_digest: "sha256:..."
```

Every downloaded executable or archive requires an official checksum or a
reviewed checksum recorded in this file. Moving tags are resolved to immutable
commits or digests before the lock is written.

`hermes-box update` applies reviewed application differences between this file
and the VM's root-local applied state. A failed update leaves both the active
release and this file unchanged. Reviewed Ubuntu, provisioner, or Lima changes
are committed to this file and applied by `rebuild`.

### 5.3 File selection

Configuration-file selection uses this precedence:

1. `--config PATH`.
2. `HERMES_BOX_CONFIG`.
3. `./hermes-box.yaml`.

Host-state selection uses `HERMES_BOX_HOME`, then `~/.hermes-box`. Values
inside `hermes-box.yaml` have no ambient environment or command-line
overrides; the file is the one visible configuration authority.

No arbitrary `HERMES_BOX_*` component settings are supported. Versions change
through the lock file and update command, not ambient shell state.

## 6. Component installation contracts

### 6.1 Node.js and uv

Node.js and uv are guest tooling, not user state.

- Install exact ARM64 Linux releases verified by checksum.
- Install under `/opt/hermes-box/tooling/<name>/<version>`.
- Expose them through `/opt/hermes-box/tooling/current/<name>/bin`.
- Never update them implicitly while updating another component.
- Retain one previous release for rollback.

### 6.2 Claude Code

Claude Code is installed from an exact npm package version into:

```text
/opt/hermes-box/releases/claude/<version>
```

Automatic updates are disabled so the lock file remains authoritative.
`hermes-box update claude` is the supported update path. Claude's normal user
state remains under `/home/agent/.claude` on the data disk.

Candidate validation runs:

```text
claude --version
claude doctor
```

It must not modify or clear the existing authentication state during staging.

### 6.3 Codex

Codex is installed from the exact official standalone ARM64 Linux archive into:

```text
/opt/hermes-box/releases/codex/<version>
```

`CODEX_HOME=/home/agent/.codex` is persistent. The visible executable is the
active release symlink, while sessions, auth, configuration, skills, and
standalone metadata remain on the data disk.

Candidate validation runs:

```text
codex --strict-config --version
codex doctor
codex --help
```

Vendor self-updaters are disabled for every managed component. A reviewed lock
change followed by `hermes-box update codex` is the only supported Codex update
path; the same rule applies to Claude and Hermes.

### 6.4 Hermes Agent

Each Hermes release is an immutable Git checkout plus its local virtual
environment and interpreter:

```text
/opt/hermes-box/releases/hermes/<commit>/source
/opt/hermes-box/releases/hermes/<commit>/venv
/opt/hermes-box/releases/hermes/<commit>/python
```

Installation uses the pinned uv version, embedded CPython build, candidate's
reviewed lock file, and complete platform-matched wheel bundle. The release
builder exports the reviewed universal lock, filters it without dependency
resolution to a hash-locked Linux ARM64 requirements file, and builds one exact
Hermes project wheel with the pinned build tools. The guest installation is
equivalent to:

```text
uv pip install --offline --no-index --require-hashes -r requirements-linux-arm64.txt
uv pip install --offline --no-index --no-deps hermes_agent-<version>-py3-none-any.whl
```

The wheel bundle contains every transitive dependency selected for CPython 3.13
on Linux ARM64, the exact project wheel, and a manifest binding both filenames
and SHA-256 identities. Installation points uv at the embedded interpreter
explicitly, performs no lock resolution, and must pass with outbound networking
disabled.

`HERMES_HOME=/home/agent/.hermes` remains persistent and is never placed inside
the checkout.

The reviewed Hermes source must provide the approval gate used by this box.
Until that behavior is upstream, the release workflow fetches an exact upstream
commit, applies a fail-closed reviewed patch, runs the approval regression
suite, and publishes the resulting deterministic gated-source artifact. The
lock records the upstream repository and commit as provenance and the published
gated artifact and checksum as the only install input.

v2 never patches source code during provisioning. A required Hermes behavior
must be accepted upstream, provided by a separately versioned extension with a
public interface, or incorporated into a repository-owned release artifact
during the reviewed qualification workflow. Gated approval is a fixed v2
safety contract, not a runtime toggle.

Candidate validation compiles the source, runs the approval regression suite,
checks messaging imports, starts a temporary gateway, and verifies its health
before activation.

### 6.5 Executor

Executor runs its published Linux ARM64 container under system Podman. v2 does
not unpack OCI layers itself or run the bundled Bun payload directly. Rootless
Podman would add user lingering, subordinate-ID, and persistent image-store
complexity without creating a meaningful boundary from a passwordless-sudo
agent.

The lock records both the reviewed multi-platform index digest and selected
Linux ARM64 child digest. `/data/executor` is mounted into the container as its
durable `/data` directory.

An update performs:

1. Resolve and verify the reviewed Linux ARM64 child on the host, upload its
   OCI archive, and load it into system Podman with runtime pulling disabled.
2. Stop Hermes and Executor.
3. Create an encrypted snapshot of `/data/executor` only.
4. Start the candidate container against the durable data.
5. Verify HTTP health and the authenticated MCP surface.
6. Restart Hermes and verify MCP discovery.
7. Retain the previous image digest for rollback.

If the candidate fails health checks, rollback always restores the pre-update
Executor snapshot before starting the previous image. It never rolls back
unrelated workspace or agent state.

The encrypted snapshot lives under
`~/.hermes-box/backups/<name>/transactions/` and is retained with the one
previous image until the next successful Executor update replaces both.

### 6.6 Release artifact qualification

The checked-in release inputs have two classes:

- Official immutable artifacts recorded directly in `release/pins.env` and the
  qualification lock template.
- Repository-owned deterministic artifacts: the static guest provisioner,
  gated Hermes source, and complete Linux ARM64 Hermes wheel closure.

Repository-owned artifacts are built twice on native Linux ARM64 and must
produce identical SHA-256 files. The provisioner package closure is resolved
inside the pinned Ubuntu 26.04 ARM64 OCI child from one dated Ubuntu archive
snapshot. The builder verifies every signed `InRelease`, records the exact
`Packages` metadata and `.deb` hashes, and rejects live-archive indexes; the
runtime root remains the dated Canonical cloud image in the lock. PyYAML, pip,
setuptools, and every other Python build or runtime wheel are exact hashed
inputs installed with hash enforcement. The Hermes source and wheel jobs must
prove the gated approval suite and complete offline install.

The resulting archives, checksums, and attestations are published at immutable
release URLs. Those exact URLs are downloaded and verified again before the
generated candidate lock is used in the isolated Lima lifecycle matrix. Only
that proven generated lock may be promoted to the repository root in a
reviewed change. A CI artifact by itself is never a qualified lock.

## 7. Public CLI API

### 7.1 Invocation and global flags

```text
hermes-box [GLOBAL FLAGS] COMMAND [ARGS]
```

Global flags:

```text
--config PATH    Select hermes-box.yaml
--json           Emit one JSON result object on stdout
--quiet          Suppress progress; errors still print
--no-color       Disable ANSI styling
-h, --help       Show contextual help
```

Environment variables:

```text
HERMES_BOX_CONFIG  Configuration file path
HERMES_BOX_HOME    Host state root; defaults to ~/.hermes-box
NO_COLOR           Disable ANSI styling
```

The CLI writes progress and diagnostics to stderr. Successful result data goes
to stdout. With `--json`, stdout contains exactly one JSON object and no human
text.

`--json` is intentionally unsupported for `ssh`, `exec`, `logs -f`,
`completion`, and `help`, whose stdout is an interactive, streamed, or textual
payload.

### 7.2 Command list

The complete v2 command list is:

```text
create
start
stop
ssh
exec
status
logs
open
setup
update
rollback
backup
restore
rebuild
doctor
key
destroy
completion
version
help
```

No hidden advanced command set exists. Debug details belong in `doctor`,
`status --json`, or host logs.

### 7.3 `create`

```text
hermes-box create
```

Creates the configured box, data disk, and encryption identity; boots Ubuntu;
applies the lock; starts services; verifies infrastructure health; and creates
the first encrypted recovery backup before returning success.

Behavior:

- Refuses if the logical box, Lima VM, or data disk already exists.
- Verifies Lima, host architecture, image checksum, config, lock, and port
  availability before creating anything.
- Does not require Claude, Codex, Hermes, or Executor human authentication to
  return success.
- Prints a single ordered authentication handoff after infrastructure health
  passes.
- On any failure before that first backup verifies, removes only the VM, disk,
  Keychain item, and metadata created by this invocation. A retry first
  completes cleanup of any owned `incomplete-create` record, then starts fresh.

There are no `--lean`, `--without-executor`, or component-selection flags. All
four requested products are part of the v2 box.

### 7.4 `start`

```text
hermes-box start
```

Starts a stopped VM using the already applied lock, waits for the data disk,
starts services, and runs health checks. It never installs or activates a
release.

If the repository lock differs from `/var/lib/hermes-box/applied.lock`, `start`
starts the applied versions and reports drift. The operator must use `update`
for application entries or `rebuild` for platform entries.

If `/var/lib/hermes-box/update.json` records an interrupted activation, `start`
restores the recorded previous activation before starting services, verifies
it, and clears the journal. This is crash recovery, not reconciliation to the
repository lock.

Starting an already running healthy box succeeds as a no-op. Starting an
already running degraded box runs verification and returns a health error
without restarting it implicitly.

### 7.5 `stop`

```text
hermes-box stop
```

Stops Hermes and Executor, syncs the data disk, and stops the VM. Stopping an
already stopped box succeeds as a no-op.

### 7.6 `ssh`

```text
hermes-box ssh
```

Opens an interactive shell as `agent` in `/workspace` and creates or attaches
the tmux session named `main`.

Terminal requirements:

- Dark-green bottom status bar with white text.
- Mouse window selection enabled.
- True color, focus events, clipboard escapes, and extended keys preserved.
- Ghostty metadata forwarded.
- `Shift+Enter` remains distinct through tmux.
- `tm` creates or attaches the same `main` session.

Interactive detach exits the SSH connection. Reconnecting attaches the same
session.

### 7.7 `exec`

```text
hermes-box exec -- COMMAND [ARGS...]
```

Runs one noninteractive command as `agent` without tmux. It preserves argument
boundaries and passes through the remote exit status in `0..255`. It never
invokes a shell unless the caller explicitly requests one. Interactive `ssh`
likewise returns the underlying SSH client status.

### 7.8 `status`

```text
hermes-box status [--check]
```

Reports:

- Box state and Ubuntu release.
- VM CPU, memory, and disk use.
- Data-disk mount and free space.
- Hermes and Executor service health.
- Claude, Codex, Hermes, Executor, Node, and uv desired/applied versions.
- Version drift.
- Human setup requirements such as missing login or Executor first-admin
  creation.
- Host loopback forwarding state.
- Last successful backup.

`--check` also performs read-only upstream release discovery and reports
possible stable application releases. These are informational candidates, not
qualified pins; only a reviewed lock change can select one. Without it,
`status` performs no network release queries.

For a stopped box, `status` reads desired and applied pins from the repository
lock and durable host state, so drift and `--check` remain available without
starting the VM. Runtime health and running-version observations remain empty
until the box is started.

Human setup requirements are not infrastructure failures. A newly created box
may be `running` with components marked `setup-required`.

Stable `status` result shape inside the common JSON envelope:

```json
{
  "state": "running",
  "healthy": true,
  "setup_required": ["claude", "codex", "hermes", "executor"],
  "components": {},
  "storage": {},
  "ports": {},
  "last_backup": null,
  "updates": []
}
```

### 7.9 `logs`

```text
hermes-box logs [hermes|executor|recovery] [-f] [-n LINES]
```

The default target is `hermes`. `recovery` shows
`hermes-box-recover.service`. `-f` follows until interrupted.

There are no separate component-specific log commands.

### 7.10 `open`

```text
hermes-box open executor
```

Verifies the loopback listener and opens the Executor portal in the default
browser. Executor is the only v2 browser surface, so no other targets are
defined initially.

### 7.11 `setup`

```text
hermes-box setup executor [--token-stdin]
```

Completes the post-portal Executor-to-Hermes connection. It reads the
destination-local Executor token from a no-echo TTY prompt or stdin, verifies
that the authenticated MCP endpoint exposes only the reviewed `execute` and
`resume` tools, writes the token to Hermes' protected environment over stdin,
restarts Hermes, and verifies discovery. Tokens are never accepted as command
arguments or printed.

### 7.12 `update`

```text
hermes-box update COMPONENT
hermes-box update all
```

Components:

```text
claude codex hermes executor node uv all
```

Rules:

- The desired pin comes only from the repository lock. The command performs no
  version selection and never writes that file.
- A component with no desired/applied difference succeeds as a no-op.
- Only one component is activated at a time, including during `update all`.
- `update all` uses dependency order: Node, uv, Claude, Codex, Hermes, then
  Executor.
- The candidate is staged and verified before the active release changes.
- Root-local applied metadata changes only after runtime health passes.
- A failed update restores the prior release and leaves desired state
  unchanged.
- Updating Node also verifies Claude and Hermes; updating uv also verifies
  Hermes. Shared-tooling updates are not healthy until their dependents pass.
- `update all` stops at the first failure and reports already completed
  components; it does not roll back unrelated successful updates.
- Ubuntu and Lima are not update targets. They use `rebuild` after their lock
  entries have been reviewed.

Selecting a newer version, including a prerelease, is a repository change and
therefore goes through the normal branch, review, and merge workflow before
this command runs.

### 7.13 `rollback`

```text
hermes-box rollback COMPONENT
```

Components:

```text
claude codex hermes executor node uv
```

Activates the immediately previous release and verifies it. Only one previous
release is guaranteed. The repository lock is not changed, so `status` reports
drift afterward. Making the rollback permanent requires reverting the lock in
Git through review.

Every rollback restores the component-scoped encrypted transaction snapshot
defined in section 11 before reactivating the prior release. Executor therefore
restores its pre-update `/data/executor` state whenever the candidate may have
changed its database.

### 7.14 `backup`

```text
hermes-box backup [LABEL]
```

Creates an encrypted, self-contained recovery bundle.

Sequence:

1. Materialize and verify every root-applied artifact in the host cache.
2. Stop Hermes and Executor, reversibly freeze the active `agent` user slice
   (including tmux and interactive tools), and record which services were
   running.
3. Flush and freeze the `/data` filesystem.
4. Stream a read-only archive of `/data`, excluding `/data/cache`, to the host;
   unfreeze it immediately after the data stream completes.
5. Add the root-applied lock and every exact
   verified install input needed to reconstruct its guest.
6. Encrypt to the box's age recipient.
7. Write the archive and its adjacent envelope containing the archive SHA-256,
   format, filename, and recipient fingerprint.
8. Verify the archive can be opened and enumerated.
9. Thaw the `agent` user slice and restart only services that were previously
   running. Existing SSH and tmux processes resume; they are not terminated or
   recreated.

The host invokes the guest helper in a root system scope so freezing
`user-1000.slice` cannot freeze the backup process itself. An interruption trap
is installed before freezing. Every exit path first unfreezes `/data`, then
thaws the user slice, then restores previously running services. This brief,
reversible pause is the simple consistency boundary: no process can mutate
durable state while it is copied.

Output:

```text
~/.hermes-box/backups/main/
├── 20260622-120000-configured.tar.zst.age
└── 20260622-120000-configured.envelope.json
```

The external envelope records the encrypted archive SHA-256 and age recipient
fingerprint. The archive contains its own manifest describing the plaintext
contents. Neither checksum is self-referential.

The backup contains credentials, private work, the Ubuntu image, and exact
component install inputs, but no root filesystem, installed release, Lima
binary, or host key. This intentionally trades backup size for a restore that
does not depend on old upstream artifacts remaining available.

After a new backup and envelope both verify, retention removes archives beyond
the most recent `backup.keep` count. It never deletes the only valid backup.

### 7.15 `restore`

```text
hermes-box --config DESTINATION/hermes-box.yaml restore BACKUP --identity PATH [--lock PATH]
```

Restore always targets an absent box described by a destination configuration
directory. It refuses to replace an existing VM or data disk. The age identity
is an explicit input so cross-host recovery does not depend on a pre-existing
Keychain entry. The identity must match the recipient fingerprint in the
backup envelope.

Sequence:

1. Verify checksum and decrypt the archive.
2. Validate its schema and safe relative paths.
3. Create a fresh data disk.
4. Restore durable data.
5. Create a fresh Ubuntu VM.
6. Apply the backup's root-applied lock unless `--lock PATH` is explicitly
   supplied as a separately reviewed desired state. Every artifact required
   only by that alternate lock must be materialized and verified before the
   destination VM or disk is created.
7. Start and verify the box.

The supplied identity is used only to decrypt the source backup. After restore,
the destination generates its own Keychain-backed age identity for future
backups; it does not silently retain or import the source identity.

This deliberately avoids temporary candidate machines and replacement
transactions.

### 7.16 `rebuild`

```text
hermes-box rebuild
```

Replaces the VM root while preserving the current data disk.

Sequence:

1. Materialize and verify both the repository-desired artifacts and the
   root-applied recovery artifacts, and copy the root-applied lock into the
   host recovery transaction, before deleting anything.
2. Create and verify a fresh encrypted backup. If the broken guest cannot
   produce one, require and verify an existing backup before continuing.
3. Stop services when reachable, then stop the VM.
4. Delete the Lima VM and disposable root disk, retaining the data disk.
5. Create a fresh VM from the repository-locked Ubuntu image.
6. Attach the existing data disk.
7. Apply the lock and run full health checks.

Applying the repository lock during rebuild intentionally brings platform and
application components to the same reviewed desired state in one fresh root;
`update` remains the lower-disruption path for application-only drift.

If creation fails, the data disk and backup remain intact. Recovery recreates
the previous root from the root-applied lock and artifacts captured before
deletion; v2 does not attempt an in-place root rollback.

That recovery is automatic within the same command: after desired-state health
fails, the command restores the pre-rebuild data backup, recreates the prior
root, reapplies the captured root-applied lock, and verifies it before
returning the original failure. There is no separate recovery-lock flag.

The repository lock must already contain any reviewed Ubuntu, Lima
compatibility, guest-provisioner, or foundational package change. `rebuild`
never edits it.

Hermes Box does not install or update Lima itself. A repository lock that
requires another qualified Lima version fails preflight and tells the operator
which host version to install before retrying.

`rebuild` is the only supported platform mutation path.

### 7.17 `doctor`

```text
hermes-box doctor
```

Runs bounded diagnostics without changing state:

- Host architecture, Lima version, and virtualization availability.
- Config and lock parsing.
- Logical-to-physical VM and disk ownership.
- VM state and data-disk mount.
- DNS and outbound HTTPS.
- Loopback-only port forwarding.
- systemd failed units.
- Component executable and version checks.
- Hermes gateway, Executor HTTP, and MCP health.
- tmux and terminfo capabilities.
- Backup key availability and latest backup verification.

`doctor` prints exact repair commands but does not run them.

### 7.18 `key`

```text
hermes-box key export PATH
```

Exports the box's age backup identity. The exported file is created with mode
`0600` and contains the ability to decrypt every backup for that box. The
identity normally lives only in macOS Keychain. Restore consumes an exported
identity directly and never imports it implicitly.

There are no SSH-key commands because Lima owns transport authentication and
restored boxes receive fresh host transport credentials.

### 7.19 `destroy`

```text
hermes-box destroy
hermes-box destroy --force
```

By default, creates and verifies a final encrypted backup, then removes the VM,
root disk, and data disk. It preserves existing backups and the encryption key.
`--force` is the explicit disaster-recovery escape hatch: it permits removal
when the guest cannot produce a final backup, after printing the latest valid
backup or stating that none exists. It never treats a partial backup as valid.
`rebuild` is the only operation that removes a root VM while preserving its
data disk, so `destroy` cannot create an orphaned disk.

Backups and Keychain identities are never deleted by `destroy`. They require
normal host file or Keychain management outside Hermes Box.

### 7.20 Utility commands

```text
hermes-box completion [bash|zsh|fish]
hermes-box version
hermes-box help [COMMAND]
```

`version` reports the host CLI version, qualified Lima version, config schema,
and lock schema. It does not start a VM.

## 8. Command behavior contract

### 8.1 Idempotency

- `start` and `stop` are idempotent.
- `status`, `logs` without `-f`, and `doctor` are read-only.
- `create`, `restore`, and `rebuild` never adopt unowned Lima resources.
- `update` is safe to retry after a failed staging step.
- `backup` creates a new immutable archive on every successful call.

### 8.2 Locking

Only one mutating operation may run per logical box. Mutating commands acquire
`<resolved HERMES_BOX_HOME>/locks/<name>.lock` before preflight and hold it
through final verification.

Read-only commands may run concurrently. `logs -f` does not hold the mutation
lock.

An active lock reports the owning PID, command, and start time. Hermes Box does
not break a live lock automatically.

### 8.3 Cancellation

The CLI forwards cancellation to the current child operation, then performs
bounded cleanup of resources it created during that command. It never deletes
a pre-existing VM or disk during cancellation cleanup.

An interrupted update is recovered to its recorded previous activation by the
next `start` or mutating command. Each activation writes only one tiny durable
journal containing component, previous pin, candidate pin, and phase, then
clears it after health succeeds. The journal lives on the disposable root next
to the releases it describes. An interrupted backup removes only its
incomplete output and restarts any services that were running before it began.
An interrupted rebuild preserves the data disk and recovery backup. `destroy`
aborts without deleting anything when its final backup cannot be verified
unless the operator explicitly supplied `--force`.

### 8.4 Exit status

The public exit statuses are intentionally small for every command except
`exec` and `ssh`:

```text
0  Requested outcome completed or idempotent no-op
1  Runtime, integrity, health, or external-command failure
2  Invalid command, flags, config, or lock file
```

Detailed categories belong in human diagnostics and the JSON `error.code`, not
an expanding set of process exit numbers. `exec` passes through its remote
process status and `ssh` the underlying SSH client status in `0..255` because
shell automation depends on those contracts.

### 8.5 JSON API

Every command that supports `--json` returns one envelope:

```json
{
  "schema": 1,
  "ok": true,
  "command": "start",
  "box": "main",
  "result": {}
}
```

Stable result keys by command:

| Command | Result keys |
| --- | --- |
| `create` | `state`, `healthy`, `setup_required`, `components`, `ports`, `backup` |
| `start` | `state`, `healthy`, `setup_required`, `components`, `ports` |
| `stop` | `state` |
| `status` | `state`, `healthy`, `setup_required`, `components`, `storage`, `ports`, `last_backup`, `updates` |
| `logs` | `target`, `lines` |
| `open` | `target`, `url` |
| `setup` | `target`, `connected`, `tools` |
| `update` | `components`, `changed`, `failed` |
| `rollback` | `component`, `previous`, `current`, `desired` |
| `backup` | `archive`, `envelope`, `archive_sha256` |
| `restore`, `rebuild` | `state`, `healthy`, `components`, `backup` |
| `doctor` | `healthy`, `checks` |
| `key export` | `path`, `recipient_fingerprint` |
| `destroy` | `backup`, `removed` |
| `version` | `cli`, `lima`, `config_schema`, `lock_schema` |

Component objects use the same keys everywhere:

```json
{
  "desired": "0.0.0",
  "applied": "0.0.0",
  "running": "0.0.0",
  "previous": null,
  "state": "healthy"
}
```

### 8.6 JSON errors

With `--json`, failures emit one object on stdout and human diagnostics remain
on stderr:

```json
{
  "schema": 1,
  "ok": false,
  "command": "start",
  "box": "main",
  "error": {
    "code": "preflight_failed",
    "message": "host loopback port 4788 is already in use",
    "recovery": "change ports.executor in hermes-box.yaml",
    "details": {"port": 4788}
  }
}
```

Error codes are stable within schema `1`.
`details` is an optional object with command-specific structured context. In
particular, a failed `update all` includes `completed`, `failed_component`, and
`rolled_back` fields there.

The schema-1 error-code set is:

```text
invalid_input
not_found
already_exists
busy
preflight_failed
integrity_failed
health_failed
external_failed
```

## 9. Box and component states

The public box states are:

```text
absent      No owned VM exists
stopped     Owned VM exists and is stopped
running     VM and infrastructure are healthy
degraded    VM is running but one or more infrastructure checks failed
```

Transient provisioning and update phases appear as operation progress, not
persistent public states. The private update journal exists only to recover a
crash at an activation boundary.

Component states are:

```text
healthy
setup-required
stopped
drifted
failed
```

Missing human authentication yields `setup-required`, not `failed`.

## 10. First-run handoff

After `create`, the CLI prints only the required human sequence:

```text
1. hermes-box ssh
2. claude
3. codex login --device-auth
4. hermes auth add openai-codex --type oauth
5. hermes model
6. hermes-box open executor
7. Create the Executor admin account and integrations
8. `hermes-box setup executor` and enter the portal-provided token
9. hermes-box key export /secure/path/main-age-key.txt
10. hermes-box backup configured
```

`setup executor` transfers the token over stdin or a no-echo prompt, stores it
in Hermes' protected persistent environment, writes the authenticated MCP
configuration, restarts Hermes, and runs `hermes mcp test executor`. The token
is never a process argument.

## 11. Update safety requirements

The host has one artifact-materialization boundary:

```text
Materialize(lock, component|all, contentAddressedCache) -> VerifiedArtifactSet
```

It resolves only the immutable URLs, commits, digests, and checksums already in
the lock. It has no install or activation logic.

The static guest helper owns the transaction boundary:

```text
Apply(lock, artifactSet, component|all)
Rollback(component)
Recover()
```

Each guest component implementation stages, verifies, smokes, activates,
checks health, and rolls back. Staging and verification may not write durable
user state. If a smoke test needs writable state, it uses a temporary copy.

Before activation, the host creates an encrypted transaction snapshot of the
durable directories that the component may migrate: `.claude` for Claude,
`.codex` for Codex, `.hermes` for Hermes, `.claude` plus `.hermes` for Node,
`.hermes` for uv, and `/data/executor` for Executor. Interactive Claude or
Codex processes make their updates fail as `busy`; Hermes is stopped for its
snapshot. Rollback restores the component snapshot before activating the prior
release. Node and uv rollback rerun the same dependent Claude/Hermes health
checks required during their update. Each component retains one encrypted
snapshot beside its one previous release until the next successful update of
that component replaces both.

Full backups and component snapshots share the same service and filesystem
freeze machinery, but only a full backup freezes and thaws the whole `agent`
user slice. Component snapshots instead reject active interactive Claude or
Codex processes and pause the affected system services, keeping the scoped
transaction boundary explicit.

The activation boundary is one atomic symlink change for native applications
and one systemd unit image-digest update for Executor.

## 12. Backup format

The encrypted archive contains an internal manifest:

```json
{
  "schema": 1,
  "format": "hermes-box-recovery-v1",
  "created_at": "2026-06-22T12:00:00Z",
  "box": "main",
  "applied_lock_sha256": "...",
  "data_paths": ["home/agent", "executor"],
  "artifacts": [{"name": "ubuntu-image", "sha256": "..."}],
  "excluded_paths": ["cache"]
}
```

The adjacent plaintext envelope is:

```json
{
  "schema": 1,
  "format": "hermes-box-recovery-v1-envelope",
  "archive": "20260622-120000-configured.tar.zst.age",
  "archive_sha256": "...",
  "recipient_fingerprint": "..."
}
```

The encrypted archive contains:

```text
manifest.json
applied.lock
data/home/agent/...
data/executor/...
artifacts/sha256/...
```

Archive entries must be relative, normalized, and unable to traverse the
destination. Symlinks may target only paths inside the restored data tree.

## 13. Security contract

Hermes Box v2 promises:

- No host filesystem mounts.
- No host SSH-agent, container-socket, or credential-store forwarding.
- Host-visible services bound to loopback only.
- Verified Ubuntu image and application artifacts.
- Immutable component pins in a reviewable lock file.
- Encrypted backups by default.
- No secrets in config, lock, command arguments, host metadata, or Git.
- A host-controlled way to stop the VM even when guest SSH fails. Destructive
  removal requires a verified backup unless the operator explicitly waives it
  with `destroy --force`.

Hermes Box v2 does not promise:

- Containment between `agent`, Hermes, Claude, Codex, and Executor inside the
  guest.
- Protection from data exfiltration over allowed outbound networking.
- Protection of Executor credentials from an autonomous guest root user.
- A firewall-backed strict or offline mode.
- An out-of-band guest root console or repair environment. If guest boot, SSH,
  or the data mount is broken, `doctor` reports the host-visible evidence and
  recovery uses `rebuild` rather than manual root repair.

## 14. Repository structure

The repository layout is:

```text
hermes-box/
├── bin/hermes-box
├── cmd/hermes-box/
├── cmd/hermes-box-guest/
├── internal/
│   ├── app/                  CLI orchestration
│   ├── box/                  Logical box state and ownership
│   ├── config/               YAML and lock validation
│   ├── lima/                 Narrow Lima command adapter
│   ├── artifacts/            Host fetch, verification, and cache
│   ├── guestupdate/          Guest transaction and component implementations
│   ├── backup/               Archive, age encryption, and restore
│   └── process/
├── guest/
│   ├── cloud-init.yaml
│   ├── bootstrap.sh          Minimal cloud-init bootstrap
│   ├── hermes.service
│   ├── executor.service
│   ├── tm
│   └── tmux.conf
├── docs/
│   └── HERMES_BOX_V2_SPEC.md
├── release/                 Reproducible ARM64 input construction
├── tests/
│   ├── static.sh
│   ├── tmux.sh
│   └── lifecycle.sh
├── hermes-box.yaml
└── hermes-box.lock
```

The Lima adapter must remain narrow. Only create, start, stop, delete, shell,
copy, inspect, disk attach, and port-forward operations belong in it.

## 15. Verification matrix

### 15.1 Safe local tests

The normal repository gate must cover:

- Go formatting, vet, race tests, and lint.
- Config and lock parsing, unknown-key rejection, and precedence.
- CLI help and JSON schemas.
- Artifact checksum and digest selection.
- Archive traversal and symlink rejection.
- Update transaction failure at every phase.
- Atomic activation and rollback.
- Redaction of secrets from errors and logs.

### 15.2 Isolated lifecycle tests

A release candidate is not accepted until an isolated box proves:

1. Fresh `create` from empty host state.
2. Ubuntu 26.04 ARM64 and data-disk mount.
3. Claude, Codex, Hermes, Executor, Node, and uv version checks.
4. tmux mouse, Ghostty metadata, true color, and `Shift+Enter`.
5. Hermes and Executor health.
6. Three consecutive `stop` and `start` cycles.
7. Update and rollback of each component.
8. Encrypted backup and restore through a different destination config.
9. Root `rebuild` with the original data disk.
10. Workspace, auth-state, and Executor-data persistence.
11. Loopback-only host exposure.
12. Cleanup of only the isolated VM, disk, Keychain item, and backups.

## 16. Implementation sequence

The implementation is organized as four independently reviewable layers:

### Stage 1: Foundation

- Config and lock schemas.
- Artifact materialization and verification.
- Lima preflight and ownership model.
- Ubuntu VM and persistent data disk.
- Static guest helper, systemd, tmux, and loopback forwarding.
- Internal isolated-machine harness; no public lifecycle release yet.

### Stage 2: Complete running box

- Node and uv tooling.
- Claude, Codex, and Hermes versioned releases.
- System Executor container.
- Keychain-backed age identity.
- Initial and operator-requested encrypted backup.
- `create`, `start`, `stop`, `ssh`, `exec`, `status`, `logs`, `open`, `setup`,
  `backup`, and `doctor`.
- First-run handoff and portal opening.

### Stage 3: Updates

- Static guest update transaction engine.
- Per-component staging and validation.
- `update` and `rollback`.
- Drift reporting.

### Stage 4: Recovery

- Encrypted self-contained backup and restore.
- Root rebuild.
- Final-backup-gated `destroy`.
- Full lifecycle matrix and operator documentation.
- Verify the README, AGENTS guidance, and operator docs remain v2-only so
  smolvm commands, provisioning-time source patching, and legacy portable
  formats cannot be confused with this product version.

The first public v2 release occurs only after Stage 4. No layer carries v1
compatibility code, and the complete branch must pass the lifecycle matrix
before release.

## 17. Qualified implementation decisions

The initial v2 implementation fixes these choices:

1. Lima 2.1.3 on Apple Silicon, installed by the operator and verified by host
   preflight. Hermes Box never upgrades Lima.
2. The official Ubuntu 26.04 ARM64 cloud image build dated `20260612`, pinned by
   immutable URL and SHA-256.
3. A separately named Lima `<name>-data` ext4 disk attached to a disposable VM
   root.
4. Claude Code from its exact official npm tarball, verified by registry
   SHA-512 SRI and installed offline with lifecycle scripts disabled.
5. Codex from its exact official Linux ARM64 musl bundle and SHA-256.
6. Hermes from a deterministic repository-owned gated-source artifact, an
   exact standalone CPython archive, and a complete reviewed offline wheel
   closure for the pinned `uv.lock`.
7. Executor from the exact Linux ARM64 child of a reviewed OCI index, loaded
   into system Podman with no runtime pull.

Changing any of these is an explicit reviewed update to the release inputs and
qualification matrix, not a runtime fallback.

## 18. Acceptance definition

Hermes Box v2 is ready when a new Mac can:

```text
git clone <repository>
cd hermes-box
hermes-box create
hermes-box ssh
```

and receive a healthy Ubuntu 26.04 box containing Claude Code, Codex, Hermes
Agent, and Executor; then survive repeated stop/start cycles, component
updates, an encrypted backup/restore, and a complete root rebuild without
copying or restoring the original root filesystem.
