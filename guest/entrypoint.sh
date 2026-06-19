#!/usr/bin/env bash
set -euo pipefail

if [[ ${HERMES_BOX_RESTORE_MODE:-false} == true ]]; then
  while [[ ! -f /var/lib/hermes-box/restore-ready ]]; do
    sleep 1
  done
fi

exec /usr/local/sbin/hermes-box-start "$@"
