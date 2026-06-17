# shellcheck shell=bash
# Interactive SSH sessions immediately enter the unprivileged Hermes account.
# Noninteractive SSH commands do not read this file and remain unaffected.
if [[ $- == *i* && -n ${SSH_CONNECTION:-} ]]; then
  exec sudo -iu hermes
fi

if [[ -f ~/.profile ]]; then
  # shellcheck disable=SC1090
  . ~/.profile
fi
