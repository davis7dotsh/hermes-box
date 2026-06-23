#!/usr/bin/env bash
# shellcheck disable=SC2016 # grep patterns intentionally contain workflow literals.
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"

shell_files=(bin/hermes-box guest/bootstrap.sh guest/hermes-box-recover guest/tm)
while IFS= read -r path; do
  shell_files+=("$path")
done < <(find release -maxdepth 1 -type f -name '*.sh' -print 2>/dev/null | sort)
while IFS= read -r path; do
  shell_files+=("$path")
done < <(find tests -maxdepth 1 -type f -name '*.sh' ! -name static.sh -print | sort)

for path in "${shell_files[@]}"; do
  bash -n "$path"
done

if command -v shellcheck >/dev/null 2>&1; then
  shellcheck -e SC1091 "${shell_files[@]}"
else
  printf 'shellcheck not installed; skipping shell lint\n' >&2
fi

if command -v visudo >/dev/null 2>&1; then
  visudo -cf guest/hermes-box.sudoers
else
  printf 'visudo not installed; skipping sudoers parse\n' >&2
fi

grep -Fq 'agent ALL=(ALL:ALL) NOPASSWD: ALL' guest/hermes-box.sudoers
grep -Fq 'User=agent' guest/hermes.service
grep -Fq '/data/executor:/data' guest/executor.service
grep -Fq '127.0.0.1:4788:4788' guest/executor.service
grep -Fq 'Before=hermes.service executor.service' guest/hermes-box-recover.service
grep -Fq '/var/lib/hermes-box/update.json' guest/hermes-box-recover
grep -Fq 'xterm-ghostty.terminfo' release/build-provisioner.sh
grep -Eq '^UBUNTU_APT_SNAPSHOT=[0-9]{8}T[0-9]{6}Z$' release/pins.env
grep -Fq 'configure-apt-snapshot.sh' release/build-provisioner.sh
grep -Fq 'gpgv --keyring "$keyring" "$inrelease"' release/configure-apt-snapshot.sh
grep -Fq 'https://snapshot.ubuntu.com/ubuntu/' release/configure-apt-snapshot.sh
grep -Fq 'Dir::State::status=$work/dpkg-status' release/build-provisioner.sh
grep -Fq 'apt-get --assume-yes --download-only' release/build-provisioner.sh
grep -Fq 'HERMES_BOX_APT_CACHE' release/build-provisioner.sh
grep -Fq 'HERMES_BOX_APT_CACHE' .github/workflows/release-artifacts.yml
if grep -RFq 'APT::Snapshot=' release .github/workflows/release-artifacts.yml; then
  printf 'release flow must use the exact manual Ubuntu snapshot URL\n' >&2
  exit 1
fi
if grep -Fq 'APT::Snapshot=' .github/workflows/release-artifacts.yml; then
  printf 'workflow must not apply the guest package snapshot to runner tooling\n' >&2
  exit 1
fi
grep -Fq -- '--require-hashes' release/lib.sh
grep -Fq 'remove_python_bytecode "$work/source"' release/build-hermes-source.sh
grep -Fq '"$work/source/hermes_box_release/test_gated_approval.py"' release/build-hermes-source.sh
grep -Fq '"$work/source/hermes_box_release/patch-hermes-gated-approval.py"' release/build-hermes-source.sh
grep -Fq 'python="$work/python/bin/python3.13"' release/build-hermes-source.sh
grep -Fq 'python="$work/python/bin/python3.13"' release/build-hermes-wheels.sh
grep -Fq -- '--python-platform aarch64-manylinux_2_17' release/build-hermes-wheels.sh
grep -Fq -- '--generate-hashes --no-header --no-annotate' release/build-hermes-wheels.sh
grep -Fq 'requirements-linux-arm64.txt' release/build-hermes-wheels.sh
grep -Fq 'project_wheel=' release/build-hermes-wheels.sh
grep -Fq 'pip wheel --no-deps --no-build-isolation' release/build-hermes-wheels.sh
grep -Fq '"--require-hashes", "--requirement", wheelManifest.Requirements' internal/guestupdate/installers.go
grep -Fq '"--no-deps", wheelManifest.ProjectWheel' internal/guestupdate/installers.go
if grep -Fq 'uv sync' release/build-hermes-wheels.sh internal/guestupdate/installers.go; then
  printf 'Hermes offline install must not re-resolve the universal uv lock\n' >&2
  exit 1
fi
if grep -Fq '"-m", "pytest"' internal/guestupdate/installers.go; then
  printf 'Hermes guest approval validation must use the sealed regression runner\n' >&2
  exit 1
fi
grep -Fq 'tar -xf "$upstream" -C "$work/source" --strip-components=1' release/build-hermes-source.sh
if grep -Fq 'git -C "$work/source" fetch' release/build-hermes-source.sh; then
  printf 'Hermes source builder downloaded a verified archive but ignored it\n' >&2
  exit 1
