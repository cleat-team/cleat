#!/usr/bin/env bash
# Gathers closingIssuesReferences and commit messages for the PR, then hands
# off to check-negated-closing-references.py -- the actual text analysis
# lives there (see its own docstring), since a guard that reasons about
# negation and clause boundaries should not be a shell script.
#
# PR_BODY comes from the SAME env source as check-closing-references.sh
# (github.event.pull_request.body, through env: rather than interpolated into
# run:, since a PR body is attacker-controlled text). closingIssuesReferences
# and the commit messages are not in the pull_request webhook payload at all
# -- both require a live API read via `gh pr view`.
set -euo pipefail

: "${PR_NUMBER:?PR_NUMBER is required}"
: "${REPO:?REPO is required}"

closing_issues=$(gh pr view "$PR_NUMBER" --repo "$REPO" \
  --json closingIssuesReferences --jq '[.closingIssuesReferences[].number]')

# messageHeadline + messageBody, one commit per line pair, joined so a
# negation split across the two (rare, but the two are one message to a
# human reader) is not hidden by only reading one field.
commit_messages=$(gh pr view "$PR_NUMBER" --repo "$REPO" \
  --json commits --jq '[.commits[] | .messageHeadline + "\n" + .messageBody] | join("\n")')

export PR_BODY="${PR_BODY-}"
export COMMIT_MESSAGES="$commit_messages"
export CLOSING_ISSUES="$closing_issues"
exec python3 "$(dirname "$0")/check-negated-closing-references.py"
