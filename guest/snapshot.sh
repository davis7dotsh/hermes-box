#!/usr/bin/env bash
set -euo pipefail

rootfs=/workspace/.hermes-box-rootfs.tar.gz
workspace=/tmp/hermes-box-workspace.tar.gz
warnings=/tmp/hermes-box-snapshot-warnings.log

rm -f "$rootfs" "$workspace" "$warnings"

tar \
  --one-file-system \
  --ignore-failed-read \
  --numeric-owner \
  -C / \
  -czpf "$rootfs" \
  --exclude=./workspace \
  --exclude=./proc \
  --exclude=./sys \
  --exclude=./dev \
  --exclude=./run \
  --exclude=./storage \
  --exclude=./packed_layers \
  --exclude=./tmp \
  . 2>"$warnings"

tar \
  --ignore-failed-read \
  --numeric-owner \
  -C /workspace \
  -czpf "$workspace" \
  --exclude=.hermes-box-rootfs.tar.gz \
  . 2>>"$warnings"

sync
