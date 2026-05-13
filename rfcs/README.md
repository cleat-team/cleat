# Cleat RFC Process

The RFC (Request for Comments) process is the way to propose, discuss, and
adopt significant changes to the cleat project. It ensures that changes are
well-designed, documented, and have community consensus before implementation.

## When to write an RFC

Use the RFC process for changes that:

- Introduce a new public API (HostCalls methods, CLI commands, config options)
- Change existing API semantics in a breaking way
- Add new dependencies or infrastructure
- Change the database schema in a non-backward-compatible way
- Change the WASM boundary (host function imports/exports)
- Introduce new workflow semantics (child workflows, signals, versioning)
- Remove or deprecate existing features

Small bug fixes, documentation improvements, and test additions do not need an
RFC. When in doubt, open a GitHub issue first to discuss.

## Process

### Step 1: Open a GitHub Issue

Open an issue on the [cleat GitHub repository](https://github.com/cleat-team/cleat/issues)
using the RFC template. Include:

- A clear title prefixed with `RFC:` (e.g., `RFC: Task routing for worker pools`)
- A filled-out RFC template (copy from `rfcs/000-template.md`)
- Any supporting context (links to related issues, prior art, performance data)

### Step 2: Discussion

The issue will be discussed in the comments for a minimum of:

- **1 week** for medium-impact changes
- **2 weeks** for major changes

During this period, the author may update the proposal based on feedback. The
goal is to build rough consensus that the change is desirable and the design is
sound.

### Step 3: Build consensus

The author should:

- Respond to all questions and concerns raised in comments
- Update the RFC text in the issue description to reflect consensus
- Identify and resolve open questions
- List any dissenting opinions and why they were not addressed

Consensus does not mean unanimity. It means that the maintainers and community
agree that the proposal is an improvement over the status quo, even if not
perfect.

### Step 4: Submit a PR with the RFC

Once discussion has converged:

1. Copy the RFC into the `rfcs/` directory as a numbered markdown file
   (e.g., `rfcs/001-task-routing.md`)
2. Set the status to "Proposed"
3. Submit a pull request adding the file

The PR title should match the RFC title. The PR description should link to the
original discussion issue.

### Step 5: Maintainer vote

Maintainers will vote on the RFC within 2 weeks of the PR being submitted:

| Change severity | Threshold | Examples |
|----------------|-----------|----------|
| Major | 2/3 of maintainers | Breaking API changes, schema changes, new dependencies |
| Medium | Simple majority | Non-breaking API additions, new CLI flags, config options |
| Minor | Lazy consensus (no objections in 1 week) | Documentation-only RFCs, internal refactors with no user impact |

Votes are cast as PR reviews. Maintainers may request changes or approve
conditionally.

### Step 6: Accepted

If approved:

1. Update the RFC status to "Accepted"
2. Merge the PR
3. Create one or more GitHub issues tracking the implementation
4. If the implementation spans multiple PRs, create an epic to track them

### Step 7: Implementation

Implementation follows the normal PR process:

- Link implementation PRs to the RFC issue
- Reference the RFC file path in implementation PR descriptions
- Update the RFC status to "Implemented" when all tracked issues are resolved

## RFC numbering

RFC files are numbered sequentially. The next available number is the current
highest number in the `rfcs/` directory plus one. The `000-template.md` file
serves as the template and is not an actual RFC.

## Status definitions

| Status | Meaning |
|--------|---------|
| Draft | Author is still iterating on the design |
| Proposed | Ready for community review and discussion |
| Accepted | Maintainers have approved; implementation may begin |
| Rejected | Proposal was evaluated and declined by maintainers |
| Implemented | Changes are merged and released |
| Obsolete | Superseded by a newer RFC or no longer relevant |

## Fast-track

Minor, non-controversial changes may be fast-tracked:

- The discussion period may be shortened to 3 days
- The maintainer vote may be skipped in favor of lazy consensus
- Fast-track requests must be explicitly called out in the RFC and approved by
  at least one maintainer

Examples of fast-track candidates: clarifying documentation, adding optional
config fields with safe defaults, renaming internal methods.
