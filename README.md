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
├── hermes                   Agent account with passwordless full sudo
├── codex                    Interactive Codex CLI/TUI, full access inside VM
├── node + npm               Current Node.js 24 LTS toolchain
├── tmux                     Persistent terminal sessions for Codex and Hermes
├── sshd                     Bound to host loopback only
├── supervisord
│   ├── optional native Executor self-host service
│   └── hermes gateway run
└── /workspace               Private persistent disk
    ├── hermes-home/         Auth, config, sessions, memory, skills, logs
    ├── codex-home/          Codex binary, auth, config, sessions, skills, logs
    ├── executor/data/       Optional Executor database and secret store
    └── work/                Hermes working directory
```

Interactive SSH logins authenticate as `boxadmin` and immediately enter a
login shell as `hermes`. Noninteractive SSH commands remain explicit and
predictable.

## Security Boundaries

- No host directories are mounted.
- No host SSH agent, Docker socket, or GPU is exposed.
- SSH accepts only the generated project key.
- Root login, passwords, forwarding, tunnels, and X11 are disabled.
- SSH is published only on `127.0.0.1`/`::1`; startup aborts otherwise.
- Hermes has passwordless full `sudo` inside the isolated VM.
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

Hermes is pinned to commit
`81eaedd0f5c471c7ee748990066135a684f3c962`. The pin is a security boundary:
the guest provisioning applies a narrow gated-approval source extension whose
upstream anchors are verified against that revision. Provisioning also fetches
that commit's installer by immutable URL and verifies its SHA-256 digest. The
managed `uv` binary is pinned to `0.11.21` by release-archive digest because
`0.11.22` reproducibly deadlocks while building Hermes under smolvm 1.0.4.
Fresh builders also retry guest DNS and package-index readiness with a bounded
deadline before installing anything. If smolvm reports networking enabled but
boots a disposable builder without an IPv4 route, Hermes Box cycles that
builder before provisioning and fails after three unhealthy boots. Builders
explicitly use smolvm's `virtio-net` backend rather than its portless TSI
default so package installation gets a real guest NIC and DNS path. Hermes Box
also raises smolvm 1.0.4's extraction-cache ceiling for runtime creation and
verifies the packed-layer marker immediately, preventing that version's cache
eviction from surfacing later as a failed first boot.

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

To install the optional Executor gateway at the same time, also set:

```bash
HERMES_BOX_EXECUTOR_ENABLED=true
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

Keep an interactive session alive across SSH disconnects:

```bash
tmux new -As codex
codex
```

Detach with `Ctrl-b`, then `d`. Reconnect and run `tmux new -As codex` again
to reattach to the same session.

The messaging gateway is managed by Supervisor because Hermes Box does not
boot systemd. After changing Discord or other gateway configuration, reload it
with:

```bash
sudo supervisorctl restart hermes
sudo supervisorctl status hermes
cat /workspace/hermes-home/gateway_state.json
```

Do not run `hermes gateway install` or `hermes gateway start` in the box. The
upstream `hermes gateway status` command checks systemd and may label the
Supervisor-managed process as manual even when it is healthy. Supervisor,
`gateway_state.json`, and the gateway log are authoritative.

Hermes authentication and configuration live entirely inside
`/workspace/hermes-home`.

## Command Approval Gate

New Hermes Box images install a conservative, one-command permission reviewer
on top of Hermes 0.16.0. Stock Hermes supports `manual`, `smart`, and `off`; the
`gated` mode here is a source extension, not a YAML-only setting. Provisioning
therefore follows a hard order:

1. Install the pinned Hermes source revision.
2. Verify and patch exact upstream anchors in `tools/approval.py` and
   `gateway/run.py`.
3. Compile the patched files and run the gated regression suite.
4. Only then seed `approvals.mode: gated` in the persistent profile.

