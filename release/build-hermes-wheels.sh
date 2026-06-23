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
  "$work/python-build-wheels" "$work/project-wheel"

python_archive="$work/python.tar.gz"
download "$PYTHON_URL" "$python_archive" "$PYTHON_SHA256"
tar -xf "$python_archive" -C "$work/python" --strip-components=1
python="$work/python/bin/python3.13"
[[ -x $python ]] || { printf 'embedded Python executable was not found: %s\n' "$python" >&2; exit 1; }
tar --zstd -xf "$source_artifact" -C "$work/source" --strip-components=1
verify_sha256 "$work/source/uv.lock" "$HERMES_UV_LOCK_SHA256"

uv venv --python "$python" "$work/builder"
download_python_build_wheels "$work/python-build-wheels"
install_python_build_tools "$work/builder/bin/python" "$work/python-build-wheels"
uv export --directory "$work/source" --locked --extra all --no-dev \
  --no-emit-project --format requirements.txt \
  --output-file "$work/requirements-universal.txt"
# uv export intentionally preserves every platform fork in uv.lock. Compile
# that already hash-locked export without dependency resolution so Linux ARM64
# gets a closed requirements file and Windows-only forks cannot re-enter the
# guest install. Reject any hash that was not present in the reviewed lock.
uv pip compile "$work/requirements-universal.txt" --no-deps \
  --python "$python" --python-platform aarch64-manylinux_2_17 \
  --generate-hashes --no-header --no-annotate \
  --output-file "$work/requirements-linux-arm64.txt"
comm -23 \
  <(grep -Eo 'sha256:[a-f0-9]{64}' "$work/requirements-linux-arm64.txt" | sort -u) \
  <(grep -Eo 'sha256:[a-f0-9]{64}' "$work/requirements-universal.txt" | sort -u) \
  >"$work/unreviewed-hashes.txt"
[[ ! -s $work/unreviewed-hashes.txt ]] || {
  printf 'platform requirements introduced hashes outside uv.lock:\n' >&2
  cat "$work/unreviewed-hashes.txt" >&2
  exit 1
}
if grep -Eq "sys_platform[[:space:]]*==[[:space:]]*['\"]win32['\"]|concurrent-log-handler|portalocker|pywin32|pywinpty|tzdata" \
  "$work/requirements-linux-arm64.txt"; then
  printf 'Linux ARM64 requirements retained a Windows-only dependency\n' >&2
  exit 1
fi
"$work/builder/bin/python" -m pip download --require-hashes --only-binary=:all: \
  --disable-pip-version-check \
  --dest "$work/wheelhouse" --requirement "$work/requirements-linux-arm64.txt"
SOURCE_DATE_EPOCH=$SOURCE_DATE_EPOCH PYTHONDONTWRITEBYTECODE=1 \
  "$work/builder/bin/python" -m pip wheel --no-deps --no-build-isolation \
  --disable-pip-version-check --wheel-dir "$work/project-wheel" "$work/source"
mapfile -t project_wheels < <(find "$work/project-wheel" -maxdepth 1 -type f -name '*.whl' -print | sort)
[[ ${#project_wheels[@]} -eq 1 ]] || {
  printf 'Hermes build produced %d project wheels, expected exactly one\n' "${#project_wheels[@]}" >&2
  exit 1
}
project_wheel=${project_wheels[0]}
project_wheel_name=$(basename "$project_wheel")
[[ $project_wheel_name == hermes_agent-${HERMES_VERSION}-py3-none-any.whl ]] || {
  printf 'unexpected Hermes project wheel identity: %s\n' "$project_wheel_name" >&2
  exit 1
}
install -m 0644 "$project_wheel" "$work/wheelhouse/$project_wheel_name"
cp "$work/requirements-linux-arm64.txt" "$work/wheelhouse/requirements-linux-arm64.txt"
cp "$release_root/python-build-requirements.txt" "$work/wheelhouse/python-build-requirements.txt"

uv venv --python "$python" "$work/offline-venv"
UV_OFFLINE=1 uv pip install --python "$work/offline-venv/bin/python" \
  --offline --no-index --find-links "$work/wheelhouse" --require-hashes \
  --requirement "$work/wheelhouse/requirements-linux-arm64.txt"
UV_OFFLINE=1 uv pip install --python "$work/offline-venv/bin/python" \
  --offline --no-index --find-links "$work/wheelhouse" --no-deps \
  "$work/wheelhouse/$project_wheel_name"
HERMES_GATED_APPROVAL_MODULE="$work/source/tools/gated_approval.py" \
HERMES_GATED_APPROVAL_PATCHER="$work/source/hermes_box_release/patch-hermes-gated-approval.py" \
HERMES_GATED_APPROVAL_SOURCE="$work/source" \
  "$work/offline-venv/bin/python" "$work/source/hermes_box_release/test_gated_approval.py"
"$work/offline-venv/bin/hermes" --help >/dev/null

{
  printf 'schema=2\nplatform=linux-arm64\npython=%s\nuv=%s\nhermes_commit=%s\nuv_lock_sha256=%s\n' \
    "$PYTHON_VERSION" "$UV_VERSION" "$HERMES_COMMIT" "$HERMES_UV_LOCK_SHA256"
  printf 'requirements=%s\tsha256=%s\n' \
    requirements-linux-arm64.txt "$(sha256_file "$work/wheelhouse/requirements-linux-arm64.txt")"
  printf 'project_wheel=%s\tsha256=%s\n' \
    "$project_wheel_name" "$(sha256_file "$work/wheelhouse/$project_wheel_name")"
  for wheel in "$work/wheelhouse"/*.whl; do
    printf 'wheel=%s\tsha256=%s\n' "$(basename "$wheel")" "$(sha256_file "$wheel")"
  done
} >"$work/wheelhouse/manifest.txt"
artifact="$output_dir/hermes-wheels-cp313-linux-arm64.tar.zst"
rm -f "$artifact"
deterministic_tar_zstd "$work/wheelhouse" "$artifact"
sha256_file "$artifact" >"$artifact.sha256"
printf '%s\n' "$artifact"