fi
grep -Fq 'gated Hermes source archive contains Python bytecode' release/verify-release.sh
grep -Fq 'offline Linux ARM64 requirements contain a Windows-only dependency' release/verify-release.sh
grep -Eq '^pip==[^ ]+ --hash=sha256:[a-f0-9]{64}$' release/python-build-requirements.txt
grep -Eq '^PyYAML==[^ ]+ --hash=sha256:[a-f0-9]{64}$' release/python-build-requirements.txt
grep -Eq '^setuptools==[^ ]+ --hash=sha256:[a-f0-9]{64}$' release/python-build-requirements.txt
(
  # shellcheck source=release/pins.env
  source release/pins.env
  grep -Fxq "pip==$PIP_VERSION --hash=sha256:$PIP_SHA256" release/python-build-requirements.txt
  grep -Fxq "PyYAML==$PYYAML_VERSION --hash=sha256:$PYYAML_SHA256" release/python-build-requirements.txt
  grep -Fxq "setuptools==$SETUPTOOLS_VERSION --hash=sha256:$SETUPTOOLS_SHA256" release/python-build-requirements.txt
)
if grep -REn 'uv run .*--with|pip (install|download).*setuptools==' release/*.sh; then
  printf 'release scripts contain an unhashed Python build-tool install\n' >&2
  exit 1
fi

grep -Fq 'pull_request:' .github/workflows/release-artifacts.yml
grep -Fq 'shell: bash' .github/workflows/release-artifacts.yml
grep -Fq 'v2.0.0-baseline-assets' .github/workflows/release-artifacts.yml
grep -Fq 'v2.0.0-assets' .github/workflows/release-artifacts.yml
grep -Fq -- '-> release/qualification.lock.template' .agents/skills/update-hermes-box/references/components.md
grep -Fq 'git cat-file -t "$GITHUB_REF"' .github/workflows/release-artifacts.yml
grep -Fq 'git merge-base --is-ancestor "$tag_commit" refs/remotes/origin/main' .github/workflows/release-artifacts.yml
grep -Fq '"internal/**"' .github/workflows/release-artifacts.yml
grep -Fq 'Prove the exact Executor child loads and runs in Podman' .github/workflows/release-artifacts.yml
grep -Fq 'release/build-provisioner.sh "$artifact_dir"' .github/workflows/release-artifacts.yml
if grep -Fq 'sudo release/build-provisioner.sh' .github/workflows/release-artifacts.yml; then
  printf 'release builder must preserve the setup-go path inside the root container\n' >&2
  exit 1
fi
grep -Fq -- '--cgroups=disabled' .github/workflows/release-artifacts.yml
grep -Fq -- '--volume "$smoke_data:/data" "$runtime_tag"' .github/workflows/release-artifacts.yml
grep -Fq -- "--format '{{.State.Status}}'" .github/workflows/release-artifacts.yml
grep -Fq 'actions/attest-build-provenance@' .github/workflows/release-artifacts.yml
grep -Fq 'HERMES_BOX_E2E_LOCK' tests/lifecycle.sh
grep -Fq 'HERMES_BOX_E2E_ARTIFACT_DIR' tests/lifecycle.sh
grep -Fq 'HERMES_BOX_E2E_BASELINE_LOCK' tests/lifecycle.sh
grep -Fq 'HERMES_BOX_E2E_BASELINE_ARTIFACT_DIR' tests/lifecycle.sh
grep -Fq 'HERMES_BOX_E2E_ARTIFACT_CACHE' tests/lifecycle.sh
grep -Fq 'update_components=(node uv claude codex hermes executor)' tests/lifecycle.sh
grep -Fq 'update "$update_component"' tests/lifecycle.sh
grep -Fq 'rollback "$update_component"' tests/lifecycle.sh
grep -Fq -- '--assert-status "$desired_lock" "$applied_lock" "$component" "$previous_lock"' tests/lifecycle.sh
grep -Fq 'lifecycle failure diagnostics exposed the lock URL credential' tests/lifecycle.sh
grep -Fq '[[ ! -e $restore_config_dir/hermes-box.lock ]]' tests/lifecycle.sh
if grep -Fq 'workflow_dispatch:' .github/workflows/release-artifacts.yml; then
  printf 'release publication must be driven only by the immutable asset tag\n' >&2
  exit 1
fi

if [[ -f hermes-box.lock ]]; then
  if grep -Eq '__[A-Z0-9_]+__' hermes-box.lock; then
    printf 'promoted root lock contains an unresolved qualification placeholder\n' >&2
    exit 1
  fi
  go run ./release/validate-lock.go hermes-box.lock
  grep -Fq '/releases/download/v2.0.0-assets/' hermes-box.lock
else
  printf 'root hermes-box.lock is intentionally absent until qualification and promotion\n' >&2
fi

if grep -RFniE 'HERMES_BOX_MACHINE_NAME|hermes-box\.conf|secret-env\.txt|supervisorctl|hermes-box init|package configured' \
  README.md AGENTS.md PORTABLE_RESTORE.md EXECUTOR_CONNECTIONS.md; then
  printf 'v2 operator documentation contains a v1 concept\n' >&2
  exit 1
fi

./tests/tmux.sh
./tests/ssh-provisioning.sh
./bin/hermes-box help >/dev/null

launcher_tmp_root=${TMPDIR:-/tmp}
launcher_smoke_dir=$(mktemp -d "${launcher_tmp_root%/}/hermes-box-launcher-smoke.XXXXXX")
trap 'rm -rf "$launcher_smoke_dir"' EXIT
mkdir -p "$launcher_smoke_dir/home"
cat >"$launcher_smoke_dir/hermes-box.yaml" <<'EOF'
schema: 1
name: launcher-smoke
vm:
  cpus: 1
  memory: 512MiB
  root_disk: 10GiB
  data_disk: 1GiB
ports:
  executor: 65432
backup:
  keep: 1
EOF

if launcher_smoke_output=$(
  HERMES_BOX_HOME="$launcher_smoke_dir/home" \
    ./bin/hermes-box --config "$launcher_smoke_dir/hermes-box.yaml" status 2>&1
); then
  printf 'launcher smoke unexpectedly succeeded without a lock file\n' >&2
  exit 1
fi
if [[ $launcher_smoke_output == *'unknown Hermes Box environment setting'* ]]; then
  printf 'launcher injected an unsupported Hermes Box environment setting\n' >&2
  exit 1
fi
grep -Fq "read $launcher_smoke_dir/hermes-box.lock" <<<"$launcher_smoke_output"

printf 'static checks passed\n'
