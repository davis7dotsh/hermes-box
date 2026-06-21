#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
temporary=$(mktemp -d "${TMPDIR:-/tmp}/hermes-box-executor-runtime.XXXXXX")
trap 'rm -rf -- "$temporary"' EXIT

fail() {
  printf 'executor runtime test failed: %s\n' "$*" >&2
  exit 1
}

mock_bin=$temporary/bin
mkdir -p "$mock_bin"

cat >"$mock_bin/timeout" <<'MOCK_TIMEOUT'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$MOCK_TIMEOUT_LOG"
while (($# > 0)); do
  case $1 in
    --signal=* | --kill-after=*) shift ;;
    *)
      shift
      break
      ;;
  esac
done
exec "$@"
MOCK_TIMEOUT

cat >"$mock_bin/flock" <<'MOCK_FLOCK'
#!/usr/bin/env bash
set -euo pipefail
if [[ ${1:-} == -u ]]; then
  owner=$(<"$MOCK_FLOCK_DIR/owner")
  if [[ $owner != "$PPID" ]]; then
    printf 'mock flock owner mismatch: owner=%s caller=%s\n' "$owner" "$PPID" >&2
    exit 73
  fi
  rm "$MOCK_FLOCK_DIR/owner"
  rmdir "$MOCK_FLOCK_DIR"
  exit 0
fi
printf '%s\n' "$PPID" >>"$MOCK_FLOCK_LOG"
for _ in {1..6000}; do
  if mkdir "$MOCK_FLOCK_DIR" 2>/dev/null; then
    printf '%s\n' "$PPID" >"$MOCK_FLOCK_DIR/owner"
    exit 0
  fi
  sleep 0.01
done
printf 'mock flock timed out\n' >&2
exit 1
MOCK_FLOCK

