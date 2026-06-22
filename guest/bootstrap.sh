#!/usr/bin/env bash
set -euo pipefail

if [[ $EUID -ne 0 ]]; then
  echo "bootstrap must run as root" >&2
  exit 1
fi

public_key_file=${1:?public key file is required}

# smolvm's builder hostname is not always present in /etc/hosts. Add it before
# the first sudo invocation so provisioning stays quiet and name lookups do not
# stall while apt and the pinned installers are running.
guest_hostname=$(hostname)
if ! grep -Eq "[[:space:]]$guest_hostname([[:space:]]|$)" /etc/hosts; then
  printf '127.0.1.1 %s\n' "$guest_hostname" >>/etc/hosts
fi

hermes_home=/workspace/hermes-home
codex_home=/workspace/codex-home
supported_hermes_commit=81eaedd0f5c471c7ee748990066135a684f3c962
hermes_installer_sha256=dbd9d555ed4ac67bd1fc71ba6a39b410cf2af0ebcfd8f4889e086af78c9ddcaa
uv_version=0.11.21
uv_archive_sha256=88e800834007cc5efd4675f166eb2a51e7e3ad19876d85fa8805a6fb5c922397
download_curl_args=(
  --proto '=https'
  --tlsv1.2
  -fsSL
  --connect-timeout 15
  --max-time 600
  --retry 5
  --retry-delay 2
  --retry-max-time 600
  --retry-all-errors
)

export DEBIAN_FRONTEND=noninteractive
cat >/etc/apt/apt.conf.d/80-hermes-box-network <<'EOF'
Acquire::ForceIPv4 "true";
Acquire::Retries "3";
Acquire::http::Timeout "30";
Acquire::https::Timeout "30";
EOF

apt_ready=false
for attempt in {1..6}; do
  set +e
  timeout --signal=TERM --kill-after=30s 3m \
    apt-get update -o APT::Update::Error-Mode=any
  apt_status=$?
  set -e
  if [[ $apt_status -eq 0 ]]; then
    apt_ready=true
    break
  fi
  if [[ $apt_status -eq 124 || $apt_status -eq 137 ]]; then
    printf 'apt index refresh timed out after 3 minutes (attempt %d/6)\n' \
      "$attempt" >&2
  else
    printf 'apt index refresh failed with status %d (attempt %d/6)\n' \
      "$apt_status" "$attempt" >&2
  fi
  if [[ $attempt -lt 6 ]]; then
    sleep 5
  fi
done
if [[ $apt_ready != true ]]; then
  printf 'apt index refresh failed after 6 bounded attempts\n' >&2
  exit 1
fi
# The pinned installer's python-deps stage checks for these three build
# packages and starts a second apt update/install when any is absent. Keep them
# in this one bounded transaction so the stage cannot duplicate apt work.
apt_packages=(
  build-essential
  ca-certificates
  curl
  git
  libffi-dev
  openssh-server
  procps
  python3
  python3-dev
  ripgrep
  sudo
  supervisor
  tmux
  xz-utils
)
if [[ ${HERMES_BOX_EXECUTOR_ENABLED:-false} == true ]]; then
  apt_packages+=(skopeo)
fi
set +e
timeout --signal=TERM --kill-after=30s 20m \
  apt-get install -y --no-install-recommends "${apt_packages[@]}"
apt_install_status=$?
set -e
if [[ $apt_install_status -eq 124 || $apt_install_status -eq 137 ]]; then
  printf 'apt package installation timed out after 20 minutes\n' >&2
  exit 1
fi
if [[ $apt_install_status -ne 0 ]]; then
  printf 'apt package installation failed with status %d\n' \
    "$apt_install_status" >&2
  exit "$apt_install_status"
fi

if ! id boxadmin >/dev/null 2>&1; then
  useradd --uid 1001 --create-home --shell /bin/bash boxadmin
