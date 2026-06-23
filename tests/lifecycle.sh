#!/usr/bin/env bash
# shellcheck disable=SC2016 # remote command strings expand only in the guest.
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"

if [[ ${HERMES_BOX_E2E:-} != 1 ]]; then
  printf 'set HERMES_BOX_E2E=1 to run the destructive isolated lifecycle test\n' >&2
  exit 1
fi

# Qualification inputs are captured before the harness takes ownership of all
# mutable homes. The candidate lock need not exist at the repository root.
candidate_lock_input=${HERMES_BOX_E2E_LOCK:-}
baseline_lock_input=${HERMES_BOX_E2E_BASELINE_LOCK:-}
candidate_artifacts=${HERMES_BOX_E2E_ARTIFACT_DIR:-}
baseline_artifacts=${HERMES_BOX_E2E_BASELINE_ARTIFACT_DIR:-}
artifact_cache=${HERMES_BOX_E2E_ARTIFACT_CACHE:-}
unset HERMES_BOX_E2E HERMES_BOX_E2E_LOCK HERMES_BOX_E2E_BASELINE_LOCK \
  HERMES_BOX_E2E_ARTIFACT_DIR HERMES_BOX_E2E_BASELINE_ARTIFACT_DIR \
  HERMES_BOX_E2E_ARTIFACT_CACHE

for forbidden in HERMES_BOX_CONFIG HERMES_BOX_HOME LIMA_HOME; do
  if [[ -n ${!forbidden:-} ]]; then
    printf 'refusing inherited %s; lifecycle owns all state roots\n' "$forbidden" >&2
    exit 1
  fi
done

[[ -n $candidate_lock_input && -f $candidate_lock_input ]] || {
  printf 'set HERMES_BOX_E2E_LOCK to an explicit candidate lock\n' >&2
  exit 1
}
[[ -n $baseline_lock_input && -f $baseline_lock_input ]] || {
  printf 'set HERMES_BOX_E2E_BASELINE_LOCK to an explicit baseline lock\n' >&2
  exit 1
}
candidate_lock_input=$(cd "$(dirname "$candidate_lock_input")" && pwd)/$(basename "$candidate_lock_input")
baseline_lock_input=$(cd "$(dirname "$baseline_lock_input")" && pwd)/$(basename "$baseline_lock_input")
GOTOOLCHAIN=go1.24.13 go run ./release/validate-lock.go "$candidate_lock_input"
GOTOOLCHAIN=go1.24.13 go run ./release/validate-lock.go "$baseline_lock_input"
GOTOOLCHAIN=go1.24.13 go run ./release/validate-lock.go \
  --lifecycle "$baseline_lock_input" "$candidate_lock_input"

for input_dir in "$candidate_artifacts" "$baseline_artifacts"; do
  if [[ -z $input_dir || ! -d $input_dir ]]; then
    printf 'qualification input directory does not exist: %s\n' "$input_dir" >&2
    exit 1
  fi
  if find "$input_dir" -type l -print -quit | grep -q .; then
    printf 'qualification input directory contains a symbolic link: %s\n' "$input_dir" >&2
    exit 1
  fi
done
if [[ -n $artifact_cache && ! -d $artifact_cache ]]; then
  printf 'qualification artifact cache does not exist: %s\n' "$artifact_cache" >&2
  exit 1
fi

temporary=$(mktemp -d "${TMPDIR:-/tmp}/hermes-box-lifecycle.XXXXXX")
config_dir=$temporary/source-config
restore_config_dir=$temporary/restore-config
export HERMES_BOX_HOME=$temporary/hermes-home
export LIMA_HOME=$temporary/lima-home
name="e2e-$$-$(date +%s)"
restore_name="$name-restore"
executor_port=$((50000 + $$ % 9000))
restore_executor_port=$((executor_port + 1))
config=$config_dir/hermes-box.yaml
lock=$config_dir/hermes-box.lock
restore_config=$restore_config_dir/hermes-box.yaml
identity=$temporary/source-age-key.txt
mkdir -p "$config_dir" "$restore_config_dir" "$HERMES_BOX_HOME" "$LIMA_HOME"

