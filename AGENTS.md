# Agent Guide

Hermes Box v2 is a Go host CLI plus a static Linux ARM64 guest helper and small
guest assets. It runs an Ubuntu 26.04 VM through Lima on Apple Silicon. Read
`README.md` and `docs/HERMES_BOX_V2_SPEC.md` before changing behavior, and read
every file you inspect in full.

There is no v1 compatibility. Do not restore smolvm commands, environment
configuration, rootfs snapshots, source patching, Supervisor, or portable-v1
behavior.

## Contributor setup

Repository work does not require a VM.

```bash
uname -sm
command -v go limactl ssh
command -v shellcheck tmux || true
go version
limactl --version
make check
```

The supported host is macOS ARM64 with Go 1.24 or newer and Lima 2.1.3. The
initial guest qualification uses the official Ubuntu 26.04 ARM64 cloud-image
build dated 20260612; its immutable URL and checksum belong in
`hermes-box.lock`. `shellcheck` and `tmux` are recommended; safe checks report
when either is unavailable.

Do not install Node or Python dependencies. Do not create `hermes-box.yaml` or
an alternate lock just to run routine checks.

## Change rules

- Keep host orchestration under `cmd/hermes-box` and `internal`.
- Keep the static guest transaction engine under `cmd/hermes-box-guest` and
  `internal/guestupdate`.
- Keep cloud-init, systemd, tmux, and minimal bootstrap assets under `guest`.
- Keep `hermes-box.lock` input-only. Runtime commands must never edit it.
- Preserve one config-directory-to-one-box ownership and exact Lima resource
  names. Never discover destructive targets by broad prefix.
- Keep root state reconstructable and durable state on `/data` only.
- Do not add host mounts, SSH-agent forwarding, socket forwarding, or secrets
  in arguments, config, logs, metadata, or Git.
- Use `make format` after changing Go files and `make check` after every code or
  script change.
- Do not run `go build` directly unless diagnosing the launcher or guest
  artifact workflow.
- Do not run `tests/lifecycle.sh` during routine validation.

For config or CLI changes, update tests in `internal/config`, `internal/app`,
and `tests/static.sh`. For guest transactions, test staging, integrity,
activation, rollback, crash recovery, and durable-state restoration. For tmux
changes, run `tests/tmux.sh`.

Before finishing, confirm `git status --short` contains only intended shared
work. Other agents may be editing the same checkout; never discard or rewrite
their changes.

## Runtime safety

Lima VM and disk names are host-global within `LIMA_HOME`. The default `main`
box can contain credentials, sessions, Executor secrets, and private work.

- Treat the default config, box name `main`, Executor port `4788`, default
  `~/.hermes-box`, and the user's normal Lima home as primary resources.
- Do not run `create`, `stop`, `start`, `update`, `rollback`, `backup`,
  `restore`, `rebuild`, `setup`, `key`, or `destroy` against primary resources
  unless the task explicitly authorizes that exact operation.
- Read-only `status`, `doctor`, and bounded log inspection are allowed when
  relevant, but verify the selected config first.
- Never run an end-to-end test with a user config, user `HERMES_BOX_HOME`, or
  user `LIMA_HOME`.
- Backups and component transaction snapshots contain secrets.

The only supported lifecycle test entrypoint is:

```bash
HERMES_BOX_E2E=1 ./tests/lifecycle.sh
```

The script creates and validates a unique temporary config, `HERMES_BOX_HOME`,
and `LIMA_HOME`; refuses primary names and ports; and owns cleanup of only those
temporary paths. Do not bypass its guards or reproduce its commands manually
against primary resources.

If an isolated lifecycle run is interrupted, reuse the exact config and homes
printed by the script when cleaning up. Never point cleanup at the defaults.

## Release artifact boundary

CI can reproducibly cross-compile the static Linux ARM64 guest helper and pack
the repository-owned guest provisioner files. That artifact is only a build
input. It is not qualified for `hermes-box.lock` until its SHA-256, the exact
Ubuntu image, required `.deb` inputs, every application artifact, and the full
offline Hermes Python/wheel set have been independently reviewed and recorded.
