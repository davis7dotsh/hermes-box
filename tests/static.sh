#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"

bash -n bin/hermes-box
bash -n guest/bootstrap.sh
bash -n guest/install-node.sh
bash -n guest/start.sh
bash -n guest/entrypoint.sh
bash -n guest/executor.sh
python3 -c 'compile(open("guest/extract-executor.py", encoding="utf-8").read(), "guest/extract-executor.py", "exec")'
python3 -c 'compile(open("guest/hermes_gated_approval.py", encoding="utf-8").read(), "guest/hermes_gated_approval.py", "exec")'
python3 -c 'compile(open("guest/patch-hermes-gated-approval.py", encoding="utf-8").read(), "guest/patch-hermes-gated-approval.py", "exec")'
python3 -c 'compile(open("tests/hermes-gated-approval.py", encoding="utf-8").read(), "tests/hermes-gated-approval.py", "exec")'
bash -n guest/snapshot.sh
bash -n guest/restore.sh
bash -n guest/workspace-seed.sh
bash -n guest/boxadmin.bash_profile
bash -n tests/lifecycle.sh
bash -n tests/executor-extract.sh
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
    guest/entrypoint.sh \
    guest/executor.sh \
    guest/snapshot.sh \
    guest/restore.sh \
    guest/workspace-seed.sh \
    guest/boxadmin.bash_profile \
    tests/lifecycle.sh \
    tests/executor-extract.sh \
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
grep -Fq '"--net-backend", "virtio-net"' internal/app/lifecycle.go
grep -Fq 'SMOLVM_PACK_CACHE_MAX_BYTES=17179869184' internal/app/host.go
grep -Fq '"pack", ".smolvm-extracted"' internal/app/host.go
grep -Fq "BatchMode=yes" internal/app/host.go
grep -Fq 'user=hermes' guest/supervisord.conf
grep -Fq 'HERMES_HOME="/workspace/hermes-home"' guest/supervisord.conf
grep -Fq 'CODEX_HOME=/workspace/codex-home' guest/bootstrap.sh
grep -Fq 'CODEX_INSTALL_DIR=$CODEX_HOME/bin' guest/bootstrap.sh
grep -Fq 'hermes-box-install-node 24' guest/bootstrap.sh
grep -Fq 'uv_version=0.11.21' guest/bootstrap.sh
grep -Fq '88e800834007cc5efd4675f166eb2a51e7e3ad19876d85fa8805a6fb5c922397' guest/bootstrap.sh
grep -Fq 'APT::Update::Error-Mode=any' guest/bootstrap.sh
grep -Fq 'apt index refresh failed after 12 attempts' guest/bootstrap.sh
grep -Fq 'raw.githubusercontent.com/NousResearch/hermes-agent/$HERMES_INSTALL_COMMIT/scripts/install.sh' guest/bootstrap.sh
grep -Fq 'dbd9d555ed4ac67bd1fc71ba6a39b410cf2af0ebcfd8f4889e086af78c9ddcaa' guest/bootstrap.sh
grep -Fq 'timeout --signal=TERM --kill-after=30s 20m' guest/bootstrap.sh
grep -Fq 'latest-v${node_major}.x' guest/install-node.sh
grep -Fq 'tmux \' guest/bootstrap.sh
grep -Fq -- '--extra messaging' guest/bootstrap.sh
grep -Fq 'chown -hR hermes:hermes /usr/local/lib/hermes-agent/venv' guest/bootstrap.sh
grep -Fq 'unable to repair Hermes virtualenv ownership' guest/start.sh
grep -Fq 'hermes ALL=(ALL:ALL) NOPASSWD: ALL' guest/hermes-box.sudoers
grep -Fq 'approval_policy = "never"' guest/bootstrap.sh
grep -Fq 'sandbox_mode = "danger-full-access"' guest/bootstrap.sh
grep -Fq 'trust_level = "trusted"' guest/bootstrap.sh
grep -Fq 'codex_version=0.141.0' guest/start.sh
grep -Fq 'b70030338592de3e361f3cde83d624f88061df300abe31b62075a5c5a058a6fc' guest/start.sh
grep -Fq 'startup_log=/var/log/hermes-box-startup.log' guest/start.sh
grep -Fq 'HERMES_BOX_RESTORE_MODE' guest/entrypoint.sh
grep -Fq -- '--connect-timeout 15 --max-time 600 --retry 5 --retry-all-errors' guest/start.sh
grep -Fq "venv/bin/python -c 'import discord'" internal/app/host.go
grep -Fq 'codex --strict-config --version' internal/app/host.go
grep -Fq 'skopeo \' guest/bootstrap.sh
grep -Fq 'skopeo copy' guest/executor.sh
grep -Fq 'source_image=$repository@$digest' guest/executor.sh
grep -Fq 'runtime_root=/workspace/.hermes-box-runtime/executor' guest/executor.sh
grep -Fq -- '--exclude=./.hermes-box-runtime' guest/snapshot.sh
grep -Fq -- '--exclude=./codex-home/tmp' guest/snapshot.sh
grep -Fq -- '--exclude=./codex-home/tmp' guest/restore.sh
grep -Fq -- '--exclude=./var/lib/hermes-box/restore-ready' guest/snapshot.sh
grep -Fq 'guest", "executor.sh"), "/tmp/hermes-box-executor.sh"' internal/app/lifecycle.go
grep -Fq '/tmp/hermes-box-executor.sh' guest/bootstrap.sh
grep -Fq '/usr/local/sbin/hermes-box-executor' guest/bootstrap.sh
grep -Fq 'filter="data"' guest/extract-executor.py
grep -Fq '.wh..wh..opq' guest/extract-executor.py
grep -Fq 'EXECUTOR_ALLOW_LOCAL_NETWORK=false' guest/executor.sh
grep -Fq 'EXECUTOR_HOST=0.0.0.0' guest/executor.sh
grep -Fq 'BUN_FEATURE_FLAG_DISABLE_IPV6=1' guest/executor.sh
grep -Fq 'find /workspace/executor ! -type l' guest/start.sh
grep -Fq "grep -qx 'BUN_FEATURE_FLAG_DISABLE_IPV6=1'" internal/app/host.go
grep -Fq 'ghcr.io/rhyssullivan/executor-selfhost:v1.5.12@sha256:' internal/config/config.go
grep -Fq 'MCP_EXECUTOR_API_KEY' internal/app/executor.go
grep -Fq 'tools.executor.coreTools.connections.list' internal/app/executor.go
grep -Fq '81eaedd0f5c471c7ee748990066135a684f3c962' internal/config/config.go
grep -Fq 'upstream anchor drift' guest/patch-hermes-gated-approval.py
grep -Fq 'Configuration is deliberately last' guest/patch-hermes-gated-approval.py
grep -Fq 'HERMES_GATED_APPROVAL_PATCHER=/tmp/hermes-box-patch-hermes-gated-approval.py' guest/bootstrap.sh

./tests/workspace-seed.sh
./tests/executor-extract.sh
python3 ./tests/hermes-gated-approval.py

./bin/hermes-box help >/dev/null
./bin/hermes-box executor help >/dev/null

override_help=$(
  HERMES_BOX_CONFIG="$root/tests/config-precedence.conf" \
    HERMES_BOX_SSH_PORT=2298 \
    ./bin/hermes-box help
)
grep -Fq "127.0.0.1:2298" <<<"$override_help"

echo "static checks passed"
