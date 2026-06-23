#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "$0")" && pwd)
release_root=''
repo_root=''
# shellcheck source=release/lib.sh
source "$script_dir/lib.sh"
require_linux_arm64
require_command curl tar uv zstd

output_dir=${1:-"${TMPDIR:-/tmp}/hermes-box-release"}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$output_dir" "$work/python" "$work/python-build-wheels"

python_archive="$work/python.tar.gz"
download "$PYTHON_URL" "$python_archive" "$PYTHON_SHA256"
tar -xf "$python_archive" -C "$work/python" --strip-components=1
python=$(find "$work/python" -type f -name python3 -perm -111 | head -1)
[[ -n $python ]] || { printf 'embedded Python executable was not found\n' >&2; exit 1; }
uv venv --python "$python" "$work/builder"
download_python_build_wheels "$work/python-build-wheels"
install_python_build_tools "$work/builder/bin/python" "$work/python-build-wheels"

upstream="$work/upstream.tar.gz"
download "$HERMES_UPSTREAM_ARCHIVE" "$upstream" "$HERMES_UPSTREAM_SHA256"
mkdir -p "$work/source"
tar -xf "$upstream" -C "$work/source" --strip-components=1
verify_sha256 "$work/source/uv.lock" "$HERMES_UV_LOCK_SHA256"

HERMES_GATED_APPROVAL_MODULE="$release_root/hermes/gated_approval.py" \
HERMES_GATED_APPROVAL_PATCHER="$release_root/hermes/patch-hermes-gated-approval.py" \
  "$work/builder/bin/python" \
  "$release_root/hermes/patch-hermes-gated-approval.py" \
  --source "$work/source" --module "$release_root/hermes/gated_approval.py" \
  --config "$work/config.yaml" --commit "$HERMES_COMMIT"
HERMES_GATED_APPROVAL_MODULE="$release_root/hermes/gated_approval.py" \
HERMES_GATED_APPROVAL_PATCHER="$release_root/hermes/patch-hermes-gated-approval.py" \
  "$work/builder/bin/python" "$release_root/hermes/test_gated_approval.py"

remove_python_bytecode "$work/source"
assert_no_python_bytecode "$work/source"
mkdir -p "$work/package/hermes-agent-$HERMES_VERSION"
cp -a "$work/source/." "$work/package/hermes-agent-$HERMES_VERSION/"
artifact="$output_dir/hermes-agent-gated-$HERMES_VERSION.tar.zst"
rm -f "$artifact"
deterministic_tar_zstd "$work/package" "$artifact" "hermes-agent-$HERMES_VERSION"
sha256_file "$artifact" >"$artifact.sha256"
printf '%s\n' "$artifact"
