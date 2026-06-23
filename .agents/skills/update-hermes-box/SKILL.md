---
name: update-hermes-box
description: Safely audit and upgrade Hermes Box v2 pins and release inputs through the reviewed input-only lock, reproducible ARM64 release artifacts, trusted guest installers, encrypted transaction snapshots, isolated Lima lifecycle proof, independent review, and a ready pull request. Use for Hermes Agent, Lima, Ubuntu, the guest provisioner, uv, Node, Python, Claude Code, Codex, Executor, Go, or CI action updates and for general Hermes Box dependency freshness passes.
---

# Update Hermes Box

Update one or more v2 inputs without weakening artifact integrity, transactional
rollback, backup closure, or the host boundary. Treat discovery, release-input
construction, implementation, isolated proof, review, and PR follow-through as
one workflow.

Read [references/components.md](references/components.md) completely before
planning edits, then apply the sections for the selected components.

## Invariants

- Read `AGENTS.md`, `README.md`, and every inspected repository file fully.
- Preserve unrelated changes. Stop if they overlap and cannot be separated.
- Use official releases, repositories, indexes, checksums, and attestations.
- Keep `hermes-box.lock` input-only. Runtime commands never select versions or
  write it.
- Keep the data disk durable and the VM root reconstructable. Never restore a
  root filesystem or add v1/smolvm compatibility.
- Keep install and activation logic in the static trusted guest helper. The host
  only materializes verified artifacts, creates encrypted snapshots, uploads
  inputs, and invokes the guest protocol.
- Never put secrets in config, locks, arguments, logs, metadata, or Git.
- Never operate the primary box, default homes, or primary port during proof.
- Keep a current pin when the candidate cannot pass the same integrity,
  rollback, backup, and lifecycle checks. "Latest" is not qualification.
- Publish a ready PR unless the user explicitly requests a draft.

## Inputs

Resolve before asking a question:

1. Components and target immutable versions, commits, or digests.
2. Base branch, normally `main`.
3. Whether the change affects only applications or requires `rebuild`.
4. Whether isolated lifecycle operations are explicitly authorized.
5. Whether the task is audit-only or includes implementation and publication.

Ask one question only when a missing answer changes safety or scope.

## Workflow

### 1. Establish the baseline

Inspect the worktree and remote before switching anything:

```bash
git status --short --branch
git remote -v
gh auth status
git fetch origin
```

Start from current `main` only when doing so will not overwrite shared work:

```bash
git switch main
git pull --ff-only origin main
git switch -c codex/update-<component-or-group>
```

Run the contributor probes from `AGENTS.md`, including `limactl --version`, and
run `make check` when a baseline result is needed.

### 2. Map the authoritative seams

Search and then read every matched file fully. The usual authorities are:

- `release/pins.env`: reviewed upstream versions, URLs, commits, digests, and
  checksums.
- `release/qualification.lock.template`: candidate lock with placeholders only
  for repository-built artifacts.
- `release/`: deterministic provisioner, gated Hermes source, wheel closure,
  lock rendering, and verification.
- `.github/workflows/release-artifacts.yml`: native ARM64 replay, publication,
  attestation, and exact-URL verification.
- `internal/config`: lock schema and validation.
- `internal/component`: dependency order and fixed durable snapshot scopes.
- `internal/guestupdate`: trusted installers, validation, activation, rollback,
  crash recovery, freeze/thaw, and scoped restore.
- `internal/app/default_components.go`: host artifact materialization and guest
  protocol construction.
- `internal/app/default_backup.go`: self-contained backup closure and encrypted
  transaction snapshots.
- README, operator docs, static tests, and lifecycle assertions.

Do not treat a generated workflow artifact as the repository lock. The root
lock is promoted only after publication and lifecycle qualification.

### 3. Resolve provenance and compatibility

For each candidate:

1. Identify the latest requested stable release and publication date.
2. Resolve moving tags to immutable commits or OCI digests.
3. Select the exact Darwin ARM64, Linux ARM64, npm, cloud-image, or OCI input
   used by Hermes Box.
4. Verify publisher checksums, npm SRI, OCI index and child digests, or an
   independently reviewed checksum.
5. Compare every release note and upstream revision since the current pin.
6. Record breaking changes, migrations, security changes, package/runtime
   requirements, and obsolete workarounds.
7. Reject prereleases unless explicitly requested.

Useful GitHub probes include:

```bash
gh release list --repo OWNER/REPO --limit 10
gh release view TAG --repo OWNER/REPO --json tagName,targetCommitish,publishedAt,body,url,assets
gh api repos/OWNER/REPO/git/ref/tags/TAG
gh api repos/OWNER/REPO/compare/OLD...NEW
```

Inspect OCI indexes and npm metadata directly; never substitute a mutable tag
for the immutable identities required by the lock.

### 4. Rehearse fragile changes in `/tmp`

Use disposable downloads, clones, and extracted archives. Depending on the
component, prove:

- the candidate artifact hash and archive shape;
- release-time Hermes approval patch anchors and regression behavior;
- an offline Hermes wheel closure against the exact Python, uv, and `uv.lock`;
- the trusted guest installer's expected executable layout;
- OCI index selection and Linux ARM64 child identity;
- bounded cold and cache/retry behavior for an installer with prior failures.

