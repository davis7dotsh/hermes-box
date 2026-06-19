#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"

if [[ ${HERMES_BOX_E2E:-} != 1 ]]; then
  printf 'set HERMES_BOX_E2E=1 to run the destructive disposable lifecycle test\n' >&2
  exit 1
fi

: "${HERMES_BOX_MACHINE_NAME:?set a disposable machine name}"
: "${HERMES_BOX_BUILDER_NAME:?set a disposable builder name}"
: "${HERMES_BOX_SSH_PORT:?set a disposable SSH port}"
: "${HERMES_BOX_NETWORK_MODE:?set HERMES_BOX_NETWORK_MODE=full}"

if [[ ${HERMES_BOX_EXECUTOR_ENABLED:-false} == true ]]; then
  : "${HERMES_BOX_EXECUTOR_PORT:?set a disposable Executor port}"
fi

if [[ $HERMES_BOX_MACHINE_NAME == hermes-box ||
  $HERMES_BOX_BUILDER_NAME == hermes-builder ||
  $HERMES_BOX_SSH_PORT == 2222 ]]; then
  printf 'refusing to use the primary machine, builder, or SSH port\n' >&2
  exit 1
fi
if [[ ${HERMES_BOX_EXECUTOR_ENABLED:-false} == true &&
  $HERMES_BOX_EXECUTOR_PORT == 4788 ]]; then
  printf 'refusing to use the primary Executor port\n' >&2
  exit 1
fi

data_root=$(mktemp -d "${TMPDIR:-/tmp}/hermes-box-e2e.XXXXXX")
export HERMES_BOX_DATA_DIR=$data_root

cleanup() {
  ./bin/hermes-box destroy --force >/dev/null 2>&1 || true
  rm -rf -- "$data_root"
}
trap cleanup EXIT

./bin/hermes-box init
./bin/hermes-box status

./bin/hermes-box ssh 'sudo -n -u hermes true'
if ./bin/hermes-box ssh 'sudo -n true' >/dev/null 2>&1; then
  printf 'boxadmin unexpectedly obtained root sudo\n' >&2
  exit 1
fi
./bin/hermes-box ssh 'sudo -iu hermes hermes --version'
./bin/hermes-box ssh 'sudo -iu hermes hermes --help >/dev/null'
./bin/hermes-box ssh 'sudo -iu hermes codex --strict-config --version'
./bin/hermes-box ssh 'sudo -iu hermes codex --help >/dev/null'
./bin/hermes-box ssh \
  'sudo -u hermes sudo supervisorctl status hermes | grep -Fq RUNNING'
if [[ ${HERMES_BOX_EXECUTOR_ENABLED:-false} == true ]]; then
  ./bin/hermes-box ssh \
    'test ! -S /var/run/docker.sock && sudo -u hermes sudo supervisorctl status executor'
  ./bin/hermes-box ssh \
    'sudo -u hermes test -L /workspace/.hermes-box-runtime/executor/current && sudo -u hermes test -s /workspace/.hermes-box-runtime/executor/current/.manifest-digest'
  curl --max-time 10 -fsS \
    "http://127.0.0.1:$HERMES_BOX_EXECUTOR_PORT/api/health" >/dev/null
  mcp_status=$(
    curl --max-time 10 -sS -o /dev/null -w '%{http_code}' \
      "http://127.0.0.1:$HERMES_BOX_EXECUTOR_PORT/mcp"
  )
  if [[ $mcp_status != 401 ]]; then
    printf 'unauthenticated Executor MCP returned %s, want 401\n' \
      "$mcp_status" >&2
    exit 1
  fi
  ./bin/hermes-box ssh \
    'sudo -u hermes sudo sh -c "printf executor-before-restore > /workspace/executor/data/hermes-box-e2e.txt"'
else
  ./bin/hermes-box ssh 'test ! -e /var/run/docker.sock'
fi

./bin/hermes-box ssh \
  'sudo -u hermes sh -c "printf before-restore > /workspace/work/hermes-box-e2e.txt"'
backup=$(./bin/hermes-box snapshot end-to-end)
./bin/hermes-box ssh \
  'sudo -u hermes sh -c "printf after-snapshot > /workspace/work/hermes-box-e2e.txt"'

./bin/hermes-box restore "$backup"
restored=$(
  ./bin/hermes-box ssh \
    'sudo -u hermes cat /workspace/work/hermes-box-e2e.txt'
)
if [[ $restored != before-restore ]]; then
  printf 'restore mismatch: %s\n' "$restored" >&2
  exit 1
fi
if [[ ${HERMES_BOX_EXECUTOR_ENABLED:-false} == true ]]; then
  executor_restored=$(
    ./bin/hermes-box ssh \
      'sudo -u hermes sudo cat /workspace/executor/data/hermes-box-e2e.txt'
  )
  if [[ $executor_restored != executor-before-restore ]]; then
    printf 'Executor restore mismatch: %s\n' "$executor_restored" >&2
    exit 1
  fi
  curl --max-time 10 -fsS \
    "http://127.0.0.1:$HERMES_BOX_EXECUTOR_PORT/api/health" >/dev/null
fi

./bin/hermes-box destroy --force
rm -rf -- "$data_root"
trap - EXIT
printf 'disposable lifecycle test passed\n'
