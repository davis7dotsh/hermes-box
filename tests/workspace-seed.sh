#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
temporary=$(mktemp -d "${TMPDIR:-/tmp}/hermes-box-workspace-seed.XXXXXX")
trap 'rm -rf -- "$temporary"' EXIT

workspace=$temporary/workspace
snapshot_dir=$temporary/snapshot
seed=$temporary/seed
mkdir -p "$workspace" "$snapshot_dir" "$seed/work"

printf 'base-content\n' >"$seed/work/base.txt"
printf 'snapshot-1\n' >"$snapshot_dir/workspace-snapshot.id"
COPYFILE_DISABLE=1 tar -C "$seed" -cpf "$snapshot_dir/workspace-snapshot.tar" .

printf 'legacy-content\n' >"$workspace/legacy.txt"
printf 'snapshot-1\n' >"$workspace/.hermes-box-snapshot-id"
"$root/guest/workspace-seed.sh" "$workspace" "$snapshot_dir"

test -f "$workspace/legacy.txt"
test ! -f "$workspace/work/base.txt"
grep -Fqx 'snapshot-1' "$snapshot_dir/workspace-restored.id"

rm -f "$snapshot_dir/workspace-restored.id"
printf 'wrong-snapshot\n' >"$workspace/.hermes-box-snapshot-id"
"$root/guest/workspace-seed.sh" "$workspace" "$snapshot_dir"

test ! -f "$workspace/legacy.txt"
grep -Fqx 'base-content' "$workspace/work/base.txt"
grep -Fqx 'snapshot-1' "$snapshot_dir/workspace-restored.id"

echo "workspace seed checks passed"
