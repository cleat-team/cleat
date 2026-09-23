#!/usr/bin/env bash
# A pull request's closing keyword must name its issue as `#N`, not `cleat#N`.
#
# THE FAILURE THIS PREVENTS. GitHub links an issue to a pull request -- and
# closes it when the PR merges into the default branch -- only for `#N` or the
# full `owner/repo#N`. `cleat#N` is neither: it renders as text, links nothing,
# and the issue stays open after the fix lands. Nothing goes red, so each one is
# found later by someone asking why a fixed issue is still open.
#
# Measured 2026-09-23 against closingIssuesReferences, which is GitHub's own
# answer to "what will this PR close":
#
#   gh pr view <N> --json body,closingIssuesReferences \
#     --jq '[.closingIssuesReferences[].number]'
#
#   #2061  "Closes #1973"        -> [1973]   linked
#   #2054  "Fixes cleat#2039"    -> []       merged 15:12Z; #2039 still open
#   #2056  "Fixes cleat#2055"    -> []       merged 15:12Z; #2055 closed BY HAND
#                                            25s later (its ClosedEvent has no closer)
#   #2062  "Closes cleat#2044"   -> []       open
#
# That day's release tracker (#2058) carried a "hygiene" section listing seven
# fixed-but-open issues to verify and close by hand, all of this shape.
#
# WHAT IS FLAGGED. A closing keyword -- close/closes/closed, fix/fixes/fixed,
# resolve/resolves/resolved, any case, optional colon -- followed by `<name>#N`
# where <name> has no slash. That is `cleat#N`, and equally `cleat-ports#N`,
# which links nothing either. `owner/repo#N` has a slash and does link, so it
# passes. A bare `cleat#N` in prose with no keyword in front is a mention, not
# a claim to close anything, and passes.
#
# The body arrives in PR_BODY, set by the workflow from the event payload
# through `env:` rather than interpolated into the script: a PR body is
# attacker-controlled text, and `${{ }}` inside `run:` is shell injection.
#
# Usage:
#   PR_BODY="$(gh pr view <N> --json body --jq .body)" scripts/check-closing-references.sh
set -euo pipefail

body="${PR_BODY-}"
if [ -z "$body" ]; then
  echo "PR body is empty: no closing references to check."
  exit 0
fi

# grep exits 1 on no match; under pipefail that would abort the script, so the
# no-match case is taken explicitly rather than swallowed with `|| true` around
# the whole pipeline (which would also hide a grep that failed to run).
if ! bad=$(printf '%s\n' "$body" |
  grep -oiE '\b(close[sd]?|fix(e[sd])?|resolve[sd]?):?[[:space:]]+[^[:space:]/#]+#[0-9]+'); then
  echo "No closing keyword names an issue as <repo>#N."
  exit 0
fi

echo "============================================"
echo "  CLOSING REFERENCE CHECK — FAILED"
echo "============================================"
echo ""
echo "These closing references link nothing, so the issue will NOT close when"
echo "this PR merges:"
echo ""
printf '%s\n' "$bad" | sed 's/^/  /'
echo ""
echo "Write the issue as #N (or owner/repo#N), e.g.:"
printf '%s\n' "$bad" | sed -E 's/[^[:space:]/#]+#([0-9]+)$/#\1/; s/^/  /'
echo ""
echo "Edit the PR description; this check re-runs on the edit. Confirm with:"
echo "  gh pr view <N> --json closingIssuesReferences --jq '[.closingIssuesReferences[].number]'"
exit 1
