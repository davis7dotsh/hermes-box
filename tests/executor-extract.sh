#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
temporary=$(mktemp -d "${TMPDIR:-/tmp}/hermes-box-executor-extract.XXXXXX")
trap 'rm -rf -- "$temporary"' EXIT

source_dir=$temporary/source
layer_one=$temporary/layer-one
layer_two=$temporary/layer-two
destination=$temporary/destination
mkdir -p \
  "$source_dir" \
  "$layer_one/app/opaque" \
  "$layer_one/usr/local/bin" \
  "$layer_two/app/opaque" \
  "$layer_two/usr/local/bin"

printf 'keep\n' >"$layer_one/app/keep"
printf 'remove\n' >"$layer_one/app/remove"
printf 'old\n' >"$layer_one/app/opaque/old"
printf 'old bun\n' >"$layer_one/usr/local/bin/bun"
: >"$layer_two/app/.wh.remove"
: >"$layer_two/app/opaque/.wh..wh..opq"
: >"$layer_two/usr/.wh.local"
printf 'new\n' >"$layer_two/app/opaque/new"
mkdir -p "$layer_two/app/apps/host-selfhost/src"
printf 'serve\n' >"$layer_two/app/apps/host-selfhost/src/serve.ts"
printf '#!/bin/sh\n' >"$layer_two/usr/local/bin/bun"
chmod 0755 "$layer_two/usr/local/bin/bun"

COPYFILE_DISABLE=1 tar -C "$layer_one" -czf "$temporary/layer-one.tar.gz" .
COPYFILE_DISABLE=1 tar -C "$layer_two" -czf "$temporary/layer-two.tar.gz" .
layer_one_digest=$(shasum -a 256 "$temporary/layer-one.tar.gz" | awk '{print $1}')
layer_two_digest=$(shasum -a 256 "$temporary/layer-two.tar.gz" | awk '{print $1}')
mv "$temporary/layer-one.tar.gz" "$source_dir/$layer_one_digest"
mv "$temporary/layer-two.tar.gz" "$source_dir/$layer_two_digest"

printf '{"os":"linux","architecture":"arm64"}\n' >"$temporary/config.json"
config_digest=$(shasum -a 256 "$temporary/config.json" | awk '{print $1}')
mv "$temporary/config.json" "$source_dir/$config_digest"

printf '%s\n' \
  '{' \
  '  "schemaVersion": 2,' \
  "  \"config\": {\"digest\": \"sha256:$config_digest\"}," \
  '  "layers": [' \
  "    {\"digest\": \"sha256:$layer_one_digest\"}," \
  "    {\"digest\": \"sha256:$layer_two_digest\"}" \
  '  ]' \
  '}' >"$source_dir/manifest.json"

manifest_digest=$(
  "$root/guest/extract-executor.py" "$source_dir" "$destination"
)
[[ $manifest_digest =~ ^sha256:[0-9a-f]{64}$ ]]
grep -Fqx keep "$destination/app/keep"
test ! -e "$destination/app/remove"
test ! -e "$destination/app/opaque/old"
grep -Fqx new "$destination/app/opaque/new"
test -x "$destination/usr/local/bin/bun"

unsafe_source=$temporary/unsafe-source
mkdir -p "$unsafe_source"
python3 - "$temporary/unsafe.tar.gz" <<'PY'
import io
import tarfile
import sys

with tarfile.open(sys.argv[1], "w:gz") as archive:
    content = b"escape\n"
    member = tarfile.TarInfo("../app/apps/host-selfhost/src/serve.ts")
    member.size = len(content)
    archive.addfile(member, io.BytesIO(content))
PY
unsafe_layer_digest=$(shasum -a 256 "$temporary/unsafe.tar.gz" | awk '{print $1}')
mv "$temporary/unsafe.tar.gz" "$unsafe_source/$unsafe_layer_digest"
cp "$source_dir/$config_digest" "$unsafe_source/$config_digest"
printf '%s\n' \
  '{' \
  '  "schemaVersion": 2,' \
  "  \"config\": {\"digest\": \"sha256:$config_digest\"}," \
  "  \"layers\": [{\"digest\": \"sha256:$unsafe_layer_digest\"}]" \
  '}' >"$unsafe_source/manifest.json"

unsafe_log=$temporary/unsafe.log
if "$root/guest/extract-executor.py" \
  "$unsafe_source" "$temporary/unsafe-destination" >/dev/null 2>"$unsafe_log"; then
  printf 'unsafe Executor layer was accepted\n' >&2
  exit 1
fi
grep -Fq 'unsafe OCI layer path' "$unsafe_log"
test ! -e "$temporary/app/apps/host-selfhost/src/serve.ts"

echo "Executor extraction checks passed"
