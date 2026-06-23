#!/usr/bin/env bash
set -euo pipefail

release_root=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck disable=SC2034 # Used by scripts that source this library.
repo_root=$(cd "$release_root/.." && pwd)
# shellcheck source=release/pins.env
source "$release_root/pins.env"

require_command() {
  local command
  for command in "$@"; do
    command -v "$command" >/dev/null 2>&1 || {
      printf 'required command is unavailable: %s\n' "$command" >&2
      exit 1
    }
  done
}

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

verify_sha256() {
  local path=$1 expected=$2 actual
  actual=$(sha256_file "$path")
  [[ $actual == "$expected" ]] || {
    printf 'sha256 mismatch for %s: expected %s, got %s\n' "$path" "$expected" "$actual" >&2
    exit 1
  }
}

download() {
  local url=$1 output=$2 expected=$3
  if [[ ! -f $output ]]; then
    curl --fail --location --proto '=https' --tlsv1.2 --output "$output" "$url"
  fi
  verify_sha256 "$output" "$expected"
}

download_python_build_wheels() {
  local destination=$1
  mkdir -p "$destination"
  download "$PIP_URL" "$destination/pip-$PIP_VERSION-py3-none-any.whl" "$PIP_SHA256"
  download "$SETUPTOOLS_URL" \
    "$destination/setuptools-$SETUPTOOLS_VERSION-py3-none-any.whl" \
    "$SETUPTOOLS_SHA256"
  download "$PYYAML_URL" \
    "$destination/pyyaml-$PYYAML_VERSION-cp313-cp313-manylinux2014_aarch64.manylinux_2_17_aarch64.manylinux_2_28_aarch64.whl" \
    "$PYYAML_SHA256"
}

install_python_build_tools() {
  local python=$1 wheels=$2
  uv pip install --python "$python" --no-index --find-links "$wheels" \
    --require-hashes --requirement "$release_root/python-build-requirements.txt"
}

require_linux_arm64() {
  [[ $(uname -s) == Linux && $(uname -m) == aarch64 ]] || {
    printf 'this qualification step requires native Linux ARM64\n' >&2
    exit 1
  }
}

deterministic_tar_zstd() {
  local directory=$1 output=$2 member=${3:-.}
  require_command tar zstd
  TZ=UTC tar --sort=name --format=posix \
    --pax-option=delete=atime,delete=ctime \
    --mtime="@$SOURCE_DATE_EPOCH" --owner=0 --group=0 --numeric-owner \
    -C "$directory" -cf - "$member" | zstd -q -19 -T1 -o "$output"
}

remove_python_bytecode() {
  local root=$1
  find "$root" -type d -name __pycache__ -prune -exec rm -rf {} +
  find "$root" -type f \( -name '*.pyc' -o -name '*.pyo' \) -delete
}

assert_no_python_bytecode() {
  local root=$1 unexpected
  unexpected=$(find "$root" \( -type d -name __pycache__ -o -type f \( -name '*.pyc' -o -name '*.pyo' \) \) -print -quit)
  [[ -z $unexpected ]] || {
    printf 'Python bytecode must not enter deterministic artifacts: %s\n' "$unexpected" >&2
    exit 1
  }
}
