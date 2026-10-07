# Cleat Release Process

## Branching model

cleat follows gitflow. Five branch kinds, and the release flow is fully determined
by them:

| Branch | Cut from | Merges into | Lifetime |
|--------|----------|-------------|----------|
| `main` | — | — | permanent; every commit is a tagged release |
| `develop` | — | — | permanent; the integration branch, and the repo default |
| `feature/`, `bugfix/`, `fix/`, `docs/` | `develop` | `develop` | until merged |
| `release/vX.Y.Z` | `develop` | **`main` and `develop`** | until released |
| `hotfix/...` | **`main`** | **`main` and `develop`** | until released |

The two rows in bold are the ones that make it gitflow rather than a naming
convention. A release or hotfix branch merges into *both* long-lived branches;
skipping the back-merge into `develop` is what makes the branches drift apart.

### Merge method per hop

gitflow is defined by the merge graph, so the method is not a matter of taste:

| Hop | Method | Why |
|-----|--------|-----|
| `feature/*` -> `develop` | **Squash** | Keeps develop's history one commit per change. |
| `release/*` -> `main` | **Merge commit** | `main` must descend from the released history. |
| `main` -> `develop` | **Merge commit** | Carries the tag back with the version bump; the only merge that makes the tag an ancestor of `develop`. |
| `hotfix/*` -> `main` | **Merge commit** | Same as a release. |
| `hotfix/*` -> `develop` | **Merge commit** | The fix reaches develop by merging, never by cherry-pick. |

This table used to say that GitHub cannot enforce a merge method per target
branch, so it was all convention and "on the merger to pick the right one from
the dropdown". **That is no longer true, and it is false in the direction that
bites.** The dropdown still exists repo-wide, but for anything targeting
`develop` it no longer decides anything: `develop` carries the ruleset
**`22686489`, "develop merge queue"**, and a merge queue rule is where a merge
method *can* be pinned to one branch.

```bash
gh api repos/cleat-team/cleat/rulesets/22686489 \
  --jq '{method:.rules[0].parameters.merge_method, bypass:.bypass_actors}'
# -> {"bypass":[],"method":"SQUASH"}                   (2026-09-27)
```

(The keys come out alphabetically because `gh`'s `--jq` is gojq, which sorts
object keys — read the values, not the order.)

So **the two rows above whose whole point is the second parent — `main -> develop`
and `hotfix/* -> develop` — cannot be merged as written.** The queue squashes
them, and `bypass_actors` is empty, so nobody can step around it. The `-> main`
rows are unaffected: `main` carries no ruleset and the dropdown governs there.

Rebase merging is disabled. Re-derive the repo-wide settings with:

```bash
gh api repos/cleat-team/cleat \
  --jq '{merge:.allow_merge_commit,squash:.allow_squash_merge,rebase:.allow_rebase_merge}'
# -> {"merge":true,"rebase":false,"squash":true}      (2026-08-10)
```

#### The back-merge needs an admin merge-method flip

Because the queue forces the method and nothing can bypass it, the back-merge is
a three-step operation with an admin action in the middle:

1. Set the ruleset's `merge_method` to `MERGE`.
2. **Enqueue** the back-merge PR, and leave the ruleset on `MERGE` until it has
   finished merging.
3. Restore it to `SQUASH`.

Step 1 and 3 are the owner's; they are item 155 of `#2058`. **Do not leave the
queue on `MERGE`** — `feature/* -> develop` is squash by design, and a queue set
to `MERGE` silently stops doing that for every PR that follows.

**The flip must precede the ENQUEUE, not merely the merge — and this was learned
the expensive way on v0.3.2.** The three steps above used to read "1. Set the
method to `MERGE`. 2. Merge the back-merge PR. 3. Restore it", which is not wrong
so much as under-specified: it reads as though the method is consulted when the
merge happens, so any flip that precedes the merge is sufficient. **It is
captured when the entry is ENQUEUED.** Measured:

| | |
|---|---|
| back-merge PR enqueued | in the same call as the flip, microseconds before it took |
| ruleset reads `MERGE` | for the full fifteen minutes the entry sat in the queue (verified live at both ends) |
| it merged | still as a **squash** — one parent, lineage lost |

Nothing failed. No check went red. `MERGE` was the true state of the ruleset
throughout. The step-2 wording above is therefore "**Enqueue**", not "Merge",
because that is the ordering that matters, and the ruleset must stay on `MERGE`
until the entry has actually merged — restoring right after the enqueue would
leave the merge itself to whatever method is live when it lands.

**Verify the result by parent count, and the expected value depends on the PR.**
Every state field is identical either way — `MERGED` is true, the checks are
green, `mergeStateStatus` is `CLEAN` — so only the parent count distinguishes a
squashed back-merge from a correct one:

```bash
gh api repos/cleat-team/cleat/commits/$(git rev-parse origin/develop) \
  --jq '.parents | length'          # back-merge: expect 2.  feature/*: expect 1.
git merge-base --is-ancestor vX.Y.Z origin/develop && echo "tag is an ancestor"
```

A bare parent count means nothing without the PR kind: a `feature/*` squash is
correct at **1**, and a back-merge is correct at **2**. Applying the wrong
expectation gives a confident wrong answer, which is why both are stated here
rather than the number alone.

**The failure this prevents is not hypothetical, and it is silent.** #1798 existed
to make `v0.2.0` an ancestor of `develop`, and its own body argued that "a real
merge is the only shape that works" because a squash "carries the content, not the
lineage". It was squashed by this queue regardless — its merge commit `4753106e`
has **one** parent — and the lineage never arrived:

```bash
git merge-base --is-ancestor v0.2.0 origin/develop   # exit 1
git describe --tags --abbrev=0 origin/develop        # v0.1.0 -- the release before main's v0.2.0
```

(`v0.1.0` is a lightweight tag on an ordinary commit that `develop` does contain;
`v0.2.0` is annotated and sits on `main` alone. So `describe` is not failing — it
is answering correctly about the tag it can reach, and the answer is the wrong
release.)

Nothing failed; no check went red; the branch model is simply wrong. A squashed
back-merge looks like a successful merge in every place you would normally look,
which is why the method is a step in the checklist rather than a judgement call.

### Branch protection

Both `main` and `develop` are protected with `enforce_admins: true` and force
pushes disabled, so **nothing reaches either branch except through a pull
request** — including for admins. The release checklist below is written against
that fact; any instruction telling you to commit or push directly to `main` is
wrong.

| Branch | Required checks | Re-derive |
|--------|-----------------|-----------|
| `main` | `Build`, `Lint` | `gh api repos/cleat-team/cleat/branches/main/protection --jq .required_status_checks.contexts` |
| `develop` | every context in `tiers.yaml: required_contexts` | `gh api repos/cleat-team/cleat/branches/develop/protection --jq '.required_status_checks.contexts \| length'` |

