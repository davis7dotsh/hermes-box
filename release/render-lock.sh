#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "$0")" && pwd)
release_root=''
repo_root=''
# shellcheck source=release/lib.sh
source "$script_dir/lib.sh"
require_command go sed

artifact_dir=${1:?usage: render-lock.sh ARTIFACT_DIR [OUTPUT] [ASSET_RELEASE]}
output=${2:-"$artifact_dir/hermes-box.lock"}
asset_release=${3:-$ASSET_RELEASE}
case $asset_release in
  "$ASSET_RELEASE" | "$BASELINE_ASSET_RELEASE") ;;
  *) printf 'unsupported immutable asset release: %s\n' "$asset_release" >&2; exit 1 ;;
esac
provisioner="$artifact_dir/hermes-box-provisioner-linux-arm64.tar.zst"
hermes_source="$artifact_dir/hermes-agent-gated-$HERMES_VERSION.tar.zst"
hermes_wheels="$artifact_dir/hermes-wheels-cp313-linux-arm64.tar.zst"
for artifact in "$provisioner" "$hermes_source" "$hermes_wheels"; do
  [[ -f $artifact ]] || { printf 'missing release artifact: %s\n' "$artifact" >&2; exit 1; }
done

provisioner_sha=$(sha256_file "$provisioner")
source_sha=$(sha256_file "$hermes_source")
wheels_sha=$(sha256_file "$hermes_wheels")
sed \
  -e "s/__ASSET_RELEASE__/$asset_release/g" \
  -e "s/__PROVISIONER_SHA256__/$provisioner_sha/" \
  -e "s/__HERMES_SOURCE_SHA256__/$source_sha/" \
  -e "s/__HERMES_WHEELS_SHA256__/$wheels_sha/" \
  "$release_root/qualification.lock.template" >"$output"
if grep -q '__[A-Z0-9_]*__' "$output"; then
  printf 'qualification lock still contains unresolved placeholders\n' >&2
  exit 1
fi
GOTOOLCHAIN="$GO_TOOLCHAIN" go run "$release_root/validate-lock.go" "$output"
printf '%s\n' "$output"
