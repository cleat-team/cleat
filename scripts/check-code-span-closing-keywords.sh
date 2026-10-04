#!/usr/bin/env bash
# Gathers the PR body and closingIssuesReferences, then hands off to
# check-code-span-closing-keywords.py -- the text analysis lives there, since a
# guard that has to tell a code span from prose should not be a shell script.
#
# WHY THIS EXISTS (cleat#3028). A closing keyword inside a code span is INERT:
# GitHub does not read it, so `Closes #3018` written in backticks closes
# nothing, the PR merges green, and the issue stays open with no detector
# except a person asking why. Two of the last forty merged PR bodies carry one,
# and they are the two cases -- #3021 (nothing live, issue stayed open) and
# #3038 (a QUOTE, with a live closer elsewhere, closed correctly). The
# discriminator that separates them, and the reason this check can exist at all
# without revoking the sibling guards' code-span exemption, is
# `closingIssuesReferences`. See the .py's docstring for the full account.
#
# READING IT LIVE, not through the step's `env:`. That is the negated wrapper's
# lesson (cleat#2703): a step-level env that does not carry silently turned
# "unset" into "empty", and the check then ran, found nothing, and reported OK.
# Reading live removes the cross-step wiring entirely.
#
# --self-test asserts on the TEXT as well as the status, and it carries #3038's
# shape as a fixture: a code span whose issue IS closed live elsewhere must
# PASS, because flagging it would revoke the exemption its siblings rely on.
set -euo pipefail

self_test() {
  local self_path stub_dir failures=0 rc out
  self_path="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"
  stub_dir="$(mktemp -d)"
  # shellcheck disable=SC2064
  trap "rm -rf '$stub_dir'" EXIT

  check() { # <label> <want-rc> <want-text> <got-rc> <got-text>
    # An empty want-text makes the text assertion vacuous -- `grep -qF -- ""`
    # matches every input -- so it is refused at the call (cleat#3024).
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

  run() { # <stub body> <stub closing> ; sets rc and out
    rc=0
    out=$(PATH="$stub_dir:$PATH" PR_NUMBER=1 REPO=owner/repo \
        STUB_BODY="$1" STUB_CLOSING="$2" STUB_COMMITS='' \
        bash "$self_path" 2>&1) || rc=$?
  }

  # 1. No PR_NUMBER -- exits before any gh call.
  rc=0; out=$(env -u PR_NUMBER -u REPO bash "$self_path" 2>&1) || rc=$?
  check "no PR_NUMBER" 2 "UNMEASURED:" "$rc" "$out"

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

  # 2. #3021's shape: a code-spanned closer, and nothing live to close it. 1.
  # (The backticks below are the FIXTURE, not command substitution: the
  # single quotes keep them literal on purpose.)
  # shellcheck disable=SC2016
  run 'This is what a trapped closer looks like: `Closes #3018`.' '[]'
  check "a trapped closer, nothing live" 1 "CODE-SPAN CLOSING KEYWORD" "$rc" "$out"

  # 3. #3038's shape -- THE ACCEPTANCE FIXTURE. A quote, with a live closer
  #    elsewhere. It must pass, or this check revokes the exemption its sibling
  #    guards depend on.
  # (The backticks below are the FIXTURE, not command substitution: the
  # single quotes keep them literal on purpose.)
  # shellcheck disable=SC2016
  run 'It says `Closes #3003` as an example, and the real one is here: Closes #3003.' '[3003]'
  check "#3038: a quoted span, closed live elsewhere" 0 "No closing keyword is trapped in a code span." "$rc" "$out"

  # 4. A live closer and no code span at all -- the ordinary PR.
  run 'Closes #42.' '[42]'
  check "an ordinary live closer" 0 "No closing keyword is trapped in a code span." "$rc" "$out"

  # 5. The MALFORMED form in a code span is the sibling guards subject, not
  #    this one, and must never be flagged here.
  # (The backticks below are the FIXTURE, not command substitution: the
  # single quotes keep them literal on purpose.)
  # shellcheck disable=SC2016
  run 'A PR quoting the bad form: `Closes cleat#99`.' '[]'
  check "the malformed form in a span is not ours" 0 "No closing keyword is trapped in a code span." "$rc" "$out"

  # 6. THE KNOWN FALSE POSITIVE, pinned rather than left to be discovered -- and
  #    it is the check's OWN documentation that trips it. A body whose subject is
  #    this inertness carries a code-spanned closer and no live reference, so the
  #    discriminator fires on it correctly and the author is wronged anyway.
  #    WS-3's point, which the 40-PR sample could not have shown: a sample cannot
  #    contain a case nobody has written yet, and this class did not exist before
  #    this check did. It is asserted here so it is a known shape with a stated
  #    remedy, not a surprise on someone's merge.
  # (The backticks below are the FIXTURE, not command substitution: the
  # single quotes keep them literal on purpose.)
  # shellcheck disable=SC2016
  run 'A code-spanned `Closes #3018` is inert, so the issue stays open.' '[]'
  check "KNOWN FP: a body documenting the inertness fires" 1 "CODE-SPAN CLOSING KEYWORD" "$rc" "$out"

  # 7. And the remedy that message prescribes must actually pass -- advice that
  #    does not work is worse than none, since it sends the reader to a second
  #    red build.
  # (The backticks below are the FIXTURE, not command substitution: the
  # single quotes keep them literal on purpose.)
  # shellcheck disable=SC2016
  run 'A code-spanned `Closes #<n>` is inert, and so is `Closes cleat#N`.' '[]'
  check "the prescribed remedy passes" 0 "No closing keyword is trapped in a code span." "$rc" "$out"

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

# USAGE GUARD -- exit 2, the "could not establish" status, never 1. See the
# negated wrapper's header for why the `: "${X:?}"` form cannot express this.
if [ -z "${PR_NUMBER-}" ] || [ -z "${REPO-}" ]; then
  echo "UNMEASURED: PR_NUMBER and REPO must both be set." >&2
  echo "            This check reads the PR body and closing references live, so it" >&2
  echo "            has nothing to measure without them. (This is the check's own" >&2
  echo "            precondition failing; it says nothing about the PR.)" >&2
  exit 2
fi

pr_body=$(gh pr view "$PR_NUMBER" --repo "$REPO" --json body --jq '.body // ""')

closing_issues=$(gh pr view "$PR_NUMBER" --repo "$REPO" \
  --json closingIssuesReferences --jq '[.closingIssuesReferences[].number]')

export PR_BODY="$pr_body"
export CLOSING_ISSUES="$closing_issues"
exec python3 "$(dirname "$0")/check-code-span-closing-keywords.py"
