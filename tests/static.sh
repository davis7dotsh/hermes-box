#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"

bash -n bin/hermes-box
bash -n guest/bootstrap.sh
bash -n guest/install-node.sh
bash -n guest/start.sh
bash -n guest/snapshot.sh
bash -n guest/restore.sh
bash -n guest/workspace-seed.sh
bash -n guest/boxadmin.bash_profile
bash -n tests/lifecycle.sh
bash -n tests/workspace-seed.sh
visudo -cf guest/hermes-box.sudoers

test -z "$(gofmt -l ./cmd ./internal)"
go vet ./...
go test ./...

if command -v shellcheck >/dev/null 2>&1; then
  shellcheck \
    bin/hermes-box \
    guest/bootstrap.sh \
    guest/install-node.sh \
    guest/start.sh \
    guest/snapshot.sh \
    guest/restore.sh \
    guest/workspace-seed.sh \
    guest/boxadmin.bash_profile \
    tests/lifecycle.sh \
    tests/workspace-seed.sh
else
  printf 'shellcheck not installed; skipping local shell lint\n' >&2
fi

grep -Fq "PermitRootLogin no" guest/bootstrap.sh
grep -Fq "AllowAgentForwarding no" guest/bootstrap.sh
grep -Fq "AllowTcpForwarding no" guest/bootstrap.sh
grep -Fq "rm -f /etc/ssh/ssh_host_*" guest/bootstrap.sh
grep -Fq "runtime-ownership-repaired" guest/start.sh
grep -Fq "groupadd --system messagebus" guest/start.sh
grep -Fq "workspace-restored.id" guest/workspace-seed.sh
grep -Fq "strict mode is unavailable" internal/app/host.go
grep -Fq "no-egress mode is unavailable" internal/app/host.go
grep -Fq 'backupFormat = "hermes-box-v2"' internal/app/backup.go
grep -Fq "hermes-gateway.log" internal/app/lifecycle.go
grep -Fq "BatchMode=yes" internal/app/host.go
grep -Fq 'user=hermes' guest/supervisord.conf
grep -Fq 'HERMES_HOME="/workspace/hermes-home"' guest/supervisord.conf
grep -Fq 'CODEX_HOME=/workspace/codex-home' guest/bootstrap.sh
grep -Fq 'CODEX_INSTALL_DIR=$CODEX_HOME/bin' guest/bootstrap.sh
grep -Fq 'hermes-box-install-node 24' guest/bootstrap.sh
grep -Fq 'latest-v${node_major}.x' guest/install-node.sh
grep -Fq 'tmux \' guest/bootstrap.sh
grep -Fq -- '--extra messaging' guest/bootstrap.sh
grep -Fq 'chown -hR hermes:hermes /usr/local/lib/hermes-agent/venv' guest/bootstrap.sh
grep -Fq 'chown -hR hermes:hermes /usr/local/lib/hermes-agent/venv' guest/start.sh
grep -Fq 'hermes ALL=(ALL:ALL) NOPASSWD: ALL' guest/hermes-box.sudoers
grep -Fq 'approval_policy = "never"' guest/bootstrap.sh
grep -Fq 'sandbox_mode = "danger-full-access"' guest/bootstrap.sh
grep -Fq 'trust_level = "trusted"' guest/bootstrap.sh
grep -Fq 'https://chatgpt.com/codex/install.sh' guest/start.sh
grep -Fq "venv/bin/python -c 'import discord'" internal/app/host.go
grep -Fq 'codex --strict-config --version' internal/app/host.go

./tests/workspace-seed.sh

./bin/hermes-box help >/dev/null

override_help=$(
  HERMES_BOX_CONFIG="$root/tests/config-precedence.conf" \
    HERMES_BOX_SSH_PORT=2224 \
    ./bin/hermes-box help
)
grep -Fq "127.0.0.1:2224" <<<"$override_help"

echo "static checks passed"
