# Hermes Box

Hermes Box creates one rebuildable Ubuntu 26.04 ARM64 agent VM on an Apple
Silicon Mac. Claude Code, Codex, Hermes Agent, and Executor are installed by
default.

This repository is the v2 implementation. It deliberately does not read,
upgrade, restore, or adopt v1 smolvm machines, images, snapshots, packages, or
configuration.

## The simple model

```text
Apple Silicon Mac
├── hermes-box              One Go CLI
├── Lima + Apple VZ         Disposable Ubuntu 26.04 root VM
└── <name>-data             One persistent ext4 disk
    ├── home/agent          Claude, Codex, and Hermes state plus workspace
    └── executor            Executor database and secrets
```

The VM root is replaceable. All durable guest state lives on the data disk.
Software versions and verified download inputs live in the reviewed
`hermes-box.lock`. `start` never updates anything; `update` applies application
pins from that lock, and `rebuild` replaces the root for platform changes.

There is one security boundary: the VM protects the host. The `agent` user has
passwordless sudo inside the VM, so Claude, Codex, Hermes, and Executor are not
isolated from each other.

## Host requirements

- Apple Silicon Mac
- Go 1.24 or newer
- Lima 2.1.3, the currently qualified host runtime
- `ssh`

Optional contributor tools are `shellcheck` and `tmux`. The repository has no
Node or Python dependency installation step.

The initial platform qualification targets Lima 2.1.3 and the official Ubuntu
26.04 ARM64 cloud-image build dated 20260612. Their exact artifact URLs and
checksums belong in `hermes-box.lock`, not in runtime code.

## Configuration

One configuration directory owns exactly one box. Its two inputs are safe to
commit:

- `hermes-box.yaml`: human choices such as box name, CPUs, memory, disks, port,
  and backup retention.
- `hermes-box.lock`: reviewed Ubuntu, provisioner, tool, application, and
  container pins with checksums or digests.

The configuration file is selected by `--config`, then `HERMES_BOX_CONFIG`,
then `./hermes-box.yaml`. Host state is selected by `HERMES_BOX_HOME`, then
`~/.hermes-box`.

Example configuration:

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

The lock is input-only. Runtime commands never edit it. Version changes belong
in a normal reviewed pull request before they are applied to a box.

## Create a box

From a checkout containing the reviewed config and lock:

```bash
./bin/hermes-box create
```

`create` verifies all inputs first, creates a separately named Lima data disk,
boots a fresh Ubuntu root, applies the lock, starts systemd services, runs
health checks, and creates the first encrypted backup.

It then prints the human authentication handoff:

```text
hermes-box ssh
claude
codex login --device-auth
hermes auth add openai-codex --type oauth
hermes model
hermes-box open executor
hermes-box setup executor
hermes-box key export /secure/path/main-age-key.txt
hermes-box backup configured
```

Browser grants, device codes, account creation, OAuth consent, and backup-key
storage remain human actions.

## Interactive shell

```bash
./bin/hermes-box ssh
```

SSH opens `/workspace` as `agent` and automatically creates or attaches the
tmux session named `main`. Inside the guest, `tm` always returns to that same
session.

The shared tmux setup has a dark-green bottom bar with white text, clickable
windows, true color, focus events, clipboard passthrough, Ghostty metadata, and
extended-key support so `Shift+Enter` remains distinct in TUIs such as Codex.

Detach with `Ctrl-b d`; the next SSH connection reattaches.

## Commands

```text
create                                      Create and verify a new box
start                                       Start applied versions; never update
stop                                        Stop services, sync data, and stop VM
ssh                                         Attach the main interactive tmux session
exec -- COMMAND [ARGS...]                   Run a command without tmux
status [--check]                            Show health, drift, setup, and backups
logs [hermes|executor|recovery] [-f] [-n N] Read bounded service logs
open executor                               Open the loopback-only Executor portal
setup executor [--token-stdin]              Connect Executor to Hermes safely
update COMPONENT|all                        Apply reviewed application lock drift
rollback COMPONENT                          Activate the one retained prior release
backup [LABEL]                              Create an encrypted recovery bundle
restore BACKUP --identity PATH [--lock P]   Restore into an absent destination
rebuild                                     Replace root and retain the data disk
doctor                                      Run bounded read-only diagnostics
key export PATH                             Export the age backup identity
destroy [--force]                           Back up, then remove VM and data disk
completion [bash|zsh|fish]                  Print shell completion
version                                     Print CLI and schema versions
help [COMMAND]                              Show help
```

Global flags are `--config PATH`, `--json`, `--quiet`, `--no-color`, and
`--help`. Progress goes to stderr. Supported `--json` commands emit exactly one
schema-versioned object on stdout.

## Updates

