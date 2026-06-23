# v2 release artifacts

Hermes Box uses a two-phase qualification flow. A pull request builds a
candidate that can be exercised before its repository-owned URLs exist. Only a
pushed immutable asset tag publishes those bytes. The exact published URLs are
then downloaded and the same lifecycle is rerun before the generated lock is
promoted to the repository root.

The checked-in lock template contains every directly published upstream pin.
It leaves the immutable asset-release name and three SHA-256 placeholders for
artifacts owned by this repository:
the guest provisioner, gated Hermes source, and Linux ARM64 Python wheel
closure. A workflow artifact is a candidate, not a release, and the root
`hermes-box.lock` is intentionally absent during the bootstrap PR.

## Build contract

Build on native Linux ARM64. The provisioner job runs as root in the exact
Ubuntu 26.04 ARM64 OCI child pinned by `UBUNTU_BUILDER_IMAGE`, and resolves its
complete `.deb` closure only from `UBUNTU_APT_SNAPSHOT`. The builder removes
old APT lists, rejects any non-snapshot index, verifies every signed
`InRelease` with Ubuntu's archive keyring, and records the SHA-256 of every
`InRelease` and `Packages` index in the provisioner manifest. Runtime
qualification still boots the dated Canonical cloud image `20260612`.

The release-only Python environment is equally closed: the standalone Python,
uv, PyYAML, pip, and setuptools wheels have exact URLs and SHA-256 pins.
`python-build-requirements.txt` installs those wheels offline with
`--require-hashes`. Hermes' exported runtime requirements retain their lock
hashes, and the setuptools build-backend wheel is copied from that same
verified tool set into the offline wheel closure.

```bash
artifact_dir=${RUNNER_TEMP:-/tmp}/hermes-box-release
sudo release/build-provisioner.sh "$artifact_dir"
release/build-hermes-source.sh "$artifact_dir"
release/build-hermes-wheels.sh "$artifact_dir"
release/render-lock.sh "$artifact_dir" "$artifact_dir/hermes-box.lock"
release/verify-release.sh "$artifact_dir"
```

The workflow builds in two clean directories and compares all three checksum
files. It uploads this seven-file candidate without publishing it:

```text
hermes-box-provisioner-linux-arm64.tar.zst
hermes-box-provisioner-linux-arm64.tar.zst.sha256
hermes-agent-gated-0.17.0.tar.zst
hermes-agent-gated-0.17.0.tar.zst.sha256
hermes-wheels-cp313-linux-arm64.tar.zst
hermes-wheels-cp313-linux-arm64.tar.zst.sha256
hermes-box.lock
```

## First-release bootstrap: establish a real baseline

The first v2 release cannot honestly prove six update and rollback paths in the
same pull request that introduces their implementation. An immutable release
tag may publish only a commit already reachable from reviewed `main`, while the
candidate lock must name the immutable URLs created by that tag. Inventing an
"older" lock or relaxing the update matrix would hide the exact transaction
paths this qualification exists to test.

Bootstrap therefore uses two immutable asset releases and three small reviewed
changes:

1. Merge the implementation change without a root `hermes-box.lock`.
2. At that merge commit, create and push the one-use annotated tag
   `v2.0.0-baseline-assets`. The workflow renders URLs for that tag and
   publishes the reviewed initial pins. Download and retain its seven files;
   this lock is a baseline only and must never be promoted to the root.
3. From the new `main`, open a component-qualification change that advances
   `node`, `uv`, `claude`, `codex`, `hermes`, and `executor` to six real newer
   immutable identities while leaving the host and Ubuntu platform pins
   unchanged. Update the gated Hermes patch and tests if its upstream anchors
   changed. Its pull-request workflow produces the final candidate.
4. Run the isolated lifecycle with the baseline release and final candidate.
   Merge only after all six exact candidate activations and baseline rollbacks
   pass.
5. Tag that reviewed merge as `v2.0.0-assets`, re-run the lifecycle from the
   published URLs, then promote only that final generated lock in a third
   reviewed change.

Create the baseline tag only after the implementation merge is visible on
`main`:

```bash
git fetch origin main
baseline_commit=$(git rev-parse origin/main)
git tag -a v2.0.0-baseline-assets "$baseline_commit" \
  -m 'Hermes Box v2 baseline ARM64 assets'
git push origin refs/tags/v2.0.0-baseline-assets
```

The publish job peels the annotated tag, fetches `origin/main`, and refuses to
attest or publish unless the tagged commit is an ancestor of that reviewed
branch. The same check protects the final asset tag.

Download the baseline release exactly as shown for the final release below,
using `v2.0.0-baseline-assets` as the base URL, and preserve its generated lock
and three archives as the baseline inputs for every lifecycle pass.

## Phase 1: qualify the final pull-request candidate