Any commit mismatch, missing anchor, compile failure, or regression failure
aborts image creation. The resulting gate sends bounded, secret-redacted turn
context to `openai-codex / gpt-5.5` with low reasoning and maps configured
`fast` service to the Responses API `priority` tier. It auto-approves only a
single invocation when scope is `once`, risk is at most `medium`, and
confidence is at least `0.75`. It never writes session or permanent approval
state. Malformed output, reviewer errors, timeouts, unknown/excessive risk,
unsupported scope, and low confidence all fall through to the existing human
approval flow with the reviewer reason attached. An explicit reviewer denial
blocks the command, while Hermes' deterministic hardline blocklist remains
authoritative before the model is called. Cron stays fail-closed with
`cron_mode: deny`.

The seeded profile block is:

```yaml
approvals:
  mode: gated
  timeout: 60
  cron_mode: deny
  gateway_timeout: 300
  gate:
    enabled: true
    provider: openai-codex
    model: gpt-5.5
    reasoning_effort: low
    service_tier: fast
    timeout: 30
    scope: once
    min_confidence: 0.75
    max_context_chars: 12000
    auto_approve_max_risk: medium
    escalate_on_error: true
    escalate_on_low_confidence: true
security:
  redact_secrets: true
  tirith_enabled: true
```

The gate uses Hermes' existing Codex OAuth credentials. If that provider has
not been authenticated yet, reviewer calls safely escalate to the normal human
approval. Authenticate interactively inside the box when ready:

```bash
hermes login --provider openai-codex
sudo supervisorctl restart hermes
```

Do not change `HERMES_BOX_HERMES_COMMIT` casually. Port the patch against the
new revision and rerun its regression suite before updating the pin.

## Configure Codex

Codex `0.141.0` is installed from its official standalone release archive,
verified by SHA-256, including the interactive TUI. Its persistent standalone
layout remains compatible with `codex update`. Open the box and sign in using
the device flow:

```bash
./bin/hermes-box ssh
codex login --device-auth
codex
```

Codex defaults to `approval_policy = "never"` and
`sandbox_mode = "danger-full-access"`. This is the persistent equivalent of
`--yolo`: Codex can autonomously read, write, execute, and use the network
inside the VM, while the Hermes Box boundary still isolates it from the host.
The default `/workspace/work` directory is pre-trusted. The `hermes` account
has passwordless full `sudo` inside the VM; the smolvm boundary protects the
host rather than restricting the agent within its box.

Codex's executable, login cache, configuration, sessions, and update metadata
live under `/workspace/codex-home`, so snapshots preserve them together. Update
the standalone installation as the `hermes` user:

```bash
codex update
codex --version
```

The login cache is stored as `/workspace/codex-home/auth.json`. Treat snapshots
and portable packages as credentials after signing in.

## Optional Executor Gateway

