#!/usr/bin/env bash
# shellcheck disable=SC2016 # grep fixtures intentionally contain bootstrap literals.
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"

expected=$(cat <<'EOF'
PubkeyAuthentication yes
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitEmptyPasswords no
PermitRootLogin no
AllowUsers agent
AuthorizedKeysFile /etc/ssh/authorized_keys.d/%u
AllowAgentForwarding no
AllowTcpForwarding no
X11Forwarding no
PermitTunnel no
EOF
)

actual=$(awk '
  /^    content: \|$/ { in_content = 1; next }
  in_content && /^      / { sub(/^      /, ""); print; next }
  in_content { exit }
' guest/cloud-init.yaml)
[[ $actual == "$expected" ]] || {
  printf 'cloud-init SSH fixture does not match the reviewed sshd policy\n' >&2
  diff -u <(printf '%s\n' "$expected") <(printf '%s\n' "$actual") >&2 || true
  exit 1
}

grep -Fq -- '- path: /etc/ssh/sshd_config.d/00-hermes-box.conf' guest/cloud-init.yaml
grep -Fq 'owner: root:root' guest/cloud-init.yaml
grep -Fq 'permissions: "0644"' guest/cloud-init.yaml
if grep -Fq '.ssh/authorized_keys' guest/cloud-init.yaml; then
  printf 'sshd policy must not fall back to the persistent agent home\n' >&2
  exit 1
fi

required_line=$(grep -nF '"$provisioner/cloud-init.yaml"' guest/bootstrap.sh | head -1 | cut -d: -f1)
key_line=$(grep -nF 'install -D -o root -g root -m 0600' guest/bootstrap.sh | cut -d: -f1)
schema_line=$(grep -nF 'cloud-init schema -c "$provisioner/cloud-init.yaml"' guest/bootstrap.sh | cut -d: -f1)
apply_line=$(grep -nF 'cloud-init single --name write_files --frequency always' guest/bootstrap.sh | cut -d: -f1)
validate_line=$(grep -nF '/usr/sbin/sshd -t' guest/bootstrap.sh | cut -d: -f1)
reload_line=$(grep -nF 'systemctl reload ssh.service' guest/bootstrap.sh | cut -d: -f1)
remove_home_key_line=$(grep -nF 'rm -f /home/agent/.ssh/authorized_keys' guest/bootstrap.sh | cut -d: -f1)
bind_line=$(grep -nF 'mountpoint -q /home/agent || mount --bind' guest/bootstrap.sh | cut -d: -f1)
for line in "$required_line" "$key_line" "$schema_line" "$apply_line" "$validate_line" "$reload_line" "$remove_home_key_line" "$bind_line"; do
  [[ $line =~ ^[0-9]+$ ]] || {
    printf 'SSH provisioning wiring is incomplete\n' >&2
    exit 1
  }
done
((required_line < key_line && key_line < schema_line && schema_line < apply_line && apply_line < validate_line && validate_line < reload_line && reload_line < remove_home_key_line && remove_home_key_line < bind_line)) || {
  printf 'SSH policy must be required, applied, validated, and reloaded before the persistent-home bind\n' >&2
  exit 1
}

if [[ $(grep -Fc 'cloud-init.yaml hermes.service' release/build-provisioner.sh) -ne 2 ]]; then
  printf 'cloud-init must be copied and checksummed in the provisioner manifest\n' >&2
  exit 1
fi
grep -Fq './cloud-init.yaml ./hermes.service' release/build-provisioner.sh || {
  printf 'cloud-init must be required in the final provisioner archive\n' >&2
  exit 1
}
grep -Fq "authorizedkeysfile /etc/ssh/authorized_keys.d/%u" guest/bootstrap.sh

if command -v cloud-init >/dev/null 2>&1; then
  cloud-init schema -c guest/cloud-init.yaml
fi

printf 'SSH provisioning checks passed\n'
