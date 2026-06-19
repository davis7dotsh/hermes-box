#!/usr/bin/env bash
set -euo pipefail

startup_log=/var/log/hermes-box-startup.log
startup_stage=initialization
log_startup() {
  printf '%s stage=%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    "$startup_stage" "$1" | tee -a "$startup_log"
}
startup_failed() {
  status=$?
  log_startup "failed status=$status line=${BASH_LINENO[0]}"
  exit "$status"
}
trap startup_failed ERR
log_startup started

# smolvm 1.0.4 remaps root-owned packed files to the host UID/GID. The guest
# has no legitimate UID 501, so repair those paths once before starting
# services. Some dangling symlinks cannot be lchowned through the packed
# overlay, but they do not grant access to a guest user.
find / -xdev -maxdepth 1 -type f -regextype posix-extended \
  -regex '/[0-9a-f]{16}' -delete
ownership_marker=/var/lib/hermes-box/runtime-ownership-repaired
if [[ ! -f $ownership_marker ]]; then
  find / -xdev -uid 501 -gid 20 ! -type l \
    -exec chown root:root {} + 2>/dev/null || true
  find / -xdev -uid 501 -gid 20 -type l \
    -exec chown -h root:root {} \; 2>/dev/null || true
  if find / -xdev -uid 501 -gid 20 ! -type l -print -quit |
    grep -q .; then
    printf 'unable to repair packed root ownership\n' >&2
    exit 1
  fi
  : >"$ownership_marker"
  chown root:root "$ownership_marker"
  chmod 0600 "$ownership_marker"
fi
chown root:root /tmp /var/tmp
chmod 1777 /tmp /var/tmp

if ! id boxadmin >/dev/null 2>&1; then
  useradd --uid 1001 --create-home --shell /bin/bash boxadmin
fi
if ! id hermes >/dev/null 2>&1; then
  useradd --uid 1002 --create-home --shell /bin/bash hermes
fi
if ! id sshd >/dev/null 2>&1; then
  useradd --system --home-dir /run/sshd --shell /usr/sbin/nologin sshd
fi
# smolvm can remap the builder-owned virtualenv while packing. Hermes needs
# write access so optional provider and gateway dependencies can be installed.
if [[ -d /usr/local/lib/hermes-agent/venv ]]; then
  startup_stage=repair-hermes-venv
  log_startup started
  find /usr/local/lib/hermes-agent/venv ! -type l \
    -exec chown hermes:hermes {} +
  find /usr/local/lib/hermes-agent/venv -type l \
    -exec chown -h hermes:hermes {} \; 2>/dev/null || true
  if find /usr/local/lib/hermes-agent/venv ! -type l \
    \( ! -user hermes -o ! -group hermes \) -print -quit | grep -q .; then
    printf 'unable to repair Hermes virtualenv ownership\n' >&2
    exit 1
  fi
fi
# The packed root can retain dpkg's D-Bus helper override while losing the
# corresponding system group. Recreate it so later apt installs do not abort.
if [[ -f /var/lib/dpkg/statoverride ]] &&
  grep -Eq '^[^[:space:]]+[[:space:]]+messagebus[[:space:]]+' \
    /var/lib/dpkg/statoverride &&
  ! getent group messagebus >/dev/null; then
  groupadd --system messagebus
fi
passwd --delete boxadmin
passwd --lock hermes

# smolvm 1.0.4 remaps ownership of root-created overlay files while packing.
# Repair the small set of setuid/config files that sudo validates strictly.
chown root:root \
  /usr/bin/sudo \
  /usr/bin/sudoedit \
  /usr/bin/sudoreplay \
  /usr/bin/cvtsudoers \
  /usr/sbin/visudo \
  /usr/sbin/sudo_logsrvd \
  /usr/sbin/sudo_sendlog \
  /etc/sudo.conf \
  /etc/sudoers \
  /etc/sudoers.d \
  /etc/sudoers.d/README \
  /etc/sudoers.d/hermes-box \
  /etc/pam.d/sudo \
  /etc/pam.d/sudo-i
chown -hR root:root /usr/libexec/sudo
chmod 4755 /usr/bin/sudo
chmod 0440 /etc/sudoers /etc/sudoers.d/hermes-box

guest_hostname=$(hostname)
if ! grep -Eq "[[:space:]]$guest_hostname([[:space:]]|$)" /etc/hosts; then
  printf '127.0.1.1 %s\n' "$guest_hostname" >>/etc/hosts
fi

rm -f /home/hermes/.hermes
find /home/boxadmin ! -type l -exec chown boxadmin:boxadmin {} +
find /home/boxadmin -type l \
  -exec chown -h boxadmin:boxadmin {} \; 2>/dev/null || true
find /home/hermes ! -type l -exec chown hermes:hermes {} +
find /home/hermes -type l \
  -exec chown -h hermes:hermes {} \; 2>/dev/null || true
if find /home/boxadmin ! -type l \
  \( ! -user boxadmin -o ! -group boxadmin \) -print -quit | grep -q .; then
  printf 'unable to repair boxadmin home ownership\n' >&2
  exit 1
fi
if find /home/hermes ! -type l \
  \( ! -user hermes -o ! -group hermes \) -print -quit | grep -q .; then
  printf 'unable to repair Hermes home ownership\n' >&2
  exit 1
fi
install -d -o boxadmin -g boxadmin -m 0700 /home/boxadmin/.ssh
chown boxadmin:boxadmin /home/boxadmin/.ssh/authorized_keys
chmod 0600 /home/boxadmin/.ssh/authorized_keys
install -o boxadmin -g boxadmin -m 0644 \
  /usr/local/share/hermes-box/boxadmin.bash_profile \
  /home/boxadmin/.bash_profile