cat >"$mock_bin/mv" <<'MOCK_MV'
#!/usr/bin/env bash
set -euo pipefail
case ${1:-} in
  -T | -Tf)
    shift
    if [[ ${1:-} == -- ]]; then
      shift
    fi
    (($# == 2)) || exit 64
    source=$1
    destination=$2
    [[ -n $destination ]] || exit 64
    rm -rf -- "$destination"
    exec /bin/mv "$source" "$destination"
    ;;
  *) exec /bin/mv "$@" ;;
esac
MOCK_MV

cat >"$mock_bin/skopeo" <<'MOCK_SKOPEO'
#!/usr/bin/env bash
set -euo pipefail
destination=${!#}
arguments=("$@")
case $destination in
  oci:*)
    cache=${destination#oci:}
    cache=${cache%:executor}
    mkdir -p "$cache/blobs/sha256"
    growth_file=$cache/oci-put-blob.mock-growing
    for ((tick = 0; tick < ${MOCK_SKOPEO_SILENT_GROWTH_TICKS:-0}; tick++)); do
      dd if=/dev/zero bs=8192 count=1 >>"$growth_file" 2>/dev/null
      sleep "${MOCK_SKOPEO_GROWTH_INTERVAL:-0.4}"
    done
    rm -f -- "$growth_file"
    if [[ ${MOCK_SKOPEO_NOISY_STALL:-false} == true ]]; then
      trap 'exit 143' TERM
      while true; do
        printf 'warning: registry retry made no byte progress\n' >&2
        sleep 0.1
      done
    fi
    if [[ ${MOCK_SKOPEO_FAIL_WITH_PARTIAL_BLOB:-false} == true ]]; then
      partial_blob=$(mktemp "$cache/oci-put-blob.mock.XXXXXX")
      printf 'partial blob bytes\n' >"$partial_blob"
      printf 'pull cache=partial %s\n' "${arguments[*]}" >>"$MOCK_SKOPEO_LOG"
      exit 42
    fi
    cache_state=cold
    if [[ -f $cache/blobs/sha256/mock-complete-blob ]]; then
      cache_state=warm
    else
      printf 'verified blob\n' >"$cache/blobs/sha256/mock-complete-blob"
    fi
    printf 'pull cache=%s %s\n' "$cache_state" "${arguments[*]}" >>"$MOCK_SKOPEO_LOG"
    if [[ ${MOCK_SKOPEO_FAIL_AFTER_COMPLETE_BLOB:-false} == true && $cache_state == cold ]]; then
      exit 42
    fi
    if [[ -n ${MOCK_SKOPEO_WAIT_UNTIL:-} ]]; then
      : >"$MOCK_SKOPEO_WAIT_UNTIL.started"
      for _ in {1..1000}; do
        [[ -e $MOCK_SKOPEO_WAIT_UNTIL ]] && break
        sleep 0.01
      done
      [[ -e $MOCK_SKOPEO_WAIT_UNTIL ]] || exit 124
    fi
    printf '{}\n' >"$cache/index.json"
    ;;
  dir:*)
    directory=${destination#dir:}
    printf 'materialize %s\n' "${arguments[*]}" >>"$MOCK_SKOPEO_LOG"
    mkdir -p "$directory"
    printf '{}\n' >"$directory/manifest.json"
    ;;
  *) exit 64 ;;
esac
MOCK_SKOPEO

cat >"$mock_bin/bun-payload" <<'MOCK_BUN'
#!/usr/bin/env bash
set -euo pipefail
printf '%s|ipv6=%s|host=%s|port=%s\n' \
  "$*" \
  "${BUN_FEATURE_FLAG_DISABLE_IPV6:-}" \
  "${EXECUTOR_HOST:-}" \
  "${PORT:-}" >>"$MOCK_BUN_LOG"

release_service_resources() {
  if [[ -n ${MOCK_BUN_BIND_DIR:-} ]]; then
    rmdir "$MOCK_BUN_BIND_DIR" 2>/dev/null || true
  fi
  if [[ -f ${MOCK_FLOCK_DIR:-}/owner ]]; then
    lock_owner=$(<"$MOCK_FLOCK_DIR/owner")
    if [[ $lock_owner == "$$" ]]; then
      rm "$MOCK_FLOCK_DIR/owner"
      rmdir "$MOCK_FLOCK_DIR"
    fi
  fi
}

if [[ ${1:-} == build ]]; then
  entrypoint=${2:-}
  runtime_real=$(cd "$HERMES_BOX_EXECUTOR_RUNTIME_ROOT" && pwd -P)
  [[ $entrypoint == \
    "$runtime_real"/releases/*/app/apps/host-selfhost/src/serve.ts ]] || {
    printf 'unexpected Bun build entrypoint: %s\n' "$entrypoint" >&2
    exit 71
  }
  validation_root=${EXECUTOR_DATA_DIR%/data}
  validation_real=$(cd "$validation_root" && pwd -P)
  [[ $(pwd -P) == "$validation_real" ]] || {
    printf 'Bun dependency probe ran outside isolated state: %s\n' "$PWD" >&2
    exit 71
  }
  [[ $EXECUTOR_DATA_DIR == \
    "$HERMES_BOX_EXECUTOR_RUNTIME_ROOT"/.validation-*/data ]] || {
    printf 'Bun dependency probe used non-isolated data: %s\n' \
      "${EXECUTOR_DATA_DIR:-unset}" >&2
    exit 71
  }
  [[ $EXECUTOR_DATA_DIR != "$HERMES_BOX_EXECUTOR_DATA_DIR" ]] || {
    printf 'Bun dependency probe used durable Executor data\n' >&2
    exit 71
  }
  [[ $EXECUTOR_DB_PATH == "$validation_root/data/data.db" ]] || {
    printf 'Bun dependency probe used non-isolated database: %s\n' \
      "${EXECUTOR_DB_PATH:-unset}" >&2
    exit 71
  }
  [[ $HOME == "$validation_root/home" && \
    $XDG_CONFIG_HOME == "$validation_root/config" && \
    $XDG_DATA_HOME == "$validation_root/share" && \
    $XDG_CACHE_HOME == "$validation_root/cache" && \
    $TMPDIR == "$validation_root/tmp" ]] || {
    printf 'Bun dependency probe did not isolate home and XDG state\n' >&2
    exit 71
  }
  [[ $EXECUTOR_SECRET_KEY == hermes-box-validation-only-secret-key && \
    $BETTER_AUTH_SECRET == hermes-box-validation-only-auth-secret ]] || {
    printf 'Bun dependency probe did not use deterministic validation secrets\n' >&2
    exit 71
  }
  outdir=
  for argument in "$@"; do
    case $argument in
      --outdir=*) outdir=${argument#--outdir=} ;;
    esac
  done
  [[ $* == *'--target=bun'* && $outdir == "$validation_root/build" ]] || {
    printf 'Bun dependency probe did not isolate build output\n' >&2
    exit 71
  }
  printf 'validation side effect\n' >"$EXECUTOR_DATA_DIR/import-probe"
  printf 'validation side effect\n' >"$XDG_CONFIG_HOME/import-probe"
  printf 'validation side effect\n' >"$HOME/import-probe"
  host_selfhost=${entrypoint%/src/serve.ts}
  for required in \
    "$entrypoint" \
    "$host_selfhost/src/app.ts" \
    "$host_selfhost/src/config.ts" \
    "$host_selfhost/src/mcp/org-path.ts" \
    "$host_selfhost/../../node_modules/@effect/platform-bun/package.json" \
    "$host_selfhost/../../node_modules/.bun/effect@4.0.0-beta.59/node_modules/effect/package.json"; do
    if [[ ! -s $required ]]; then
      printf 'unable to resolve imported module: %s\n' "$required" >&2
      exit 71
    fi
  done
  mkdir -p "$outdir"
  printf 'mock bundle\n' >"$outdir/serve.js"
elif [[ ${1:-} == run ]]; then
  [[ -f ${MOCK_FLOCK_DIR:-}/owner ]] || {
    printf 'service started without the launcher lock\n' >&2
    exit 70
  }
  lock_owner=$(<"$MOCK_FLOCK_DIR/owner")
  if [[ $lock_owner != "$$" ]]; then
    printf 'service does not own launcher lock: owner=%s service=%s\n' \
      "$lock_owner" "$$" >&2
    exit 70
  fi
  trap release_service_resources EXIT
  if [[ -n ${MOCK_BUN_BIND_DIR:-} ]] && ! mkdir "$MOCK_BUN_BIND_DIR"; then
    printf 'Executor port is already bound\n' >&2
    exit 72
  fi
  if [[ -n ${MOCK_BUN_RUN_WAIT_UNTIL:-} ]]; then
    : >"$MOCK_BUN_RUN_WAIT_UNTIL.started"
    for _ in {1..1000}; do
      [[ -e $MOCK_BUN_RUN_WAIT_UNTIL ]] && break
      sleep 0.01
    done
    [[ -e $MOCK_BUN_RUN_WAIT_UNTIL ]] || exit 124
  fi
elif [[ ${1:-} == --version ]]; then
  printf '1.3.14\n'
fi
MOCK_BUN

cat >"$mock_bin/extractor" <<'MOCK_EXTRACTOR'
#!/usr/bin/env bash
set -euo pipefail
destination=$2
mkdir -p \
  "$destination/app" \
  "$destination/usr/local/bin" \
  "$destination/app/apps/host-selfhost/src" \
  "$destination/app/apps/host-selfhost/src/mcp" \
  "$destination/app/apps/host-selfhost/dist" \
  "$destination/app/node_modules/.bun/effect@4.0.0-beta.59/node_modules/effect" \
  "$destination/app/node_modules/@effect/platform-bun"
cp "$MOCK_BUN_TEMPLATE" "$destination/usr/local/bin/bun"
chmod 0755 "$destination/usr/local/bin/bun"
printf '{"name":"executor-workspace"}\n' >"$destination/app/package.json"
printf 'mock lockfile\n' >"$destination/app/bun.lock"
printf '%s\n' \
  '{"name":"@executor-js/host-selfhost","scripts":{"start":"bun run src/serve.ts"}}' \
  >"$destination/app/apps/host-selfhost/package.json"
cat >"$destination/app/apps/host-selfhost/src/serve.ts" <<'SERVE'
import "./app";
import "./config";
import "./mcp/org-path";
import "@effect/platform-bun";
SERVE
printf 'await Bun.write("./import-probe", "must not execute");\n' \
  >>"$destination/app/apps/host-selfhost/src/serve.ts"
printf 'await Bun.write("%s/import-probe", "must not execute");\n' \
  "$HERMES_BOX_EXECUTOR_DATA_DIR" \
  >>"$destination/app/apps/host-selfhost/src/serve.ts"
printf 'export const generation = %q;\n' "$MOCK_GENERATION" \
  >>"$destination/app/apps/host-selfhost/src/serve.ts"
printf 'export {};\n' >"$destination/app/apps/host-selfhost/src/app.ts"
printf 'export {};\n' >"$destination/app/apps/host-selfhost/src/config.ts"
printf 'export {};\n' >"$destination/app/apps/host-selfhost/src/mcp/org-path.ts"
printf '<!doctype html><title>Executor</title>\n' \
  >"$destination/app/apps/host-selfhost/dist/index.html"
printf '{"name":"effect"}\n' \
  >"$destination/app/node_modules/.bun/effect@4.0.0-beta.59/node_modules/effect/package.json"
if [[ ${MOCK_EXTRACTOR_OMIT_IMPORTED_MODULE:-false} != true ]]; then
  printf '{"name":"@effect/platform-bun"}\n' \
    >"$destination/app/node_modules/@effect/platform-bun/package.json"
fi
printf '%s\n' "$MOCK_GENERATION" >"$destination/.payload-generation"
printf 'sha256:%s\n' "$MOCK_MANIFEST_HEX"
MOCK_EXTRACTOR

chmod 0755 "$mock_bin"/*
export PATH="$mock_bin:$PATH"
export MOCK_BUN_TEMPLATE=$mock_bin/bun-payload
export MOCK_SKOPEO_LOG=$temporary/skopeo.log
export MOCK_BUN_LOG=$temporary/bun.log
export MOCK_TIMEOUT_LOG=$temporary/timeout.log
export MOCK_FLOCK_LOG=$temporary/flock.log

source_digest=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
source_repository=registry.example/team/executor
source_image=$source_repository@$source_digest
image_v1=$source_repository:v1@$source_digest
image_v2=$source_repository:stable@$source_digest
manifest_a=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
manifest_b=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
manifest_c=dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd

new_case() {
  case_root=$(mktemp -d "$temporary/case.XXXXXX")
  runtime_root=$case_root/runtime
  data_root=$case_root/data
  mkdir -p "$runtime_root/releases" "$data_root"
  : >"$MOCK_SKOPEO_LOG"
  : >"$MOCK_BUN_LOG"
  : >"$MOCK_TIMEOUT_LOG"
  : >"$MOCK_FLOCK_LOG"
  unset \
    MOCK_BUN_BIND_DIR \
    MOCK_BUN_RUN_WAIT_UNTIL \
    MOCK_EXTRACTOR_OMIT_IMPORTED_MODULE \
    MOCK_SKOPEO_FAIL_AFTER_COMPLETE_BLOB \
    MOCK_SKOPEO_FAIL_WITH_PARTIAL_BLOB \
    MOCK_SKOPEO_GROWTH_INTERVAL \
    MOCK_SKOPEO_NOISY_STALL \
    MOCK_SKOPEO_SILENT_GROWTH_TICKS \
    MOCK_SKOPEO_WAIT_UNTIL
  export MOCK_FLOCK_DIR=$case_root/flock-held
  export HERMES_BOX_EXECUTOR_IMAGE=$image_v1
  export HERMES_BOX_EXECUTOR_HOST_PORT=4788
  export HERMES_BOX_EXECUTOR_RUNTIME_ROOT=$runtime_root
  export HERMES_BOX_EXECUTOR_DATA_DIR=$data_root
  export HERMES_BOX_EXECUTOR_EXTRACTOR=$mock_bin/extractor
  export HERMES_BOX_EXECUTOR_PULL_STALL_SECONDS=1
  export MOCK_MANIFEST_HEX=$manifest_a
  export MOCK_GENERATION=downloaded
}

make_release() {
  local manifest=$1
  local reference=$2
  local release=$runtime_root/releases/$manifest

  mkdir -p \
    "$release/app" \
    "$release/usr/local/bin" \
    "$release/app/apps/host-selfhost/src" \
    "$release/app/apps/host-selfhost/src/mcp" \
    "$release/app/apps/host-selfhost/dist" \
    "$release/app/node_modules/.bun/effect@4.0.0-beta.59/node_modules/effect" \
    "$release/app/node_modules/@effect/platform-bun"
  cp "$MOCK_BUN_TEMPLATE" "$release/usr/local/bin/bun"
  chmod 0755 "$release/usr/local/bin/bun"
  printf '{"name":"executor-workspace"}\n' >"$release/app/package.json"
  printf 'mock lockfile\n' >"$release/app/bun.lock"
  printf '%s\n' \
    '{"name":"@executor-js/host-selfhost","scripts":{"start":"bun run src/serve.ts"}}' \
    >"$release/app/apps/host-selfhost/package.json"
  cat >"$release/app/apps/host-selfhost/src/serve.ts" <<'SERVE'
import "./app";
import "./config";
import "./mcp/org-path";
import "@effect/platform-bun";
export const cached = true;
SERVE
  printf 'await Bun.write("./import-probe", "must not execute");\n' \
    >>"$release/app/apps/host-selfhost/src/serve.ts"
  printf 'await Bun.write("%s/import-probe", "must not execute");\n' \
    "$HERMES_BOX_EXECUTOR_DATA_DIR" \
    >>"$release/app/apps/host-selfhost/src/serve.ts"
  printf 'export {};\n' >"$release/app/apps/host-selfhost/src/app.ts"
  printf 'export {};\n' >"$release/app/apps/host-selfhost/src/config.ts"
  printf 'export {};\n' >"$release/app/apps/host-selfhost/src/mcp/org-path.ts"
  printf '<!doctype html><title>Executor</title>\n' \
    >"$release/app/apps/host-selfhost/dist/index.html"
  printf '{"name":"effect"}\n' \
    >"$release/app/node_modules/.bun/effect@4.0.0-beta.59/node_modules/effect/package.json"
  printf '{"name":"@effect/platform-bun"}\n' \
    >"$release/app/node_modules/@effect/platform-bun/package.json"
  printf '%s\n' "$reference" >"$release/.image-reference"
  printf '%s\n' "$source_image" >"$release/.repository-digest"
  printf 'sha256:%s\n' "$manifest" >"$release/.manifest-digest"
}

run_launcher() {
  bash "$root/guest/executor.sh" >/dev/null
}

wait_for_file() {
  local path=$1
  local attempt

  for ((attempt = 0; attempt < 1000; attempt++)); do
    [[ -e $path ]] && return 0
    sleep 0.01
  done
  fail "timed out waiting for $path"
}

wait_for_lines() {
  local path=$1
  local expected=$2
  local attempt count

  for ((attempt = 0; attempt < 1000; attempt++)); do
    count=$(wc -l <"$path")
    ((count >= expected)) && return 0
    sleep 0.01
  done
  fail "timed out waiting for $expected lines in $path"
}

assert_validation_isolated() {
  if compgen -G "$runtime_root/.validation-*" >/dev/null; then
    fail "Bun dependency validation left temporary state"
  fi
  if find "$runtime_root/releases" "$data_root" \
    -name import-probe -print -quit | grep -q .; then
    fail "Bun dependency validation mutated a release or durable data"
  fi
  if find "$runtime_root/releases" \
    -path '*/.executor-selfhost/*' -print -quit | grep -q .; then
    fail "Bun dependency validation generated state inside a release"
  fi
}

new_case
make_release "$manifest_a" "$image_v1"
ln -s "releases/$manifest_a" "$runtime_root/current"
run_launcher
[[ ! -s $MOCK_SKOPEO_LOG ]] || fail "warm cache performed an image download"
grep -Fq 'run src/serve.ts|ipv6=1|host=0.0.0.0|port=4788' "$MOCK_BUN_LOG" ||
  fail "warm start did not preserve the Executor runtime safeguards"
[[ ! -e $runtime_root/current/app/node_modules/effect && \
  ! -L $runtime_root/current/app/node_modules/effect ]] ||
  fail "test release unexpectedly provided the false root Effect sentinel"
[[ -s \
  $runtime_root/current/app/node_modules/.bun/effect@4.0.0-beta.59/node_modules/effect/package.json ]] ||
  fail "test release does not match the published Bun package layout"
assert_validation_isolated

: >"$MOCK_BUN_LOG"
rm "$runtime_root/current/.repository-digest"
export HERMES_BOX_EXECUTOR_IMAGE=$image_v2
run_launcher
[[ ! -s $MOCK_SKOPEO_LOG ]] || fail "tag-only image change performed a download"
[[ $(<"$runtime_root/current/.image-reference") == "$image_v2" ]] ||
  fail "tag-only reuse did not retain the human image reference"
[[ $(<"$runtime_root/current/.repository-digest") == "$source_image" ]] ||
  fail "legacy cache did not repair canonical repository digest metadata"

rm "$runtime_root/current/.manifest-digest"
run_launcher
[[ ! -s $MOCK_SKOPEO_LOG ]] || fail "missing manifest marker performed a download"
[[ $(<"$runtime_root/current/.manifest-digest") == "sha256:$manifest_a" ]] ||
  fail "missing manifest marker was not repaired"

new_case
make_release "$manifest_a" "$image_v1"
printf '#!/usr/bin/env bash\nexit 1\n' >"$runtime_root/releases/$manifest_a/usr/local/bin/bun"
ln -s "releases/$manifest_a" "$runtime_root/current"
export MOCK_GENERATION=replacement
run_launcher
[[ -s $MOCK_SKOPEO_LOG ]] || fail "corrupt Bun did not trigger a verified download"
[[ $(<"$runtime_root/current/.payload-generation") == replacement ]] ||
  fail "corrupt same-digest release discarded the verified staging payload"
[[ -f $runtime_root/current/app/apps/host-selfhost/src/serve.ts ]] ||
  fail "replacement release is incomplete"
[[ $(readlink "$runtime_root/current") == "releases/$manifest_a-replacement-"* ]] ||
  fail "corrupt release was not replaced through an atomic activation target"
[[ ! -e $runtime_root/releases/$manifest_a ]] ||
  fail "obsolete corrupt release survived successful activation"
if grep -Fq '30m skopeo copy --override-os linux --override-arch arm64' \
  "$MOCK_TIMEOUT_LOG"; then
  fail "active image pulls still have a destructive total deadline"
fi
grep -Fq '5m skopeo copy --override-os linux --override-arch arm64' "$MOCK_TIMEOUT_LOG" ||
  fail "cached OCI materialization is not independently bounded"
grep -Fq '30s env BUN_FEATURE_FLAG_DISABLE_IPV6=1' "$MOCK_TIMEOUT_LOG" ||
  fail "Bun dependency validation is not bounded"
grep -Fq -- 'build ' "$MOCK_BUN_LOG" ||
  fail "release validation did not build the real serve.ts import graph"
grep -Fq -- '--target=bun' "$MOCK_BUN_LOG" ||
  fail "release validation did not target the bundled Bun runtime"
assert_validation_isolated
: >"$MOCK_SKOPEO_LOG"
run_launcher
[[ ! -s $MOCK_SKOPEO_LOG ]] || fail "verified replacement was not reusable on warm start"

new_case
make_release "$manifest_a" "$image_v1"
rm "$runtime_root/releases/$manifest_a/app/apps/host-selfhost/src/mcp/org-path.ts"
ln -s "releases/$manifest_a" "$runtime_root/current"
export MOCK_GENERATION=contract-replacement
run_launcher 2>/dev/null
[[ -s $MOCK_SKOPEO_LOG ]] || fail "missing imported dependency did not trigger a download"
[[ $(<"$runtime_root/current/.payload-generation") == contract-replacement ]] ||
  fail "runtime with a missing imported dependency was not replaced"
[[ -s $runtime_root/current/app/apps/host-selfhost/src/mcp/org-path.ts ]] ||
  fail "replacement runtime did not restore the unresolved serve.ts import"

new_case
export MOCK_EXTRACTOR_OMIT_IMPORTED_MODULE=true
if run_launcher 2>/dev/null; then
  fail "cold payload with an unresolved serve.ts dependency unexpectedly activated"
fi
[[ ! -e $runtime_root/current && ! -L $runtime_root/current ]] ||
  fail "cold payload activated before Bun dependency validation"

new_case
make_release "$manifest_b" "$image_v1"
mkdir -p \
  "$runtime_root/.download-stale" \
  "$runtime_root/.current-stale" \
  "$runtime_root/.oci-cache-stale" \
  "$runtime_root/.validation-stale" \
  "$runtime_root/releases/.staging-stale" \
  "$runtime_root/releases/.obsolete-stale" \
  "$runtime_root/releases/$manifest_c"
ln -s "releases/$manifest_a" "$runtime_root/current"
run_launcher
[[ ! -s $MOCK_SKOPEO_LOG ]] || fail "complete orphaned release was not reused"
[[ $(readlink "$runtime_root/current") == "releases/$manifest_b" ]] ||
  fail "orphaned release was not activated"
for stale in \
  "$runtime_root/.download-stale" \
  "$runtime_root/.current-stale" \
  "$runtime_root/.oci-cache-stale" \
  "$runtime_root/.validation-stale" \
  "$runtime_root/releases/.staging-stale" \
  "$runtime_root/releases/.obsolete-stale" \
  "$runtime_root/releases/$manifest_c"; do
  [[ ! -e $stale && ! -L $stale ]] || fail "stale path survived activation: $stale"
done
[[ -x $runtime_root/current/usr/local/bin/bun ]] || fail "cleanup deleted the active release"

new_case
export MOCK_SKOPEO_FAIL_AFTER_COMPLETE_BLOB=true
if run_launcher 2>/dev/null; then
  fail "simulated failure after a completed blob unexpectedly succeeded"
fi
oci_cache=$runtime_root/.oci-cache-${source_digest#sha256:}
[[ -f $oci_cache/blobs/sha256/mock-complete-blob ]] ||
  fail "interrupted pull discarded the OCI blob cache"
if compgen -G "$runtime_root/.download-*" >/dev/null; then
  fail "interrupted pull left a per-process download directory"
fi
run_launcher
[[ $(grep -c '^pull ' "$MOCK_SKOPEO_LOG") -eq 2 ]] ||
  fail "interrupted pull did not perform exactly one bounded retry"
grep -Fq 'pull cache=warm' "$MOCK_SKOPEO_LOG" ||
  fail "bounded retry did not reuse the completed blob"
[[ ! -e $oci_cache ]] ||
  fail "successful activation retained the repullable OCI cache"

new_case
export MOCK_SKOPEO_FAIL_WITH_PARTIAL_BLOB=true
if run_launcher 2>/dev/null; then
  fail "simulated mid-blob interruption unexpectedly succeeded"
fi
oci_cache=$runtime_root/.oci-cache-${source_digest#sha256:}
compgen -G "$oci_cache/oci-put-blob.mock.*" >/dev/null ||
  fail "mid-blob interruption did not exercise a partial OCI temporary"
[[ ! -e $oci_cache/blobs/sha256/mock-complete-blob ]] ||
  fail "partial OCI temporary was treated as a completed blob"
if run_launcher 2>/dev/null; then
  fail "second simulated mid-blob interruption unexpectedly succeeded"
fi
partial_count=$(find "$oci_cache" -maxdepth 1 -type f -name 'oci-put-blob.mock.*' | wc -l)
[[ $partial_count -eq 1 ]] ||
  fail "repeated mid-blob failures accumulated $partial_count transport temporaries"
unset MOCK_SKOPEO_FAIL_WITH_PARTIAL_BLOB
run_launcher
[[ $(grep -c 'pull cache=partial' "$MOCK_SKOPEO_LOG") -eq 2 ]] ||
  fail "repeated mid-blob interruptions were not recorded"
grep -Fq 'pull cache=cold' "$MOCK_SKOPEO_LOG" ||
  fail "mid-blob retry did not correctly restart the incomplete blob"
if grep -Fq 'pull cache=warm' "$MOCK_SKOPEO_LOG"; then
  fail "mid-blob retry incorrectly claimed resumability"
fi
[[ ! -e $oci_cache ]] ||
  fail "successful mid-blob retry retained the disposable OCI cache"

new_case
export MOCK_SKOPEO_SILENT_GROWTH_TICKS=4
export MOCK_SKOPEO_GROWTH_INTERVAL=0.4
run_launcher 2>/dev/null
[[ -L $runtime_root/current ]] ||
  fail "silent byte-growing pull was terminated despite real OCI progress"
[[ $(grep -c '^pull ' "$MOCK_SKOPEO_LOG") -eq 1 ]] ||
  fail "silent byte-growing pull did not complete in one attempt"

new_case
export MOCK_SKOPEO_NOISY_STALL=true
if run_launcher 2>/dev/null; then
  fail "noisy zero-byte stalled pull unexpectedly succeeded"
fi
[[ ! -e $runtime_root/current && ! -L $runtime_root/current ]] ||
  fail "noisy zero-byte stalled pull activated an incomplete release"

new_case
pull_gate=$case_root/allow-pull
service_gate=$case_root/allow-service-exit
export MOCK_SKOPEO_WAIT_UNTIL=$pull_gate
export MOCK_BUN_RUN_WAIT_UNTIL=$service_gate
export MOCK_BUN_BIND_DIR=$case_root/executor-port
bash "$root/guest/executor.sh" >"$case_root/first.out" 2>"$case_root/first.err" &
first_pid=$!
wait_for_file "$pull_gate.started"
bash "$root/guest/executor.sh" >"$case_root/second.out" 2>"$case_root/second.err" &
second_pid=$!
wait_for_lines "$MOCK_FLOCK_LOG" 2
[[ $(grep -c '^pull ' "$MOCK_SKOPEO_LOG") -eq 1 ]] ||
  fail "overlapping launchers entered the pull concurrently"
: >"$pull_gate"
wait_for_file "$service_gate.started"
sleep 0.1
[[ $(grep -c '^run src/serve.ts' "$MOCK_BUN_LOG") -eq 1 ]] ||
  fail "waiting launcher started a second Executor service"
[[ -d $MOCK_BUN_BIND_DIR ]] ||
  fail "long-lived Executor mock did not retain its exclusive bind"
kill -0 "$second_pid" 2>/dev/null ||
  fail "waiting launcher exited instead of waiting for the service lock"
: >"$service_gate"
wait "$first_pid" || fail "first overlapping launcher failed"
wait "$second_pid" || fail "second overlapping launcher failed"
[[ $(grep -c '^pull ' "$MOCK_SKOPEO_LOG") -eq 1 ]] ||
  fail "waiting launcher repulled instead of reusing the activated release"
[[ $(grep -c '^run src/serve.ts' "$MOCK_BUN_LOG") -eq 2 ]] ||
  fail "waiting launcher did not start after the first service exited"
[[ -L $runtime_root/current ]] || fail "overlapping launchers did not activate a release"
[[ ! -d $MOCK_BUN_BIND_DIR ]] || fail "Executor mock left its bind after exit"
[[ ! -d $MOCK_FLOCK_DIR ]] || fail "launcher lock remained held after service exit"

printf 'executor runtime checks passed\n'
