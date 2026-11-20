# changelog.d: one file per change

A change that ships adds ONE file here instead of editing `CHANGELOG.md`. Two PRs can no longer conflict on the changelog, because they no longer
touch the same file: that was three rebase-and-re-review rounds in one day (2026-11-19) and a duplicate entry on develop.

## The file

Named `<issue>-<slug>.md`: the issue (or, when there is none, the PR) number, a hyphen, a lowercase slug. If one issue needs two entries, the slugs
differ. The name is the collision-avoidance, so use the number you actually have.

    changelog.d/4113-side-effect-freezes-a-value.md

```
---
section: fixed
---
- **`side_effect` freezes a value, as the migration guide already said it did (cleat#4113).** The host compared the value a guest ... (one entry,
  as long as it needs to be; continuation lines are indented two spaces, and nothing else is in the body)
```

`section` is one of the headings `CHANGELOG.md` already has under a release, in this order when folded:

| `section:` | heading | for |
|---|---|---|
| `upgrade` | `### UPGRADE NOTES` | a breaking change or anything an operator must do before or after upgrading |
| `added` | `### Added` | new capability |
| `changed` | `### Changed` | a behaviour change that is not a fix |
| `fixed` | `### Fixed` | a defect fixed |
| `removed` | `### Removed` | something taken out |

An `upgrade` entry belongs ONLY in `upgrade`: it is what a reader of a release reads first, so do not also add it under `fixed`.

The body is the entry as it will read in `CHANGELOG.md`, starting with `- **`. Write in the past tense (what the defect WAS), which survives its own
fix; and give a number a date and the command that re-derives it, as everywhere else in this repo.

## When no fragment is needed

A PR that changes only tests, docs, CI, scripts or tooling needs none: the guard classifies the changed paths. If a path it counts as shipped code
should not carry an entry (a pure refactor, a comment), say so in the PR body on a line of its own:

    no-changelog: <the reason, one line>

The reason is required. The guard prints which files it counted as shipped when it fails, so the escape is a decision you can see.

## What the guard refuses

`scripts/check-changelog-fragments.py` (Lint):
1. a malformed fragment: bad name, no `section:`, an unknown section, a body that does not start with `- `, or an empty body;
2. a PR that edits `CHANGELOG.md` directly. The exception is a PR that FOLDS: it deletes at least one fragment in the same diff, or it comes from a
   `release/*` or `hotfix/*` branch;
3. a PR that changes shipped code and adds no fragment and carries no `no-changelog:` line.

The one other exception is correcting an entry that already exists (a typo, a duplicate): put `changelog-edit: <reason>` on a line of its own in the PR body.

In a merge-queue run the PR body is not available, so rules 1 and 2 run and rule 3 is reported as `checked on the pull request`.

## Grandfathered PRs (temporary)

A pull request numbered **below #4191** was opened before this scheme and may still edit `CHANGELOG.md` directly without a fragment (the guard prints
`GRANDFATHERED`). New PRs, and old ones that rebase anyway, use fragments. The cutoff is `GRANDFATHER_BELOW` in `scripts/check-changelog-fragments.py`;
**remove it, and its self-test cases, once those PRs have merged, at the 0.6.0 release fold at the latest.** A merge-queue run reads the PR number from its
head ref (`gh-readonly-queue/<base>/pr-<N>-<sha>`); a PR whose number cannot be established is NOT grandfathered.

## Folding at release prep

    scripts/fold-changelog.py --dry-run                 # print what would be folded; change nothing
    scripts/fold-changelog.py                           # fold into `## [Unreleased]`, delete the fragments
    scripts/fold-changelog.py --release 0.6.0 --date 2026-12-01   # ... and cut the release heading

Entries go at the TOP of their section, newest issue number first, and the order is the same on every machine. The fold is its own PR: it is the
only PR that edits `CHANGELOG.md`, so it cannot conflict with a feature PR.

## Backports

A backport onto a `release/*` branch carries its fragment with the cherry-pick (the file is part of the commit) and the release branch folds its own
fragments at its own release prep. A fix that ships in both 0.5.x and 0.6.0 therefore appears in both changelogs, which is accurate. Do not delete a
fragment on `develop` because it was backported.
