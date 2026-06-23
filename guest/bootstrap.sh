#!/usr/bin/env bash
set -euo pipefail

if ((EUID != 0)); then
  printf 'bootstrap must run as root\n' >&2
  exit 1
fi

provisioner=${1:?usage: bootstrap.sh PROVISIONER_DIR LIMA_DATA_MOUNT}
data_source=${2:?usage: bootstrap.sh PROVISIONER_DIR LIMA_DATA_MOUNT}

for required in \
  "$provisioner/hermes-box-guest" \
  "$provisioner/hermes.service" \
  "$provisioner/executor.service" \
  "$provisioner/hermes-box-recover.service" \
  "$provisioner/hermes-box-recover" \
  "$provisioner/hermes-box.sudoers" \
  "$provisioner/cloud-init.yaml" \
  "$provisioner/tm" \
  "$provisioner/tmux.conf" \
  "$provisioner/xterm-ghostty.terminfo"; do
  [[ -f $required ]] || {
    printf 'missing provisioner input: %s\n' "$required" >&2
    exit 1
  }
done

if [[ -d $provisioner/debs ]]; then
  mapfile -t debs < <(find "$provisioner/debs" -maxdepth 1 -type f -name '*.deb' -print | sort)
  ((${#debs[@]} > 0)) || {
    printf 'provisioner deb directory is empty\n' >&2
    exit 1
  }
  dpkg --install "${debs[@]}"
fi

[[ $data_source == /* && -d $data_source ]] || {
  printf 'Lima data mount must be an absolute directory: %s\n' "$data_source" >&2
  exit 1
}
mountpoint -q "$data_source" || {
  printf 'Lima data disk is not mounted: %s\n' "$data_source" >&2
  exit 1
}
filesystem=$(findmnt --noheadings --output FSTYPE --target "$data_source" | tr -d '[:space:]')
[[ $filesystem == ext4 ]] || {
  printf 'persistent data mount must be ext4, got %s\n' "$filesystem" >&2
  exit 1
}

mkdir -p /data
if ! grep -Fq "$data_source /data " /etc/fstab; then
  printf '%s /data none bind,x-systemd.requires-mounts-for=%s 0 0\n' "$data_source" "$data_source" >>/etc/fstab
fi
mountpoint -q /data || mount --bind "$data_source" /data

if ! id agent >/dev/null 2>&1; then
  useradd --uid 1000 --create-home --shell /bin/bash agent
fi
[[ $(id -u agent) == 1000 && $(id -g agent) == 1000 ]] || {
  printf 'agent must have uid/gid 1000\n' >&2
  exit 1
}

# Lima initially places its transport key in the disposable home. Keep that
# host-owned transport identity on the root disk before the persistent home is
# mounted over it, so restores receive a fresh key instead of carrying one in
# the data backup.
[[ -s /home/agent/.ssh/authorized_keys ]] || {
  printf 'Lima transport key is missing before persistent-home bind\n' >&2
  exit 1
}
install -D -o root -g root -m 0600 \
  /home/agent/.ssh/authorized_keys /etc/ssh/authorized_keys.d/agent

# Apply the reviewed SSH fragment only after the Lima transport key has a
# root-owned home outside the soon-to-be-persistent agent home. Reloading sshd
# preserves this provisioning connection while making every new connection use
# only the root-owned key path.
command -v cloud-init >/dev/null 2>&1 || {
  printf 'cloud-init is required to apply the reviewed SSH configuration\n' >&2
  exit 1
}
cloud-init schema -c "$provisioner/cloud-init.yaml"
cloud-init single --name write_files --frequency always \
  --file "$provisioner/cloud-init.yaml"
[[ $(stat -c '%U:%G:%a' /etc/ssh/sshd_config.d/00-hermes-box.conf) == root:root:644 ]] || {
  printf 'Hermes Box sshd configuration has unsafe ownership or permissions\n' >&2
  exit 1
}
[[ $(stat -c '%U:%G:%a' /etc/ssh/authorized_keys.d/agent) == root:root:600 ]] || {
  printf 'Lima transport key has unsafe ownership or permissions\n' >&2
  exit 1
}
/usr/sbin/sshd -t
effective_sshd=$(/usr/sbin/sshd -T -C user=agent,host=localhost,addr=127.0.0.1)
grep -Fxq 'authorizedkeysfile /etc/ssh/authorized_keys.d/%u' <<<"$effective_sshd" || {
  printf 'sshd did not select the root-owned Hermes Box authorized-keys path\n' >&2
  exit 1
}
grep -Fxq 'passwordauthentication no' <<<"$effective_sshd" || {
  printf 'sshd did not disable password authentication\n' >&2
  exit 1
}
grep -Fxq 'kbdinteractiveauthentication no' <<<"$effective_sshd" || {
  printf 'sshd did not disable keyboard-interactive authentication\n' >&2
  exit 1
}
systemctl reload ssh.service
systemctl is-active --quiet ssh.service
rm -f /home/agent/.ssh/authorized_keys

mkdir -p /data/home/agent/workspace /data/executor /data/cache
chown -R 1000:1000 /data/home /data/executor /data/cache
chmod 0700 /data/home/agent /data/executor

mkdir -p /home/agent
if ! grep -Fq '/data/home/agent /home/agent ' /etc/fstab; then
  printf '/data/home/agent /home/agent none bind,x-systemd.requires-mounts-for=/data 0 0\n' >>/etc/fstab
fi
mountpoint -q /home/agent || mount --bind /data/home/agent /home/agent
ln -sfn /home/agent/workspace /workspace

install -D -m 0755 "$provisioner/hermes-box-guest" /usr/local/libexec/hermes-box-guest
install -D -m 0755 "$provisioner/hermes-box-recover" /usr/local/libexec/hermes-box-recover
install -D -m 0755 "$provisioner/tm" /usr/local/bin/tm
install -D -m 0644 "$provisioner/tmux.conf" /etc/tmux.conf
tic -x -o /usr/share/terminfo "$provisioner/xterm-ghostty.terminfo"
infocmp -x xterm-ghostty >/dev/null
install -D -m 0644 "$provisioner/hermes.service" /etc/systemd/system/hermes.service
install -D -m 0644 "$provisioner/executor.service" /etc/systemd/system/executor.service
install -D -m 0644 "$provisioner/hermes-box-recover.service" /etc/systemd/system/hermes-box-recover.service
install -D -m 0440 "$provisioner/hermes-box.sudoers" /etc/sudoers.d/hermes-box

cat >/etc/profile.d/hermes-box.sh <<'EOF'
export PATH=/opt/hermes-box/tooling/current/node/bin:/opt/hermes-box/tooling/current/uv/bin:/opt/hermes-box/current/claude/bin:/opt/hermes-box/current/codex/bin:/opt/hermes-box/current/hermes/bin:$PATH
export CODEX_HOME=/home/agent/.codex
export HERMES_HOME=/home/agent/.hermes
export DISABLE_AUTOUPDATER=1
if [[ $- == *i* && -n ${SSH_CONNECTION:-} ]]; then
  exec tm
fi
EOF
chmod 0644 /etc/profile.d/hermes-box.sh

mkdir -p /opt/hermes-box/releases /opt/hermes-box/current \
  /opt/hermes-box/tooling/current /var/lib/hermes-box /etc/hermes-box
chmod 0700 /var/lib/hermes-box
systemctl daemon-reload
systemctl disable hermes.service executor.service >/dev/null 2>&1 || true
systemctl enable --now hermes-box-recover.service
