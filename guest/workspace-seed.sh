#!/usr/bin/env bash
set -euo pipefail

workspace=${1:-/workspace}
snapshot_dir=${2:-/var/lib/hermes-box}
snapshot_id_file=$snapshot_dir/workspace-snapshot.id
snapshot_tar=$snapshot_dir/workspace-snapshot.tar
restored_id_file=$snapshot_dir/workspace-restored.id
legacy_id_file=$workspace/.hermes-box-snapshot-id

if [[ ! -f $snapshot_id_file || ! -f $snapshot_tar ]]; then
  exit 0
fi

expected_id=$(cat "$snapshot_id_file")
snapshot_id_bytes=$(wc -c <"$snapshot_id_file")
snapshot_id_bytes=${snapshot_id_bytes//[[:space:]]/}
if [[ ! $expected_id =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$ ]] ||
  ((snapshot_id_bytes != ${#expected_id} + 1)); then
  printf 'invalid workspace snapshot ID metadata: %s\n' \
    "$snapshot_id_file" >&2
  exit 1
fi

current_id=
if [[ -f $restored_id_file ]]; then
  current_id=$(cat "$restored_id_file")
elif [[ -f $legacy_id_file ]]; then
  if ! marker_uid=$(stat -c %u "$legacy_id_file" 2>/dev/null); then
    marker_uid=$(stat -f %u "$legacy_id_file")
  fi
  if [[ $marker_uid == "$(id -u)" ]]; then
    current_id=$(cat "$legacy_id_file")
  fi
fi

if [[ $current_id != "$expected_id" ]]; then
  find "$workspace" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
  tar -C "$workspace" -xpf "$snapshot_tar"
  sync
fi

printf '%s\n' "$expected_id" >"$restored_id_file"
if [[ $(id -u) == 0 ]]; then
  chown root:root "$restored_id_file"
fi
chmod 0600 "$restored_id_file"