fi
if ! id hermes >/dev/null 2>&1; then
  useradd --uid 1002 --create-home --shell /bin/bash hermes
fi
passwd --delete boxadmin
passwd --lock hermes

install -o root -g root -m 0755 \
  /tmp/hermes-box-install-node.sh \
  /usr/local/sbin/hermes-box-install-node
/usr/local/sbin/hermes-box-install-node 24

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

install -o root -g root -m 0440 \
  /tmp/hermes-box-sudoers /etc/sudoers.d/hermes-box
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
  /tmp/hermes-box-entrypoint.sh /usr/local/sbin/hermes-box-entrypoint
install -o root -g root -m 0755 \
  /tmp/hermes-box-executor.sh /usr/local/sbin/hermes-box-executor
install -o root -g root -m 0755 \
  /tmp/hermes-box-extract-executor.py \
  /usr/local/sbin/hermes-box-extract-executor
install -o root -g root -m 0755 \
  /tmp/hermes-box-workspace-seed.sh \
  /usr/local/sbin/hermes-box-workspace-seed
install -o root -g root -m 0644 \
  /tmp/hermes-box-supervisord.conf /etc/supervisor/supervisord.conf

if [[ ${HERMES_INSTALL_COMMIT:-} != "$supported_hermes_commit" ]]; then
  printf 'unsupported Hermes commit for pinned installer: %s\n' \
    "${HERMES_INSTALL_COMMIT:-<unset>}" >&2
  exit 1
fi

# The upstream installer deliberately reuses a managed uv binary when one is
# already present. Pin that binary to the last version proven on smolvm 1.0.4:
# uv 0.11.22 reproducibly deadlocks while building the editable Hermes project.
# Downloading the release archive directly and verifying its reviewed digest
# keeps fresh image creation independent of whichever uv release is newest.
uv_archive=/tmp/hermes-box-uv.tar.gz
uv_extract=/tmp/hermes-box-uv
rm -rf -- "$uv_archive" "$uv_extract"
curl "${download_curl_args[@]}" \
  "https://github.com/astral-sh/uv/releases/download/$uv_version/uv-aarch64-unknown-linux-gnu.tar.gz" \
  -o "$uv_archive"
printf '%s  %s\n' "$uv_archive_sha256" "$uv_archive" |
  sha256sum --check --status
mkdir -p "$uv_extract"
tar -xzf "$uv_archive" --no-same-owner -C "$uv_extract"
install -d -o hermes -g hermes -m 0750 "$hermes_home/bin"
install -o hermes -g hermes -m 0755 \
  "$uv_extract/uv-aarch64-unknown-linux-gnu/uv" \
  "$hermes_home/bin/uv"
install -o hermes -g hermes -m 0755 \
  "$uv_extract/uv-aarch64-unknown-linux-gnu/uvx" \
  "$hermes_home/bin/uvx"
rm -rf -- "$uv_archive" "$uv_extract"
"$hermes_home/bin/uv" --version

curl "${download_curl_args[@]}" \
  "https://raw.githubusercontent.com/NousResearch/hermes-agent/$HERMES_INSTALL_COMMIT/scripts/install.sh" \
  -o /tmp/hermes-install.sh
printf '%s  %s\n' "$hermes_installer_sha256" /tmp/hermes-install.sh |
  sha256sum --check --status
chmod 0755 /tmp/hermes-install.sh

installer_args=(
  --non-interactive
  --hermes-home "$hermes_home"
  --commit "$HERMES_INSTALL_COMMIT"
)
installer_stages=(repository venv python-deps path config complete)
for installer_stage in "${installer_stages[@]}"; do
  set +e
  HERMES_HOME="$hermes_home" \
    timeout --signal=TERM --kill-after=30s 20m \
    /tmp/hermes-install.sh \
    "${installer_args[@]}" \
    --stage "$installer_stage"
  installer_status=$?
  set -e
  if [[ $installer_status -eq 124 || $installer_status -eq 137 ]]; then
    printf 'Hermes installer stage %s timed out after 20 minutes\n' \
      "$installer_stage" >&2
    exit 1
  fi
  if [[ $installer_status -ne 0 ]]; then
    printf 'Hermes installer stage %s failed with status %d\n' \
      "$installer_stage" "$installer_status" >&2
    exit "$installer_status"
  fi
