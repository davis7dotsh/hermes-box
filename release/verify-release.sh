#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "$0")" && pwd)
release_root=''
repo_root=''
# shellcheck source=release/lib.sh
source "$script_dir/lib.sh"
require_command go tar zstd

artifact_dir=${1:?usage: verify-release.sh ARTIFACT_DIR}
for artifact in \
  "$artifact_dir/hermes-box-provisioner-linux-arm64.tar.zst" \
  "$artifact_dir/hermes-agent-gated-$HERMES_VERSION.tar.zst" \
  "$artifact_dir/hermes-wheels-cp313-linux-arm64.tar.zst"; do
  [[ -f $artifact && -f $artifact.sha256 ]] || { printf 'missing artifact/checksum: %s\n' "$artifact" >&2; exit 1; }
  expected=$(tr -d '[:space:]' <"$artifact.sha256")
  verify_sha256 "$artifact" "$expected"
  tar --zstd -tf "$artifact" >/dev/null
done

if tar --zstd -tf "$artifact_dir/hermes-agent-gated-$HERMES_VERSION.tar.zst" | \
  grep -E '(^|/)(__pycache__|[^/]+\.py[co])(/|$)' >/dev/null; then
  printf 'gated Hermes source archive contains Python bytecode\n' >&2
  exit 1
fi
[[ -f $artifact_dir/hermes-box.lock ]] || { printf 'generated lock is missing\n' >&2; exit 1; }
GOTOOLCHAIN="$GO_TOOLCHAIN" go run "$release_root/validate-lock.go" \
  "$artifact_dir/hermes-box.lock" "$artifact_dir"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
tar --zstd -xf "$artifact_dir/hermes-box-provisioner-linux-arm64.tar.zst" \
  -C "$work" ./manifest.txt
grep -Fxq "apt_snapshot=$UBUNTU_APT_SNAPSHOT" "$work/manifest.txt"
grep -Eq '^apt_index=.*InRelease[[:space:]]+sha256=[a-f0-9]{64}$' "$work/manifest.txt"
grep -Eq '^apt_index=.*Packages[^[:space:]]*[[:space:]]+sha256=[a-f0-9]{64}$' "$work/manifest.txt"

rm -rf "${work:?}"/*
tar --zstd -xf "$artifact_dir/hermes-wheels-cp313-linux-arm64.tar.zst" \
  -C "$work" ./python-build-requirements.txt ./manifest.txt
cmp "$release_root/python-build-requirements.txt" "$work/python-build-requirements.txt"
grep -Eq '^wheel=setuptools-.*[[:space:]]+sha256=[a-f0-9]{64}$' "$work/manifest.txt"