The count is not written here on purpose. It was, in this table and in nine
workflow files, and every copy said 32 while branch protection required 33 —
`Web Dashboard` was added on 2026-09-17 and declared in-tree on 2026-09-20
(cleat#1937). `tiers.yaml: required_contexts` is the single in-tree list, and
`scripts/check-required-contexts.py` diffs it against the live one whenever the
caller has the admin scope to read it — saying so plainly when it cannot.

`main` deliberately requires less than `develop`. Everything reaching `main` has
already passed the full gate on `develop`; the release PR re-runs the suites
anyway (the workflows trigger on `branches: [main, develop]`), but only `Build`
and `Lint` block the merge.

**DCO does not run on PRs into `main`.** It gates *contribution*, and
contribution enters at `develop`, where every commit is checked on the way in. A
release PR carries no new authorship — just the whole release, which for v0.2.0
was 443 non-merge commits with 284 of them predating the sign-off convention —
so the check could never go green and no contributor could act on it. See the
comment block in `.github/workflows/dco-check.yml`. Re-derive:

```bash
# The range is pinned, not derived from `git merge-base`. #466 made develop an
# ancestor of main, so the live merge-base is now develop's own head and the
# derived range is empty — it would report 0, not 443. 97abac8..d23529e is the
# v0.2.0 release as it stood.
git rev-list --no-merges --count 97abac8..d23529e                   # 443
git rev-list --no-merges 97abac8..d23529e | while read -r c; do \
  [ -z "$(git show -s --format='%(trailers:key=Signed-off-by,valueonly)' "$c")" ] \
    && echo "$c"; done | wc -l                                      # 284
```

(Both measured 2026-08-10, and re-derived after #466 landed.)

### The 2026-08-10 reconnect

Before 2026-08-10 the repo was squash-only, so `main` could not descend from
`develop` and did not: since their common ancestor `97abac8` they had 2 and 448
commits respectively, with neither an ancestor of the other, while their trees
were byte-identical. PR #466 repaired this with a real merge commit
(`main` = `177ca8b`, parents `fb4347d` and `ab90dad`). This is why `git log
main` shows two flattened snapshots (`467a689`, `fb4347d`) before the graph
becomes continuous, and why anything written about this repo's release process
before that date describes a world that no longer exists.

```bash
git merge-base --is-ancestor origin/develop origin/main && echo connected
git diff --stat origin/main origin/develop     # empty: same content
```

The repair took two attempts, and the failure is the clearest possible
illustration of why it was needed. PR #463 merged `develop` into `main`
directly and was conflict-free — until a PR landed on `develop` touching
`.github/workflows/dco-check.yml`. `main` carried its own copy of that file
from the v0.2.0 squash, so with the merge base still at `97abac8` git read the
two as independent edits and the merge conflicted. It had been clean an hour
earlier only because the copies happened to be byte-identical. #463 was closed
and #466 carried a merge commit built explicitly against `develop`'s tree.
**Under squash-only merges this conflict was going to recur, widening, at every
release.**

## Versioning

Cleat follows [Semantic Versioning](https://semver.org/) (MAJOR.MINOR.PATCH).

| Bump | When | Example | Impact |
|------|------|---------|--------|
| **MAJOR** | Breaking changes to the public API, WASM boundary, or database schema | `1.0.0` -> `2.0.0` | Requires migration; old workflows may not replay |
| **MINOR** | New features without breaking existing APIs | `1.0.0` -> `1.1.0` | Backward compatible; new functionality available |
| **PATCH** | Bug fixes, security patches, documentation | `1.0.0` -> `1.0.1` | No behavioral changes for correct code |

### What constitutes a breaking change (MAJOR bump)

- **WASM boundary**: adding, removing, or changing the signature of any host
  function import (`cleat_call`, `cleat_sleep`, etc.)
- **Database schema**: non-backward-compatible changes to `workflow_defs`,
  `workflow_instances`, `event_history`, or `workflow_signals` tables
- **HostCalls interface**: changing the signature of methods in the
  `cleat.HostCalls` interface
- **Replay semantics**: changes that alter how historical events are replayed,
  causing previously-completed workflows to fail or produce different results
- **CLI commands**: removing or changing the behavior of existing `cleat` or
  `cleat-worker` flags
- **Go types**: removing or changing exported types in SDK packages
  (`cleat/cleat`, `cleat/cleattest`)

### What does NOT require a MAJOR bump

- Adding new methods to the `cleat.HostCalls` interface (new methods have
  default implementations)
- Adding new CLI flags or subcommands
- Adding new database columns with NULL defaults
- Adding new WASM host functions (existing workflows do not use them)
- Bug fixes that correct behavior to match documented semantics
- Performance improvements

## Release cadence

| Release type | Cadence | Examples |
|-------------|---------|----------|
| **Minor** | Monthly | `v0.5.0`, `v0.6.0`, `v1.0.0`, `v1.1.0` |
| **Patch** | As needed | Security fixes, critical bug fixes |
| **Major** | Rare | Only when breaking changes are unavoidable |

### Release schedule

- Minor releases are cut on the first Tuesday of each month
- Patch releases are cut within 24-48 hours of the fix being merged
- Major releases are announced at least 4 weeks in advance via the Discord
  `#announcements` channel and GitHub Discussions

## Support tiers

| Release | Support |
|---------|---------|
| Latest minor | Full support: bug fixes and security patches |
| Previous minor | Security patches only |
| Older releases | No support -- upgrade to a supported version |

Only the latest minor version receives patches. Users on older versions must
upgrade to receive fixes.

Example: if `v1.3.0` is the latest release:
- `v1.3.x` receives bug fixes and security patches
- `v1.2.x` receives security patches only
- `v1.1.x` and earlier receive no updates

## Deprecation policy

### Timeline

Deprecated features follow a 2-minor-version notice period before removal:

1. **vX.0**: Feature is deprecated with a warning emitted at runtime/log time
2. **vX+1.0**: Warning continues; documentation marked as deprecated
3. **vX+2.0**: Feature is removed

### Deprecation warnings

When a workflow uses a deprecated API at runtime, the worker logs a warning:

```
[DEPRECATED] DurableCallWithoutOptions is deprecated since v0.6.0.
Use DurableCallWithOptions() with an explicit CallOptions value.
This API will be removed in v0.8.0.
```

### Communication

Deprecations are communicated via:

1. **CHANGELOG.md** -- listed under "Deprecated" in the release notes
2. **Worker runtime logs** -- warning on first use
3. **`cleat vet` output** -- warnings for deprecated patterns
4. **Discord #announcements** -- summary of deprecations in each release

## Release checklist

### For maintainers

Follow these steps for each release:

### 1. Cut the release branch

Release preparation happens on a branch off `develop`, never on `main` or
`develop` directly:

```bash
git fetch origin
git checkout -b release/vX.Y.Z origin/develop
```

From here until the release lands, only version bumps, CHANGELOG edits, and
bugfixes go on this branch. New features continue to land on `develop` and ship
in the next release.

### 2. Prepare the CHANGELOG

Open `CHANGELOG.md` and:

- Move all entries from the `## [Unreleased]` section to a new section for the
  release version
- Categorize entries as: `Added`, `Changed`, `Deprecated`, `Removed`, `Fixed`,
  `Security`
- Add a comparison link at the bottom of the file
- Verify the changelog follows [keepachangelog.com](https://keepachangelog.com/)
  format

### 3. Update version strings

Check for any hardcoded version strings in the codebase:

```bash
grep -r 'v[0-9]\+\.[0-9]\+\.[0-9]\+' --include="*.go" --include="*.rs" --include="*.mod" .
```

If any go.mod or version constants reference the old version, update them.
`--include="*.mod"` is not decoration: `cleat/go.mod`'s own `require
github.com/cleat-team/cleat vX.Y.Z` line is exactly this kind of reference
(cleat#1888 found it stuck at a version that was never even tagged), and the
pattern above missed it entirely without that flag.

That grep will not find the Homebrew formula, which is Ruby — but as of
cleat#2068 nothing here needs to bump it by hand.

**That grep is not the SDK version check, and does not stand in for one
(cleat#2454).** It reaches one of the six places an SDK version lives — the
Rust metadata stamper, via `--include="*.rs"` — and is structurally blind to
the Python, Java and AssemblyScript ones, and to `pyproject.toml`, because
none of those carry a bare `vX.Y.Z` in a `.go`/`.rs`/`.mod` file. Verify SDK
version agreement with the script written for exactly this:

```bash
scripts/check-sdk-version-agreement.py --expected <version>
```

For each SDK it compares the manifest against every stamper that embeds the
same version (see its docstring for the full list — e.g. python:
`python-sdk/pyproject.toml` against `python-sdk/cleat_sdk/version.py` and
`python-sdk/scripts/stamp_metadata.py`) and fails on any mismatch. The Go SDK
is deliberately excluded: `wasm/build.go` derives `sdkVersion` rather than
hardcoding it, so there is nothing for the script to compare.

It also will not find `python-sdk/pyproject.toml`, which is TOML
(`version = "0.2.0"`, no `v` prefix, so the pattern's own anchor can't see
it). Bump it by hand on the release branch before tagging. This one is not
optional the way the Homebrew formula isn't needed: as of cleat#2127,
`.github/workflows/publish-pypi.yml` reads `python-sdk/pyproject.toml`'s
version on every `v*` tag push and refuses to publish if it disagrees with
the tag — `::error::tag vX.Y.Z (version X.Y.Z) does not match
pyproject.toml's version ...`, failing the job before any upload runs. A
mismatch here is not a warning to notice later; it is a failed release. See
`CONTRIBUTING.md`'s "SDK versions: which numbers are load-bearing" for why
the Python version is load-bearing while the three inert `0.1.0`s (Rust,
Java, AssemblyScript) are deliberately left alone.

### Releasing a Homebrew formula bump — now automatic

`packaging/homebrew/Formula/cleat.rb.tmpl` in this repo is a **template**, not
an installable formula: its `url` and `sha256` are the literal placeholder
tokens `__CLEAT_TAG__` and `__CLEAT_SHA256__`. The installable formula lives
in `cleat-team/homebrew-tap` (`brew install cleat-team/tap/cleat`), and is
generated, not authored — `.github/workflows/release.yml`'s `homebrew-bump`
job, which runs after `goreleaser` on every `v*` tag push:

1. Computes the sha256 of `https://github.com/cleat-team/cleat/archive/refs/tags/vX.Y.Z.tar.gz`
   (GitHub's own auto-generated source archive for the tag — not one of
   goreleaser's build artifacts, so this does not depend on anything
   goreleaser produced beyond the tag itself existing).
2. Runs `scripts/render-homebrew-formula.sh vX.Y.Z <sha256>` to substitute
   the template's two placeholders.
3. Pushes the rendered file to `cleat-team/homebrew-tap`'s `Formula/cleat.rb`
   on `main`, over HTTPS using the `HOMEBREW_TAP_TOKEN` secret (a
   fine-grained PAT scoped to Contents: read-and-write on
   `cleat-team/homebrew-tap` only — deploy keys are disabled by org policy,
   so this cannot use SSH). Skips the push if the rendered file is identical
   to what is already there.

**This is hand-maintained on purpose exactly once: the template.**
goreleaser's `brews:` generator packages built binaries, and there is no
macOS `cleat-worker` binary to package — the worker needs CGO and the
release job cannot link a CGO darwin binary on ubuntu (see
`IMPROVEMENT-PLAN.md` §3.54). The formula is a source build, which is what
gives macOS a working worker at all, so it cannot be generated from the
release artifacts the way the rest of `.goreleaser.yml`'s output is. Editing
the template's install/test logic goes through a normal PR here, same as any
other file; only `url`/`sha256` are generated, and they never live in this
repo as real values, so there is nothing here to go stale between releases.

**`homebrew-bump` has two failures that look alike and have different remedies.**
Read the message before acting: get this wrong and you rotate a healthy token
without touching the actual fault.

| the job fails with | what it means | the remedy |
|---|---|---|
| `401`/`403` from the server | the credential expired or lost its scope | **rotate** — an owner regenerates a fine-grained PAT scoped identically (`cleat-team/homebrew-tap`, Contents: read and write, no other repos or permissions) and updates `HOMEBREW_TAP_TOKEN` on `cleat-team/cleat` |
| `fatal: unable to access '…': URL rejected: Malformed input to a URL function` | git rejected the **URL**, before sending any request — so the token's *value* is malformed, not its credential | **re-set the same token's value**, cleanly — below |

**Rotating ahead of a known expiry is still right** — fine-grained PATs always
expire and this one is no exception. Nothing below changes that; it is only about
telling that failure apart from one that looks like it.

`URL rejected: Malformed input to a URL function` **cannot be produced by a
`401` or a `403`.** Those are HTTP responses; this error happens while parsing
the URL, before a request exists. So the message alone says which case you are
in, and neither of the other two candidate causes — an expired token, a wrongly
scoped one — is involved.

That case is not hypothetical. Measured 2026-09-27 on v0.3.2 (run 36326029213,
attempt 1): the other three jobs succeeded, and within `homebrew-bump` the
checkout, the tarball hash and the formula render all succeeded — only the push
failed. The secret existed, unexpired, and correctly scoped.

**The malformation is stray whitespace or a control character — and not the
obvious one.** A trailing newline is the intuitive guess and it is wrong: git has
a case for that and reports it differently (`warning: url contains a newline in
its password component`). Verified by running each against the real tap URL — a
carriage return, a space or a tab produces the observed message; a newline does
not.

Re-set the value with:

```bash
printf '%s' '<token>' | gh secret set HOMEBREW_TAP_TOKEN -R cleat-team/cleat
```

`printf '%s'` and not `echo`, which appends the newline that this is so often
mistaken for.

**But note what is not recoverable.** Actions secrets are write-only, and a
fine-grained PAT's value is shown once, at creation. So "re-set the value" is
available only to someone who still holds it. If nobody does, a new PAT is the
only route — that is a rotation, and the first row above applies. Either way,
read the message before regenerating anything.

**Verifying it worked**, either by re-deriving the CI job's own steps
locally with a fake tag (the same check `packaging/homebrew/formula_test.go`'s
`TestRenderProducesAPinnedTaggedFormula` runs on every PR that touches this
area) or, after a real release, against what actually landed:

```bash
# Dry run: does the render mechanism itself work, with no tag or network needed?
scripts/render-homebrew-formula.sh v9.9.9 "$(printf '0%.0s' {1..64})"

# After a real release: did the tap actually get the new version?
gh api repos/cleat-team/homebrew-tap/contents/Formula/cleat.rb --jq '.content' \
  | base64 -d | grep -E '^\s*(url|sha256)\s'

brew style   cleat-team/tap/cleat
brew install cleat-team/tap/cleat
brew test    cleat && brew uninstall cleat
```

`brew test` runs `cleat-worker --verify-backend`, so it fails if the formula
produced a worker that cannot construct the wasmtime backend.

Testing a structural change to the formula itself, ahead of a release and
without touching the tap: `brew install --HEAD --build-from-source
packaging/homebrew/Formula/cleat.rb.tmpl` builds from the `head` line (the
`develop` branch), which never touches `url`/`sha256` at all.

### Releasing the Helm chart — also automatic

As of cleat#2099, `charts/cleat` is published as an OCI artifact to
`ghcr.io/cleat-team/charts/cleat`, versioned with the release, by
`.github/workflows/release.yml`'s `helm-chart-publish` job — which runs
after `goreleaser` on every `v*` tag push:

1. Derives the chart's `version`/`appVersion` from the tag with the leading
   `v` stripped (`vX.Y.Z` → `X.Y.Z`; Helm's chart `version` must be strict
   SemVer). These are passed to `helm package --version --app-version`,
   which overrides `Chart.yaml` at package time — the tracked file's
   `0.1.0` is never edited for a release.
2. Rewrites `charts/cleat/values.yaml`'s `image.tag` (also only in this
   ephemeral checkout, never committed) from `latest` to the release tag —
   `vX.Y.Z`, matching the tag the `cleat-worker` image was just pushed
   under in the `goreleaser` job. A bare `helm install` then deploys the
   worker this chart was tested against, not whatever `latest` resolves to
   later.
3. Runs `helm lint charts/cleat`, packages it, and `helm push`es the
   resulting `.tgz` to `oci://ghcr.io/cleat-team/charts`.

**Package visibility.** Same caveat as the `cleat-worker` image: ghcr.io
package visibility (public/private) is a repository/org setting, not
something this workflow controls. `packages: write` is enough for the push
to succeed; until an owner makes
`ghcr.io/cleat-team/charts/cleat` public, `helm install
oci://ghcr.io/cleat-team/charts/cleat` 403s for anyone not authenticated to
this org — check this once alongside the image's own visibility, not
per release.

**Verifying it worked**, either by rehearsing the packaging steps locally
with a fake version and no tag or network needed (the same thing the
`Release Dry Run` job in `ci.yml` does against a throwaway local registry,
see 4a below), or after a real release, against what actually landed:

```bash
# Dry run: does the chart even lint and package, with no tag needed?
helm lint charts/cleat
helm package charts/cleat --version 9.9.9 --app-version 9.9.9 -d /tmp/chart-dryrun

# After a real release: pull and inspect what actually got pushed.
helm pull oci://ghcr.io/cleat-team/charts/cleat --version X.Y.Z -d /tmp/chart-check
tar xzOf /tmp/chart-check/cleat-X.Y.Z.tgz cleat/values.yaml | grep -A2 '^image:'
```

The unpacked `values.yaml` should show `tag: vX.Y.Z`, not `latest`.

Installing it:

```bash
helm install cleat oci://ghcr.io/cleat-team/charts/cleat --version X.Y.Z
```

### 4. Run multi-database tests

Run the WorkflowStore test suite against all three supported backends to verify
that no regressions were introduced:

```bash
# Requires MySQL and SQL Server running locally or via Docker.
make test-all-dbs
```

Verify that all three migration directories are in sync:

```bash
ls migrations/postgres/ migrations/mysql/ migrations/mssql/
```

Each directory should contain the same set of migration files (adapted for
dialect syntax). If a migration is missing from one backend, add it before
proceeding with the release.

### 4a. The release build is already exercised — but know what it does not cover

The `Release Dry Run` job in `ci.yml` runs `goreleaser build --snapshot --clean` on **every
PR**, with the same `gcc-aarch64-linux-gnu` cross compiler and `setup-qemu-action` the
`Release` workflow uses. `goreleaser build` runs the same builds and the same post-build hooks
as `goreleaser release`, so `scripts/verify-release-worker.sh` executes both published
`cleat-worker` binaries with `--verify-backend` there too.

This exists because the release path had never executed before a tag. `.goreleaser.yml` built
`cleat-worker` with `CGO_ENABLED=0` for months, producing binaries that exited 1 at startup,
and nothing ran them (`IMPROVEMENT-PLAN.md` §3.54).

Reproduce locally with the same command:

```bash
goreleaser build --snapshot --clean     # output in dist/, which is gitignored
```

**What the dry run does not cover:** archive creation, checksums, the changelog, and the
GitHub upload — `build` stops before all of it. It also does not run the `Build Svelte UI` or
`Validate no dirty dist/` steps, so a stale dashboard is still only caught at tag time.

**It is not a required check.** `.github/required-checks.txt` mirrors branch protection;
making this blocking is a repository settings change.

The same job also rehearses `helm-chart-publish` (cleat#2099): `helm lint`, `helm
template`, `helm package`, and `helm push` against a throwaway registry
(`registry:2`, started in the job and torn down after) started on
`localhost`, not `ghcr.io` — this job carries no ghcr.io credentials and
should not need any to prove the packaging and push mechanics work. It does
not cover the real `ghcr.io` push, the tag-derived version numbers (it uses
a fixed `0.3.0-dryrun`), or package visibility — those are exercised for
real only by a tag, per the Helm section above.

### 5. Commit and open the release PR into `main`

```bash
git add CHANGELOG.md
git commit --signoff -m "release: vX.Y.Z"
git push -u origin release/vX.Y.Z

gh pr create --base main --head release/vX.Y.Z --title "release: vX.Y.Z"
```

Wait for `Build` and `Lint`, then merge — **with "Create a merge commit"**, not
squash. Squashing here discards the second parent and breaks the branch model;
see [Merge method per hop](#merge-method-per-hop).

### 6. Tag `main`

The tag goes on the merge commit that now sits at the head of `main`, and it is
the tag — not the merge — that publishes the release:

```bash
git fetch origin
git tag -a vX.Y.Z -m "Release vX.Y.Z" origin/main
git push origin vX.Y.Z
```

Tag `origin/main` explicitly rather than whatever your local checkout is on. The
annotated tag matters: GoReleaser reads its message.

### 7. Back-merge `main` into `develop`

`main` now merges into `develop`, carrying the CHANGELOG and version bump back so
the branches do not drift. **From `main`, not from the release branch** — the tag
sits on a commit that is on `main` and on no release branch, so this is the only
merge that makes it an ancestor of `develop`, which is what `#2058:155` verifies:

```bash
git merge-base --is-ancestor vX.Y.Z origin/develop   # must exit 0 after this step
gh pr create --base develop --head main --title "chore: back-merge main into develop"
```

Merging `main` rather than the release branch loses nothing: main's history
contains the release branch, so the version bump and the CHANGELOG arrive either
way, and only this shape brings the tag with them.

Merge this one **with "Create a merge commit"** as well — which the queue will not
let you do: it pins `develop` to `SQUASH`. Flip the ruleset to `MERGE` first and
restore it to `SQUASH` afterwards, per
[the back-merge flip](#the-back-merge-needs-an-admin-merge-method-flip). This step
is the one that gets skipped, and skipping it — by omission or by squash — is how
`main` and `develop` diverge; the two `git` commands in that section are how you
tell, and they are worth running before the next release rather than after.

**Check what the enqueue did, not what it said.** `gh pr merge` is the one command
here whose message and exit status are both uninformative, and **both directions are
measured** — this applies to every enqueue, not only this step:

- **The message is evidence in neither direction.** A *successful* enqueue prints
  `! The merge strategy for develop is set by the merge queue`. That is a
  refusal-shaped line, and it is what success looks like.
- **The exit status is evidence in neither direction either.** `gh pr merge` exits
  `0` on a refusal as well. The sharpest instance, from WS-1 (2026-09-27):
  enqueueing a **draft** PR printed `Pull request is a draft` and **exited 0**. A
  zero status on a refusal is the shape `gofmt -l && echo clean` has — the command
  reporting success about a thing it did not do.

So read the queue, which is the only view showing both the outcome and the
**position**:

```bash
gh api graphql -f query="{repository(owner:\"cleat-team\",name:\"cleat\"){
  pullRequest(number:<PR>){mergeQueueEntry{state position}}}}" \
  --jq '.data.repository.pullRequest.mergeQueueEntry
        | if . == null then "NOT IN QUEUE" else "\(.state) pos \(.position)" end'
```

An empty queue reads `NOT IN QUEUE` — which is also what a merged PR reads, hence the
next paragraph.

Position is not decoration: the queue evaluates your PR against **the entries ahead
of it**, not against `develop`, so a `CLEAN`/`MERGEABLE` PR can sit `UNMERGEABLE` at
position 3 because it collides with the one in front of it. An entry evicted for that
reason still reads `OPEN`, so "still queued" and "silently kicked out" are the same
reading if you poll only the PR state.

**And when you want the whole queue rather than one PR's entry, ask for the queue.**
`mergeQueueEntry` answers a question about a single PR and cannot show you what else is
waiting, so assembling the picture from it takes one call per PR — each answering a
smaller question than you asked. `mergeQueue` returns every entry with its position and
state together:

```bash
gh api graphql -f query='{repository(owner:"cleat-team",name:"cleat"){
  mergeQueue(branch:"develop"){entries(first:10){nodes{
    position state pullRequest{number}}}}}}' \
  --jq '.data.repository.mergeQueue.entries.nodes[]
        | "pos=\(.position) \(.state) #\(.pullRequest.number)"'
```

    pos=1 AWAITING_CHECKS #2536
    pos=2 AWAITING_CHECKS #2546
    pos=3 AWAITING_CHECKS #2543

The two are companions rather than alternatives: this is the only view that shows the
queue **as a queue**, which is what makes an entry's position readable in context.
**`branch` is an ARGUMENT here, not a field** — `mergeQueue.branch` errors, and it is
the shape a reader will try first.

**A long wait at position 1 is a CI pass, not a stall — and the queue's own runs are
where you see it.** The queue does not check your branch tip; it builds the merge commit
it *would* create and runs the checks against **that**, which is how it can tell whether
your PR survives the entries ahead of it — and, as below, that commit is the one that
lands:

```bash
gh run list --event merge_group --limit 20 \
  --json headBranch,status,name --jq '.[] | "\(.status)  \(.name)\n    \(.headBranch)"'
```

    queued  CI/CD Pipeline
        gh-readonly-queue/develop/pr-2543-340a862612222a9682c9a29c92cf616fb57ad76a
    completed  DCO Check
        gh-readonly-queue/develop/pr-2543-340a862612222a9682c9a29c92cf616fb57ad76a

Read that ref before drawing anything from it. It sits under `refs/heads/`, so **a
workflow gated on `refs/heads/main` will not match it** and will not run for a queued
PR — which is the point of the name.

**The `<sha>` in it is not the PR's head; it names the parent that this PR's merge
commit will have.** It is the commit the trial was merged *onto* — the head of the queue
at that moment, which for an entry behind another is the **pre-computed** merge of the
entry ahead of it, before that entry has landed.

That is checkable from the graph rather than only observable, which is what makes it a
mechanism: `pr-2536-8395ac2d…` corresponds to a merge commit whose parent is `8395ac2d`,
and `pr-2543-340a8626…` to one whose parent is `340a8626`. Compare it against the PR's
head and it will not match — `#2543`'s head was `0c8aeca1`, which its ref never named.

**And the trial merge is the commit that lands.** The queue raises the ref, runs the
checks against it, and that same commit reaches `develop` — measured on `#2536`, whose
queue ref pointed at `f01820b0` before merging and whose merge commit is `f01820b0`.

So the wait is a real CI pass rather than a formality, and the precise reason is worth
stating because the loose version is falsifiable: **the checks that GATE the merge are
the checks that apply to what ships.** Develop's ordinary push CI does run on the landed
commit too — measured on `f01820b0`, `gh run list` shows eight `push` runs against the
same SHA — but by then the merge has happened, so those gate nothing. It is the
`merge_group` runs that decided, and they decided against the commit that shipped.

**And an entry behind another is queued, not stalled.** Two PRs sitting at positions 2
and 3 while a third holds position 1 is the queue working — it evaluates one entry at a
time, against the entries ahead. **Position is the only field that says so:** `state`
reads `AWAITING_CHECKS` for every entry in the queue, including the one that is running,
so it cannot distinguish the head from the two waiting behind it.

**The `state` field has five values, and one of them reads like another.** Enumerate them
from the schema rather than by watching traffic — the type is the authority and it is one
call:

```bash
gh api graphql -f query='{__type(name:"MergeQueueEntryState"){enumValues{name description}}}'
```

| state | means |
|---|---|
| `QUEUED` | the entry has entered the queue |
| `AWAITING_CHECKS` | the queue is checking the trial merge |
| `MERGEABLE` | checks passed, the merge is imminent |
| `UNMERGEABLE` | **it will not merge as it stands** |
| `LOCKED` | the schema says only "currently locked" |

**`UNMERGEABLE` is the one worth knowing, because a reader would misread it as
`AWAITING_CHECKS`.** Both say "not merged yet"; only one of them is going to merge. That
is the same distinction `position` draws for a different pair, and it is the state
someone polling their own PR most needs to recognise — an entry that cannot merge looks
exactly like one that is still waiting unless you read this field.

**And a null is ambiguous, so read the PR state beside it.** A PR that has merged and
a PR that was never enqueued both return `null` for `mergeQueueEntry`:

```bash
gh pr view <PR> --json state,mergedAt     # OPEN + mergedAt=null + no entry == nothing happened
```

### 8. Verify CI

Pushing the tag triggers `.github/workflows/release.yml` (GoReleaser), which:

1. Builds `cleat`, `cleat-worker`, and `cleat-gen` for linux and darwin on
   amd64 and arm64 — **four archives, no Windows build.** `.tar.gz` for linux,
   `.zip` for darwin. Re-derive with:

   ```bash
   python3 -c "import yaml; d=yaml.safe_load(open('.goreleaser.yml')); \
     print(sorted({(g,a) for b in d['builds'] for g in b['goos'] for a in b['goarch']}))"
   ```
2. Bundles `LICENSE`, `README.md`, and the built dashboard (`web/dist/`) into
   each archive
3. Creates a GitHub Release with the archives and `checksums.txt` attached

Monitor the CI pipeline at:
https://github.com/cleat-team/cleat/actions

### 9. Verify the release

Once CI completes:

1. Navigate to https://github.com/cleat-team/cleat/releases/tag/vX.Y.Z
2. Verify:
   - Release title and description are correct
   - Binary assets are attached for all target platforms
   - Checksum file is present
   - Source code archive is attached
3. Smoke-test the install path **against a clean module cache**, so a locally
   cached copy cannot make a broken release look installable:

```bash
GOMODCACHE=$(mktemp -d) GOFLAGS=-mod=mod GOWORK=off \
  go install github.com/cleat-team/cleat/cmd/cleat@vX.Y.Z
cleat version  # should show vX.Y.Z
```

`GOWORK=off` matters: this repo has a committed `go.work`, and with the
workspace active `go install` resolves modules from the local tree rather than
from the published version, which is a green that measured nothing.

This step is not ceremony. `v0.1.0` was published and could not be installed at
all — `go install pkg@version` refuses any module whose `go.mod` carries a
`replace` directive, and the root module carried one until v0.2.0.

4. Verify the Helm chart published alongside it — see "Releasing the Helm
   chart" above for the full pull/inspect commands:

```bash
helm pull oci://ghcr.io/cleat-team/charts/cleat --version X.Y.Z -d /tmp/chart-check
```

### 9a. If the release run fails

`release.yml` runs four jobs. Their `needs:` lines are the whole story of what a
failure leaves behind:

| job | needs | publishes |
|---|---|---|
| `goreleaser` | — | the GitHub Release and its binary/.deb assets |
| `container-image` | `goreleaser` | `ghcr.io/cleat-team/cleat-worker` |
| `homebrew-bump` | `goreleaser` | the formula in `cleat-team/homebrew-tap` |
| `helm-chart-publish` | `goreleaser`, `container-image` | `oci://ghcr.io/cleat-team/charts/cleat` |

Read the two dependents side by side, because the difference is deliberate.
`homebrew-bump` needs nothing the image produces — it hashes GitHub's own
auto-generated tag archive — so it does not wait on the image.
`helm-chart-publish` pins the chart's `image.tag` to this release, so it does.

Until cleat#2488 the image push was the last *step* of the `goreleaser` job, so
a ghcr.io failure failed that job and skipped both dependents. Measured on
v0.3.1 (run 36313519805): the tap still pinned `v0.2.0`, and both `Bump the
Homebrew tap` and `Publish the Helm chart` were skipped, for a release whose
nine binary assets were all present.

**Re-run first.** "Re-run failed jobs" in the Actions UI is the recovery path
that is always available, including on the release that has just failed:

```bash
gh run rerun <run-id> --failed
```

**The dispatch route, and its one delay.** `Release` also takes a
`workflow_dispatch`, which covers what a re-run cannot — a run older than 30
days or deleted, and a tag pushed with a token that does not trigger workflows,
so that no run exists to re-run:

```bash
gh workflow run Release --ref vX.Y.Z
```

Dispatch **on the tag**, never on a branch. Every job takes the version from
`GITHUB_REF_NAME`, and a dispatch sets that to the ref it was dispatched on — so
a dispatch on `main` hands `main` to every tag check in the file. The
`goreleaser` job's first step now refuses that by name; before cleat#2488 it
validated the tag nowhere at all, because it relied entirely on the
`push: tags:` trigger that a dispatch does not go through.

**That command will 404 at first, and it is not a broken workflow.** GitHub
serves a `workflow_dispatch` only once the workflow is registered with that
trigger on the **default branch** — and this repository's default branch is
`develop`, not `main`. The trigger is on `main` from cleat#2490, but it reaches
`develop` only at the next back-merge. Until then the re-run above is the whole
recovery path, and a 404 here means "not registered yet", not "not configured".

Re-running is safe now where it was not before. A run that failed after the
upload stage leaves the tag's assets already attached to the Release, and the
retry used to die on the first one:

```
422 Validation Failed [{Resource:ReleaseAsset Field:name Code:already_exists}]
```

`release.replace_existing_artifacts: true` in `.goreleaser.yml` deletes the
asset it collided with and retries the upload. It is not a wipe of the release:
only the colliding asset goes, and only once an upload has already failed.
Measured on v0.3.1's second attempt — all nine assets, then `release failed
after 1m0s ... failed to publish artifacts`.

**What this cannot do.** A fix reaches a tag only by being *in* the tag. A
re-run and a dispatch both execute the workflow file, and read
`.goreleaser.yml`, as they exist at the ref being run, so neither can retro-fit
a tag cut before a fix landed. Checked 2026-09-27: neither `v0.3.0` nor
`v0.3.1` carries `workflow_dispatch` or `replace_existing_artifacts`, so
v0.3.1's own failure is not repairable by dispatching it. That case needs a new
patch tag, or the missing steps run by hand.

**Establish what a partial release actually left; do not assume.**

```bash
# The Release and its assets.
gh release view vX.Y.Z --json assets --jq '.assets[].name'

# The tap. No credentials needed; prints the version it is pinned to.
gh api repos/cleat-team/homebrew-tap/contents/Formula/cleat.rb \
  --jq '.content' | base64 -d | grep -E 'url|sha256'

# The chart.
helm pull oci://ghcr.io/cleat-team/charts/cleat --version X.Y.Z -d /tmp/chart-check
```

The **container image cannot be checked from a normal checkout**, and it fails
quietly rather than loudly: an anonymous `docker manifest inspect
ghcr.io/cleat-team/cleat-worker:vX.Y.Z` returns `denied` for a package that is
private *and* for one that does not exist. Verified 2026-09-27 against a
deliberately impossible package name, which returned the identical `denied` —
so that output is not evidence either way. Check it with an authenticated
`docker login ghcr.io`, or ask an owner.

### 10. Announce

Post in Discord `#announcements`:

```
Release vX.Y.Z is now available!

Highlights:
- Feature 1: brief description
- Feature 2: brief description
- Bug fix: brief description

Install: go install github.com/cleat-team/cleat/cmd/cleat@latest
Release notes: https://github.com/cleat-team/cleat/releases/tag/vX.Y.Z
```

Update the `#release-notes` thread with the changelog.

## Hotfix releases

For critical security fixes or production outages, a hotfix patch release may
be cut outside the normal cadence:

A hotfix is the one branch cut from `main` rather than `develop` — that is what
lets it ship without dragging in whatever `develop` has accumulated since the
last release.

```bash
git fetch origin
git checkout -b hotfix/worker-panic-on-nil-input origin/main
```

1. Apply the fix, add a regression test, and bump the PATCH version in
   `CHANGELOG.md`. Note the branch prefix is `hotfix/` — `hotfix-vX.Y.Z` fails
   `Validate branch name`, and a PR's head branch cannot be renamed, so the
   mistake costs a close-and-reopen.
2. Open a PR into `main`, merge it **with a merge commit**, then tag as in
   step 6 above.
3. Open a second PR from the same branch into `develop` and merge it **with a
   merge commit**.

Step 3 is a merge, not a cherry-pick. An earlier version of this document said
"cherry-pick the fix into `main` after release", which was the only thing
possible while the repo was squash-only — a cherry-pick makes a *copy* of the
commit, so git cannot tell that the two branches carry the same fix and every
subsequent release re-presents it as a conflict. Merging records the shared
ancestry once and the question does not come back.

## Breaking change policy

### Communication timeline

| When | Action |
|------|--------|
| 4+ weeks before release | RFC submitted to `rfcs/` with "Proposed" status |
| RFC acceptance | Breaking change is accepted with migration plan |
| Release - 2 weeks | Announcement in Discord #announcements and GitHub Discussions |
| Release date | Breaking change ships in a MAJOR version bump |

### What to include in the breaking change announcement

- What is changing and why
- Exact migration steps (code examples)
- Timeline for old-API removal
- Migration tooling or codemods, if available
- Who to contact with questions

### Migration path requirements

Every breaking change MUST provide:

1. A documented migration path in the release notes
2. A grace period where old and new APIs coexist (at least one minor version)
3. A `cleat vet` check that flags deprecated usage
4. An automated migration tool or script where practical

### Exceptions

Immediate breaking changes (no grace period) are allowed only for:

- **Security fixes** that cannot be backported
- **Data corruption fixes** where the old behavior produces incorrect results
- **Legal/compliance requirements**

Any exception must be approved by 2/3 of maintainers and announced with a
clear explanation.
