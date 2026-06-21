#!/usr/bin/env bash
set -euo pipefail

canonical_repository_digest() {
  local reference=$1
  local reference_digest repository_with_tag repository last_component

  [[ $reference == *@* ]] || return 1
  reference_digest=${reference##*@}
  [[ $reference_digest =~ ^sha256:[0-9a-f]{64}$ ]] || return 1
  repository_with_tag=${reference%@*}
  repository=$repository_with_tag
  last_component=${repository_with_tag##*/}
  if [[ $last_component == *:* ]]; then
    repository=${repository_with_tag%:*}
  fi
  [[ -n $repository ]] || return 1
  printf '%s@%s\n' "$repository" "$reference_digest"
}

image=${HERMES_BOX_EXECUTOR_IMAGE:?Executor image is required}
host_port=${HERMES_BOX_EXECUTOR_HOST_PORT:?Executor host port is required}
source_image=$(canonical_repository_digest "$image") || {
  printf 'invalid Executor image reference: %s\n' "$image" >&2
  exit 1
}
source_digest=${source_image##*@}
runtime_root=${HERMES_BOX_EXECUTOR_RUNTIME_ROOT:-/workspace/.hermes-box-runtime/executor}
releases=$runtime_root/releases
current=$runtime_root/current
oci_cache=$runtime_root/.oci-cache-${source_digest#sha256:}
data=${HERMES_BOX_EXECUTOR_DATA_DIR:-/workspace/executor/data}
extractor=${HERMES_BOX_EXECUTOR_EXTRACTOR:-/usr/local/sbin/hermes-box-extract-executor}
pull_stall_seconds=${HERMES_BOX_EXECUTOR_PULL_STALL_SECONDS:-600}

if [[ ! $host_port =~ ^[0-9]+$ ]] || ((10#$host_port < 1 || 10#$host_port > 65535)); then
  printf 'invalid Executor host port: %s\n' "$host_port" >&2
  exit 1
fi
if [[ ! $pull_stall_seconds =~ ^[0-9]+$ ]] || ((10#$pull_stall_seconds < 1)); then
  printf 'invalid Executor pull stall timeout: %s\n' "$pull_stall_seconds" >&2
  exit 1
fi

cleanup() {
  local path
  for path in \
    "${download:-}" \
    "${staging:-}" \
    "${next_link:-}" \
    "${validation_root:-}"; do
    if [[ -n $path ]]; then
      rm -rf -- "$path"
    fi
  done
  if [[ ${runtime_lock_held:-false} == true ]]; then
    flock -u 9 || true
    exec 9>&-
    runtime_lock_held=false
  fi
}
trap cleanup EXIT

run_with_stall_deadline() {
  local stall_seconds=$1
  local progress_path=$2
  shift 2
  local fifo=$runtime_root/.pull-output-$$
  local command_pid relay_pid command_status=0
  local last_usage current_usage
  local poll_interval=1 polls_per_second=1 idle_polls=0 max_idle_polls

  if ((stall_seconds < 10)); then
    poll_interval=0.1
    polls_per_second=10
  fi
  max_idle_polls=$((stall_seconds * polls_per_second))

  rm -f -- "$fifo"
  mkfifo -m 0600 "$fifo"
  exec 8<>"$fifo"
  exec 7<"$fifo"
  cat 8>&- <&7 >&2 &
  relay_pid=$!
  "$@" 8>&- 7<&- >"$fifo" 2>&1 &
  command_pid=$!
  exec 8>&-
  exec 7<&-
  rm -f -- "$fifo"

  last_usage=$(du -sk "$progress_path" 2>/dev/null || printf '0')
  last_usage=${last_usage%%[[:space:]]*}

  while kill -0 "$command_pid" 2>/dev/null; do
    sleep "$poll_interval"
    current_usage=$(du -sk "$progress_path" 2>/dev/null || printf '0')
    current_usage=${current_usage%%[[:space:]]*}
    if [[ $current_usage != "$last_usage" ]]; then
      last_usage=$current_usage
      idle_polls=0
      continue
    fi
    if ! kill -0 "$command_pid" 2>/dev/null; then
      break
    fi
    ((idle_polls += 1))
    if ((idle_polls >= max_idle_polls)); then
      printf '\nExecutor image pull wrote no OCI data for %s seconds; stopping it\n' \
        "$stall_seconds" >&2
      kill -TERM "$command_pid" 2>/dev/null || true
      for _ in {1..30}; do
        if ! kill -0 "$command_pid" 2>/dev/null; then
          break
        fi
        sleep 1
      done
      kill -KILL "$command_pid" 2>/dev/null || true
      wait "$command_pid" 2>/dev/null || true
      kill "$relay_pid" 2>/dev/null || true
      wait "$relay_pid" 2>/dev/null || true
      return 124
    fi
  done

  wait "$command_pid" || command_status=$?
  wait "$relay_pid" 2>/dev/null || true
  return "$command_status"
}

prune_oci_transport_temps() {
  local path

  for path in "$oci_cache"/oci-put-blob*; do
    [[ -e $path || -L $path ]] || continue
    if [[ -f $path || -L $path ]]; then
      rm -f -- "$path"
    fi
  done
}

write_private_metadata() {
  local path=$1
  local value=$2
  local temporary=${path}.tmp-$$

  printf '%s\n' "$value" >"$temporary"
  chmod 0600 "$temporary"
  mv -f -- "$temporary" "$path"
}

repair_release_metadata() {
  local path=$1
  local name manifest_hex reference recorded_source

  [[ -d $path && ! -L $path ]] || return 1
  name=${path##*/}
  [[ $name =~ ^([0-9a-f]{64})(-replacement-[0-9]+(-[0-9]+)?)?$ ]] || return 1
  manifest_hex=${BASH_REMATCH[1]}

  if [[ ! -s $path/.manifest-digest ]]; then
    write_private_metadata "$path/.manifest-digest" "sha256:$manifest_hex"
  fi
  if [[ ! -s $path/.repository-digest ]]; then
    [[ -s $path/.image-reference ]] || return 1
    reference=$(<"$path/.image-reference")
    recorded_source=$(canonical_repository_digest "$reference") || return 1
    [[ $recorded_source == "$source_image" ]] || return 1
    write_private_metadata "$path/.repository-digest" "$recorded_source"
  fi
}

payload_complete() {
  local path=$1
  local expected_manifest=$2
  local recorded_image recorded_source recorded_manifest required

  [[ -d $path && ! -L $path ]] || return 1
  [[ -s $path/.image-reference && -s $path/.repository-digest ]] || return 1
  [[ -s $path/.manifest-digest ]] || return 1
  recorded_image=$(<"$path/.image-reference")
  recorded_source=$(<"$path/.repository-digest")
  recorded_manifest=$(<"$path/.manifest-digest")
  [[ -n $recorded_image ]] || return 1
  [[ $recorded_source == "$source_image" ]] || return 1
  [[ $recorded_manifest == "$expected_manifest" ]] || return 1
  [[ -x $path/usr/local/bin/bun ]] || return 1
  for required in \
    app/package.json \
    app/bun.lock \
    app/apps/host-selfhost/package.json \
    app/apps/host-selfhost/src/serve.ts \
    app/apps/host-selfhost/dist/index.html; do
    [[ -s $path/$required ]] || return 1
  done
  grep -Fq 'executor-workspace' "$path/app/package.json" || return 1
  grep -Fq '@executor-js/host-selfhost' \
    "$path/app/apps/host-selfhost/package.json" || return 1
  grep -Fq 'bun run src/serve.ts' \
    "$path/app/apps/host-selfhost/package.json" || return 1
}

runtime_usable() {
  local path=$1
  local validation_root validation_status=0

  path=$(cd "$path" && pwd -P) || return 1
  validation_root=$(mktemp -d "$runtime_root/.validation-XXXXXX") || return 1
  if ! install -d -m 0700 \
    "$validation_root/home" \
    "$validation_root/config" \
    "$validation_root/share" \
    "$validation_root/cache" \
    "$validation_root/tmp" \
    "$validation_root/data"; then
    rm -rf -- "$validation_root"
    return 1
  fi
  (
    umask 077
    cd "$validation_root"
    timeout --signal=TERM --kill-after=5s 30s \
      env \
      BUN_FEATURE_FLAG_DISABLE_IPV6=1 \
      HOME="$validation_root/home" \
      XDG_CONFIG_HOME="$validation_root/config" \
      XDG_DATA_HOME="$validation_root/share" \
      XDG_CACHE_HOME="$validation_root/cache" \
      TMPDIR="$validation_root/tmp" \
      EXECUTOR_DATA_DIR="$validation_root/data" \
      EXECUTOR_DB_PATH="$validation_root/data/data.db" \
      EXECUTOR_SECRET_KEY=hermes-box-validation-only-secret-key \
      BETTER_AUTH_SECRET=hermes-box-validation-only-auth-secret \
      "$path/usr/local/bin/bun" build \
        "$path/app/apps/host-selfhost/src/serve.ts" \
        --target=bun \
        --outdir="$validation_root/build"
  ) >/dev/null || validation_status=$?
  if ((validation_status == 0)) && [[ ! -s $validation_root/build/serve.js ]]; then
    validation_status=1
  fi
  rm -rf -- "$validation_root" || return 1
  return "$validation_status"
}

release_valid() {
  local path=$1
  local name manifest_hex

  repair_release_metadata "$path" || return 1
  name=${path##*/}
  [[ $name =~ ^([0-9a-f]{64})(-replacement-[0-9]+(-[0-9]+)?)?$ ]] || return 1
  manifest_hex=${BASH_REMATCH[1]}
  payload_complete "$path" "sha256:$manifest_hex"
}

release_usable() {
  local path=$1

  release_valid "$path" || return 1
  runtime_usable "$path"
}

current_release_path() {
  local target

  [[ -L $current ]] || return 1
  target=$(readlink "$current")
  [[ $target =~ ^releases/[0-9a-f]{64}(-replacement-[0-9]+(-[0-9]+)?)?$ ]] || return 1
  printf '%s/%s\n' "$runtime_root" "$target"
}

find_reusable_release() {
  local candidate

  for candidate in "$releases"/*; do
    [[ -e $candidate || -L $candidate ]] || continue
    if release_usable "$candidate"; then
      printf '%s\n' "$candidate"
      return 0
    fi
  done
  return 1
}

activate_release() {
  local release=$1
  local name=${release##*/}
  local displaced_current

  write_private_metadata "$release/.image-reference" "$image"
  next_link=$runtime_root/.current-$$
  rm -rf -- "$next_link"
  ln -s "releases/$name" "$next_link"
  if [[ -e $current && ! -L $current ]]; then
    displaced_current=$runtime_root/.current-displaced-$$
    rm -rf -- "$displaced_current"
    mv -T -- "$current" "$displaced_current"
  fi
  mv -Tf -- "$next_link" "$current"
  next_link=
}

reap_stale_paths() {
  local active=$1
  local selected path

  selected=$(current_release_path) || return 1
  [[ $selected == "$active" ]] || return 1

  for path in \
    "$runtime_root"/.download-* \
    "$runtime_root"/.current-* \
    "$runtime_root"/.oci-cache-* \
    "$runtime_root"/.validation-* \
    "$releases"/.staging-* \
    "$releases"/.obsolete-*; do
    [[ -e $path || -L $path ]] || continue
    rm -rf -- "$path"
  done
  for path in "$releases"/*; do
    [[ -e $path || -L $path ]] || continue
    if [[ $path != "$active" ]]; then
      rm -rf -- "$path"
    fi
  done
}

install -d -m 0700 "$runtime_root" "$releases" "$oci_cache" "$data"
exec 9>"$runtime_root/launcher.lock"
if ! flock -x -w 3300 9; then
  printf 'timed out waiting for the Executor runtime lock\n' >&2
  exit 1
fi
runtime_lock_held=true

release=
if candidate=$(current_release_path) && release_usable "$candidate"; then
  release=$candidate
elif candidate=$(find_reusable_release); then
  release=$candidate
fi

if [[ -z $release ]]; then
  download=$runtime_root/.download-$$
  staging=$releases/.staging-$$
  rm -rf -- "$download" "$staging"

  prune_oci_transport_temps
  run_with_stall_deadline "$pull_stall_seconds" "$oci_cache" \
    skopeo copy \
    --override-os linux \
    --override-arch arm64 \
    --retry-times 3 \
    --preserve-digests \
    "docker://$source_image" \
    "oci:$oci_cache:executor"

  timeout --signal=TERM --kill-after=30s 5m \
    skopeo copy \
    --override-os linux \
    --override-arch arm64 \
    --preserve-digests \
    "oci:$oci_cache:executor" \
    "dir:$download"

  manifest_digest=$(
    timeout --signal=TERM --kill-after=30s 10m \
      "$extractor" "$download" "$staging"
  )
  if [[ ! $manifest_digest =~ ^sha256:[0-9a-f]{64}$ ]]; then
    printf 'invalid Executor manifest digest: %s\n' "$manifest_digest" >&2
    exit 1
  fi
  write_private_metadata "$staging/.image-reference" "$image"
  write_private_metadata "$staging/.repository-digest" "$source_image"
  write_private_metadata "$staging/.manifest-digest" "$manifest_digest"
  payload_complete "$staging" "$manifest_digest"
  runtime_usable "$staging"

  release=$releases/${manifest_digest#sha256:}
  if release_usable "$release"; then
    rm -rf -- "$staging"
    staging=
  else
    if [[ -e $release || -L $release ]]; then
      replacement=$releases/${manifest_digest#sha256:}-replacement-$$
      replacement_index=0
      while [[ -e $replacement || -L $replacement ]]; do
        ((replacement_index += 1))
        replacement=$releases/${manifest_digest#sha256:}-replacement-$$-$replacement_index
      done
      mv -T -- "$staging" "$replacement"
      release=$replacement
    else
      mv -T -- "$staging" "$release"
    fi
    staging=
  fi
  rm -rf -- "$download"
  download=
fi

release_valid "$release"
activate_release "$release"
selected=$(current_release_path)
[[ $selected == "$release" ]]
release_valid "$selected"
reap_stale_paths "$selected"

cd "$selected/app/apps/host-selfhost"
exec env \
  BUN_FEATURE_FLAG_DISABLE_IPV6=1 \
  EXECUTOR_HOST=0.0.0.0 \
  PORT=4788 \
  EXECUTOR_DATA_DIR="$data" \
  EXECUTOR_ALLOW_LOCAL_NETWORK=false \
  EXECUTOR_WEB_BASE_URL="http://localhost:$host_port" \
  "$selected/usr/local/bin/bun" run src/serve.ts
