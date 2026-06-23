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
source_artifact=${2:-"$output_dir/hermes-agent-gated-$HERMES_VERSION.tar.zst"}
[[ -f $source_artifact ]] || {
  printf 'build the gated Hermes source first: %s\n' "$source_artifact" >&2
  exit 1
}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$output_dir" "$work/python" "$work/source" "$work/wheelhouse" \
  "$work/python-build-wheels"

python_archive="$work/python.tar.gz"
download "$PYTHON_URL" "$python_archive" "$PYTHON_SHA256"
tar -xf "$python_archive" -C "$work/python" --strip-components=1
python="$work/python/bin/python3.13"
[[ -x $python ]] || { printf 'embedded Python executable was not found: %s\n' "$python" >&2; exit 1; }
tar --zstd -xf "$source_artifact" -C "$work/source" --strip-components=1
verify_sha256 "$work/source/uv.lock" "$HERMES_UV_LOCK_SHA256"

uv export --directory "$work/source" --locked --extra all --no-dev \
  --no-emit-project --format requirements.txt --output-file "$work/requirements.txt"
uv venv --python "$python" "$work/builder"
download_python_build_wheels "$work/python-build-wheels"
install_python_build_tools "$work/builder/bin/python" "$work/python-build-wheels"
"$work/builder/bin/python" -m pip download --require-hashes --only-binary=:all: \
  --disable-pip-version-check \
  --dest "$work/wheelhouse" --requirement "$work/requirements.txt"
# The project build backend is not part of the runtime dependency export.
install -m 0644 \
  "$work/python-build-wheels/setuptools-$SETUPTOOLS_VERSION-py3-none-any.whl" \
  "$work/wheelhouse/"
cp "$work/requirements.txt" "$work/wheelhouse/requirements.txt"
cp "$release_root/python-build-requirements.txt" "$work/wheelhouse/python-build-requirements.txt"

cp -a "$work/source" "$work/offline-source"
rm -rf "$work/offline-source/.venv"
UV_NO_NETWORK=1 uv sync --directory "$work/offline-source" --locked --extra all \
  --no-dev --offline --no-index --no-managed-python --python "$python" \
  --find-links "$work/wheelhouse"
HERMES_GATED_APPROVAL_MODULE="$work/offline-source/tools/gated_approval.py" \
HERMES_GATED_APPROVAL_PATCHER="$release_root/hermes/patch-hermes-gated-approval.py" \
HERMES_GATED_APPROVAL_SOURCE="$work/offline-source" \
  "$work/offline-source/.venv/bin/python" "$release_root/hermes/test_gated_approval.py"
"$work/offline-source/.venv/bin/hermes" --help >/dev/null

{
  printf 'schema=1\npython=%s\nuv=%s\nhermes_commit=%s\nuv_lock_sha256=%s\n' \
    "$PYTHON_VERSION" "$UV_VERSION" "$HERMES_COMMIT" "$HERMES_UV_LOCK_SHA256"
  for wheel in "$work/wheelhouse"/*.whl; do
    printf 'wheel=%s\tsha256=%s\n' "$(basename "$wheel")" "$(sha256_file "$wheel")"
  done
} >"$work/wheelhouse/manifest.txt"
artifact="$output_dir/hermes-wheels-cp313-linux-arm64.tar.zst"
rm -f "$artifact"
deterministic_tar_zstd "$work/wheelhouse" "$artifact"
sha256_file "$artifact" >"$artifact.sha256"
printf '%s\n' "$artifact"
