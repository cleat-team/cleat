#!/usr/bin/env bash
# Gathers the PR body and closingIssuesReferences, then hands off to
# check-closing-reference-declared.py -- the actual text analysis lives
# there (see its own docstring).
#
# READ LIVE, via `gh pr view`, in this one process -- not through the
# workflow step's `env:`. cleat#2703 found exactly that wiring silently
# broken for the negated-reference check next door: GitHub Actions
# step-level `env:` does not carry across steps, so a value meant to arrive
# that way arrived empty instead, and an empty string passed straight
# through the UNMEASURED-on-None guard, so the check ran, found nothing,
# and reported clean. This has only one step, so the same cross-step gap
# cannot occur here either way -- but reading live rather than trusting a
# single step's own `env:` is the same discipline check-negated-closing-
# references.sh adopted after that incident, and there is no reason for a
# new script in the same file to reintroduce the weaker form.
set -euo pipefail

# --self-test (same shape as check-negated-closing-references.sh's own):
# asserts the wrapper's usage guard AND its pass-through of the Python
# script's status and text, using a stub `gh` on PATH rather than the
# network.
self_test() {
  local self_path stub_dir failures=0 rc out
  self_path="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"
  stub_dir="$(mktemp -d)"
  # shellcheck disable=SC2064
  trap "rm -rf '$stub_dir'" EXIT

  check() { # <label> <want-rc> <want-text> <got-rc> <got-text>
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

  # 2. PR_NUMBER set, REPO unset -- the half a single-variable check misses.
  rc=0; out=$(env -u REPO PR_NUMBER=1 bash "$self_path" 2>&1) || rc=$?
  check "PR_NUMBER set, REPO unset" 2 "UNMEASURED:" "$rc" "$out"

  # 3. Both set: the inner script's status must pass through unchanged.
  mkdir -p "$stub_dir"
  cat > "$stub_dir/gh" <<'STUB_GH'
#!/usr/bin/env bash
json=""
while [ $# -gt 0 ]; do case "$1" in --json) json="$2"; shift 2 ;; *) shift ;; esac; done
case "$json" in
  author)                  printf '%s' "${STUB_IS_BOT:-false}" ;;
  body)                    printf '%s' "${STUB_BODY-}" ;;
  closingIssuesReferences) printf '%s' "${STUB_CLOSING-}" ;;
  *) echo "stub gh: unhandled --json '$json'" >&2; exit 9 ;;
esac
STUB_GH
  chmod +x "$stub_dir/gh"

  # 3a. Nothing declared -- the FINDING status, 1.
  rc=0; out=$(PATH="$stub_dir:$PATH" PR_NUMBER=1 REPO=owner/repo \
      STUB_BODY='Refactors the retry loop.' STUB_CLOSING='[]' \
      bash "$self_path" 2>&1) || rc=$?
  check "both set, nothing declared" 1 "No linked issue" "$rc" "$out"

  # 3b. A real link -- clean, status 0.
  rc=0; out=$(PATH="$stub_dir:$PATH" PR_NUMBER=1 REPO=owner/repo \
      STUB_BODY='Adds the audit.' STUB_CLOSING='[42]' \
      bash "$self_path" 2>&1) || rc=$?
  check "both set, a real link" 0 "links 1 issue" "$rc" "$out"

  # 3c. An explicit "closes nothing" statement -- clean, status 0.
  rc=0; out=$(PATH="$stub_dir:$PATH" PR_NUMBER=1 REPO=owner/repo \
      STUB_BODY='Closes nothing. Pure refactor.' STUB_CLOSING='[]' \
      bash "$self_path" 2>&1) || rc=$?
  check "both set, explicitly closes nothing" 0 "explicitly says" "$rc" "$out"

  # 3d. cleat#3354: a BOT author (dependabot and friends) -- exempt, status 0,
  # even with nothing linked and nothing declared. This is the case that was
  # structurally unsatisfiable before this exemption: a bot never closes an
  # issue and the body is machine-generated, so neither of the two escapes
  # above is available to it.
  rc=0; out=$(PATH="$stub_dir:$PATH" PR_NUMBER=1 REPO=owner/repo \
      STUB_IS_BOT='true' STUB_BODY='Bumps foo from 1.2.3 to 1.2.4.' STUB_CLOSING='[]' \
      bash "$self_path" 2>&1) || rc=$?
  check "bot author, nothing linked, nothing declared" 0 "author is a bot" "$rc" "$out"

  # 3e. The exemption must not swallow a REAL finding for a human author --
  # same body/closing shape as 3a, with the bot flag explicitly false. Proves
  # 3d is reading the flag rather than passing everything now.
  rc=0; out=$(PATH="$stub_dir:$PATH" PR_NUMBER=1 REPO=owner/repo \
      STUB_IS_BOT='false' STUB_BODY='Refactors the retry loop.' STUB_CLOSING='[]' \
      bash "$self_path" 2>&1) || rc=$?
  check "human author, nothing linked, nothing declared, still a finding" 1 "No linked issue" "$rc" "$out"

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

# USAGE GUARD -- exits 2 (UNMEASURED), not the finding status 1, for the
# same reason check-negated-closing-references.sh's own guard does: a
# caller that forgot to set PR_NUMBER/REPO must not read as "this PR
# declares nothing", which is a claim about the PR rather than about how
# this script was invoked.
if [ -z "${PR_NUMBER-}" ] || [ -z "${REPO-}" ]; then
  echo "UNMEASURED: PR_NUMBER and REPO must both be set." >&2
  echo "            This wrapper reads the PR body and closing references live," >&2
  echo "            so it has nothing to measure without them." >&2
  exit 2
fi

# cleat#3354: a bot-authored PR (dependabot and friends) closes nothing and
# names no issue, so it can never satisfy either escape the check below
# offers -- a structural false positive, not an occasional one, and one that
# was blocking every dependency-bump PR plus (the same night) a human
# release-prep PR carried along by the same red. `gh pr view --json author`
# gives a direct `is_bot` boolean rather than a login string to pattern-match
# (`app/dependabot` here, `dependabot[bot]` elsewhere, a different name for
# every other bot) -- read live, same discipline as everything else this
# wrapper reads, and checked BEFORE the body/closing-issues reads below so a
# bot PR never pays for a question that does not apply to it.
author_is_bot=$(gh pr view "$PR_NUMBER" --repo "$REPO" --json author --jq '.author.is_bot')
if [ "$author_is_bot" = "true" ]; then
  echo "OK: author is a bot (dependabot and friends never close an issue by design;" \
    "the body is machine-generated, so there is nothing for a human to declare either way)."
  exit 0
fi

pr_body=$(gh pr view "$PR_NUMBER" --repo "$REPO" --json body --jq '.body // ""')

closing_issues=$(gh pr view "$PR_NUMBER" --repo "$REPO" \
  --json closingIssuesReferences --jq '[.closingIssuesReferences[].number]')

export PR_BODY="$pr_body"
export CLOSING_ISSUES="$closing_issues"
exec python3 "$(dirname "$0")/check-closing-reference-declared.py"
