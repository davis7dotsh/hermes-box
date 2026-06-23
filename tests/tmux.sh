#!/usr/bin/env bash
# shellcheck disable=SC2016 # grep patterns intentionally contain shell literals.
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"
temporary=$(mktemp -d "${TMPDIR:-/tmp}/hermes-box-tmux.XXXXXX")
socket="hermes-box-config-$$"

cleanup() {
  tmux -L "$socket" kill-server >/dev/null 2>&1 || true
  rm -rf -- "$temporary"
}
trap cleanup EXIT

fail() {
  printf 'tmux check failed: %s\n' "$1" >&2
  exit 1
}

assert_equal() {
  [[ $1 == "$2" ]] || fail "$3: got '$1', want '$2'"
}

assert_contains() {
  [[ $1 == *"$2"* ]] || fail "$3: '$1' does not contain '$2'"
}

bash -n guest/tm
grep -Fqx 'session=main' guest/tm
grep -Fqx 'workdir=/home/agent/workspace' guest/tm
grep -Fq 'switch-client -t "=$session"' guest/tm
grep -Fq 'attach-session -t "=$session"' guest/tm
grep -Fq 'TERM=xterm-256color' guest/tm

mkdir -p "$temporary/home/agent/workspace"
error_file=$temporary/tm-nontty.err
if HOME=$temporary guest/tm >/dev/null 2>"$error_file"; then
  fail 'tm accepted a noninteractive terminal'
fi
grep -Fq 'an interactive terminal is required outside tmux' "$error_file"

if ! command -v tmux >/dev/null 2>&1; then
  printf 'tmux not installed; skipping live config parse\n' >&2
  printf 'tmux checks passed\n'
  exit 0
fi

HOME=$temporary \
  COLORTERM=initial \
  TERM_PROGRAM=initial \
  TERM_PROGRAM_VERSION=initial \
  tmux -L "$socket" -f guest/tmux.conf \
    new-session -d -s config-test -c "$temporary"

assert_equal "$(tmux -L "$socket" show-options -gv default-terminal)" \
  tmux-256color 'default terminal'
assert_equal "$(tmux -L "$socket" show-options -gv mouse)" on 'mouse'
assert_equal "$(tmux -L "$socket" show-options -gv focus-events)" on 'focus events'
assert_equal "$(tmux -L "$socket" show-options -gv set-clipboard)" on 'clipboard'
assert_equal "$(tmux -L "$socket" show-options -gv allow-passthrough)" on 'passthrough'
assert_equal "$(tmux -L "$socket" show-options -sv extended-keys)" always 'extended keys'
assert_equal "$(tmux -L "$socket" show-options -gv status-position)" bottom 'status position'

terminal_features=$(tmux -L "$socket" show-options -gv terminal-features)
assert_contains "$terminal_features" 'xterm*:RGB' 'terminal RGB'
assert_contains "$terminal_features" 'xterm*:extkeys' 'terminal extended keys'

update_environment=$(tmux -L "$socket" show-options -gv update-environment)
for variable in COLORTERM TERM_PROGRAM TERM_PROGRAM_VERSION; do
  grep -Fxq "$variable" <<<"$update_environment" ||
    fail "update-environment is missing $variable"
done

printf 'detach-client\n' | env \
  HOME="$temporary" \
  COLORTERM=truecolor \
  TERM_PROGRAM=Ghostty \
  TERM_PROGRAM_VERSION=refresh-test \
  tmux -L "$socket" -C attach-session -t config-test >/dev/null

assert_equal "$(tmux -L "$socket" show-environment -t config-test COLORTERM)" \
  COLORTERM=truecolor 'refreshed COLORTERM'
assert_equal "$(tmux -L "$socket" show-environment -t config-test TERM_PROGRAM)" \
  TERM_PROGRAM=Ghostty 'refreshed TERM_PROGRAM'
assert_equal "$(tmux -L "$socket" show-environment -t config-test TERM_PROGRAM_VERSION)" \
  TERM_PROGRAM_VERSION=refresh-test 'refreshed TERM_PROGRAM_VERSION'

status_style=$(tmux -L "$socket" show-options -gv status-style)
assert_contains "$status_style" 'fg=white' 'status foreground'
assert_contains "$status_style" 'bg=#006400' 'status background'
window_style=$(tmux -L "$socket" show-options -gv window-status-style)
current_style=$(tmux -L "$socket" show-options -gv window-status-current-style)
assert_contains "$window_style" 'fg=white' 'window foreground'
assert_contains "$window_style" 'bg=#006400' 'window background'
assert_contains "$current_style" 'fg=white' 'current window foreground'
assert_contains "$current_style" 'bg=#006400' 'current window background'
assert_contains "$current_style" bold 'current window bold'
assert_contains "$current_style" underscore 'current window underline'
[[ $current_style != "$window_style" ]] || fail 'current window is not distinct'

if [[ $(tmux -L "$socket" display-message -p -t config-test \
  '#{>=:#{version},3.5}') == 1 ]]; then
  assert_equal "$(tmux -L "$socket" show-options -sv extended-keys-format)" \
    csi-u 'extended key format'
fi

# Prove the normal Enter byte and Ghostty's CSI-u Shift+Enter sequence remain
# distinct through tmux all the way to a pane. This does not depend on a VM or
# a real terminal client, so it remains part of the safe local gate.
key_capture=$temporary/keys.hex
key_ready=$temporary/keys.ready
tmux -L "$socket" new-window -d -t config-test -n key-test \
  "sh -c 'stty raw -echo; : > \"$key_ready\"; dd bs=1 count=8 2>/dev/null | od -An -tx1 > \"$key_capture\"'"
for _ in {1..50}; do
  [[ -e $key_ready ]] && break
  sleep 0.02
done
[[ -e $key_ready ]] || fail 'key-sequence pane did not become ready'
tmux -L "$socket" send-keys -H -t config-test:key-test.0 \
  0d 1b 5b 31 33 3b 32 75
for _ in {1..50}; do
  [[ -s $key_capture ]] && break
  sleep 0.02
done
[[ -s $key_capture ]] || fail 'key-sequence pane produced no capture'
assert_equal "$(tr -d '[:space:]' <"$key_capture")" \
  0d1b5b31333b3275 'Enter and Shift+Enter pane bytes'

printf 'tmux checks passed\n'