seed_release_artifacts() {
  local selected_lock=$1 selected_artifacts=$2 artifact checksum digest destination
  GOTOOLCHAIN=go1.24.13 go run ./release/validate-lock.go \
    "$selected_lock" "$selected_artifacts"
  for checksum in "$selected_artifacts"/*.tar.zst.sha256; do
    [[ -f $checksum ]] || { printf 'release artifact checksums are missing: %s\n' "$selected_artifacts" >&2; exit 1; }
    artifact=${checksum%.sha256}
    [[ -f $artifact ]] || { printf 'release artifact is missing: %s\n' "$artifact" >&2; exit 1; }
    digest=$(tr -d '[:space:]' <"$checksum")
    [[ $digest =~ ^[a-f0-9]{64}$ ]] || {
      printf 'invalid release checksum for %s\n' "$artifact" >&2
      exit 1
    }
    printf '%s  %s\n' "$digest" "$artifact" | shasum -a 256 -c - >/dev/null
    destination=$HERMES_BOX_HOME/artifacts/sha256/${digest:0:2}/$digest
    mkdir -p "$(dirname "$destination")"
    install -m 0600 "$artifact" "$destination"
  done
}

seed_artifacts() {
  mkdir -p "$HERMES_BOX_HOME/artifacts"
  if [[ -n $artifact_cache ]]; then
    if find "$artifact_cache" -type l -print -quit | grep -q .; then
      printf 'artifact cache contains a symbolic link; refusing it\n' >&2
      exit 1
    fi
    cp -R "$artifact_cache/." "$HERMES_BOX_HOME/artifacts/"
  fi
  seed_release_artifacts "$baseline_lock_input" "$baseline_artifacts"
  seed_release_artifacts "$candidate_lock_input" "$candidate_artifacts"
}

write_config() {
  local destination=$1 box_name=$2 port=$3
  cat >"$destination" <<EOF
schema: 1
name: $box_name
vm:
  cpus: 4
  memory: 8GiB
  root_disk: 30GiB
  data_disk: 50GiB
ports:
  executor: $port
backup:
  keep: 2
EOF
}

keychain_account() {
  local selected_config=$1 box_name=$2 canonical_dir digest
  # Match config.ResolvePath's filepath.Abs/Clean identity exactly. Resolving
  # /var to /private/var here would address a different Keychain account.
  canonical_dir=$(cd "$(dirname "$selected_config")" && pwd -L)
  digest=$(printf '%s\0%s' "$canonical_dir" "$box_name" | shasum -a 256 | awk '{print substr($1, 1, 32)}')
  printf 'box:%s:%s\n' "$box_name" "$digest"
}

delete_keychain_identity() {
  local selected_config=$1 box_name=$2 account
  command -v security >/dev/null 2>&1 || return 0
  account=$(keychain_account "$selected_config" "$box_name")
  security delete-generic-password \
    -s com.highmatter.hermes-box.backup -a "$account" >/dev/null 2>&1 || true
}

write_config "$config" "$name" "$executor_port"
write_config "$restore_config" "$restore_name" "$restore_executor_port"
cp "$baseline_lock_input" "$lock"
seed_artifacts

export HERMES_BOX_CONFIG=$config

case $name:$restore_name in
  *main* | *hermes-box*) printf 'refusing unsafe lifecycle name\n' >&2; exit 1 ;;
esac
[[ $executor_port -ne 4788 && $restore_executor_port -ne 4788 ]]
[[ $HERMES_BOX_HOME == "$temporary/hermes-home" ]]
[[ $LIMA_HOME == "$temporary/lima-home" ]]
[[ $config == "$temporary/source-config/hermes-box.yaml" ]]

cleanup() {
  printf 'isolated cleanup: source=%s restore=%s home=%s lima=%s\n' \
    "$config" "$restore_config" "$HERMES_BOX_HOME" "$LIMA_HOME" >&2
  ./bin/hermes-box --config "$restore_config" destroy --force >/dev/null 2>&1 || true
  ./bin/hermes-box --config "$config" destroy --force >/dev/null 2>&1 || true
  delete_keychain_identity "$restore_config" "$restore_name"
  delete_keychain_identity "$config" "$name"
  rm -rf -- "$temporary"
}
trap cleanup EXIT

assert_persistence() {
  local selected_config=$1
  ./bin/hermes-box --config "$selected_config" exec -- sh -eu -c '
    grep -Fxq workspace-state /home/agent/workspace/hermes-box-e2e.txt
    grep -Fxq claude-state /home/agent/.claude/hermes-box-e2e.txt
    grep -Fxq codex-state /home/agent/.codex/hermes-box-e2e.txt
    grep -Fxq hermes-state /home/agent/.hermes/hermes-box-e2e.txt
    grep -Fxq executor-state /data/executor/hermes-box-e2e.txt
  '
}

assert_loopback() {
  local port=$1 listeners
  listeners=$(lsof -nP -iTCP:"$port" -sTCP:LISTEN)
  grep -Eq "127\\.0\\.0\\.1:$port|\\[::1\\]:$port" <<<"$listeners"
  if grep -Eq "\\*:$port|0\\.0\\.0\\.0:$port|\\[::\\]:$port" <<<"$listeners"; then
    printf 'Executor port %s is not loopback-only\n%s\n' "$port" "$listeners" >&2
    exit 1
  fi
}

assert_component_status() {
  local selected_config=$1 desired_lock=$2 applied_lock=$3 component=$4 previous_lock=$5
  ./bin/hermes-box --config "$selected_config" --json status | \
    GOTOOLCHAIN=go1.24.13 go run ./release/validate-lock.go \
      --assert-status "$desired_lock" "$applied_lock" "$component" "$previous_lock"
}

printf 'isolated lifecycle: name=%s config=%s home=%s lima=%s candidate=%s\n' \
  "$name" "$config" "$HERMES_BOX_HOME" "$LIMA_HOME" "$candidate_lock_input"

./bin/hermes-box --config "$config" create
./bin/hermes-box --config "$config" status
assert_component_status "$config" "$baseline_lock_input" "$baseline_lock_input" all none
./bin/hermes-box --config "$config" exec -- \
  sh -eu -c '. /etc/os-release; test "$VERSION_ID" = 26.04; test "$(id -u)" = 1000; mountpoint -q /data; test -d /home/agent/workspace'
./bin/hermes-box --config "$config" exec -- \
  sh -eu -c 'claude --version; codex --strict-config --version; hermes --version; node --version; uv --version; systemctl is-active hermes.service executor.service'

# Exercise the public SSH dispatch through a real pseudo-terminal. The helper
# waits for the exact blessed session created by /usr/local/bin/tm, detaches the
# client, and leaves the session available for environment assertions.
command -v script >/dev/null
./bin/hermes-box --config "$config" exec -- tmux kill-session -t =main >/dev/null 2>&1 || true
(
  for _ in {1..100}; do
    if ./bin/hermes-box --config "$config" exec -- tmux has-session -t =main >/dev/null 2>&1; then
      ./bin/hermes-box --config "$config" exec -- tmux detach-client -s main >/dev/null 2>&1 || true
      exit 0
    fi
    sleep 0.1
  done
  exit 1
) &
ssh_detacher=$!
TERM=xterm-ghostty TERM_PROGRAM=ghostty TERM_PROGRAM_VERSION=e2e \
  script -q /dev/null ./bin/hermes-box --config "$config" ssh </dev/null >/dev/null
wait "$ssh_detacher"
./bin/hermes-box --config "$config" exec -- sh -eu -c '
  tmux has-session -t =main
  tmux display-message -p -t =main "#{session_name}" | grep -Fxq main
  tmux show-environment -t =main TERM_PROGRAM | grep -Fxq TERM_PROGRAM=ghostty
'
./bin/hermes-box --config "$config" exec -- sh -eu -c '
  mkdir -p /home/agent/.claude /home/agent/.codex /home/agent/.hermes /data/executor
  printf "%s\n" workspace-state > /home/agent/workspace/hermes-box-e2e.txt
  printf "%s\n" claude-state > /home/agent/.claude/hermes-box-e2e.txt
  printf "%s\n" codex-state > /home/agent/.codex/hermes-box-e2e.txt
  printf "%s\n" hermes-state > /home/agent/.hermes/hermes-box-e2e.txt
  printf "%s\n" executor-state > /data/executor/hermes-box-e2e.txt
  test "$(tmux -L hermes-box-e2e -f /etc/tmux.conf new-session -d -P -F "#{session_name}" -s e2e)" = e2e
  test "$(tmux -L hermes-box-e2e show-options -gv mouse)" = on
  test "$(tmux -L hermes-box-e2e show-options -gv allow-passthrough)" = on
  test "$(tmux -L hermes-box-e2e show-options -sv extended-keys)" = always
  test "$(tmux -L hermes-box-e2e show-options -gv status-position)" = bottom
  tmux -L hermes-box-e2e show-options -gv terminal-features | grep -Fq "xterm*:extkeys"
  tmux -L hermes-box-e2e show-options -gv update-environment | grep -Fq "TERM_PROGRAM"
  tmux -L hermes-box-e2e show-options -gv status-style | grep -Fq "fg=white"
  tmux -L hermes-box-e2e show-options -gv status-style | grep -Fq "bg=#006400"
  if test "$(tmux -L hermes-box-e2e display-message -p "#{>=:#{version},3.5}")" = 1; then
    test "$(tmux -L hermes-box-e2e show-options -sv extended-keys-format)" = csi-u
  fi
  infocmp -x xterm-ghostty >/dev/null
  grep -Fq "bg=#006400,fg=white" /etc/tmux.conf
  tmux -L hermes-box-e2e kill-server
'
assert_loopback "$executor_port"

for _ in 1 2 3; do
  ./bin/hermes-box --config "$config" stop
  ./bin/hermes-box --config "$config" start
done
assert_persistence "$config"

# Prove a failed materialization cannot change any active pin and that a
# credential-shaped URL value is not reflected in operator diagnostics.
failure_lock=$temporary/failure.lock
redaction_secret=qualification-secret-value
sed -E \
  "s|^    archive: .*node-v.*|    archive: https://127.0.0.1:1/node.tar.xz?token=$redaction_secret|" \
  "$candidate_lock_input" >"$failure_lock"
grep -Fq "token=$redaction_secret" "$failure_lock"
cp "$failure_lock" "$lock"
if failure_output=$(./bin/hermes-box --config "$config" update node 2>&1); then
  printf 'lifecycle failure injection unexpectedly succeeded\n' >&2
  exit 1
fi
if grep -Fq "$redaction_secret" <<<"$failure_output"; then
  printf 'lifecycle failure diagnostics exposed the lock URL credential\n' >&2
  exit 1
fi
cp "$baseline_lock_input" "$lock"
assert_component_status "$config" "$baseline_lock_input" "$baseline_lock_input" all none
assert_persistence "$config"

cp "$candidate_lock_input" "$lock"
update_components=(node uv claude codex hermes executor)
for update_component in "${update_components[@]}"; do
  printf 'lifecycle update and rollback: %s\n' "$update_component"
  ./bin/hermes-box --config "$config" update "$update_component"
  ./bin/hermes-box --config "$config" status
  assert_component_status "$config" "$candidate_lock_input" "$candidate_lock_input" \
    "$update_component" "$baseline_lock_input"
  ./bin/hermes-box --config "$config" rollback "$update_component"
  ./bin/hermes-box --config "$config" status
  assert_component_status "$config" "$candidate_lock_input" "$baseline_lock_input" \
    "$update_component" "$candidate_lock_input"
  assert_persistence "$config"
done
./bin/hermes-box --config "$config" update all
./bin/hermes-box --config "$config" status
assert_component_status "$config" "$candidate_lock_input" "$candidate_lock_input" \
  all "$baseline_lock_input"
assert_persistence "$config"

./bin/hermes-box --config "$config" key export "$identity"
./bin/hermes-box --config "$config" backup lifecycle
archive=$(find "$HERMES_BOX_HOME/backups/$name" -maxdepth 1 -type f \
  -name '*-lifecycle.tar.zst.age' -print | sort | tail -1)
[[ -n $archive && -f $archive && -f ${archive%.tar.zst.age}.envelope.json ]]

[[ ! -e $restore_config_dir/hermes-box.lock ]]
./bin/hermes-box --config "$restore_config" restore "$archive" --identity "$identity"
[[ -f $restore_config_dir/hermes-box.lock ]]
GOTOOLCHAIN=go1.24.13 go run ./release/validate-lock.go \
  "$restore_config_dir/hermes-box.lock"
assert_component_status "$restore_config" "$candidate_lock_input" \
  "$candidate_lock_input" all none
assert_persistence "$restore_config"
assert_loopback "$restore_executor_port"
./bin/hermes-box --config "$restore_config" doctor

./bin/hermes-box --config "$config" rebuild
assert_persistence "$config"
assert_loopback "$executor_port"
./bin/hermes-box --config "$config" doctor

./bin/hermes-box --config "$restore_config" destroy
./bin/hermes-box --config "$config" destroy
delete_keychain_identity "$restore_config" "$restore_name"
delete_keychain_identity "$config" "$name"

trap - EXIT
rm -rf -- "$temporary"
printf 'isolated lifecycle test passed\n'
