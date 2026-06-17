#!/usr/bin/env bash
set -euo pipefail

rootfs=/tmp/hermes-box-restore-rootfs.tar.gz
rootfs_files=/tmp/hermes-box-restore-rootfs-files.txt
workspace=/tmp/hermes-box-restore-workspace.tar.gz

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

find /workspace -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
tar --numeric-owner -C /workspace -xzpf "$workspace"
rm -f "$rootfs" "$rootfs_files" "$workspace"
sync
