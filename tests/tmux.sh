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
  [[ $(tmux -L "$socket" show-options -gv default-terminal) == tmux-256color ]]
  [[ $(tmux -L "$socket" show-options -gv mouse) == on ]]
  [[ $(tmux -L "$socket" show-options -gv focus-events) == on ]]
  [[ $(tmux -L "$socket" show-options -gv set-clipboard) == on ]]
  [[ $(tmux -L "$socket" show-options -gv allow-passthrough) == on ]]
  [[ $(tmux -L "$socket" show-options -sv extended-keys) == always ]]
  terminal_features=$(tmux -L "$socket" show-options -gv terminal-features)
  [[ $terminal_features == *xterm\*:RGB* ]]
  [[ $terminal_features == *xterm\*:extkeys* ]]
  update_environment=$(tmux -L "$socket" show-options -gv update-environment)
  for variable in COLORTERM TERM_PROGRAM TERM_PROGRAM_VERSION; do
    [[ " $update_environment " == *" $variable "* ]]
  done
  COLORTERM=truecolor \
    TERM_PROGRAM=Ghostty \
    TERM_PROGRAM_VERSION=refresh-test \
    tmux -L "$socket" new-session -d -s refresh-test -c "$temporary"
  [[ $(
    tmux -L "$socket" show-environment -g COLORTERM
  ) == COLORTERM=truecolor ]]
  [[ $(
    tmux -L "$socket" show-environment -g TERM_PROGRAM
  ) == TERM_PROGRAM=Ghostty ]]
  [[ $(
    tmux -L "$socket" show-environment -g TERM_PROGRAM_VERSION
  ) == TERM_PROGRAM_VERSION=refresh-test ]]
  [[ $(tmux -L "$socket" show-options -gv status-position) == bottom ]]
  [[ $(tmux -L "$socket" show-options -gv status-style) == *fg=white* ]]
  [[ $(tmux -L "$socket" show-options -gv status-style) == *bg=#006400* ]]
  window_style=$(tmux -L "$socket" show-options -gv window-status-style)
  current_window_style=$(
    tmux -L "$socket" show-options -gv window-status-current-style
  )
  [[ $window_style == *fg=white* && $window_style == *bg=#006400* ]]
  [[ $current_window_style == *fg=white* ]]
  [[ $current_window_style == *bg=#006400* ]]
  [[ $current_window_style == *bold* ]]
  [[ $current_window_style == *underscore* ]]
  [[ $current_window_style != "$window_style" ]]
  if [[ $(
    tmux -L "$socket" display-message -p -t config-test \
      '#{>=:#{version},3.5}'
  ) == 1 ]]; then
    [[ $(
      tmux -L "$socket" show-options -sv extended-keys-format
    ) == csi-u ]]
  fi
else
  printf 'tmux not installed; skipping live config parse\n' >&2
fi

printf 'tmux checks passed\n'
