#!/usr/bin/env bash
# Gathers the PR body, closingIssuesReferences and commit messages, then hands
# off to check-negated-closing-references.py -- the actual text analysis
# lives there (see its own docstring), since a guard that reasons about
# negation and clause boundaries should not be a shell script.
#
# ALL THREE ARE READ LIVE, via `gh pr view`, in this one process -- none of
# them is passed in through env from the workflow step. That is not the
# original design: PR_BODY was meant to come through the step's own `env:`
# (github.event.pull_request.body), matching check-closing-references.sh.
# GitHub Actions step-level `env:` does not carry across steps, so it was
# simply unset in this step, and this script's own `${PR_BODY-}` silently
# turned "unset" into an empty string -- which passed Python's `is None`
# UNMEASURED guard, so the check ran, found nothing (the body was never
# there to search), and reported OK. Found by cleat-review on #2703,
# measured directly: with PR_BODY supplied it flags #2154 correctly; with it
# unset, exactly as the real workflow runs, the same head's script reports
# clean. Reading it live removes the cross-step wiring entirely, the same
# way this script already reads commits rather than relying on git history
# being checked out.
set -euo pipefail

: "${PR_NUMBER:?PR_NUMBER is required}"
: "${REPO:?REPO is required}"

pr_body=$(gh pr view "$PR_NUMBER" --repo "$REPO" --json body --jq '.body // ""')

closing_issues=$(gh pr view "$PR_NUMBER" --repo "$REPO" \
  --json closingIssuesReferences --jq '[.closingIssuesReferences[].number]')

# messageHeadline + messageBody, one commit per line pair, joined so a
# negation split across the two (rare, but the two are one message to a
# human reader) is not hidden by only reading one field.
commit_messages=$(gh pr view "$PR_NUMBER" --repo "$REPO" \
  --json commits --jq '[.commits[] | .messageHeadline + "\n" + .messageBody] | join("\n")')

export PR_BODY="$pr_body"
export COMMIT_MESSAGES="$commit_messages"
export CLOSING_ISSUES="$closing_issues"
exec python3 "$(dirname "$0")/check-negated-closing-references.py"
