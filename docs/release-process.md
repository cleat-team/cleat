# Cleat Release Process

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

### 1. Prepare the CHANGELOG

```bash
git checkout main
git pull origin main
```

Open `CHANGELOG.md` and:

- Move all entries from the `## [Unreleased]` section to a new section for the
  release version
- Categorize entries as: `Added`, `Changed`, `Deprecated`, `Removed`, `Fixed`,
  `Security`
- Add a comparison link at the bottom of the file
- Verify the changelog follows [keepachangelog.com](https://keepachangelog.com/)
  format

### 2. Update version strings

Check for any hardcoded version strings in the codebase:

```bash
grep -r 'v[0-9]\+\.[0-9]\+\.[0-9]\+' --include="*.go" --include="*.rs" .
```

If any go.mod or version constants reference the old version, update them.

### 3. Commit and tag

```bash
git add CHANGELOG.md
git commit -m "Release vX.Y.Z"

# Create an annotated tag
git tag -a vX.Y.Z -m "Release vX.Y.Z"
```

### 4. Push tag

```bash
git push origin main
git push origin vX.Y.Z
```

### 5. Verify CI

Pushing the tag triggers the CI pipeline (GoReleaser), which:

1. Builds binaries for all target platforms (linux/amd64, linux/arm64,
   darwin/amd64, darwin/arm64, windows/amd64)
2. Creates a GitHub Release with the binaries and checksums attached
3. Publishes the release to the GitHub Release page

Monitor the CI pipeline at:
https://github.com/rcownie/cleat/actions

### 6. Verify the release

Once CI completes:

1. Navigate to https://github.com/rcownie/cleat/releases/tag/vX.Y.Z
2. Verify:
   - Release title and description are correct
   - Binary assets are attached for all target platforms
   - Checksum file is present
   - Source code archive is attached
3. Run the release binary locally for a smoke test:

```bash
go install github.com/rcownie/cleat/cmd/cleat@vX.Y.Z
cleat version  # should show vX.Y.Z
```

### 7. Announce

Post in Discord `#announcements`:

```
Release vX.Y.Z is now available!

Highlights:
- Feature 1: brief description
- Feature 2: brief description
- Bug fix: brief description

Install: go install github.com/rcownie/cleat/cmd/cleat@latest
Release notes: https://github.com/rcownie/cleat/releases/tag/vX.Y.Z
```

Update the `#release-notes` thread with the changelog.

## Hotfix releases

For critical security fixes or production outages, a hotfix patch release may
be cut outside the normal cadence:

1. Create a branch from the latest tag: `git checkout -b hotfix-vX.Y.Z+1 vX.Y.Z`
2. Apply the fix and bump the PATCH version
3. Follow the standard release checklist (steps 1-7)
4. Cherry-pick the fix into `main` after release

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
