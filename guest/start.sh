#!/usr/bin/env bash
set -euo pipefail

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
chown -hR boxadmin:boxadmin /home/boxadmin
chown -hR hermes:hermes /home/hermes
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

snapshot_dir=/var/lib/hermes-box
snapshot_id_file=$snapshot_dir/workspace-snapshot.id
snapshot_tar=$snapshot_dir/workspace-snapshot.tar
restored_id_file=$snapshot_dir/workspace-restored.id

if [[ -f $snapshot_id_file && -f $snapshot_tar ]]; then
  expected_id=$(cat "$snapshot_id_file")
  current_id=
  if [[ -f $restored_id_file ]]; then
    current_id=$(cat "$restored_id_file")
  fi

  if [[ $current_id != "$expected_id" ]]; then
    find /workspace -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
    tar -C /workspace -xpf "$snapshot_tar"
    printf '%s\n' "$expected_id" >"$restored_id_file"
    chown root:root "$restored_id_file"
    chmod 0600 "$restored_id_file"
    sync
  fi
fi

chown hermes:hermes /workspace
chmod 0750 /workspace
install -d -o hermes -g hermes -m 0700 /workspace/hermes-home
install -d -o hermes -g hermes -m 0750 /workspace/hermes-home/logs
install -d -o hermes -g hermes -m 0750 /workspace/work

hermes_env=/workspace/hermes-home/.env
touch "$hermes_env"
if ! grep -q '^HERMES_CODEX_EVENT_STALE_TIMEOUT_SECONDS=' "$hermes_env"; then
  printf '\nHERMES_CODEX_EVENT_STALE_TIMEOUT_SECONDS=120\n' >>"$hermes_env"
fi
chown hermes:hermes "$hermes_env"
chmod 0600 "$hermes_env"

exec /usr/bin/supervisord -n -c /etc/supervisor/supervisord.conf