Hermes Box can run a digest-pinned
[Executor](https://github.com/RhysSullivan/executor) service inside the VM.
Enable it before `init`:

```bash
HERMES_BOX_EXECUTOR_ENABLED=true
HERMES_BOX_EXECUTOR_PORT=4788
```

After the box starts, open the portal and create the first admin account:

```bash
./bin/hermes-box executor open
```

Executor's UI and MCP endpoint share that loopback-only port. Create an API key
in the portal, then store it in the per-machine macOS Keychain entry. The key is
prompted for securely and is never passed as a command-line argument:

```bash
./bin/hermes-box executor auth set
./bin/hermes-box executor mcp-test
```

The host CLI can inspect the service without exposing secrets:

```bash
./bin/hermes-box executor status
./bin/hermes-box executor status --json
./bin/hermes-box executor logs -f
./bin/hermes-box executor connections
./bin/hermes-box executor connections --json
./bin/hermes-box executor tools
./bin/hermes-box executor tools "calendar events" --namespace google
```

Connection creation, OAuth, and policy changes intentionally stay in the web
portal. This makes browser-based setup through Computer Use straightforward:
open the portal with the CLI, create the connection, follow the provider's OAuth
flow in the same browser session, and use the exact callback URL displayed by
Executor. For Google testing, a localhost HTTP redirect is appropriate, but the
OAuth client's authorized redirect URI must exactly match Executor's displayed
value. Confirm the result afterward with `executor connections` and
`executor tools`. See [EXECUTOR_CONNECTIONS.md](EXECUTOR_CONNECTIONS.md) for
provider-dashboard credential retrieval, secret handoff, policy, and validation
steps for Google, YouTube, X, Discord, Airtable, GitHub, and Notion.

Once the MCP test passes, register the in-VM endpoint with Hermes:

```bash
./bin/hermes-box executor connect-hermes
```

This validates from inside the guest that the MCP server exposes only `execute`
and `resume` before changing Hermes' persistent configuration. It then writes
the bearer-token reference, copies the normalized token into
`/workspace/hermes-home/.env` over SSH stdin, runs `hermes mcp test executor`,
and restarts the supervised Hermes gateway. The token therefore exists in both
the host Keychain and Hermes' guest credential store, both scoped to this box.
Start a new Hermes CLI session after the command returns so it discovers the
new MCP tools.

Hermes Box does not run nested Docker. On first boot, `skopeo` copies the
digest-pinned ARM64 image and a constrained extractor installs its application
plus bundled Bun runtime under disposable
`/workspace/.hermes-box-runtime/executor`.
Supervisor then runs the published self-host payload natively before Hermes.
Executor's database and generated secret keys live under
`/workspace/executor/data`, so normal snapshots preserve them while excluding
the repullable 2.5 GB runtime. The service keeps
`EXECUTOR_ALLOW_LOCAL_NETWORK=false`; smolvm remains the only host port
publisher.

The Executor launcher also sets
`BUN_FEATURE_FLAG_DISABLE_IPV6=1`. smolvm 1.0.4 can present a default IPv6
route even when external IPv6 traffic is blackholed, and Bun 1.3.14's HTTP
client may wait on that path instead of falling back to working IPv4. The flag
is scoped to Executor and keeps its Google, YouTube, and Notion requests on the
working IPv4 path. Remove it only after the pinned Bun runtime has been proven
to fall back correctly in this topology.

Image layers are verified by SHA-256, rejected if they contain unsafe paths,
and applied with OCI whiteout handling. Interrupted downloads and extractions
remain in temporary directories and are not activated; the completed runtime
is selected with an atomic symlink update.

Executor provides account and tool policy enforcement, but it is not a
credential-isolation boundary: the `hermes` user has passwordless root inside
the same VM. Use a separate machine if Executor credentials must be hidden from
Hermes itself.

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
./bin/hermes-box executor open
./bin/hermes-box executor status
./bin/hermes-box executor logs -f
./bin/hermes-box executor auth status
./bin/hermes-box executor connections
./bin/hermes-box executor tools
./bin/hermes-box executor mcp-test
./bin/hermes-box executor connect-hermes
./bin/hermes-box package configured-agent
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
5. Records the stable SSH-key fingerprint and writes SHA-256 checksums.
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

Create a self-contained portable archive:

```bash
./bin/hermes-box package configured-agent
```

Convert an already completed snapshot without starting its machine:

```bash
./bin/hermes-box package --snapshot backups/existing.hermesbox configured-agent
```

`package` takes a fresh consistent snapshot, then bundles:

- An archive-specific `AGENTS.md` with expand, restore, and run instructions
- The runnable Hermes Box project
- `images/hermes-base.smolmachine`
- The new `backups/*.hermesbox` snapshot
- A portable `hermes-box.conf` using repository-local data directories

For Executor-enabled boxes, the generated configuration preserves whether
Executor is enabled, its loopback port, and the exact digest-pinned image.
Executor's database, integrations, policies, OAuth tokens, API credentials,
and Hermes MCP token are carried inside the `/workspace` snapshot.

It writes both files under `backups/`:

```text
hermes-box-portable-YYYYMMDD-HHMMSS-configured-agent.tar
hermes-box-portable-YYYYMMDD-HHMMSS-configured-agent.tar.sha256
```

Portable archives intentionally exclude the SSH private and public keys. Keep
one stable private key per box in an encrypted secret store such as 1Password;
the same key restores every archive made by that box. The archives may still
include Hermes OAuth tokens, API keys, sessions, memories, and generated work,
so encrypt them at rest too.

The source Mac's Keychain entries and browser sessions are not included. The
repullable Executor runtime under `/workspace/.hermes-box-runtime` is also
excluded, so the destination needs outbound network access on first start to
fetch the pinned runtime again. After restore, add a destination-local Executor
API key with
`./bin/hermes-box executor auth set` if host-side management commands are
needed; Hermes' in-guest Executor connection is restored from the snapshot.

On another compatible host, install the prerequisites and restore without
reconfiguring Hermes:

```bash
shasum -a 256 -c hermes-box-portable-*.tar.sha256
tar -xpf hermes-box-portable-*.tar
cd hermes-box
printf '\nHERMES_BOX_SSH_KEY=%s\n' /secure/path/hermes-box-ed25519 >>hermes-box.conf
./bin/hermes-box restore backups/*.hermesbox
./bin/hermes-box status
./bin/hermes-box executor status
./bin/hermes-box ssh \
  'sudo -iu hermes env HERMES_HOME=/workspace/hermes-home hermes mcp test executor'
```

The packaged `AGENTS.md` contains this recovery sequence and links back to the
[Hermes Box repository](https://github.com/davis7dotsh/hermes-box) for the
latest documentation and troubleshooting guidance.

Host environment variables referenced by an optional `secret-env.txt` must
still exist on the restore host. See [PORTABLE_RESTORE.md](PORTABLE_RESTORE.md).

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
HERMES_BOX_SSH_KEY=/secure/path/hermes-box-ed25519
HERMES_BOX_HERMES_COMMIT=81eaedd0f5c471c7ee748990066135a684f3c962
HERMES_BOX_EXECUTOR_ENABLED=false
HERMES_BOX_EXECUTOR_PORT=4788
```

`HERMES_BOX_EXECUTOR_IMAGE` defaults to the reviewed v1.5.12 multi-platform OCI
image pinned by digest. Overrides are accepted only when they also contain an
explicit tag and full SHA-256 digest. The guest always resolves and verifies
the Linux ARM64 child image.

Explicit `HERMES_BOX_*` environment variables override the config file. This
makes disposable test machines safe to run with different names and ports.
Configuration files are parsed as assignments rather than executed as shell
code. Plain, single-quoted, double-quoted, and optional `export` assignments are
supported.

Set `HERMES_BOX_DATA_DIR` to keep `images/`, `backups/`, and `state/` under a
different directory. The disposable lifecycle suite uses a temporary data
directory so it cannot replace primary recovery artifacts.

Set `HERMES_BOX_SSH_KEY` to the separately stored stable private key for the
box. Hermes Box derives its public key automatically. An explicitly configured
key is never generated or replaced when missing, and portable archives never
contain either key file.

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
HERMES_BOX_EXECUTOR_ENABLED=true \
HERMES_BOX_EXECUTOR_PORT=4789 \
HERMES_BOX_NETWORK_MODE=full \
./tests/lifecycle.sh
```

Never reuse the primary machine name, builder name, SSH port, or Executor port
for destructive tests. The script creates and removes its own isolated data
directory.

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
│   ├── bootstrap.sh          Installs Hermes and seeds persistent state
│   ├── boxadmin.bash_profile
│   ├── hermes-box.sudoers    Grants the agent full passwordless sudo
│   ├── executor.sh           Installs and runs the pinned Executor payload
│   ├── extract-executor.py   Safely applies the selected OCI image layers
│   ├── hermes_gated_approval.py  Conservative one-shot reviewer module
│   ├── patch-hermes-gated-approval.py  Strict pinned-source installer
│   ├── install-node.sh       Installs the latest checksum-verified Node 24 LTS
│   ├── restore.sh
│   ├── snapshot.sh
│   ├── start.sh              Installs Codex once, then starts services
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

The Hermes browser and Node-based Hermes TUI payloads are intentionally removed
from the base image to keep smolvm pack and restore operations reliable. The
native Codex TUI is installed into the persistent workspace on first boot.
Hermes CLI, inference, skills, messaging, gateway, and web-search tooling remain
available.

The Go CLI preserves the `hermes-box-v2` backup format and can restore snapshots
created by the original Bash host wrapper.
