#!/usr/bin/env bash
set -euo pipefail

image=${HERMES_BOX_EXECUTOR_IMAGE:?Executor image is required}
host_port=${HERMES_BOX_EXECUTOR_HOST_PORT:?Executor host port is required}
digest=${image##*@}
repository_with_tag=${image%@*}
repository=$repository_with_tag
last_component=${repository_with_tag##*/}
if [[ $last_component == *:* ]]; then
  repository=${repository_with_tag%:*}
fi
source_image=$repository@$digest
runtime_root=/workspace/.hermes-box-runtime/executor
releases=$runtime_root/releases
current=$runtime_root/current
data=/workspace/executor/data

if [[ ! $host_port =~ ^[0-9]+$ ]] || ((10#$host_port < 1 || 10#$host_port > 65535)); then
  printf 'invalid Executor host port: %s\n' "$host_port" >&2
  exit 1
fi

cleanup() {
  for path in "${download:-}" "${staging:-}" "${next_link:-}"; do
    if [[ -n $path ]]; then
      rm -rf -- "$path"
    fi
  done
}
trap cleanup EXIT

install -d -m 0700 "$runtime_root" "$releases" "$data"

installed_image=
if [[ -f $current/.image-reference ]]; then
  installed_image=$(cat "$current/.image-reference")
fi

if [[ $installed_image != "$image" ||
  ! -x $current/usr/local/bin/bun ||
  ! -f $current/app/apps/host-selfhost/src/serve.ts ]]; then
  download=$runtime_root/.download-$$
  staging=$releases/.staging-$$
  rm -rf -- "$download" "$staging"

  skopeo copy \
    --override-os linux \
    --override-arch arm64 \
    --retry-times 3 \
    --preserve-digests \
    "docker://$source_image" \
    "dir:$download"

  manifest_digest=$(
    /usr/local/sbin/hermes-box-extract-executor "$download" "$staging"
  )
  release=$releases/${manifest_digest#sha256:}
  printf '%s\n' "$image" >"$staging/.image-reference"
  printf '%s\n' "$manifest_digest" >"$staging/.manifest-digest"
  chmod 0600 "$staging/.image-reference" "$staging/.manifest-digest"

  if [[ -d $release ]]; then
    install -m 0600 "$staging/.image-reference" "$release/.image-reference"
    install -m 0600 "$staging/.manifest-digest" "$release/.manifest-digest"
    rm -rf -- "$staging"
  else
    mv "$staging" "$release"
  fi

  next_link=$runtime_root/.current-$$
  ln -s "releases/${manifest_digest#sha256:}" "$next_link"
  mv -Tf "$next_link" "$current"
  rm -rf -- "$download"
  download=
  staging=
  next_link=
fi

test "$(cat "$current/.image-reference")" = "$image"
test -s "$current/.manifest-digest"
"$current/usr/local/bin/bun" --version

cd "$current/app/apps/host-selfhost"
exec env \
  BUN_FEATURE_FLAG_DISABLE_IPV6=1 \
  EXECUTOR_HOST=0.0.0.0 \
  PORT=4788 \
  EXECUTOR_DATA_DIR="$data" \
  EXECUTOR_ALLOW_LOCAL_NETWORK=false \
  EXECUTOR_WEB_BASE_URL="http://localhost:$host_port" \
  "$current/usr/local/bin/bun" run src/serve.ts
