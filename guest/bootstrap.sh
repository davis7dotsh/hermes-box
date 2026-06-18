#!/usr/bin/env bash
set -euo pipefail

if [[ $EUID -ne 0 ]]; then
  echo "bootstrap must run as root" >&2
  exit 1
fi

public_key_file=${1:?public key file is required}
hermes_home=/workspace/hermes-home
codex_home=/workspace/codex-home

export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y --no-install-recommends \
  bash \
  ca-certificates \
  curl \
  git \
  openssh-server \
  procps \
  ripgrep \
  sudo \
  supervisor \
  xz-utils

if ! id boxadmin >/dev/null 2>&1; then
  useradd --uid 1001 --create-home --shell /bin/bash boxadmin
fi
if ! id hermes >/dev/null 2>&1; then
  useradd --uid 1002 --create-home --shell /bin/bash hermes
fi
passwd --delete boxadmin
passwd --lock hermes

install -d -o boxadmin -g boxadmin -m 0700 /home/boxadmin/.ssh
install -o boxadmin -g boxadmin -m 0600 \
  "$public_key_file" /home/boxadmin/.ssh/authorized_keys
install -d -o root -g root -m 0755 /usr/local/share/hermes-box
install -o root -g root -m 0644 \
  /tmp/hermes-box-boxadmin.bash_profile \
  /usr/local/share/hermes-box/boxadmin.bash_profile
install -o boxadmin -g boxadmin -m 0644 \
  /usr/local/share/hermes-box/boxadmin.bash_profile \
  /home/boxadmin/.bash_profile

cat >/etc/ssh/sshd_config.d/99-hermes-box.conf <<'EOF'
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin no
PubkeyAuthentication yes
AllowUsers boxadmin
AllowAgentForwarding no
AllowTcpForwarding no
GatewayPorts no
X11Forwarding no
PermitTunnel no
PermitUserEnvironment no
EOF

cat >/etc/sudoers.d/hermes-box <<'EOF'
Defaults:boxadmin !authenticate
boxadmin ALL=(hermes) NOPASSWD: ALL
EOF
chmod 0440 /etc/sudoers.d/hermes-box
visudo --check

chown hermes:hermes /workspace
chmod 0750 /workspace
install -d -o hermes -g hermes -m 0700 "$hermes_home"
install -d -o hermes -g hermes -m 0750 "$hermes_home/logs"
install -d -o hermes -g hermes -m 0700 "$codex_home"
install -d -o hermes -g hermes -m 0750 "$codex_home/bin"
install -d -o hermes -g hermes -m 0750 /workspace/work

rm -rf /home/hermes/.hermes
ln -s "$hermes_home" /home/hermes/.hermes
chown -h hermes:hermes /home/hermes/.hermes

cat >/etc/profile.d/hermes-box.sh <<'EOF'
export HERMES_HOME=/workspace/hermes-home
export CODEX_HOME=/workspace/codex-home
export CODEX_INSTALL_DIR=$CODEX_HOME/bin
export PATH=$CODEX_HOME/bin:/usr/local/bin:$PATH
cd /workspace/work 2>/dev/null || true
EOF
chmod 0644 /etc/profile.d/hermes-box.sh

install -o root -g root -m 0755 \
  /tmp/hermes-box-start.sh /usr/local/sbin/hermes-box-start
install -o root -g root -m 0755 \
  /tmp/hermes-box-workspace-seed.sh \
  /usr/local/sbin/hermes-box-workspace-seed
install -o root -g root -m 0644 \
  /tmp/hermes-box-supervisord.conf /etc/supervisor/supervisord.conf

curl -fsSL --retry 3 \
  https://hermes-agent.nousresearch.com/install.sh \
  -o /tmp/hermes-install.sh
chmod 0755 /tmp/hermes-install.sh

installer_args=(
  --skip-setup
  --skip-browser
  --hermes-home "$hermes_home"
)
if [[ -n ${HERMES_INSTALL_COMMIT:-} ]]; then
  installer_args+=(--commit "$HERMES_INSTALL_COMMIT")
fi

HERMES_HOME="$hermes_home" /tmp/hermes-install.sh "${installer_args[@]}"

cat >"$codex_home/config.toml" <<'EOF'
# Hermes Box is the outer sandbox, so Codex runs autonomously inside the VM.
approval_policy = "never"
sandbox_mode = "danger-full-access"

# The guest has no desktop keyring. Keep the login cache with the rest of the
# private Codex state under /workspace so snapshots preserve refreshed tokens.
cli_auth_credentials_store = "file"

[projects."/workspace/work"]
trust_level = "trusted"
EOF
chown hermes:hermes "$codex_home/config.toml"
chmod 0600 "$codex_home/config.toml"

# Hermes 0.16.0 defaults small Codex requests to a 12-second gap between SSE
# events. Reasoning models can legitimately stay quiet longer than that.
hermes_env=$hermes_home/.env
if ! grep -q '^HERMES_CODEX_EVENT_STALE_TIMEOUT_SECONDS=' "$hermes_env"; then
  printf '\nHERMES_CODEX_EVENT_STALE_TIMEOUT_SECONDS=120\n' >>"$hermes_env"
fi
chown hermes:hermes "$hermes_env"
chmod 0600 "$hermes_env"

# The gateway and Python CLI do not require the browser/TUI npm trees. Removing
# them keeps packed artifacts small enough for reliable smolvm 1.0.4 restores.
rm -rf \
  /root/.cache \
  /root/.npm \
  /usr/local/lib/hermes-agent/.git \
  /usr/local/lib/hermes-agent/node_modules \
  /usr/local/lib/hermes-agent/ui-tui/node_modules \
  /usr/local/lib/hermes-agent/web/node_modules \
  /usr/local/lib/hermes-agent/apps \
  /usr/local/lib/hermes-agent/tests \
  /usr/local/lib/hermes-agent/website \
  "$hermes_home/node"

# Keep the source checkout root-owned, but allow Hermes to lazy-install optional
# Python dependencies into its own virtual environment.
if [[ -d /usr/local/lib/hermes-agent/venv ]]; then
  chown -R hermes:hermes /usr/local/lib/hermes-agent/venv
fi

chown -R hermes:hermes "$hermes_home" "$codex_home" /workspace/work
sudo -iu hermes env HERMES_HOME="$hermes_home" hermes --version

install -d -m 0755 /run/sshd
ssh-keygen -A
/usr/sbin/sshd -t

# smolvm 1.0.4 pack snapshots do not preserve the named machine's /workspace
# disk. Embed a seed copy in the root overlay so the first runtime boot can
# reconstruct the private workspace without any host mount.
install -d -m 0700 /var/lib/hermes-box
rm -f /var/lib/hermes-box/runtime-ownership-repaired
snapshot_id="base-$(date +%s)-$$"
printf '%s\n' "$snapshot_id" >/workspace/.hermes-box-snapshot-id
tar -C /workspace -cpf /var/lib/hermes-box/workspace-snapshot.tar .
printf '%s\n' "$snapshot_id" >/var/lib/hermes-box/workspace-snapshot.id
chmod 0600 \
  /var/lib/hermes-box/workspace-snapshot.tar \
  /var/lib/hermes-box/workspace-snapshot.id
sync

# Runtime machines create unique host keys on first boot. Established-machine
# snapshots retain their keys because this deletion only happens in the builder.
rm -f /etc/ssh/ssh_host_*
rm -rf /var/lib/apt/lists/*
