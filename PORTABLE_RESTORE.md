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
`backups/`. Copy both files to the destination host.

Requirements:

- An ARM64 host supported by smolvm
- smolvm 1.0.4
- Go 1.24 or newer
- `shasum`, `ssh`, `ssh-keygen`, and `lsof`
- Enough free space for a 15 GiB VM
- Outbound network access to repull the pinned Executor runtime when enabled

Restore:

```bash
shasum -a 256 -c hermes-box-portable-*.tar.sha256
tar -xpf hermes-box-portable-*.tar
cd hermes-box
chmod 600 /secure/path/hermes-box-ed25519 images/hermes-base.smolmachine
printf '\nHERMES_BOX_SSH_KEY=%s\n' /secure/path/hermes-box-ed25519 >>hermes-box.conf
./bin/hermes-box restore backups/*.hermesbox
./bin/hermes-box status
./bin/hermes-box executor status
./bin/hermes-box ssh \
  'sudo -iu hermes env HERMES_HOME=/workspace/hermes-home hermes mcp test executor'
./bin/hermes-box ssh
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

Keep the archive encrypted at rest. Possession of it grants access to the
restored Hermes state and its dedicated SSH identity.

The Go host CLI remains compatible with `hermes-box-v2` snapshot directories
created by the earlier Bash wrapper.
