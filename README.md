# Hermes Box

Hermes Box runs [Hermes Agent](https://github.com/NousResearch/hermes-agent)
inside a persistent, isolated smolvm machine.

It is designed for running an autonomous agent without exposing host files,
the host SSH agent, Docker, or a GPU. Hermes gets its own persistent workspace,
while the host retains an out-of-band root console and can stop or restore the
machine regardless of what happens inside it.

## What You Get

```text
Ubuntu 24.04 VM
├── boxadmin                 Key-only SSH entry account
├── hermes                   Unprivileged agent account, no sudo
├── sshd                     Bound to host loopback only
├── supervisord
│   └── hermes gateway run
└── /workspace               Private persistent disk
    ├── hermes-home/         Auth, config, sessions, memory, skills, logs
    └── work/                Hermes working directory
```

Interactive SSH logins authenticate as `boxadmin` and immediately enter a
login shell as `hermes`. Noninteractive SSH commands remain explicit and
predictable.

## Security Boundaries

- No host directories are mounted.
- No SSH agent, Docker socket, or GPU is exposed.
- SSH accepts only the generated project key.
- Root login, passwords, forwarding, tunnels, and X11 are disabled.
- SSH is published only on `127.0.0.1`/`::1`; startup aborts otherwise.
- Hermes has no `sudo` access.
- Host root recovery remains available through `./bin/hermes-box shell`.
- Snapshots contain the full VM and may contain credentials.

This protects the host filesystem. With networking enabled, Hermes can still
reach internet and LAN services and can transmit information it knows.

## Compatibility

The current implementation is tested with:

- macOS on ARM64
- smolvm `1.0.4`
- Ubuntu `24.04`
- Hermes Agent `0.16.0`

Required host commands:

```text
go 1.24+ smolvm ssh ssh-keygen lsof
```

The host CLI is written in Go. The small `bin/hermes-box` launcher builds a
private cached binary under `state/` when the Go sources change, then executes
it. Guest provisioning remains in Bash because it is native Ubuntu
system-administration work.

## Quick Start

Clone and configure:

```bash
git clone https://github.com/davis7dotsh/hermes-box.git
cd hermes-box
cp hermes-box.conf.example hermes-box.conf
```

Edit `hermes-box.conf` and explicitly enable networking:

```bash
HERMES_BOX_NETWORK_MODE=full
```

Build the base image and start the runtime machine:

```bash
./bin/hermes-box init
./bin/hermes-box status
```

`init` creates a temporary networked builder, installs Hermes and the guest
services, packages a reusable base image, deletes the builder, and starts the
real runtime box.

The generated SSH key, base image, local configuration, snapshots, and runtime
state are ignored by Git.

## Configure Hermes

Open an interactive SSH session:

```bash
./bin/hermes-box ssh
```

You should land here without running `sudo`:

```text
hermes@container:/workspace/work$
```

Configure inference:

```bash
hermes model
```

For ChatGPT/Codex subscription authentication:

1. Select OpenAI.
2. Accept the Codex/ChatGPT subscription option.
3. Open the displayed device-login URL on the host.
4. Enter the one-time code.
5. Return to the terminal and select a model.

Check the installation and start chatting:

```bash
hermes status
hermes doctor
hermes tools
hermes
```

Hermes authentication and configuration live entirely inside
`/workspace/hermes-home`.

## Daily Commands

Run these from the repository root:

```bash
./bin/hermes-box start
./bin/hermes-box stop
./bin/hermes-box restart
./bin/hermes-box status
./bin/hermes-box ssh
./bin/hermes-box logs
./bin/hermes-box logs -f
```

Open the host-controlled root console:

```bash
./bin/hermes-box shell
```

Run a noninteractive command as Hermes:

```bash
./bin/hermes-box ssh 'sudo -iu hermes hermes status'
```

Display every command:

```bash
./bin/hermes-box help
```

## Snapshots

Create a consistent snapshot:

```bash
./bin/hermes-box snapshot configured-and-working
```

The wrapper:

1. Stops supervised services.
2. Archives the merged root filesystem.
3. Archives `/workspace`.
4. Rejects snapshots containing tar warnings.
5. Writes SHA-256 checksums.
6. Restarts the machine if it was previously running.

Snapshots are stored under `backups/*.hermesbox`.
A completed snapshot is retained and its path is reported if restarting the
machine afterward fails.

Restore:

```bash
./bin/hermes-box restore \
  backups/hermes-box-YYYYMMDD-HHMMSS-configured-and-working.hermesbox
```

Restore first validates a temporary candidate on a random loopback port. It
replaces the primary machine only after that candidate passes health checks,
and it takes a safety snapshot of the current machine before starting.

Keep `images/hermes-base.smolmachine` with your backups. Restore requires it.

## Portable Backups

A portable copy should contain:

- This repository
- `images/hermes-base.smolmachine`
- The selected `backups/*.hermesbox` directory
- `state/hermes-box-ed25519`

These files include the machine SSH identity and may include Hermes OAuth
tokens, API keys, sessions, memories, and generated work. Encrypt portable
archives at rest. See [PORTABLE_RESTORE.md](PORTABLE_RESTORE.md).

## Networking

`HERMES_BOX_NETWORK_MODE` accepts:

- `full`: unrestricted outbound networking
- `none`: rejected on smolvm 1.0.4
- `strict`: rejected on smolvm 1.0.4

Live testing found that smolvm 1.0.4 hostname allowlists could be bypassed by
direct-IP or unlisted-host traffic, and its no-network options still allowed
external HTTPS. Hermes Box therefore fails closed instead of presenting those
settings as meaningful containment.

Use `full` only when unrestricted VM egress is acceptable.

## Configuration

Copy `hermes-box.conf.example` to `hermes-box.conf` and adjust:

```bash
HERMES_BOX_MACHINE_NAME=hermes-box
HERMES_BOX_BUILDER_NAME=hermes-builder
HERMES_BOX_SSH_PORT=2222
HERMES_BOX_CPUS=4
HERMES_BOX_MEMORY_MIB=8192
HERMES_BOX_STORAGE_GB=15
HERMES_BOX_OVERLAY_GB=6
HERMES_BOX_NETWORK_MODE=full
```

Explicit `HERMES_BOX_*` environment variables override the config file. This
makes disposable test machines safe to run with different names and ports.
Configuration files are parsed as assignments rather than executed as shell
code. Plain, single-quoted, double-quoted, and optional `export` assignments are
supported.

Set `HERMES_BOX_DATA_DIR` to keep `images/`, `backups/`, and `state/` under a
different directory. The disposable lifecycle suite uses a temporary data
directory so it cannot replace primary recovery artifacts.

Optional smolvm host-secret mappings can be placed in `secret-env.txt`; use
`secret-env.txt.example` as the template.

## Testing

Run static and syntax checks:

```bash
./tests/static.sh
```

Run the complete local check suite:

```bash
make check
```

Run a disposable lifecycle test:

```bash
HERMES_BOX_E2E=1 \
HERMES_BOX_MACHINE_NAME=hermes-box-test \
HERMES_BOX_BUILDER_NAME=hermes-builder-test \
HERMES_BOX_SSH_PORT=2223 \
HERMES_BOX_NETWORK_MODE=full \
./tests/lifecycle.sh
```

Never reuse the primary machine name, builder name, or SSH port for destructive
tests. The script creates and removes its own isolated data directory.

## Project Layout

```text
hermes-box/
├── bin/hermes-box
├── cmd/hermes-box/
├── internal/
│   ├── app/
│   ├── config/
│   └── process/
├── guest/
│   ├── bootstrap.sh
│   ├── boxadmin.bash_profile
│   ├── restore.sh
│   ├── snapshot.sh
│   ├── start.sh
│   ├── supervisord.conf
│   └── workspace-seed.sh
├── tests/
├── Makefile
├── Smolfile
├── hermes-box.conf.example
├── network-hosts.txt
├── secret-env.txt.example
├── images/
├── backups/
└── state/
```

The browser and Node-based TUI payloads are intentionally removed from the
base image to keep smolvm pack and restore operations reliable. Hermes CLI,
inference, skills, messaging, gateway, and web-search tooling remain available.

The Go CLI preserves the `hermes-box-v2` backup format and can restore snapshots
created by the original Bash host wrapper.
