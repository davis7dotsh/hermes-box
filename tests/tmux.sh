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
  if [[ $1 == "$2" ]]; then
    return
  fi
  fail "$3: got '$1', want '$2'"
}

assert_contains() {
  if [[ $1 == *"$2"* ]]; then
    return
  fi
  fail "$3: '$1' does not contain '$2'"
}

assert_line() {
  if grep -Fxq -- "$2" <<<"$1"; then
    return
  fi
  fail "$3: '$1' does not contain line '$2'"
}

assert_different() {
  if [[ $1 != "$2" ]]; then
    return
  fi
  fail "$3: both values are '$1'"
}

bash -n guest/tm
bash -n guest/boxadmin.bash_profile

error_file=$temporary/tm-nontty.err
if guest/tm >/dev/null 2>"$error_file"; then
  printf 'tm unexpectedly accepted a noninteractive terminal\n' >&2
  exit 1
fi
grep -Fq 'an interactive terminal is required outside tmux' "$error_file"

grep -Fqx 'session=main' guest/tm
grep -Fqx 'workdir=/workspace/work' guest/tm
grep -Fq 'switch-client -t "=$session"' guest/tm
grep -Fq 'attach-session -t "=$session"' guest/tm
grep -Fq 'TERM=xterm-256color' guest/tm
grep -Fq 'sudo -iu hermes env' guest/boxadmin.bash_profile

if command -v tmux >/dev/null 2>&1; then
  HOME=$temporary \
    COLORTERM=initial \
    TERM_PROGRAM=initial \
    TERM_PROGRAM_VERSION=initial \
    tmux -L "$socket" -f guest/tmux.conf \
      new-session -d -s config-test -c "$temporary"
  assert_equal \
    "$(tmux -L "$socket" show-options -gv default-terminal)" \
    tmux-256color "default terminal"
  assert_equal "$(tmux -L "$socket" show-options -gv mouse)" on "mouse"
  assert_equal \
    "$(tmux -L "$socket" show-options -gv focus-events)" on "focus events"
  assert_equal \
    "$(tmux -L "$socket" show-options -gv set-clipboard)" on "clipboard"
  assert_equal \
    "$(tmux -L "$socket" show-options -gv allow-passthrough)" on \
    "visible-pane passthrough"
  assert_equal \
    "$(tmux -L "$socket" show-options -sv extended-keys)" always \
    "extended keys"
  terminal_features=$(tmux -L "$socket" show-options -gv terminal-features)
  assert_contains "$terminal_features" 'xterm*:RGB' "terminal RGB"
  assert_contains "$terminal_features" 'xterm*:extkeys' "terminal extended keys"
  update_environment=$(tmux -L "$socket" show-options -gv update-environment)
  for variable in COLORTERM TERM_PROGRAM TERM_PROGRAM_VERSION; do
    assert_line "$update_environment" "$variable" "update-environment $variable"
  done
  printf 'detach-client\n' | env \
    HOME="$temporary" \
    COLORTERM=truecolor \
    TERM_PROGRAM=Ghostty \
    TERM_PROGRAM_VERSION=refresh-test \
    tmux -L "$socket" -C attach-session -t config-test >/dev/null
  assert_equal \
    "$(tmux -L "$socket" show-environment -t config-test COLORTERM)" \
    COLORTERM=truecolor "refreshed COLORTERM"
  assert_equal \
    "$(tmux -L "$socket" show-environment -t config-test TERM_PROGRAM)" \
    TERM_PROGRAM=Ghostty "refreshed TERM_PROGRAM"
  assert_equal \
    "$(tmux -L "$socket" show-environment -t config-test TERM_PROGRAM_VERSION)" \
    TERM_PROGRAM_VERSION=refresh-test "refreshed TERM_PROGRAM_VERSION"
  assert_equal \
    "$(tmux -L "$socket" show-options -gv status-position)" bottom \
    "status position"
  status_style=$(tmux -L "$socket" show-options -gv status-style)
  assert_contains "$status_style" 'fg=white' "status foreground"
  assert_contains "$status_style" 'bg=#006400' "status background"
  window_style=$(tmux -L "$socket" show-options -gv window-status-style)
  current_window_style=$(
    tmux -L "$socket" show-options -gv window-status-current-style
  )
  assert_contains "$window_style" 'fg=white' "window foreground"
  assert_contains "$window_style" 'bg=#006400' "window background"
  assert_contains "$current_window_style" 'fg=white' \
    "current window foreground"
  assert_contains "$current_window_style" 'bg=#006400' \
    "current window background"
  assert_contains "$current_window_style" bold "current window bold"
  assert_contains "$current_window_style" underscore "current window underline"
  assert_different "$current_window_style" "$window_style" \
    "current window differentiation"
  if [[ $(
    tmux -L "$socket" display-message -p -t config-test \
      '#{>=:#{version},3.5}'
  ) == 1 ]]; then
    assert_equal \
      "$(tmux -L "$socket" show-options -sv extended-keys-format)" csi-u \
      "extended key format"
  fi
else
  printf 'tmux not installed; skipping live config parse\n' >&2
fi

printf 'tmux checks passed\n'
