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
tar --zstd -tf "$artifact_dir/hermes-agent-gated-$HERMES_VERSION.tar.zst" | \
  grep -Fxq "hermes-agent-$HERMES_VERSION/hermes_box_release/test_gated_approval.py"
tar --zstd -tf "$artifact_dir/hermes-agent-gated-$HERMES_VERSION.tar.zst" | \
  grep -Fxq "hermes-agent-$HERMES_VERSION/hermes_box_release/patch-hermes-gated-approval.py"
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
  -C "$work"
cmp "$release_root/python-build-requirements.txt" "$work/python-build-requirements.txt"
grep -Fxq 'schema=2' "$work/manifest.txt"
grep -Fxq 'platform=linux-arm64' "$work/manifest.txt"
requirements_line=$(grep -E '^requirements=[^/[:space:]]+[[:space:]]+sha256=[a-f0-9]{64}$' "$work/manifest.txt")
project_line=$(grep -E '^project_wheel=hermes_agent-[^/[:space:]]+-py3-none-any\.whl[[:space:]]+sha256=[a-f0-9]{64}$' "$work/manifest.txt")
requirements=${requirements_line#requirements=}
requirements=${requirements%%[[:space:]]*}
requirements_sha=${requirements_line##*sha256=}
project_wheel=${project_line#project_wheel=}
project_wheel=${project_wheel%%[[:space:]]*}
project_sha=${project_line##*sha256=}
verify_sha256 "$work/$requirements" "$requirements_sha"
verify_sha256 "$work/$project_wheel" "$project_sha"
grep -Fxq "wheel=$project_wheel"$'\t'"sha256=$project_sha" "$work/manifest.txt"
if grep -Eq "sys_platform[[:space:]]*==[[:space:]]*['\"]win32['\"]|concurrent-log-handler|portalocker|pywin32|pywinpty|tzdata" \
  "$work/$requirements"; then
  printf 'offline Linux ARM64 requirements contain a Windows-only dependency\n' >&2
  exit 1
fi
