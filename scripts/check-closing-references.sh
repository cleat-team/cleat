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
# where <name> is a repo-name-shaped run of letters, digits, `_`, `.` or `-`,
# with no slash. That is `cleat#N`, and equally `cleat-ports#N`, which links
# nothing either. `owner/repo#N` has a slash and does link, so it passes. A
# bare `cleat#N` in prose with no keyword in front is a mention, not a claim to
# close anything, and passes. So does anything inside code (below).
#
# cleat#2800: <name> was `[^[:space:]/#]+` until this fix -- excluding only
# space, slash and `#`, so it matched ANY single non-space/slash/hash
# character immediately before `#N`, including `(`. "Fix (#2003)" -- a
# CORRECT reference, deliberately parenthesized -- parsed the `(` as a
# malformed repo name and flagged it. Measured against the fixed class:
#
#   echo 'Fix (#2003) later'    | grep -oiE '\b(close[sd]?|fix(e[sd])?|resolve[sd]?):?[[:space:]]+[A-Za-z0-9_.-]+#[0-9]+'
#   # (no output, correctly)    vs the old [^[:space:]/#]+ class: "Fix (#2003"
#
# A repo-name charset can't produce this false positive: GitHub repo and owner
# names are letters, digits, `_`, `.` and `-` (see --self-test below for the
# fixture that pins this and the cases that must still be caught).
#
# The body arrives in PR_BODY, set by the workflow from the event payload
# through `env:` rather than interpolated into the script: a PR body is
# attacker-controlled text, and `${{ }}` inside `run:` is shell injection.
#
# --self-test asserts on the TEXT of the verdict, not only on the exit status:
# a self-test that only checked "did this exit 1" would have passed the
# broken class too, since "Fix (#2003)" being (wrongly) exit-1 IS a verdict,
# just the wrong one -- the fixture's expected substring is what tells the two
# apart.
#
# Usage:
#   PR_BODY="$(gh pr view <N> --json body --jq .body)" scripts/check-closing-references.sh
#   scripts/check-closing-references.sh --self-test
set -euo pipefail

# Runs the check against $1 (a PR body) and prints the same report the real
# invocation would. Returns 0 (nothing to flag) or 1 (a bad reference found).
check_body() {
  local body="$1"

  # Code is not a claim. GitHub links nothing inside a fenced block or an inline
  # code span, so `Closes cleat#N` written as code -- a PR QUOTING the bad form,
  # as this check's own PR did, and failed its own first run for it -- is inert
  # and must pass. Both are removed before matching; unclosed fences and
  # backticks are left alone, which errs toward flagging.
  local prose
  prose=$(printf '%s\n' "$body" | perl -0pe 's/^[ \t]*```.*?^[ \t]*```[^\n]*$//gms; s/`[^`\n]*`//g')

  # grep exits 1 on no match; under pipefail that would abort the script, so the
  # no-match case is taken explicitly rather than swallowed with `|| true` around
  # the whole pipeline (which would also hide a grep that failed to run).
  local bad
  if ! bad=$(printf '%s\n' "$prose" |
    grep -oiE '\b(close[sd]?|fix(e[sd])?|resolve[sd]?):?[[:space:]]+[A-Za-z0-9_.-]+#[0-9]+'); then
    echo "No closing keyword names an issue as <repo>#N."
    return 0
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
  printf '%s\n' "$bad" | sed -E 's/[A-Za-z0-9_.-]+#([0-9]+)$/#\1/; s/^/  /'
  echo ""
  echo "Edit the PR description; this check re-runs on the edit. Confirm with:"
  echo "  gh pr view <N> --json closingIssuesReferences --jq '[.closingIssuesReferences[].number]'"
  return 1
}

self_test() {
  local failures=0

  # name | body | want exit (0 pass, 1 flagged) | a substring the output must contain
  local -a cases=(
    "correct paren-wrapped reference (cleat#2800)|Fix (#2003) later.|0|No closing keyword"
    "correct reference, no parens|Fixes #2003.|0|No closing keyword"
    "bare cleat#N must still be caught|Fixes cleat#2039.|1|cleat#2039"
    "cleat-ports#N must still be caught|Closes cleat-ports#50.|1|cleat-ports#50"
    "owner/repo#N has a slash and links|Closes cleat-team/cleat#123.|0|No closing keyword"
    "a bare mention with no keyword passes|See cleat#2003 for context.|0|No closing keyword"
    "code-quoted bad form is inert|Explains \`Closes cleat#N\` as an example.|0|No closing keyword"
  )

  local case name body want_exit want_substr got_out got_exit
  for case in "${cases[@]}"; do
    IFS='|' read -r name body want_exit want_substr <<<"$case"
    got_exit=0
    got_out=$(check_body "$body") || got_exit=$?
    if [ "$got_exit" != "$want_exit" ]; then
      echo "FAIL [$name]: exit=$got_exit want=$want_exit"
      failures=$((failures + 1))
      continue
    fi
    if [[ "$got_out" != *"$want_substr"* ]]; then
      echo "FAIL [$name]: output missing expected substring '$want_substr':"
      printf '%s\n' "$got_out" | sed 's/^/    /'
      failures=$((failures + 1))
      continue
    fi
    echo "ok   [$name]"
  done

  if [ "$failures" -gt 0 ]; then
    echo "$failures self-test failure(s)"
    return 1
  fi
  echo "self-test: all cases passed"
  return 0
}

if [ "${1-}" = "--self-test" ]; then
  self_test
  exit $?
fi

body="${PR_BODY-}"
if [ -z "$body" ]; then
  echo "PR body is empty: no closing references to check."
  exit 0
fi

check_body "$body"
exit $?
