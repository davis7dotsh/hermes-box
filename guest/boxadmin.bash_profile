# shellcheck shell=bash
# Interactive SSH sessions immediately enter Hermes' persistent main tmux
# session. Noninteractive SSH commands do not read this file and remain
# unaffected.
if [[ $- == *i* && -n ${SSH_CONNECTION:-} ]]; then
  if sudo -iu hermes env \
    COLORTERM="${COLORTERM:-}" \
    TERM_PROGRAM="${TERM_PROGRAM:-}" \
    TERM_PROGRAM_VERSION="${TERM_PROGRAM_VERSION:-}" \
    tm; then
    exit 0
  fi
  printf 'warning: tmux session startup failed; opening a Hermes login shell\n' >&2
  exec sudo -iu hermes
fi

if [[ -f ~/.profile ]]; then
  # shellcheck disable=SC1090
  . ~/.profile
fi