Download the `hermes-box-v2-linux-arm64-<commit>` artifact from the pull
request workflow into one directory. With `gh`, one exact run can be selected
explicitly:

```bash
run_id=<successful-release-artifact-workflow-run>
candidate=/tmp/hermes-box-candidate
rm -rf "$candidate"
gh run download "$run_id" \
  --name "hermes-box-v2-linux-arm64-<commit>" \
  --dir "$candidate"
release/verify-release.sh "$candidate"
```

Run the destructive matrix only on the isolated harness. Qualification always
supplies a real reviewed baseline (or, after bootstrap, the previously
qualified lock) and its repository-owned artifacts, plus the candidate lock
and artifacts. The harness verifies that
all six component identities differ, validates and seeds both artifact sets
into its temporary content-addressed cache, then exercises update and rollback
for `node`, `uv`, `claude`, `codex`, `hermes`, and `executor` individually. It
therefore never follows a not-yet-published repository URL:

```bash
HERMES_BOX_E2E=1 \
HERMES_BOX_E2E_BASELINE_LOCK=/path/to/baseline/hermes-box.lock \
HERMES_BOX_E2E_BASELINE_ARTIFACT_DIR=/path/to/baseline/artifacts \
HERMES_BOX_E2E_LOCK="$candidate/hermes-box.lock" \
HERMES_BOX_E2E_ARTIFACT_DIR="$candidate" \
HERMES_BOX_E2E_ARTIFACT_CACHE=/path/to/combined-official-artifacts-cache \
  ./tests/lifecycle.sh
```

For the first public v2 qualification, the baseline is the reviewed
`v2.0.0-baseline-assets` release created by the bootstrap sequence above.
Later upgrades use the previously qualified root lock and its retained exact
artifacts. There is no no-drift lifecycle shortcut: a release that has not
demonstrated a real activation and rollback for every component is not
qualified.

The lifecycle owns unique temporary config, Hermes Box, Lima, VM, disk, port,
and Keychain identities. It proves persistence, tmux/Ghostty extended keys,
loopback-only Executor, backup, cross-destination restore, rebuild, and cleanup.
Never reproduce its destructive commands against normal homes.

## Phase 2: publish, re-download, and promote

Merge the qualification change without a root lock. Create the asset tag once
at that reviewed commit and push it without force:

```bash
git tag -a v2.0.0-assets <reviewed-merge-commit> \
  -m 'Hermes Box v2 qualified ARM64 assets'
git push origin refs/tags/v2.0.0-assets
```

The tag-triggered workflow rebuilds twice, attests every archive, checksum,
and generated lock, refuses an existing GitHub release, publishes the seven
files, and downloads every exact URL back for verification. Never move or
reuse `v2.0.0-assets`.

Independently download the published bytes on the lifecycle host and verify
their GitHub attestations:

```bash
published=/tmp/hermes-box-v2.0.0-assets
base=https://github.com/davis7dotsh/hermes-box/releases/download/v2.0.0-assets
rm -rf "$published"
mkdir -p "$published"
for file in \
  hermes-box-provisioner-linux-arm64.tar.zst \
  hermes-box-provisioner-linux-arm64.tar.zst.sha256 \
  hermes-agent-gated-0.17.0.tar.zst \
  hermes-agent-gated-0.17.0.tar.zst.sha256 \
  hermes-wheels-cp313-linux-arm64.tar.zst \
  hermes-wheels-cp313-linux-arm64.tar.zst.sha256 \
  hermes-box.lock; do
  curl --fail --location --proto '=https' --tlsv1.2 \
    --output "$published/$file" "$base/$file"
  gh attestation verify "$published/$file" \
    --repo davis7dotsh/hermes-box
done
release/verify-release.sh "$published"
HERMES_BOX_E2E=1 \
HERMES_BOX_E2E_BASELINE_LOCK=/path/to/baseline/hermes-box.lock \
HERMES_BOX_E2E_BASELINE_ARTIFACT_DIR=/path/to/baseline/artifacts \
HERMES_BOX_E2E_LOCK="$published/hermes-box.lock" \
HERMES_BOX_E2E_ARTIFACT_DIR="$published" \
HERMES_BOX_E2E_ARTIFACT_CACHE=/path/to/combined-official-artifacts-cache \
  ./tests/lifecycle.sh
```

Only after that exact-URL lifecycle succeeds may the generated lock be copied
to the repository root in a final reviewed pull request:

```bash
cp "$published/hermes-box.lock" ./hermes-box.lock
make check
git diff --check
```

The promotion PR records the candidate workflow run, tag workflow run,
attestation verification, both lifecycle results, and exact asset URLs. The
workflow uses pinned full action SHAs, checkout with `persist-credentials:
false`, read-only default permissions, and grants `contents: write`,
`attestations: write`, and `id-token: write` only to the tag publish job.
