# Portable Restore

This archive contains a complete Hermes Box snapshot, its smolvm base image,
and the host wrapper. It intentionally excludes the dedicated SSH key needed
to access the restored box. It contains Hermes credentials and may contain API
keys.

The packaged `AGENTS.md` provides the short expand, restore, and run sequence.
For current documentation and troubleshooting, see the
[Hermes Box repository](https://github.com/davis7dotsh/hermes-box).

For Executor-enabled boxes, the package preserves the enabled state, host
loopback port, exact digest-pinned image, and `/workspace/executor/data`.
That workspace data can contain integrations, policies, OAuth tokens, API
credentials, and Executor's generated secret store.

Create it from a configured Hermes Box:

```bash
./bin/hermes-box package configured-agent
```

The command writes the `.tar` archive and its `.sha256` checksum under
`backups/`. Copy both files together to encrypted, off-host storage before
moving them to the destination host.

Requirements:

- A macOS ARM64 host
- smolvm 1.0.4
- Go 1.24 or newer
- `shasum`, `ssh`, `ssh-keygen`, and `lsof`
- Enough free space for a 15 GiB VM
- Outbound network access to repull the pinned Executor runtime when enabled

If Go is missing or older than 1.24, install a supported macOS ARM64 release
from <https://go.dev/dl/>. If smolvm is missing or not exactly 1.0.4, install
the official Darwin ARM64 asset from the
[v1.0.4 release](https://github.com/smol-machines/smolvm/releases/tag/v1.0.4).
Rerun the requirement probes, then resume at the checksum command below; do not
create or replace a VM until they pass.

Restore:

```bash
shasum -a 256 -c hermes-box-portable-*.tar.sha256
tar -xpf hermes-box-portable-*.tar
cd hermes-box
chmod 600 /secure/path/hermes-box-ed25519 images/hermes-base.smolmachine
printf '\nHERMES_BOX_SSH_KEY=%s\n' /secure/path/hermes-box-ed25519 >>hermes-box.conf
./bin/hermes-box restore backups/*.hermesbox
./bin/hermes-box status
./bin/hermes-box ssh
```

If Executor is enabled in `hermes-box.conf`, also run:

```bash
./bin/hermes-box executor status
./bin/hermes-box ssh \
  'sudo -iu hermes env HERMES_HOME=/workspace/hermes-home hermes mcp test executor'
```

Retrieve the same stable private key used to create the source box from an
encrypted secret store such as 1Password. One key can restore every archive
from that box. Hermes Box derives the public key automatically, checks the key
fingerprint recorded by new snapshots, and preserves the supplied identity
when applying the archived root filesystem.

The packaged configuration uses repository-local `images/`, `backups/`, and
`state/` directories and preserves the source box's machine names and ports.
Change `HERMES_BOX_MACHINE_NAME`, `HERMES_BOX_BUILDER_NAME`,
`HERMES_BOX_SSH_PORT`, or `HERMES_BOX_EXECUTOR_PORT` before restoring if any
would collide on the destination host.

If the target machine name does not exist, restore creates it directly and
runs one complete validation pass. If that validation fails, Hermes Box
deletes only the machine it just created. When replacing an existing machine,
restore retains the safer two-phase flow: safety snapshot, temporary candidate
validation, replacement, and rollback on failure.

Hermes authentication, configuration, sessions, memories, skills, and work are
restored from the snapshot. No Hermes setup is required afterward. If the
archive contains an optional `secret-env.txt`, its referenced host environment
variables must be defined on the destination host before restore.

The source host's SSH key, macOS Keychain entries, and browser sessions are not
included, and the repullable Executor runtime under
`/workspace/.hermes-box-runtime` is intentionally omitted.
If host-side Executor inventory and MCP test commands are needed on the new
Mac, create a destination-local API key in the restored portal and run:

```bash
./bin/hermes-box executor auth set
./bin/hermes-box executor mcp-test
```

Keep the archive encrypted at rest. It contains the restored Hermes state;
the archive plus the separately stored private key grants SSH access to it.

The Go host CLI remains compatible with `hermes-box-v2` snapshot directories
created by the earlier Bash wrapper.