Do not install a candidate globally or use a primary VM for rehearsal.

### 5. Implement the smallest coherent update

- Update `release/pins.env`, the qualification template, tests, documentation,
  and strict validators together.
- Change trusted installer logic only when the upstream artifact contract
  changed. Keep archive traversal, checksum, digest, and executable-count
  checks fail-closed.
- Preserve the fixed component snapshot scope unless a reviewed upstream
  migration proves that more durable state can change.
- Preserve one previous release and one encrypted pre-update snapshot.
- Keep application updates explicit through `hermes-box update`; apply Ubuntu,
  provisioner, foundational package, and Lima-compatibility changes through
  `hermes-box rebuild`.
- Do not make vendor self-updaters authoritative.
- Update handoff, backup, restore, and recovery text in the same patch.

If repository-owned artifacts change, build a new immutable asset release. Do
not replace files in an existing release tag.

### 6. Build and qualify release inputs

Follow `release/README.md`. On native Linux ARM64:

```bash
artifact_dir=${RUNNER_TEMP:-/tmp}/hermes-box-release
sudo release/build-provisioner.sh "$artifact_dir"
release/build-hermes-source.sh "$artifact_dir"
release/build-hermes-wheels.sh "$artifact_dir"
release/render-lock.sh "$artifact_dir" "$artifact_dir/hermes-box.lock"
release/verify-release.sh "$artifact_dir"
```

Build the three repository-owned archives again in a clean workspace and
compare their checksum files. A candidate lock is not qualified until:

1. Both clean ARM64 builds are byte-identical.
2. Offline Hermes replay, approval tests, archive manifests, and `.deb`
   checksums pass.
3. The immutable assets and checksums are published and attested.
4. Every exact published URL is downloaded and verified again.
5. The generated lock passes the isolated lifecycle matrix.
6. That exact generated lock is promoted to the repository root through the
   reviewed PR.

Qualification PRs build without publishing. Only an explicitly authorized tag
or workflow dispatch may publish assets.

### 7. Validate locally

Run focused tests after each code or script change. Before review:

```bash
make format
make check
git diff --check
git status --short
```

`make check` is necessary but cannot prove VM provisioning, update migration,
backup/restore, or runtime behavior.

### 8. Prove the isolated lifecycle when authorized

Use only the guarded entrypoint from `AGENTS.md`:

```bash
HERMES_BOX_E2E=1 ./tests/lifecycle.sh
```

Do not reproduce its destructive commands against normal homes. At minimum
prove the affected paths among:

1. Fresh Ubuntu 26.04 create and separately named Lima data disk.
2. Locked component versions and systemd service health.
3. Three stop/start cycles and persistent data.
4. Component update, encrypted snapshot, rollback, and restored user state.
5. Full backup, restore into an absent destination, and artifact closure.
6. Root rebuild with the same data disk and automatic prior-lock recovery.
7. Loopback-only Executor exposure and isolated cleanup.

For a prior hang or regression, run the bounded reproduction cold and again
with populated caches. Preserve exact diagnostics for a rejected candidate.

### 9. Run independent reviews

Keep reviewers read-only and separate from implementation:

- Correctness/security: provenance, pin consistency, guest protocol,
  authorization, snapshot scope, rollback, backup closure, restore/rebuild,
  redaction, and test adequacy.
- Operational/UX: update/rebuild distinction, setup and handoff, failure and
  recovery text, downtime, and migration clarity.

Fix accepted findings, rerun validation, and request focused re-review.

### 10. Open and monitor the ready PR

Commit only intended files, push, and open a ready PR against the resolved base.
Include:

- old and new immutable identities;
- provenance and release-artifact construction;
- user-visible effect and required `update` or `rebuild` command;
- snapshot, rollback, backup, and migration impact;
- local and lifecycle evidence actually obtained;
- unverified paths and intentionally deferred candidates.

Monitor checks and actionable review threads on the current head:

```bash
gh pr checks PR --json name,state,bucket,link
gh pr checks PR --watch --interval 10
```

Inspect failure logs before editing. Read thread-level state through GitHub
GraphQL, address every current actionable thread, verify the pushed fix, and
resolve it. Finish only when checks pass, the PR is mergeable, review threads
are clear, and the local branch matches its remote.

## Recovery rules

- Reject a candidate on failed provenance, checksum, SRI, digest, attestation,
  deterministic replay, or offline closure verification.
- Never weaken a release-time Hermes patch anchor; port it and independently
  review the resulting gated source.
- Preserve the isolated lifecycle paths printed by the harness if cleanup is
  interrupted; never substitute defaults.
- If a component migration expands durable writes, expand its encrypted
  snapshot scope and prove rollback before activation.
- If an update breaks the recovery format, make the schema migration explicit,
  versioned, documented, and tested in the same PR.
- Defer unrelated upgrades rather than hiding them inside a coupled artifact
  rebuild.

## Done condition

Finish only when every requested candidate is qualified or rejected with
evidence, local and authorized lifecycle validation pass, accepted findings are
fixed, the ready PR is green, and actionable review threads are resolved.

Report the branch, commits, PR URL, immutable old/new identities, artifact
publication, checks, lifecycle evidence, review disposition, migration command,
and deferred work.
