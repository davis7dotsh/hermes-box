#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "$0")" && pwd)
release_root=''
repo_root=''
# shellcheck source=release/lib.sh
source "$script_dir/lib.sh"
require_linux_arm64
require_command apt-get dpkg-deb findmnt go jq zstd
[[ $EUID -eq 0 ]] || { printf 'build-provisioner.sh must run as root\n' >&2; exit 1; }
# The package closure is resolved on the pinned Ubuntu 26.04 ARM64 OCI child.
# Runtime qualification still boots the exact dated cloud image from the lock.
# shellcheck disable=SC1091
source /etc/os-release
[[ ${VERSION_ID:-} == "$UBUNTU_RELEASE" ]] || {
  printf 'provisioner must be resolved on Ubuntu %s, got %s\n' "$UBUNTU_RELEASE" "${VERSION_ID:-unknown}" >&2
  exit 1
}

output_dir=${1:-"${TMPDIR:-/tmp}/hermes-box-release"}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$output_dir" "$work/root/debs" "$work/apt/partial"

mapfile -t packages < <(sed -e '/^[[:space:]]*#/d' -e '/^[[:space:]]*$/d' "$release_root/provisioner-packages.in")
((${#packages[@]} > 0)) || { printf 'provisioner package list is empty\n' >&2; exit 1; }

"$release_root/configure-apt-snapshot.sh" "$work/apt-metadata.txt"
: >"$work/dpkg-status"
apt-get --assume-yes --download-only --no-install-recommends \
  -o "Dir::State::status=$work/dpkg-status" \
  -o "Dir::Cache::archives=$work/apt" install "${packages[@]}"
find "$work/apt" -maxdepth 1 -type f -name '*.deb' -exec cp {} "$work/root/debs/" \;
find "$work/root/debs" -type f -name '*.deb' -print -quit | grep -q . || {
  printf 'apt produced no provisioner packages\n' >&2
  exit 1
}

(
  cd "$repo_root"
  GOTOOLCHAIN="$GO_TOOLCHAIN" CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
    go build -trimpath -buildvcs=false -ldflags='-buildid=' \
    -o "$work/root/hermes-box-guest" ./cmd/hermes-box-guest
)
install -m 0755 "$repo_root/guest/bootstrap.sh" "$work/root/bootstrap-real.sh"
for file in cloud-init.yaml hermes.service executor.service hermes-box-recover.service hermes-box-recover hermes-box.sudoers tm tmux.conf xterm-ghostty.terminfo; do
  install -m 0644 "$repo_root/guest/$file" "$work/root/$file"
done
chmod 0755 "$work/root/hermes-box-recover" "$work/root/tm"

cat >"$work/root/bootstrap.sh" <<'BOOTSTRAP'
#!/usr/bin/env bash
set -euo pipefail
root=$(cd "$(dirname "$0")" && pwd)
mapfile -t candidates < <(findmnt -rn -t ext4 -o TARGET | awk '/^\/mnt\/lima-/')
((${#candidates[@]} == 1)) || {
  printf 'expected exactly one Lima ext4 data mount, found %d\n' "${#candidates[@]}" >&2
  exit 1
}
exec "$root/bootstrap-real.sh" "$root" "${candidates[0]}"
BOOTSTRAP
chmod 0755 "$work/root/bootstrap.sh"

{
  printf 'schema=1\nubuntu_release=%s\nubuntu_build=%s\ngo_toolchain=%s\n' \
    "$UBUNTU_RELEASE" "$UBUNTU_BUILD" "$GO_TOOLCHAIN"
  cat "$work/apt-metadata.txt"
  for asset in \
    hermes-box-guest bootstrap.sh bootstrap-real.sh cloud-init.yaml hermes.service executor.service \
    hermes-box-recover.service hermes-box-recover hermes-box.sudoers tm \
    tmux.conf xterm-ghostty.terminfo; do
    printf 'asset=%s\tsha256=%s\n' "$asset" "$(sha256_file "$work/root/$asset")"
  done
  for deb in "$work/root/debs"/*.deb; do
    printf 'deb=%s\tpackage=%s\tversion=%s\tsha256=%s\n' \
      "$(basename "$deb")" \
      "$(dpkg-deb --field "$deb" Package)" \
      "$(dpkg-deb --field "$deb" Version)" \
      "$(sha256_file "$deb")"
  done
} >"$work/root/manifest.txt"

artifact="$output_dir/hermes-box-provisioner-linux-arm64.tar.zst"
rm -f "$artifact"
deterministic_tar_zstd "$work/root" "$artifact"
archive_members=$work/archive-members.txt
tar --zstd -tf "$artifact" >"$archive_members"
for member in \
  ./manifest.txt ./hermes-box-guest ./bootstrap.sh ./bootstrap-real.sh \
  ./cloud-init.yaml ./hermes.service ./executor.service ./hermes-box-recover.service \
  ./hermes-box-recover ./hermes-box.sudoers ./tm ./tmux.conf \
  ./xterm-ghostty.terminfo; do
  grep -Fxq "$member" "$archive_members" || {
    printf 'provisioner archive is missing required member: %s\n' "$member" >&2
    exit 1
  }
done
verification_root=$work/archive-verification
mkdir -p "$verification_root"
tar --zstd -xf "$artifact" -C "$verification_root"
while IFS=$'\t' read -r identity field2 _ field4; do
  case $identity in
    asset=*)
      verify_sha256 "$verification_root/${identity#asset=}" "${field2#sha256=}"
      ;;
    deb=*)
      verify_sha256 "$verification_root/debs/${identity#deb=}" "${field4#sha256=}"
      ;;
  esac
done <"$verification_root/manifest.txt"
sha256_file "$artifact" >"$artifact.sha256"
printf '%s\n' "$artifact"