ln -s /workspace/hermes-home /home/hermes/.hermes
chown -h hermes:hermes /home/hermes/.hermes

install -d -o root -g root -m 0755 /run/sshd
ssh-keygen -A

startup_stage=workspace-seed
log_startup started
/usr/local/sbin/hermes-box-workspace-seed
log_startup completed

codex_home=/workspace/codex-home
chown hermes:hermes /workspace
chmod 0750 /workspace
install -d -o hermes -g hermes -m 0700 /workspace/hermes-home
install -d -o hermes -g hermes -m 0750 /workspace/hermes-home/logs
install -d -o hermes -g hermes -m 0700 "$codex_home"
install -d -o hermes -g hermes -m 0750 "$codex_home/bin"
install -d -o hermes -g hermes -m 0750 /workspace/work

codex_config=$codex_home/config.toml
if [[ ! -f $codex_config ]]; then
  cat >"$codex_config" <<'EOF'
# Hermes Box is the outer sandbox, so Codex runs autonomously inside the VM.
approval_policy = "never"
sandbox_mode = "danger-full-access"

# The guest has no desktop keyring. Keep the login cache with the rest of the
# private Codex state under /workspace so snapshots preserve refreshed tokens.
cli_auth_credentials_store = "file"

[projects."/workspace/work"]
trust_level = "trusted"
EOF
fi
chown hermes:hermes "$codex_config"
chmod 0600 "$codex_config"

# Install on the first runtime boot instead of embedding the standalone package
# in the base image. Its large workspace payload makes smolvm 1.0.4 pack
# artifacts unreliable. The install is user-owned and persists in /workspace,
# so subsequent boots are local and `codex update` needs no sudo access.
if [[ ! -x $codex_home/bin/codex ]]; then
  startup_stage=install-codex
  log_startup started
  codex_version=0.141.0
  codex_target=aarch64-unknown-linux-musl
  codex_archive_sha256=b70030338592de3e361f3cde83d624f88061df300abe31b62075a5c5a058a6fc
  codex_standalone=$codex_home/packages/standalone
  codex_releases=$codex_standalone/releases
  codex_release=$codex_releases/$codex_version-$codex_target
  codex_staging=$codex_releases/.staging-$codex_version-$$
  codex_archive=/tmp/codex-package-$codex_target.tar.gz
  rm -rf -- \
    "$codex_release" \
    "$codex_staging" \
    "$codex_standalone/.current-$$" \
    "$codex_home/bin/.codex-$$" \
    "$codex_archive"
  curl --proto '=https' --tlsv1.2 -fsSL \
    --connect-timeout 15 --max-time 600 --retry 5 --retry-all-errors \
    "https://github.com/openai/codex/releases/download/rust-v$codex_version/codex-package-$codex_target.tar.gz" \
    -o "$codex_archive"
  printf '%s  %s\n' "$codex_archive_sha256" "$codex_archive" |
    sha256sum --check --status
  install -d -o hermes -g hermes -m 0700 "$codex_releases"
  install -d -o hermes -g hermes -m 0700 "$codex_staging"
  tar -xzf "$codex_archive" --no-same-owner -C "$codex_staging"
  grep -Fq '"version": "0.141.0"' "$codex_staging/codex-package.json"
  grep -Fq '"target": "aarch64-unknown-linux-musl"' \
    "$codex_staging/codex-package.json"
  chmod 0755 \
    "$codex_staging/bin/codex" \
    "$codex_staging/codex-path/rg"
  if [[ -f $codex_staging/codex-resources/bwrap ]]; then
    chmod 0755 "$codex_staging/codex-resources/bwrap"
  fi
  ln -s bin/codex "$codex_staging/codex"
  mv "$codex_staging" "$codex_release"
  ln -s "$codex_release" "$codex_standalone/.current-$$"
  mv -Tf "$codex_standalone/.current-$$" "$codex_standalone/current"
  ln -s "$codex_standalone/current/bin/codex" "$codex_home/bin/.codex-$$"
  mv -Tf "$codex_home/bin/.codex-$$" "$codex_home/bin/codex"
  rm -f -- "$codex_archive"
  chown -hR hermes:hermes "$codex_home"
  log_startup completed
fi
sudo -iu hermes codex --strict-config --version

hermes_env=/workspace/hermes-home/.env
touch "$hermes_env"
if ! grep -q '^HERMES_CODEX_EVENT_STALE_TIMEOUT_SECONDS=' "$hermes_env"; then
  printf '\nHERMES_CODEX_EVENT_STALE_TIMEOUT_SECONDS=120\n' >>"$hermes_env"
fi
chown hermes:hermes "$hermes_env"
chmod 0600 "$hermes_env"

executor_supervisor=/run/hermes-box-supervisor-executor.conf
rm -f "$executor_supervisor"
if [[ ${HERMES_BOX_EXECUTOR_ENABLED:-false} == true ]]; then
  install -d -o hermes -g hermes -m 0700 \
    /workspace/executor \
    /workspace/executor/data \
    /workspace/.hermes-box-runtime \
    /workspace/.hermes-box-runtime/executor
  cat >"$executor_supervisor" <<'EOF'
[program:executor]
command=/usr/local/sbin/hermes-box-executor
user=hermes
environment=HOME="/home/hermes"
priority=30
autostart=true
autorestart=unexpected
startsecs=5
startretries=20
stdout_logfile=/workspace/executor/executor.log
stdout_logfile_maxbytes=25MB
stdout_logfile_backups=5
redirect_stderr=true
stopasgroup=true
killasgroup=true
stopsignal=TERM
stopwaitsecs=45
EOF
fi

startup_stage=start-supervisor
log_startup started
trap - ERR
exec /usr/bin/supervisord -n -c /etc/supervisor/supervisord.conf
