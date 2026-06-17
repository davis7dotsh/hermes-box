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

if [[ $HERMES_BOX_MACHINE_NAME == hermes-box ||
  $HERMES_BOX_BUILDER_NAME == hermes-builder ||
  $HERMES_BOX_SSH_PORT == 2222 ]]; then
  printf 'refusing to use the primary machine, builder, or SSH port\n' >&2
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
./bin/hermes-box ssh 'test ! -e /var/run/docker.sock'

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

./bin/hermes-box destroy --force
rm -rf -- "$data_root"
trap - EXIT
printf 'disposable lifecycle test passed\n'
