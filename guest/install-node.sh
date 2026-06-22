#!/usr/bin/env bash
set -euo pipefail

if [[ $EUID -ne 0 ]]; then
  echo "Node installer must run as root" >&2
  exit 1
fi

node_major=${1:-24}
if [[ ! $node_major =~ ^[0-9]+$ ]]; then
  echo "Node major version must be numeric" >&2
  exit 1
fi

case $(uname -m) in
  aarch64 | arm64)
    node_arch=arm64
    ;;
  *)
    echo "unsupported Node architecture: $(uname -m)" >&2
    exit 1
    ;;
esac

temporary_dir=$(mktemp -d /tmp/hermes-box-node.XXXXXX)
trap 'rm -rf -- "$temporary_dir"' EXIT

release_url=https://nodejs.org/dist/latest-v${node_major}.x
download_curl_args=(
  --proto '=https'
  --tlsv1.2
  -fsSL
  --connect-timeout 15
  --max-time 600
  --retry 5
  --retry-delay 2
  --retry-max-time 600
  --retry-all-errors
)
curl "${download_curl_args[@]}" "$release_url/SHASUMS256.txt" \
  -o "$temporary_dir/SHASUMS256.txt"

node_archive=$(
  awk -v major="$node_major" -v arch="$node_arch" '
    $2 ~ ("^node-v" major "\\.[0-9]+\\.[0-9]+-linux-" arch "\\.tar\\.xz$") {
      print $2
      exit
    }
  ' "$temporary_dir/SHASUMS256.txt"
)
if [[ -z $node_archive ]]; then
  echo "could not find the latest Node ${node_major} archive" >&2
  exit 1
fi

curl "${download_curl_args[@]}" "$release_url/$node_archive" \
  -o "$temporary_dir/$node_archive"
(
  cd "$temporary_dir"
  grep -F "  $node_archive" SHASUMS256.txt | sha256sum --check --strict -
)

install_dir=/usr/local/lib/nodejs/node-v${node_major}
rm -rf "$install_dir"
install -d -o root -g root -m 0755 "$install_dir"
tar -xJf "$temporary_dir/$node_archive" \
  --no-same-owner \
  --no-same-permissions \
  --strip-components=1 \
  -C "$install_dir"

# Even a checksum-verified upstream archive must not choose ownership or leave
# privileged/group-writable modes behind when it is unpacked by root.
chown -hR root:root "$install_dir"
find "$install_dir" -type d -exec chmod 0755 {} +
find "$install_dir" -type f -perm /0111 -exec chmod 0755 {} +
find "$install_dir" -type f ! -perm /0111 -exec chmod 0644 {} +

for command in node npm npx corepack; do
  if [[ -e $install_dir/bin/$command ]]; then
    ln -sfn "$install_dir/bin/$command" "/usr/local/bin/$command"
  fi
done

if id hermes >/dev/null 2>&1; then
  install -d -o hermes -g hermes -m 0750 /home/hermes/.local/bin
  for command in node npm npx corepack; do
    if [[ -e /usr/local/bin/$command ]]; then
      ln -sfn "/usr/local/bin/$command" "/home/hermes/.local/bin/$command"
      chown -h hermes:hermes "/home/hermes/.local/bin/$command"
    fi
  done
fi

node --version
npm --version
