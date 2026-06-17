# Portable Restore

This archive contains a complete Hermes Box snapshot, its smolvm base image,
the host wrapper, and the dedicated SSH key needed to access the restored box.
It also contains Hermes credentials and may contain API keys.

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

Restore:

```bash
shasum -a 256 -c hermes-box-portable-*.tar.sha256
tar -xpf hermes-box-portable-*.tar
cd hermes-box
chmod 600 state/hermes-box-ed25519 images/hermes-base.smolmachine
./bin/hermes-box restore backups/*.hermesbox
./bin/hermes-box status
./bin/hermes-box ssh
```

The packaged configuration uses repository-local `images/`, `backups/`, and
`state/` directories. The default SSH endpoint is `127.0.0.1:2222`. Change
`HERMES_BOX_SSH_PORT` in `hermes-box.conf` before restoring if that port is
already occupied.

Hermes authentication, configuration, sessions, memories, skills, and work are
restored from the snapshot. No Hermes setup is required afterward. If the
archive contains an optional `secret-env.txt`, its referenced host environment
variables must be defined on the destination host before restore.

Keep the archive encrypted at rest. Possession of it grants access to the
restored Hermes state and its dedicated SSH identity.

The Go host CLI remains compatible with `hermes-box-v2` snapshot directories
created by the earlier Bash wrapper.