Application updates are explicit:

```bash
./bin/hermes-box status --check
# Change hermes-box.lock on a branch and merge it after review.
./bin/hermes-box update codex
./bin/hermes-box update all
```

Supported components are `node`, `uv`, `claude`, `codex`, `hermes`, and
`executor`. Each update stages and checks the candidate before activation,
keeps one previous release, snapshots component-owned durable state, and rolls
back the candidate on failed health checks.

Ubuntu, the guest provisioner, and Lima compatibility are platform entries.
After their reviewed lock change, use:

```bash
./bin/hermes-box rebuild
```

`rebuild` verifies a recovery backup, creates a fresh root, reattaches the same
data disk, and reapplies the lock. There is no in-place OS upgrade.

## Backup and restore

```bash
./bin/hermes-box backup configured
./bin/hermes-box key export /secure/path/main-age-key.txt
```

Backups are age-encrypted and include the persistent data disk, the applied
lock, the Ubuntu image, and every exact install input required to reconstruct
that applied root. They do not contain a root filesystem or a Lima binary.

Store the exported identity separately; it decrypts every backup for that box.
Restore always targets an absent box and never replaces one in place:

```bash
./bin/hermes-box \
  --config /path/to/destination/hermes-box.yaml \
  restore /path/to/backup.tar.zst.age \
  --identity /secure/path/main-age-key.txt
```

See [PORTABLE_RESTORE.md](PORTABLE_RESTORE.md) for the short recovery runbook.

## Security boundaries

Hermes Box provides:

- no host directory, SSH-agent, Docker socket, GPU, or credential-store mount;
- Lima-managed SSH instead of a project-owned long-lived SSH key;
- loopback-only host exposure for Executor;
- checksum- or digest-verified Ubuntu and component inputs;
- encrypted backups and reviewed immutable pins; and
- host control to stop or replace the VM when guest SSH is broken.

It does not provide strict/offline networking, guest process isolation, or an
out-of-band root repair console. If boot, SSH, or the data mount is broken,
`doctor` reports host-visible evidence and recovery uses `rebuild`.

## Executor

Executor runs as a digest-pinned Linux ARM64 container under system Podman.
Only `/data/executor` is persistent; the Podman image store stays on the
disposable root. Open the portal and finish account/integration setup, then
register its reviewed MCP surface with Hermes:

```bash
./bin/hermes-box open executor
./bin/hermes-box setup executor
```

The token is read from a no-echo prompt or stdin, never a command argument.
See [EXECUTOR_CONNECTIONS.md](EXECUTOR_CONNECTIONS.md) for the provider setup
boundary and validation checklist.

## Contributor checks

Routine checks are safe and do not create or operate a VM:

```bash
make check
```

This runs Go formatting checks, `go vet`, race-enabled tests, Bash syntax,
ShellCheck when installed, sudoers validation when available, and tmux contract
checks.

The destructive lifecycle harness is opt-in, creates its own config directory,
`HERMES_BOX_HOME`, and `LIMA_HOME`, rejects the primary name and port, and is
never run by `make check` or CI:

```bash
HERMES_BOX_E2E=1 ./tests/lifecycle.sh
```

Read [AGENTS.md](AGENTS.md) before operating it.

The ARM64 release-artifact workflow has two phases. Pull requests build the
static guest helper, provisioner package set, gated Hermes source, offline
Hermes wheels, checksums, and a candidate lock without publishing anything.
The isolated lifecycle harness accepts explicit baseline and candidate locks
plus both repository-owned artifact directories, verifies and seeds its own
temporary artifact cache, and therefore does not require future release URLs
to exist. The two locks must differ for all six managed components; the harness
updates and rolls back each one before applying the complete candidate.

After that candidate passes review and the isolated matrix, pushing the one-use
`v2.0.0-assets` tag rebuilds, attests, and publishes the immutable inputs. The
exact published URLs are downloaded and the lifecycle runs again from those
bytes. Only then is the generated `hermes-box.lock` promoted to the repository
root in a final reviewed change. Until that promotion, the missing root lock is
intentional and normal operator `create` remains unavailable. See
[release/README.md](release/README.md) for the executable runbook.

## Structure

```text
cmd/hermes-box/          Host CLI
cmd/hermes-box-guest/    Static Linux ARM64 guest transaction helper
internal/                Config, Lima, artifacts, updates, backup, and state
guest/                   Cloud-init, systemd units, tmux, and bootstrap assets
tests/                   Safe static checks and isolated lifecycle harness
docs/HERMES_BOX_V2_SPEC.md
```

The complete API, JSON schema, state model, and implementation contract are in
[docs/HERMES_BOX_V2_SPEC.md](docs/HERMES_BOX_V2_SPEC.md).
