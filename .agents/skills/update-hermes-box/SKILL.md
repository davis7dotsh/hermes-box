---
name: update-hermes-box
description: Safely audit and upgrade pinned Hermes Box components through isolated implementation, validation, independent review, a ready pull request from current main, and monitoring until every required check and review thread is green. Use when updating Hermes Agent, smolvm, uv, Ubuntu, Codex, Executor, Node, Go, CI actions, or when performing a general Hermes Box dependency freshness pass.
---

# Update Hermes Box

Upgrade one or more Hermes Box components without weakening its host boundary,
backup compatibility, reproducibility, or recovery path. Treat release discovery,
implementation, lifecycle proof, and PR follow-through as one workflow.

Read [references/components.md](references/components.md) for each selected
component before planning edits.

## Operating contract

- Read `AGENTS.md` and `README.md` completely before changing behavior.
- Read every repository file you inspect completely.
- Preserve unrelated user changes. Stop if they overlap and cannot be separated.
- Use only official release pages, repositories, package indexes, and checksums
  for version and provenance decisions.
- Treat `state/`, `images/`, `backups/`, `hermes-box.conf`, and
  `secret-env.txt` as private runtime material. Never commit them.
- Never operate the primary machine names, ports, or data directories while
  validating an upgrade.
- Do not run `dev`, `build`, or a lifecycle test unless repository instructions
  and the user's authorization permit it.
- Keep a component pinned when the newer release cannot satisfy the same
  security, compatibility, and recovery checks. “Latest” is not itself proof.
- Publish a ready PR unless the user explicitly requests a draft.

## Inputs

Resolve these from the request and repository before asking questions:

1. Components to update.
2. Target version, tag, commit, image digest, or “latest stable.”
3. Base branch, normally `main`.
4. Whether isolated lifecycle operations are explicitly authorized.
5. Whether the task includes only an audit or also implementation and PR work.

Ask one question only when a missing answer materially changes safety or scope.

## Canonical workflow

### 1. Establish a clean baseline

Run:

```bash
git status --short --branch
git remote -v
gh auth status
git fetch origin
BASE_BRANCH=${BASE_BRANCH:-main}
git switch "$BASE_BRANCH"
git pull --ff-only origin "$BASE_BRANCH"
```

Do not switch branches when the worktree contains overlapping user changes.
Inspect open PRs when an upgrade may already be in progress.

Run the contributor probes from `AGENTS.md`, including the installed Go and
smolvm versions. Run `make check` before editing when a baseline failure would
otherwise be confused with the upgrade.

### 2. Create one focused branch

Use a concise branch derived from the component set:

```bash
git switch -c codex/update-<component-or-group>
```

Split upgrades when one component has an independent rollback boundary or
requires a substantially different lifecycle matrix. Prefer separate PRs for:

- Hermes source plus its commit-anchored approval patch.
- smolvm host/runtime behavior.
- A previously failing tool qualification such as uv.

Combine only tightly coupled upgrades that must be proven together.

### 3. Resolve official release provenance

For every component:

1. Identify the latest stable release and publication date.
2. Resolve moving tags to immutable commits or image digests.
3. Download or query the exact ARM64/macOS/Linux artifact used by Hermes Box.
4. Verify the publisher-provided SHA-256 or attestation.
5. Read release notes and compare the old and new revisions.
6. Record breaking changes, security fixes, migrations, and removals.
7. Reject prereleases unless explicitly requested.

Useful GitHub commands:

```bash
gh release list --repo OWNER/REPO --limit 10
gh release view TAG --repo OWNER/REPO --json tagName,targetCommitish,publishedAt,body,url,assets
gh api repos/OWNER/REPO/git/ref/tags/TAG
gh api repos/OWNER/REPO/compare/OLD...NEW
```

Do not trust a major-version action tag, OCI tag, or release label as an
immutable pin. Resolve and retain the reviewed commit or digest where the
repository's security model expects immutability.

### 4. Map the complete patch surface

Search all tracked source, tests, examples, CI, and documentation for:

- Version strings, tags, commits, digests, checksums, and download URLs.
- Compatibility checks and error messages.
- Workarounds tied to the old version.
- Backup manifests, restore preflight, portable archives, and generated
  handoff text.
- Runtime health checks and lifecycle assertions.
- Security promises whose implementation depends on the component.

Use `rg`, then read each matched file in full. Build a component-specific list
of code, tests, docs, migration behavior, and required validation before editing.

### 5. Rehearse fragile transformations before touching the repo

Use disposable directories under `/tmp` for upstream clones, archives, and
compatibility experiments. Examples:

