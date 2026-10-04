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

# --self-test (cleat#3024)
#
# The wrapper's USAGE STATUS is asserted by nothing otherwise: the workflow
# always sets both variables, so CI never exercises the guard, and cleat#2992
# moved it from 1 to 2 with nothing that would have gone red had it moved back.
#
# IT ASSERTS ON THE TEXT AS WELL AS THE STATUS, which is the sibling's rule and
# the reason this file needed one at all: `check-closing-references.sh`'s header
# says a self-test that only checked "did this exit 1" would have passed the
# defect it was written for. The same is true here -- `exit 2` with the FINDING's
# wording is still the collapse cleat#2992 removed, and only the text tells the
# two apart.
#
# THE THIRD CASE NEEDS A `gh` THAT DOES NOT REACH THE NETWORK. Rather than
# restructure the wrapper to accept injected inputs -- it reads all three values
# live on purpose, to remove the cross-step `env:` wiring that once made it pass
# vacuously -- the self-test puts a stub `gh` on a prepended PATH. That exercises
# the real wiring (usage guard -> gh reads -> exports -> exec) with no network
# and no production change.
self_test() {
  local self_path stub_dir failures=0 rc out
  self_path="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"
  stub_dir="$(mktemp -d)"
  # shellcheck disable=SC2064
  trap "rm -rf '$stub_dir'" EXIT

  check() { # <label> <want-rc> <want-text> <got-rc> <got-text>
    # AN EMPTY want-text MAKES THE TEXT ASSERTION BELOW VACUOUS: `grep -qF -- ""`
    # matches every input, so the case would pass on any output whatever. This is
    # not hypothetical -- the clean-path case shipped with `""` and asserted
    # nothing about the text (found by cleat-review on cleat#3045, measured both
    # ways). Refused at the call rather than left to the caller's memory.
    if [ -z "$3" ]; then
      echo "  FAIL $1: caller passed an EMPTY want-text, which matches any output at all" >&2
      failures=$((failures + 1))
      return
    fi
    if [ "$4" -ne "$2" ]; then
      echo "  FAIL $1: rc=$4 want=$2" >&2
      failures=$((failures + 1))
    elif ! printf '%s' "$5" | grep -qF -- "$3"; then
      echo "  FAIL $1: rc=$4 is right but the TEXT lacks '$3'" >&2
      printf '%s\n' "$5" | sed 's/^/        /' >&2
      failures=$((failures + 1))
    else
      echo "  ok   $1"
    fi
  }

  # 1. No PR_NUMBER at all. Exits before any gh call.
  rc=0; out=$(env -u PR_NUMBER -u REPO bash "$self_path" 2>&1) || rc=$?
  check "no PR_NUMBER" 2 "UNMEASURED:" "$rc" "$out"

  # 2. PR_NUMBER set, REPO unset -- the half that a single-variable check misses.
  rc=0; out=$(env -u REPO PR_NUMBER=1 bash "$self_path" 2>&1) || rc=$?
  check "PR_NUMBER set, REPO unset" 2 "UNMEASURED:" "$rc" "$out"

  # 3. Both set: the inner script's status must pass through unchanged.
  mkdir -p "$stub_dir"
  cat > "$stub_dir/gh" <<'STUB_GH'
#!/usr/bin/env bash
json=""
while [ $# -gt 0 ]; do case "$1" in --json) json="$2"; shift 2 ;; *) shift ;; esac; done
case "$json" in
  body)                    printf '%s' "${STUB_BODY-}" ;;
  closingIssuesReferences) printf '%s' "${STUB_CLOSING-}" ;;
  commits)                 printf '%s' "${STUB_COMMITS-}" ;;
  *) echo "stub gh: unhandled --json '$json'" >&2; exit 9 ;;
esac
STUB_GH
  chmod +x "$stub_dir/gh"

  # 3a. A negated reference the field confirms -- the FINDING status, 1.
  rc=0; out=$(PATH="$stub_dir:$PATH" PR_NUMBER=1 REPO=owner/repo \
      STUB_BODY='This does not close #1980.' STUB_CLOSING='[1980]' STUB_COMMITS='' \
      bash "$self_path" 2>&1) || rc=$?
  check "both set, a finding" 1 "NEGATED CLOSING REFERENCE" "$rc" "$out"

  # 3b. Nothing to flag -- status 0, so the pass-through is asserted in both
  # directions rather than only where a non-zero status happens to appear.
  #
  # The expected TEXT is the inner script's clean-path line, not "". An empty
  # expectation would match any output and assert nothing -- which is what this
  # case shipped with until cleat-review measured it. Asserting the wording
  # positive side out also catches the mirror of a missed finding: a wrapper
  # that reported a FINDING on a clean body would fail here on both the status
  # and the text.
  rc=0; out=$(PATH="$stub_dir:$PATH" PR_NUMBER=1 REPO=owner/repo \
      STUB_BODY='Adds the audit.' STUB_CLOSING='[]' STUB_COMMITS='' \
      bash "$self_path" 2>&1) || rc=$?
  check "both set, nothing to flag" 0 "No closing issue references on this PR." "$rc" "$out"

  if [ "$failures" -ne 0 ]; then
    echo "self-test: $failures case(s) failed" >&2
    return 1
  fi
  echo "self-test: ok"
  return 0
}

if [ "${1-}" = "--self-test" ]; then
  self_test
  exit $?
fi

# USAGE GUARD -- AND IT EXITS 2, WHICH IS THE WHOLE POINT (cleat#2992).
#
# The Python below defines three statuses: 0 no finding, 1 A FINDING, 2 could
# not establish what it measures. This guard used to be
# `: "${PR_NUMBER:?...}"`, which under `set -e` exits 1 -- the finding status --
# so a caller could not tell "this PR negates a closing reference" from "I
# invoked it without PR_NUMBER". That is the precise collapse the inner script's
# three-way split exists to prevent, and the wrapper is the documented way to
# run it. It matters most on the local path, because CI always sets both
# variables and a person running it by hand does not.
#
# IT IS AN `if`, NOT A `||` ON THE PARAMETER EXPANSION. `: "${PR_NUMBER:?}" ||
# exit 2` looks like the repair and cannot work: `${x:?}` exits the shell
# itself, before any `||` on that line is reached. The property that MAKES the
# defect is the one that defeats the obvious fix, so the guard has to test the
# variables rather than ask the expansion to complain.
#
# The message carries the same `UNMEASURED:` prefix the Python uses for status
# 2, so one grep finds every "I could not look" from either half.
if [ -z "${PR_NUMBER-}" ] || [ -z "${REPO-}" ]; then
  echo "UNMEASURED: PR_NUMBER and REPO must both be set." >&2
  echo "            This wrapper reads the PR body, closing references and commits live," >&2
  echo "            so it has nothing to measure without them. (This is the wrapper's own" >&2
  echo "            precondition failing; it says nothing about the PR.)" >&2
  exit 2
fi

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
