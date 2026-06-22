#!/usr/bin/env bash
set -euo pipefail

rootfs=/tmp/hermes-box-restore-rootfs.tar.gz
rootfs_files=/tmp/hermes-box-restore-rootfs-files.txt
workspace=/tmp/hermes-box-restore-workspace.tar.gz
authorized_key=/tmp/hermes-box-restore-authorized-key.pub

tar --numeric-owner -C / -xzpf "$rootfs"
python3 - "$rootfs_files" <<'PY'
import os
import shutil
import sys

excluded = {
    "dev",
    "packed_layers",
    "proc",
    "run",
    "storage",
    "sys",
    "tmp",
    "workspace",
}
with open(sys.argv[1], encoding="utf-8") as manifest:
    archived = {
        line.strip().removeprefix("./").rstrip("/")
        for line in manifest
        if line.strip() not in {".", "./"}
    }

for entry in os.scandir("/"):
    if entry.name in excluded:
        continue
    root = entry.path
    if entry.is_dir(follow_symlinks=False):
        for current, directories, files in os.walk(
            root, topdown=False, followlinks=False
        ):
            for name in files:
                path = os.path.join(current, name)
                relative = path.removeprefix("/")
                if relative not in archived:
                    os.unlink(path)
            for name in directories:
                path = os.path.join(current, name)
                relative = path.removeprefix("/")
                if relative not in archived:
                    if os.path.islink(path):
                        os.unlink(path)
                    else:
                        shutil.rmtree(path)
        relative_root = root.removeprefix("/")
        if relative_root not in archived:
            shutil.rmtree(root)
    elif entry.name not in archived:
        os.unlink(root)
PY

# The private SSH identity is supplied separately from the backup. Keep that
# destination key authoritative even when restoring a rootfs created with an
# older key.
install -d -o boxadmin -g boxadmin -m 0700 /home/boxadmin/.ssh
install -o boxadmin -g boxadmin -m 0600 \
  "$authorized_key" /home/boxadmin/.ssh/authorized_keys

# Host-managed boot and service files move forward with the restoring Hermes
# Box checkout. This lets current code recover older snapshots without booting
# their obsolete Supervisor or Executor layout first.
install -o root -g root -m 0755 \
  /tmp/hermes-box-current-start.sh \
  /usr/local/sbin/hermes-box-start
install -o root -g root -m 0755 \
  /tmp/hermes-box-current-entrypoint.sh \
  /usr/local/sbin/hermes-box-entrypoint
install -o root -g root -m 0755 \
  /tmp/hermes-box-current-executor.sh \
  /usr/local/sbin/hermes-box-executor
install -o root -g root -m 0755 \
  /tmp/hermes-box-current-extract-executor.py \
  /usr/local/sbin/hermes-box-extract-executor
install -o root -g root -m 0755 \
  /tmp/hermes-box-current-workspace-seed.sh \
  /usr/local/sbin/hermes-box-workspace-seed
install -o root -g root -m 0644 \
  /tmp/hermes-box-current-supervisord.conf \
  /etc/supervisor/supervisord.conf

find /workspace -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
tar \
  --numeric-owner \
  --exclude=./codex-home/tmp \
  -C /workspace \
  -xzpf "$workspace"
rm -f \
  "$rootfs" \
  "$rootfs_files" \
  "$workspace" \
  "$authorized_key" \
  /tmp/hermes-box-current-entrypoint.sh \
  /tmp/hermes-box-current-start.sh \
  /tmp/hermes-box-current-executor.sh \
  /tmp/hermes-box-current-extract-executor.py \
  /tmp/hermes-box-current-workspace-seed.sh \
  /tmp/hermes-box-current-supervisord.conf
rm -f \
  /var/lib/hermes-box/runtime-ownership-repaired \
  /var/lib/hermes-box/runtime-ownership-v*
touch /var/lib/hermes-box/restore-ready
sync