- Apply commit-anchored source patch functions to copies of the new upstream
  files and count every expected anchor.
- Compare old and new CLI help using a downloaded, checksum-verified binary
  without installing it globally.
- Inspect OCI indexes and select the exact Linux ARM64 child manifest.
- Run bounded reproductions for a previously hanging installer.

Never use a production VM or primary Hermes Box resource for rehearsal.

### 6. Implement the smallest coherent upgrade

- Update every authoritative pin and its matching checksum.
- Update strict version validators, install guidance, and test fixtures.
- Port source patches against the new exact revision; retain fail-closed anchor
  checks and compilation tests.
- Preserve backup and restore compatibility unless the PR explicitly defines
  and tests a format migration.
- Keep old workarounds until the new version proves them unnecessary. Remove a
  workaround only with a regression test demonstrating the upstream fix.
- Add packages using the ecosystem's install command when applicable; do not
  hand-edit package manifests.
- Update user-facing docs and handoff output in the same patch.

### 7. Validate locally

After every code or script change, run the focused test first. Before review,
run the repository gate:

```bash
make format
make check
git diff --check
git status --short
```

Do not claim runtime compatibility from `make check` alone when the component
participates in VM creation, provisioning, packing, restore, or first boot.

### 8. Run isolated lifecycle proof when authorized

Follow the exact isolation rules in `AGENTS.md`. Use unique machine names,
builder names, data directories, SSH ports, and Executor ports. Check that the
chosen ports are unused before creating anything.

At minimum prove:

1. Fresh builder provisioning.
2. Pack creation and runtime creation.
3. First boot and SSH health.
4. Hermes version and gated-approval regression tests.
5. Supervisor, gateway, and optional Executor health.
6. Snapshot, restore candidate verification, and portable packaging where the
   upgraded component affects those paths.
7. Stop/start persistence and cleanup of only the disposable resources.

When a component previously failed or hung, run the exact bounded reproduction
at least twice: one cold run and one cache/retry run. Save the exact failure
output if the candidate is rejected.

### 9. Run independent reviews

Keep review roles separate from implementation:

- **Correctness/security review:** version provenance, pin consistency,
  authorization boundaries, source-patch semantics, backup/restore behavior,
  runtime compatibility, edge cases, and test adequacy.
- **Operational/UX review:** setup flow, handoff text, failure messages,
  upgrade/migration clarity, recovery instructions, and whether behavior feels
  native to the existing CLI.

Reviewers must not edit. Triage findings, send accepted fixes back to the
implementation pass, revalidate, then request focused re-review.

### 10. Commit and open a ready PR

Inspect the final diff and ensure the worktree contains only intended files.
Commit with a component-focused message, push, and open a ready PR against the
resolved base branch.

The PR must state:

- Old and new versions or immutable identifiers.
- User-visible effect and migration behavior.
- Workarounds added, retained, or removed.
- Local and lifecycle validation actually performed.
- Any unverified destructive or live-machine path.
- Intentionally deferred upgrades and why.

### 11. Monitor until genuinely green

Watch GitHub Actions and external checks on the current head:

```bash
gh pr checks PR --json name,state,bucket,link
gh pr checks PR --watch --interval 10
```

For failures, inspect the Actions log before editing. Reproduce the failure
locally when possible, make the narrowest fix, rerun the full local gate,
commit, and push.

Read review threads with GitHub GraphQL so resolved/outdated state is visible.
Address every current actionable thread, verify the pushed fix, then resolve
the thread and read it back. Repeat until:

- All required checks pass on the latest head.
- The PR is mergeable and targets the resolved base branch.
- No unresolved actionable review thread remains.
- The local branch is clean and matches its remote.

## Recovery rules

- If provenance or checksum verification fails, stop and reject the candidate.
- If a commit-anchored patch drifts, port and independently review it; never
  weaken exact-match checks to make the install pass.
- If an isolated lifecycle run fails, preserve its diagnostics and follow the
  cleanup command from `AGENTS.md` using the same isolated variables.
- If a new component breaks old backups, either restore compatibility or make
  the format migration explicit, versioned, documented, and tested.
- If an external review suggestion broadens into an unrelated upgrade, defer it
  to a separate issue or PR.

## Done condition

Finish only when the requested components are updated or explicitly rejected
with evidence, focused and repository-wide validation pass, accepted review
findings are fixed, the ready PR is clean and green on its latest commit, and
all actionable review threads are resolved.

Report the branch, commits, PR URL, checks, lifecycle evidence, review
disposition, migration impact, and deferred follow-ups.
