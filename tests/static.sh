#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

bash -n "$root/bin/hermes-box"
bash -n "$root/guest/bootstrap.sh"
bash -n "$root/guest/start.sh"
bash -n "$root/guest/boxadmin.bash_profile"

grep -Fq 'while [[ -L $source_path ]]' "$root/bin/hermes-box"
grep -Fq "PermitRootLogin no" "$root/guest/bootstrap.sh"
grep -Fq "AllowAgentForwarding no" "$root/guest/bootstrap.sh"
grep -Fq "AllowTcpForwarding no" "$root/guest/bootstrap.sh"
grep -Fq "rm -f /etc/ssh/ssh_host_*" "$root/guest/bootstrap.sh"
grep -Fq "/root/.cache" "$root/guest/bootstrap.sh"
grep -Fq "/root/.npm" "$root/guest/bootstrap.sh"
grep -Fq "workspace-snapshot.tar" "$root/guest/bootstrap.sh"
grep -Fq "workspace-snapshot.tar" "$root/guest/start.sh"
grep -Fq "runtime-ownership-repaired" "$root/guest/start.sh"
grep -Fq "chown hermes:hermes /workspace" "$root/guest/bootstrap.sh"
grep -Fq "chown hermes:hermes /workspace" "$root/guest/start.sh"
grep -Fq "HERMES_CODEX_EVENT_STALE_TIMEOUT_SECONDS=120" "$root/guest/bootstrap.sh"
grep -Fq "HERMES_CODEX_EVENT_STALE_TIMEOUT_SECONDS=120" "$root/guest/start.sh"
grep -Fq "exec sudo -iu hermes" "$root/guest/boxadmin.bash_profile"
grep -Fq "hermes-box-boxadmin.bash_profile" "$root/bin/hermes-box"
grep -Fq "boxadmin.bash_profile" "$root/guest/bootstrap.sh"
grep -Fq "boxadmin.bash_profile" "$root/guest/start.sh"
grep -Fq "chmod 1777 /tmp /var/tmp" "$root/guest/start.sh"
grep -Fq "strict mode is unavailable" "$root/bin/hermes-box"
grep -Fq "no-egress mode is unavailable" "$root/bin/hermes-box"
grep -Fq "format=hermes-box-v2" "$root/bin/hermes-box"
grep -Fq "user=hermes" "$root/guest/supervisord.conf"
grep -Fq "HERMES_HOME=\"/workspace/hermes-home\"" "$root/guest/supervisord.conf"

"$root/bin/hermes-box" help >/dev/null

override_help=$(
  HERMES_BOX_CONFIG="$root/tests/config-precedence.conf" \
    HERMES_BOX_SSH_PORT=2224 \
    "$root/bin/hermes-box" help
)
grep -Fq "127.0.0.1:2224" <<<"$override_help"

echo "static checks passed"
