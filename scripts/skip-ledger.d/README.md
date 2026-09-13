# `skip-ledger.d` — one file per declaration, so two streams never collide

Every `*.tsv` in this directory is read by `scripts/check-skip-budget.sh`
exactly as if its lines were in `scripts/skip-ledger.tsv`. Same format, same
meaning, same checks:

    <job key><TAB><count><TAB><test-name regex><TAB><why>

## Put new declarations here, not in the single file

A shared file is a shared counter. Two streams appending to
`scripts/skip-ledger.tsv` conflict on every concurrent merge — cleat#1333 —
and `merge=union` in `.gitattributes` fixes that for `git merge` and
`git rebase` but **not on GitHub**, whose server-side mergeability does not
consult `.gitattributes`. Measured on one pair of heads, cleat#1395:

| | verdict |
|---|---|
| local merge, `.gitattributes` present | clean |
| local merge, `.gitattributes` removed | CONFLICT |
| GitHub, the same two heads | CONFLICTING / DIRTY |

Two streams writing *different files* have nothing to conflict over, anywhere.

## Naming

Name the file after the test it declares, lowercased, non-alphanumerics as
hyphens:

    scripts/skip-ledger.d/test-a-promise-is-settled-by-any-holder-of-its-id.tsv

Two streams declaring **the same test** then land on the same filename, which
is a real disagreement and should conflict. Two streams declaring different
tests cannot collide.

A dialect-gated engine test usually needs **two** lines — `test-go/engine` and
`cluster`, because the cluster job runs `./engine/...` too. Both go in the one
file for that test.

## The single file is not deprecated

`scripts/skip-ledger.tsv` is still read, and its existing lines are staying
there. Each is a deliberated declaration; rewriting them wholesale would be
churn with a real chance of dropping one, and would conflict with every open PR
that touches the ledger — causing the exact problem this directory removes.

## What still catches a mistake

`check-skip-budget.sh` rejects two lines sharing a `(job key, test regex)`
wherever they live, so a fragment that restates a line already in the single
file fails rather than silently doubling that job's budget.

And every line is checked on its own: a line matching no skip in the run is
reported, so a fragment for a test that was renamed or stopped skipping does
not sit here unnoticed.

## Trailing newlines are handled for you, but know why it mattered

The reader uses `awk 1`, not `cat`. `cat` concatenates bytes, so a fragment
whose last line lacks a trailing newline — which plenty of editors produce and
git commits without complaint — is joined to the first line of the next file,
and the joined line matches no job.

The damage lands on a **different file** from the one with the defect: the
fragment with the missing newline is fine, and the *next* one's declaration
disappears. A budget silently loses a grant nobody removed.

`check-skip-budget.sh --self-test` has a control for this, and it fails if the
reader goes back to `cat`.
