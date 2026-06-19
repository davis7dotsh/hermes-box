# Restoring This Hermes Box

This directory came from a Hermes Box portable archive. It contains a full
snapshot and may contain OAuth tokens, API keys, sessions, memories, and
generated work. Keep it private and encrypted at rest.

The current project and extended documentation are available in the
[Hermes Box repository](https://github.com/davis7dotsh/hermes-box).

## Expand

Run these commands from the directory containing the archive and checksum:

```bash
shasum -a 256 -c hermes-box-portable-*.tar.sha256
tar -xpf hermes-box-portable-*.tar
cd hermes-box
```

If this file is already open from the extracted `hermes-box` directory,
continue with the next section.

## Restore

Install the prerequisites listed in `PORTABLE_RESTORE.md` first. Retrieve the
stable private SSH key for this box from your encrypted secret store. Every
archive for the same box uses that one key; private and public keys are never
included in portable archives.

Save the private key to a secure temporary path, set mode `600`, then record
that absolute path in `hermes-box.conf`:

```bash
chmod 600 /secure/path/hermes-box-ed25519
printf '\nHERMES_BOX_SSH_KEY=%s\n' /secure/path/hermes-box-ed25519 >>hermes-box.conf
```

Hermes Box derives the public key automatically. Before restoring, also review
`hermes-box.conf` and change the machine names or loopback ports if they
collide with an existing smolvm machine on this host.

Then restore the packaged snapshot:

```bash
chmod 600 images/hermes-base.smolmachine
./bin/hermes-box restore backups/*.hermesbox
```

Restore verifies a temporary candidate before installing the recovered
machine.

## Run

Restore normally leaves the box running. The start command is safe to run when
it is already up:

```bash
./bin/hermes-box start
./bin/hermes-box status
./bin/hermes-box ssh
```

Inside the box, run `hermes` to start the agent. If Executor is enabled, verify
it from the host with `./bin/hermes-box executor status`.

See `PORTABLE_RESTORE.md` and the repository linked above for prerequisites,
collision handling, Executor setup, troubleshooting, and all other commands.