done

# Hermes' stock 0.16.0 approval modes do not include the conservative one-shot
# Codex reviewer. Apply the source extension only to the reviewed commit. The
# patcher verifies every upstream anchor and compiles the result before it
# writes gated mode into config.yaml, so revision drift cannot leave a YAML-only
# gate that silently does nothing.
/usr/local/lib/hermes-agent/venv/bin/python \
  /tmp/hermes-box-patch-hermes-gated-approval.py \
  --source /usr/local/lib/hermes-agent \
  --module /tmp/hermes-box-hermes-gated-approval.py \
  --config "$hermes_home/config.yaml"
HERMES_GATED_APPROVAL_SOURCE=/usr/local/lib/hermes-agent \
HERMES_GATED_APPROVAL_MODULE=/usr/local/lib/hermes-agent/tools/gated_approval.py \
HERMES_GATED_APPROVAL_PATCHER=/tmp/hermes-box-patch-hermes-gated-approval.py \
PYTHONPATH=/usr/local/lib/hermes-agent \
  /usr/local/lib/hermes-agent/venv/bin/python \
  /tmp/hermes-box-test-hermes-gated-approval.py

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

# Add the messaging adapters incrementally through the same checked-in lockfile
# used by the install stage. This preserves hash-locked dependencies instead of
# asking pip to re-resolve the entire project from the live package index. Run
# it before cache cleanup so uv can reuse the artifacts from python-deps.
(
  cd /usr/local/lib/hermes-agent
  timeout --signal=TERM --kill-after=30s 20m env \
    HERMES_HOME="$hermes_home" \
    VIRTUAL_ENV=/usr/local/lib/hermes-agent/venv \
    UV_PROJECT_ENVIRONMENT=/usr/local/lib/hermes-agent/venv \
    "$hermes_home/bin/uv" sync \
    --extra all \
    --extra messaging \
    --locked
)

# Drop caches and source-only UI/test payloads after the final locked sync.
# The npm/browser stages never run, so there are no dependency trees to install
# and immediately delete.
rm -rf \
  /root/.cache \
  /root/.npm \
  /usr/local/lib/hermes-agent/.git \
  /usr/local/lib/hermes-agent/apps \
  /usr/local/lib/hermes-agent/tests \
  /usr/local/lib/hermes-agent/website

# Keep the source checkout root-owned, but allow Hermes to lazy-install optional
# Python dependencies into its own virtual environment.
if [[ -d /usr/local/lib/hermes-agent/venv ]]; then
  chown -hR hermes:hermes /usr/local/lib/hermes-agent/venv
fi

chown -R hermes:hermes "$hermes_home" "$codex_home" /workspace/work
sudo -iu hermes env HERMES_HOME="$hermes_home" hermes --version
sudo -iu hermes /usr/local/lib/hermes-agent/venv/bin/python -c 'import discord'
sudo -iu hermes node --version
sudo -iu hermes npm --version
sudo -iu hermes tmux -V

install -d -m 0755 /run/sshd
ssh-keygen -A
/usr/sbin/sshd -t

# smolvm 1.0.4 pack snapshots do not preserve the named machine's /workspace
# disk. Embed a seed copy in the root overlay so the first runtime boot can
# reconstruct the private workspace without any host mount.
install -d -m 0700 /var/lib/hermes-box
rm -f \
  /var/lib/hermes-box/runtime-ownership-repaired \
  /var/lib/hermes-box/runtime-ownership-v*
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
