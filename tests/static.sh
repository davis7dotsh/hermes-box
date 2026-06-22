#!/usr/bin/env bash
# shellcheck disable=SC2016 # grep patterns intentionally contain shell literals.
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
bash -n guest/tm
bash -n tests/lifecycle.sh
bash -n tests/executor-extract.sh
bash -n tests/executor-runtime.sh
bash -n tests/workspace-seed.sh
bash -n tests/tmux.sh
visudo -cf guest/hermes-box.sudoers

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
    guest/tm \
    tests/lifecycle.sh \
    tests/executor-extract.sh \
    tests/executor-runtime.sh \
    tests/workspace-seed.sh \
    tests/tmux.sh
else
  printf 'shellcheck not installed; skipping local shell lint\n' >&2
fi

grep -Fq "PermitRootLogin no" guest/bootstrap.sh
grep -Fq "AllowAgentForwarding no" guest/bootstrap.sh
grep -Fq "AllowTcpForwarding no" guest/bootstrap.sh
grep -Fq "DisableForwarding yes" guest/bootstrap.sh
grep -Fq "AcceptEnv COLORTERM TERM_PROGRAM TERM_PROGRAM_VERSION" guest/bootstrap.sh
grep -Fq "rm -f /etc/ssh/ssh_host_*" guest/bootstrap.sh
grep -Fq "runtime-ownership-v2" guest/start.sh
grep -Fq 'packed_root_uid=$(stat -c %u /usr/bin/sudo)' guest/start.sh
grep -Fq '/usr/bin/sudo.ws' guest/start.sh
grep -Fq '/usr/bin/cvtsudoers.ws' guest/start.sh
grep -Fq '/usr/sbin/sudo_sendlog.ws' guest/start.sh
grep -Fq "runtime-ownership-v*" guest/bootstrap.sh
grep -Fq "runtime-ownership-v*" guest/restore.sh
grep -Fq "groupadd --system messagebus" guest/start.sh
grep -Fq "workspace-restored.id" guest/workspace-seed.sh
grep -Fq "strict mode is unavailable" internal/app/host.go
grep -Fq "no-egress mode is unavailable" internal/app/host.go
grep -Eq 'backupFormat[[:space:]]*=[[:space:]]*"hermes-box-v2"' internal/app/backup.go
grep -Fq "hermes-gateway.log" internal/app/lifecycle.go
grep -Fq '"--net-backend", "virtio-net"' internal/app/lifecycle.go
grep -Fq 'image = "ubuntu:26.04"' Smolfile
grep -Fq 'const ubuntuImage = "ubuntu:26.04"' internal/app/app.go
grep -Fq 'SMOLVM_PACK_CACHE_MAX_BYTES=17179869184' internal/app/host.go
grep -Fq '"pack", ".smolvm-extracted"' internal/app/host.go
grep -Fq "BatchMode=yes" internal/app/host.go
grep -Fq 'user=hermes' guest/supervisord.conf
grep -Fq 'HERMES_HOME="/workspace/hermes-home"' guest/supervisord.conf
grep -Fq 'defaultNetworkMode   = "full"' internal/config/config.go
grep -Fq 'HERMES_BOX_NETWORK_MODE=full' hermes-box.conf.example
grep -Fq 'CODEX_HOME=/workspace/codex-home' guest/bootstrap.sh
grep -Fq 'CODEX_INSTALL_DIR=$CODEX_HOME/bin' guest/bootstrap.sh
grep -Fq 'hermes-box-install-node 24' guest/bootstrap.sh
grep -Fq 'uv_version=0.11.23' guest/bootstrap.sh
grep -Fq '1873a77350f6621279ae1a0d2227f2bd8b67131598f14a7eb0ba2215d3da2c98' guest/bootstrap.sh
grep -Fq 'uv self version --short | grep -Fqx 0.11.23' tests/lifecycle.sh
grep -Fq 'APT::Update::Error-Mode=any' guest/bootstrap.sh
grep -Fq 'Acquire::http::Timeout "30"' guest/bootstrap.sh
grep -Fq 'Acquire::https::Timeout "30"' guest/bootstrap.sh
grep -Fq 'timeout --signal=TERM --kill-after=30s 3m' guest/bootstrap.sh
grep -Fq 'apt index refresh failed after 6 bounded attempts' guest/bootstrap.sh
grep -Fq 'apt package installation timed out after 20 minutes' guest/bootstrap.sh
grep -Fq 'raw.githubusercontent.com/NousResearch/hermes-agent/$HERMES_INSTALL_COMMIT/scripts/install.sh' guest/bootstrap.sh
grep -Fq 'dbd9d555ed4ac67bd1fc71ba6a39b410cf2af0ebcfd8f4889e086af78c9ddcaa' guest/bootstrap.sh
grep -Fq 'timeout --signal=TERM --kill-after=30s 20m' guest/bootstrap.sh
grep -Fq 'installer_stages=(repository venv python-deps path config complete)' guest/bootstrap.sh
grep -Fq 'latest-v${node_major}.x' guest/install-node.sh
grep -Fq '  tmux' guest/bootstrap.sh
grep -Fq '  ncurses-term' guest/bootstrap.sh
grep -Fq 'infocmp -x xterm-ghostty' guest/bootstrap.sh
grep -Fq 'infocmp -x xterm-ghostty >/dev/null 2>&1 || true' guest/bootstrap.sh
grep -Fq 'SendEnv=COLORTERM' internal/app/host.go
grep -Fq 'SendEnv=TERM_PROGRAM' internal/app/host.go
grep -Fq 'SendEnv=TERM_PROGRAM_VERSION' internal/app/host.go
grep -Fq 'guest", "tm"), "/tmp/hermes-box-tm"' internal/app/lifecycle.go
grep -Fq 'guest", "tmux.conf"), "/tmp/hermes-box-tmux.conf"' internal/app/lifecycle.go
grep -Fq '/tmp/hermes-box-current-tm /usr/local/bin/tm' guest/restore.sh
grep -Fq '/tmp/hermes-box-current-tmux.conf /etc/tmux.conf' guest/restore.sh
grep -Fq 'guest/tm"' internal/app/portable.go
grep -Fq 'guest/tmux.conf"' internal/app/portable.go
grep -Fq -- '--extra messaging' guest/bootstrap.sh
grep -Fq -- '--locked' guest/bootstrap.sh
grep -Fq 'chown -hR hermes:hermes /usr/local/lib/hermes-agent/venv' guest/bootstrap.sh
grep -Fq 'unable to repair Hermes virtualenv ownership' guest/start.sh
grep -Fq 'hermes ALL=(ALL:ALL) NOPASSWD: ALL' guest/hermes-box.sudoers
grep -Fq 'approval_policy = "never"' guest/bootstrap.sh
grep -Fq 'sandbox_mode = "danger-full-access"' guest/bootstrap.sh
grep -Fq 'trust_level = "trusted"' guest/bootstrap.sh
grep -Fq 'codex_version=0.141.0' guest/start.sh
grep -Fq 'b70030338592de3e361f3cde83d624f88061df300abe31b62075a5c5a058a6fc' guest/start.sh
grep -Fq 'startup_log=/var/log/hermes-box-startup.log' guest/start.sh
grep -Fq 'guest_hostname=$(hostname)' guest/bootstrap.sh
grep -Fq 'HERMES_BOX_RESTORE_MODE' guest/entrypoint.sh
grep -Fq -- '--connect-timeout 15 --max-time 600 --retry 5 --retry-all-errors' guest/start.sh
grep -Fq "venv/bin/python -c 'import discord'" internal/app/host.go
grep -Fq 'codex --strict-config --version' internal/app/host.go
grep -Fq 'curl --connect-timeout 2 --max-time 5' internal/app/host.go
grep -Fq 'curl --connect-timeout 1 --max-time 2' internal/app/host.go
grep -Fq 'startupTimeout = 2 * time.Hour' internal/app/host.go
grep -Fq '"machine", "list", "--json"' internal/app/host.go
grep -Fq '/workspace/executor/executor.log' internal/app/host.go
grep -Fq '/workspace/hermes-home/logs/supervisord.log' internal/app/host.go
grep -Fq '/workspace/hermes-home/logs/sshd.log' internal/app/host.go
grep -Fq 'supervisorctl", "restart", "hermes"' internal/app/host.go
grep -Fq 'executorHermesRefreshMarker' internal/app/host.go
grep -Fq '/proc/sys/kernel/random/boot_id' internal/app/host.go
grep -Fq 'apt_packages+=(skopeo)' guest/bootstrap.sh
grep -Fq 'HERMES_BOX_EXECUTOR_ENABLED=' internal/app/lifecycle.go
grep -Fq 'skopeo copy' guest/executor.sh
grep -Fq 'source_image=$(canonical_repository_digest "$image")' guest/executor.sh
grep -Fq '.repository-digest' guest/executor.sh
grep -Fq 'pull_stall_seconds=${HERMES_BOX_EXECUTOR_PULL_STALL_SECONDS:-600}' guest/executor.sh
grep -Fq 'run_with_stall_deadline "$pull_stall_seconds"' guest/executor.sh
grep -Fq 'du -sk "$progress_path"' guest/executor.sh
grep -Fq 'prune_oci_transport_temps' guest/executor.sh
grep -Fq '"oci:$oci_cache:executor"' guest/executor.sh
grep -Fq 'timeout --signal=TERM --kill-after=30s 5m' guest/executor.sh
grep -Fq 'timeout --signal=TERM --kill-after=5s 30s' guest/executor.sh
grep -Fq '"$path/usr/local/bin/bun" build' guest/executor.sh
grep -Fq -- '--target=bun' guest/executor.sh
grep -Fq 'EXECUTOR_SECRET_KEY=hermes-box-validation-only-secret-key' guest/executor.sh
grep -Fq 'flock -x -w 3300 9' guest/executor.sh
grep -Fq 'HERMES_BOX_EXECUTOR_RUNTIME_ROOT:-/workspace/.hermes-box-runtime/executor' guest/executor.sh
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
grep -Fq 'ghcr.io/rhyssullivan/executor-selfhost:v1.5.16@sha256:' internal/config/config.go
grep -Fq 'MCP_EXECUTOR_API_KEY' internal/app/executor.go
grep -Fq 'tools.executor.coreTools.connections.list' internal/app/executor.go
grep -Fq 'status [--json] [--sizes]' internal/app/executor.go
grep -Fq 'context.WithTimeout(ctx, a.startupDeadline())' internal/app/executor.go
grep -Fq '2bd1977d8fad185c9b4be47884f7e87f1add0ce3' internal/config/config.go
grep -Fq 'upstream anchor drift' guest/patch-hermes-gated-approval.py
grep -Fq 'Configuration is deliberately last' guest/patch-hermes-gated-approval.py
grep -Fq 'HERMES_GATED_APPROVAL_PATCHER=/tmp/hermes-box-patch-hermes-gated-approval.py' guest/bootstrap.sh
grep -Fq 'hermes auth add openai-codex --type oauth' internal/app/lifecycle.go
grep -Fq 'verify_restored_state fresh-restore' tests/lifecycle.sh
grep -Fq 'completed Codex setup and verified Executor blobs are retained' internal/app/lifecycle.go
grep -Fq 'The preserved machine may contain injected secrets' internal/app/lifecycle.go
if [[ $(grep -Fc 'deleteMachineForCleanup(a.config.BuilderName)' internal/app/lifecycle.go) -ne 2 ]]; then
  printf 'builder cleanup must use bounded fresh contexts on success and defer\n' >&2
  exit 1
