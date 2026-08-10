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
| `release/*` -> `develop` | **Merge commit** | Carries the version bump and CHANGELOG back. |
| `hotfix/*` -> `main` | **Merge commit** | Same as a release. |
| `hotfix/*` -> `develop` | **Merge commit** | The fix reaches develop by merging, never by cherry-pick. |

GitHub cannot enforce a merge method per target branch — both squash and merge
commit are enabled repo-wide, so the table above is convention and it is on the
merger to pick the right one from the dropdown. **Squashing a release or hotfix
PR silently breaks the model**: it discards the second parent, and `main` stops
being a descendant of anything.

Rebase merging is disabled. Re-derive the settings with:

```bash
gh api repos/cleat-team/cleat \
  --jq '{merge:.allow_merge_commit,squash:.allow_squash_merge,rebase:.allow_rebase_merge}'
# -> {"merge":true,"squash":true,"rebase":false}      (2026-08-10)
```

### Branch protection

Both `main` and `develop` are protected with `enforce_admins: true` and force
pushes disabled, so **nothing reaches either branch except through a pull
request** — including for admins. The release checklist below is written against
that fact; any instruction telling you to commit or push directly to `main` is
wrong.

| Branch | Required checks | Re-derive |
|--------|-----------------|-----------|
| `main` | `Build`, `Lint` | `gh api repos/cleat-team/cleat/branches/main/protection --jq .required_status_checks.contexts` |
| `develop` | 32 contexts | `gh api repos/cleat-team/cleat/branches/develop/protection --jq '.required_status_checks.contexts \| length'` |

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
grep -r 'v[0-9]\+\.[0-9]\+\.[0-9]\+' --include="*.go" --include="*.rs" .
```

If any go.mod or version constants reference the old version, update them.

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

### 7. Back-merge into `develop`

The same release branch now merges into `develop`, carrying the CHANGELOG and
version bump back so the branches do not drift:

```bash
gh pr create --base develop --head release/vX.Y.Z --title "chore: back-merge release vX.Y.Z into develop"
```

Merge this one **with "Create a merge commit"** as well. This step is the one
that gets skipped, and skipping it is how `main` and `develop` diverge — which
is exactly the state PR #466 had to repair.

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
