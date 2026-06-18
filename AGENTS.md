# Agent Guide

Hermes Box is a Go host CLI plus Bash guest-provisioning scripts for running
Hermes Agent inside smolvm. Read `README.md` before changing behavior, and read
each file you inspect in full rather than relying on excerpts.

## Default Contributor Setup

Preparing the repository for code work does not require creating or starting a
VM. Unless a task explicitly asks you to operate Hermes Box, "set up the
project" means completing only the contributor setup in this section.

1. Confirm the host tools used by local checks:

   ```bash
   uname -sm
   command -v go smolvm ssh ssh-keygen lsof
   command -v shellcheck || printf 'shellcheck is optional but recommended\n'
   go version
   smolvm --version
   ```

   The supported baseline is macOS ARM64, Go 1.24 or newer, and smolvm 1.0.4.
   `shellcheck` is recommended; `tests/static.sh` reports when it is absent.

2. Do not install project dependencies. This repository has no third-party Go
   modules and no Node/Python package setup.

3. Run the safe local suite:

   ```bash
   make check
   ```

   This formats-checks Go code, runs `go vet`, race-enabled Go tests, Bash
   syntax/static checks, ShellCheck when available, workspace-seed tests, and
   CLI help/config precedence checks. It does not create, start, stop, restore,
   or delete a smolvm machine. The final `static checks passed` line includes
   the CLI help and config-precedence assertions from `tests/static.sh`.

`hermes-box.conf` is only needed for operating a real box. Do not create it just
to work on code or run the normal checks.

## Change Workflow

- Keep the host CLI in Go under `cmd/` and `internal/`.
- Keep guest Ubuntu administration in Bash under `guest/`.
- Preserve the security boundaries and compatibility promises documented in
  `README.md`.
- Use `make format` after changing Go files.
- Run `make check` after every code or script change.
- Do not run `go build` directly unless diagnosing the launcher. The
  `bin/hermes-box` launcher maintains its ignored cached binary under `state/`.
- Keep generated artifacts and credentials out of Git. Never commit files from
  `state/`, `images/`, or `backups/`, nor `hermes-box.conf` or `secret-env.txt`.

When changing configuration or CLI behavior, check the related tests in
`internal/config/`, `internal/app/`, `tests/static.sh`, and
`tests/config-precedence.conf`. When changing guest persistence or startup,
also check `tests/workspace-seed.sh`.

## Runtime Safety

smolvm machine names are host-global, and runtime commands can affect an
existing Hermes installation containing credentials and user data.

- Treat the default `hermes-box`, `hermes-builder`, and port `2222` as primary
  user resources.
- Do not run `init`, `stop`, `restart`, `snapshot`, `restore`, `package`, or
  `destroy` against the primary resources unless the task explicitly asks for
  that operation.
- Do not run `tests/lifecycle.sh` as part of routine validation.
- Never use `HERMES_BOX_E2E=1` with the primary names or port.
- `HERMES_BOX_NETWORK_MODE=full` means unrestricted VM egress. The `none` and
  `strict` modes intentionally fail closed with smolvm 1.0.4.
- Snapshot and portable-package artifacts can contain OAuth tokens, API keys,
  sessions, memories, and generated work.

If a task explicitly requires a full lifecycle test, isolate every resource:

```bash
export HERMES_BOX_MACHINE_NAME="hermes-box-agent-$$"
export HERMES_BOX_BUILDER_NAME="hermes-builder-agent-$$"
export HERMES_BOX_SSH_PORT=2223
export HERMES_BOX_NETWORK_MODE=full
export HERMES_BOX_E2E=1
lsof -nP -iTCP:"$HERMES_BOX_SSH_PORT" -sTCP:LISTEN
# If the previous command prints a listener, choose another non-primary port.
./tests/lifecycle.sh
```

The lifecycle script creates and removes its own disposable data directory and
machines. If it is interrupted, reuse the same exported variables and run:

```bash
./bin/hermes-box destroy --force
```

Before finishing, confirm `git status --short` contains only the intended
tracked changes.
