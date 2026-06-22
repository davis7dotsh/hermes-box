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

assert_invalid_snapshot_id_is_safe() {
  local invalid_id=$1

  printf 'keep-me\n' >"$workspace/untouched.txt"
  printf '%s' "$invalid_id" >"$snapshot_dir/workspace-snapshot.id"
  if "$root/guest/workspace-seed.sh" "$workspace" "$snapshot_dir" \
    >"$temporary/invalid.out" 2>"$temporary/invalid.err"; then
    printf 'workspace seed accepted invalid snapshot metadata\n' >&2
    exit 1
  fi
  grep -Fq 'invalid workspace snapshot ID metadata' \
    "$temporary/invalid.err"
  grep -Fqx 'keep-me' "$workspace/untouched.txt"
  test ! -e "$snapshot_dir/workspace-restored.id"
}

assert_invalid_snapshot_id_is_safe ''
assert_invalid_snapshot_id_is_safe 'snapshot-1'
assert_invalid_snapshot_id_is_safe $'snapshot-1\nsecond-line\n'
assert_invalid_snapshot_id_is_safe $'../snapshot-1\n'

rm -f "$workspace/untouched.txt"
printf 'snapshot-1\n' >"$snapshot_dir/workspace-snapshot.id"

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
