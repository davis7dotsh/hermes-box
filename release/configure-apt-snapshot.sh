#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "$0")" && pwd)
release_root=''
repo_root=''
# shellcheck source=release/lib.sh
source "$script_dir/lib.sh"
require_linux_arm64
require_command apt-get awk chmod find gpgv mv sha256sum sort /usr/lib/apt/apt-helper
[[ $EUID -eq 0 ]] || {
  printf 'configure-apt-snapshot.sh must run as root\n' >&2
  exit 1
}

metadata_output=${1:-}
lists=/var/lib/apt/lists
ubuntu_sources=/etc/apt/sources.list.d/ubuntu.sources
[[ -f $ubuntu_sources ]] || {
  printf 'Ubuntu deb822 sources are missing: %s\n' "$ubuntu_sources" >&2
  exit 1
}

# The Ubuntu ARM64 container uses ports.ubuntu.com, which APT does not advertise
# as snapshot-capable. Point every Ubuntu deb822 stanza at Canonical's immutable
# snapshot URL directly instead of relying on APT's snapshot auto-discovery.
rewritten_sources=$(mktemp "${ubuntu_sources}.hermes-box.XXXXXX")
trap 'rm -f "$rewritten_sources"' EXIT
awk -v snapshot="$UBUNTU_APT_SNAPSHOT" '
  /^[[:space:]]*Snapshot:/ { next }
  /^[[:space:]]*URIs:/ {
    print "URIs: https://snapshot.ubuntu.com/ubuntu/" snapshot
    next
  }
  { print }
' "$ubuntu_sources" >"$rewritten_sources"
chmod --reference="$ubuntu_sources" "$rewritten_sources"
mv "$rewritten_sources" "$ubuntu_sources"
trap - EXIT

rm -rf "${lists:?}"/*
ready=false
for attempt in $(seq 1 30); do
  apt-get -o Acquire::https::Timeout=20 \
    -o Acquire::GzipIndexes=false update || true
  mapfile -t inreleases < <(find "$lists" -maxdepth 1 -type f -name '*InRelease' -print | sort)
  mapfile -t packages < <(find "$lists" -maxdepth 1 -type f -name '*Packages*' -print | sort)
  package_indexes_complete=true
  for suite in resolute resolute-updates resolute-backports resolute-security; do
    mapfile -t suite_inreleases < <(find "$lists" -maxdepth 1 -type f \
      -name "*_dists_${suite}_InRelease" -print)
    if ((${#suite_inreleases[@]} != 1)); then
      package_indexes_complete=false
      continue
    fi
    for component in main universe restricted multiverse; do
      mapfile -t component_packages < <(find "$lists" -maxdepth 1 -type f \
        -name "*_dists_${suite}_${component}_binary-arm64_Packages*" -print)
      expected_record=$(awk -v path="$component/binary-arm64/Packages" \
        '$3 == path && $1 ~ /^[a-f0-9]{64}$/ { print $1 ":" $2 }' "${suite_inreleases[0]}")
      IFS=: read -r expected expected_size <<<"$expected_record"
      if [[ $expected =~ ^[a-f0-9]{64}$ && $expected_size == 0 && \
        ${#component_packages[@]} == 0 ]]; then
        continue
      fi
      if ((${#component_packages[@]} != 1)); then
        package_indexes_complete=false
        continue
      fi
      if ! actual=$(/usr/lib/apt/apt-helper cat-file "${component_packages[0]}" | sha256sum); then
        package_indexes_complete=false
        continue
      fi
      actual=${actual%% *}
      if [[ ! $expected =~ ^[a-f0-9]{64}$ || ! $expected_size =~ ^[0-9]+$ || \
        $actual != "$expected" ]]; then
        package_indexes_complete=false
      fi
    done
  done
  if ((${#inreleases[@]} == 4 && ${#packages[@]} > 0)) && \
    [[ $package_indexes_complete == true ]]; then
    ready=true
    break
  fi
  printf 'snapshot verification attempt %d incomplete; retaining verified lists\n' \
    "$attempt" >&2
  sleep 15
done
[[ $ready == true ]] || {
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
  case $(basename "$inrelease") in
    *_dists_resolute_InRelease)
      expected=$UBUNTU_APT_RESOLUTE_INRELEASE_SHA256 ;;
    *_dists_resolute-updates_InRelease)
      expected=$UBUNTU_APT_RESOLUTE_UPDATES_INRELEASE_SHA256 ;;
    *_dists_resolute-backports_InRelease)
      expected=$UBUNTU_APT_RESOLUTE_BACKPORTS_INRELEASE_SHA256 ;;
    *_dists_resolute-security_InRelease)
      expected=$UBUNTU_APT_RESOLUTE_SECURITY_INRELEASE_SHA256 ;;
    *) printf 'unexpected Ubuntu InRelease: %s\n' "$inrelease" >&2; exit 1 ;;
  esac
  printf '%s  %s\n' "$expected" "$inrelease" | sha256sum --check >/dev/null
done
[[ ${#inreleases[@]} == 4 ]] || {
  printf 'expected four pinned Ubuntu InRelease indexes, found %s\n' \
    "${#inreleases[@]}" >&2
  exit 1
}

if [[ -n $metadata_output ]]; then
  {
    printf 'apt_snapshot=%s\n' "$UBUNTU_APT_SNAPSHOT"
    for index in "${inreleases[@]}" "${packages[@]}"; do
      printf 'apt_index=%s\tsha256=%s\n' \
        "$(basename "$index")" "$(sha256_file "$index")"
    done
  } >"$metadata_output"
fi
