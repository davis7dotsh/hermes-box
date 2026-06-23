#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "$0")" && pwd)
release_root=''
repo_root=''
# shellcheck source=release/lib.sh
source "$script_dir/lib.sh"
require_linux_arm64
require_command apt-get find gpgv sort
[[ $EUID -eq 0 ]] || {
  printf 'configure-apt-snapshot.sh must run as root\n' >&2
  exit 1
}

metadata_output=${1:-}
lists=/var/lib/apt/lists
rm -rf "${lists:?}"/*
apt-get -o "APT::Snapshot=$UBUNTU_APT_SNAPSHOT" update

mapfile -t inreleases < <(find "$lists" -maxdepth 1 -type f -name '*InRelease' -print | sort)
mapfile -t packages < <(find "$lists" -maxdepth 1 -type f -name '*Packages*' -print | sort)
((${#inreleases[@]} > 0 && ${#packages[@]} > 0)) || {
  printf 'Ubuntu snapshot produced no InRelease or Packages indexes\n' >&2
  exit 1
}

for index in "${inreleases[@]}" "${packages[@]}"; do
  case $(basename "$index") in
    *snapshot.ubuntu.com_ubuntu_"$UBUNTU_APT_SNAPSHOT"_*) ;;
    *) printf 'refusing non-snapshot APT index: %s\n' "$index" >&2; exit 1 ;;
  esac
done

keyring=/usr/share/keyrings/ubuntu-archive-keyring.gpg
[[ -f $keyring ]] || { printf 'Ubuntu archive keyring is missing\n' >&2; exit 1; }
for inrelease in "${inreleases[@]}"; do
  gpgv --keyring "$keyring" "$inrelease" >/dev/null
done

if [[ -n $metadata_output ]]; then
  {
    printf 'apt_snapshot=%s\n' "$UBUNTU_APT_SNAPSHOT"
    for index in "${inreleases[@]}" "${packages[@]}"; do
      printf 'apt_index=%s\tsha256=%s\n' \
        "$(basename "$index")" "$(sha256_file "$index")"
    done
  } >"$metadata_output"
fi