fi

for download_script in guest/bootstrap.sh guest/install-node.sh; do
  grep -Fq -- '--connect-timeout 15' "$download_script"
  grep -Fq -- '--max-time 600' "$download_script"
  grep -Fq -- '--retry-max-time 600' "$download_script"
  grep -Fq -- '--retry-all-errors' "$download_script"
  curl_count=$(grep -Ec '^[[:space:]]*curl ' "$download_script")
  bounded_curl_count=$(grep -Fc 'curl "${download_curl_args[@]}"' "$download_script")
  if [[ $curl_count -ne 2 || $bounded_curl_count -ne $curl_count ]]; then
    printf '%s must keep both downloads on the bounded curl arguments\n' \
      "$download_script" >&2
    exit 1
  fi
done

if grep -Fq '"$hermes_home/bin/uv" pip install' guest/bootstrap.sh; then
  printf 'bootstrap must keep messaging dependencies on the locked uv sync path\n' >&2
  exit 1
fi
if grep -Fq 'first runtime start failed; deleted' internal/app/lifecycle.go; then
  printf 'first-start failures must preserve resumable owned runtime state\n' >&2
  exit 1
fi
if grep -Fq 'executorStartTimeout' internal/app/executor.go; then
  printf 'Executor auto-start must use the centralized startup deadline\n' >&2
  exit 1
fi
if grep -Fq ': "${HERMES_BOX_NETWORK_MODE' tests/lifecycle.sh; then
  printf 'lifecycle test must exercise the default full network mode\n' >&2
  exit 1
fi
if grep -RFq 'hermes login --provider openai-codex' README.md AGENTS.md internal/app; then
  printf 'operator guidance contains the removed Hermes login command\n' >&2
  exit 1
fi

./tests/workspace-seed.sh
./tests/tmux.sh
./tests/executor-extract.sh
./tests/executor-runtime.sh
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
