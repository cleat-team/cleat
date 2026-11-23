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
  #
  # THE COMMITS COME FROM GIT, NOT FROM gh (cleat#4456), so the cases below build a real
  # repository: a base commit and N commits on refs/pull/<n>/head, served through a local
  # path the wrapper is told to fetch from (CLOSING_GIT_REMOTE). The stub `gh` answers
  # only what the API would: the body, the closing references, the two oids, and the
  # PR's commit count. Its `--json commits` branch models gh's REAL cap of 100 (measured
  # 2026-11-23 on cleat#4456: 100 of 350) and exists so the known-positive mutant can be
  # run; the wrapper itself no longer calls it.
  mkdir -p "$stub_dir"
  cat > "$stub_dir/gh" <<'STUB_GH'
#!/usr/bin/env bash
case "$1" in
  api)   # gh api repos/<o>/<r>/pulls/<n> --jq .commits
    printf '%s' "${STUB_TOTAL-}" ;;
  pr)
    json=""
    shift; while [ $# -gt 0 ]; do case "$1" in --json) json="$2"; shift 2 ;; *) shift ;; esac; done
    case "$json" in
      body)                    printf '%s' "${STUB_BODY-}" ;;
      closingIssuesReferences) printf '%s' "${STUB_CLOSING-}" ;;
      baseRefOid,headRefOid)   printf '%s %s' "${STUB_BASE-}" "${STUB_HEAD-}" ;;
      commits)                 # the OLD route, capped like the real gh: the OLDEST 100, in order.
        # NOT `git log --reverse --max-count=100`: git applies the limit BEFORE reversing, which
        # returns the NEWEST 100 -- the ones holding the last commit -- and a stub that returns
        # the commit under test cannot show that the real cap loses it (measured: the mutant that
        # reads commits the old way passed the 350-commit case against that version).
        git -C "$STUB_REPO" rev-list --reverse "$STUB_BASE..$STUB_HEAD" | head -n 100 \
          | xargs git -C "$STUB_REPO" log --no-walk=unsorted --format=%B ;;
      *) echo "stub gh: unhandled --json '$json'" >&2; exit 9 ;;
    esac ;;
  *) echo "stub gh: unhandled command '$1'" >&2; exit 9 ;;
esac
STUB_GH
  chmod +x "$stub_dir/gh"

  # make_fixture <dir> <n-commits> <pad-repeat> <last-extra-line>
  # A bare repository holding a base commit and n commits on refs/pull/1/head. Prints
  # "<base-oid> <head-oid>". The extra line is appended to the LAST commit only, so a read
  # that stops early cannot see it.
  make_fixture() {
    git init -q --bare "$1"
    git -C "$1" config uploadpack.allowAnySHA1InWant true
    awk -v n="$2" -v pad="$3" -v extra="$4" 'function emit(m) { printf "data %d\n%s\n", length(m), m }
      BEGIN {
        print "commit refs/heads/base\nmark :1\ncommitter T <t@e> 1700000000 +0000"; emit("base")
        for (i = 0; i < n; i++) {
          m = "fix(x): commit " i "\n\nRoutine change " i ". "
          for (k = 0; k < pad; k++) m = m "padding text for a realistic commit body. "
          if (i == n - 1 && extra != "") m = m "\n" extra
          print "commit refs/pull/1/head"
          print "committer T <t@e> " (1700000001 + i) " +0000"; emit(m)
          if (i == 0) print "from :1"
        }
      }' | git -C "$1" fast-import --quiet
    printf '%s %s' "$(git -C "$1" rev-parse refs/heads/base)" "$(git -C "$1" rev-parse refs/pull/1/head)"
  }

  # run_case <label> <want-rc> <want-text> <repo> <total> <body> <closing> [base head]
  run_case() {
    local label="$1" want_rc="$2" want_text="$3" repo="$4" total="$5" body="$6" closing="$7"
    local base="${8-}" head="${9-}"
    if [ -z "$base" ]; then
      base="$(git -C "$repo" rev-parse refs/heads/base)"; head="$(git -C "$repo" rev-parse refs/pull/1/head)"
    fi
    rc=0; out=$(PATH="$stub_dir:$PATH" PR_NUMBER=1 REPO=owner/repo CLOSING_GIT_REMOTE="$repo" \
        STUB_REPO="$repo" STUB_BASE="$base" STUB_HEAD="$head" STUB_TOTAL="$total" \
        STUB_BODY="$body" STUB_CLOSING="$closing" bash "$self_path" 2>&1) || rc=$?
    check "$label" "$want_rc" "$want_text" "$rc" "$out"
  }

  # 3a. A negated reference the field confirms -- the FINDING status, 1.
  make_fixture "$stub_dir/small.git" 1 0 "" >/dev/null
  run_case "both set, a finding" 1 "NEGATED CLOSING REFERENCE" "$stub_dir/small.git" 1 \
      'This does not close #1980.' '[1980]'

  # 3b. Nothing to flag -- status 0, so the pass-through is asserted in both
  # directions rather than only where a non-zero status happens to appear.
  #
  # The expected TEXT is the inner script's clean-path line, not "". An empty
  # expectation would match any output and assert nothing -- which is what this
  # case shipped with until cleat-review measured it.
  run_case "both set, nothing to flag" 0 "No closing issue references on this PR." "$stub_dir/small.git" 1 \
      'Adds the audit.' '[]'

  # 3c. MORE THAN LINUX'S 131,072-BYTE PER-STRING LIMIT, AND MORE THAN GITHUB'S API WILL RETURN (cleat#4456).
  # 350 commits is the size of the PR that hit this. gh returns 100 of them (`pr view`), the
  # REST and GraphQL routes 250, so only a read from git sees the last. The negation is in the
  # LAST commit: a read that stops early reports nothing and fails here on status and text.
  big_oids=$(make_fixture "$stub_dir/big.git" 350 22 "This does not close #1980.")
  size=$(git -C "$stub_dir/big.git" log --format=%B "${big_oids% *}..${big_oids#* }" | wc -c | tr -d ' ')
  if [ "$size" -le 131072 ]; then
    echo "  FAIL big fixture: $size bytes of messages does not exceed 131072, so it cannot reproduce the limit" >&2
    failures=$((failures + 1))
  else
    run_case "350 commits, $size bytes of messages, a negation in the last" 1 "NEGATED CLOSING REFERENCE" \
        "$stub_dir/big.git" 350 'Adds things.' '[1980]'
  fi

  # 3d. The API's own count disagrees with what git returned: UNMEASURED, never a clean read of a prefix.
  run_case "the PR says 351 commits, git has 350" 2 "UNMEASURED: read 350 commit(s) from git but the PR has 351" \
      "$stub_dir/big.git" 351 'Adds things.' '[1980]'

  # 3e. The head moved between the API read and the fetch.
  run_case "the head moved after the API read" 2 "UNMEASURED: refs/pull/1/head is" \
      "$stub_dir/big.git" 350 'Adds things.' '[1980]' "${big_oids% *}" "${big_oids% *}"

  # 3f. The remote cannot be fetched from.
  run_case "the fetch fails" 2 "UNMEASURED: could not fetch" "$stub_dir/does-not-exist.git" 350 'Adds things.' '[1980]' \
      "${big_oids% *}" "${big_oids#* }"

  # 3g. gh failing is UNMEASURED, not the shell's own status and not a finding.
  mkdir -p "$stub_dir/broken"
  printf '#!/usr/bin/env bash\necho "gh: HTTP 502" >&2\nexit 7\n' > "$stub_dir/broken/gh"
  chmod +x "$stub_dir/broken/gh"
  rc=0; out=$(PATH="$stub_dir/broken:$PATH" PR_NUMBER=1 REPO=owner/repo bash "$self_path" 2>&1) || rc=$?
  check "gh fails" 2 "UNMEASURED: could not read the PR's" "$rc" "$out"

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

# THE THREE VALUES TRAVEL THROUGH FILES, AND THE COMMITS COME FROM GIT (cleat#4456).
#
# TWO DEFECTS LIVED IN READING THE COMMITS, and the first hid the second.
#
# 1. This used to `export COMMIT_MESSAGES="$commit_messages"` and `exec python3`. Linux
#    caps ONE environment string at 131,072 bytes (MAX_ARG_STRLEN), whatever the total
#    allowance, so on a big enough PR `dirname` and then `python3` each died with
#    "Argument list too long" (exit 126) -- neither of this guard's statuses. Reproduced
#    on that script in a Linux container before this was written: 337,206 bytes, rc=126.
#    macOS has no per-string cap, so the same input passes on a developer's machine.
#
# 2. `gh pr view --json commits` returns the OLDEST 100 commits and says nothing. On
#    cleat#4456, the 350-commit re-cut: 100. The REST `pulls/N/commits` route returns
#    250 and GraphQL `commits(first:100)` paginated also 250, though `totalCount` says
#    350 and the last page says hasNextPage=false (all measured 2026-11-23). So fixing (1)
#    alone would have made the guard read under a third of the very PR it exists for and
#    report CLEAN -- the same fail-open as the crash, silent. Found by cleat-review2 on the
#    first version of this fix, whose own text claimed the 350.
#
# So the commits are read from git: the PR head and base fetched into a scratch BARE
# repository (the job's own checkout is depth 1), `git log base..head`. The expected count
# is the PR's own (REST `.commits`, 350 here); a different count, a head that moved
# between the API read and the fetch, or a fetch that failed is UNMEASURED -- never a
# clean read of a prefix. On cleat#4456 this read 350 commits, 1,256,568 bytes of message.
#
# `exec` is gone too: it replaced this shell, so no cleanup could run after it.
# ANYTHING THIS SCRIPT CANNOT READ OR RUN IS UNMEASURED (status 2), never the shell's own
# status: gh failing, mktemp or git failing, and python3 exiting with something other than
# 0/1/2 are the wrapper's problem and say nothing about the PR.
script_dir="$(cd "$(dirname "$0")" && pwd)"
work_dir="$(mktemp -d)" || {
  echo "UNMEASURED: could not create a scratch directory for the PR's text." >&2
  echo "            This is a failure of the check, not a finding about the PR." >&2
  exit 2
}
# shellcheck disable=SC2329  # invoked by the trap below
cleanup() { [ -n "${work_dir-}" ] && [ -d "$work_dir" ] && rm -rf -- "$work_dir"; return 0; }
trap cleanup EXIT

unmeasured() { # <what happened>
  echo "UNMEASURED: $1" >&2
  echo "            This is a failure of the check, not a finding about the PR." >&2
  exit 2
}

# fetch <outfile> <what> <--json field> <--jq program>
fetch() {
  gh pr view "$PR_NUMBER" --repo "$REPO" --json "$3" --jq "$4" > "$1" \
    || unmeasured "could not read the PR's $2 with gh (PR $PR_NUMBER in $REPO)."
}

fetch "$work_dir/pr_body" "body" body '.body // ""'
fetch "$work_dir/closing_issues" "closing references" closingIssuesReferences '[.closingIssuesReferences[].number]'
fetch "$work_dir/oids" "base and head commits" baseRefOid,headRefOid '"\(.baseRefOid) \(.headRefOid)"'
# `|| [ -n ... ]`: `read` fails on a last line with no newline although it has filled the variables.
read -r base_oid head_oid < "$work_dir/oids" || [ -n "${head_oid-}" ]
[ -n "${base_oid-}" ] && [ -n "${head_oid-}" ] || unmeasured "gh returned no base and head commit for PR $PR_NUMBER."
expected=$(gh api "repos/$REPO/pulls/$PR_NUMBER" --jq .commits) \
  || unmeasured "could not read the PR's commit count with gh (PR $PR_NUMBER in $REPO)."
case "$expected" in ''|*[!0-9]*) unmeasured "the PR's commit count is not a number: '$expected'." ;; esac

# CLOSING_GIT_REMOTE exists for the self-test, which serves a local repository. In CI it is unset.
remote="${CLOSING_GIT_REMOTE:-https://github.com/$REPO.git}"
git init -q --bare "$work_dir/repo" || unmeasured "git could not create a scratch repository."
# --filter=blob:none: only the commits are wanted, and a re-cut's history is large.
GIT_TERMINAL_PROMPT=0 git -C "$work_dir/repo" -c credential.helper= \
  -c 'credential.helper=!gh auth git-credential' fetch -q --no-tags --filter=blob:none \
  "$remote" "refs/pull/$PR_NUMBER/head" "$base_oid" \
  || unmeasured "could not fetch refs/pull/$PR_NUMBER/head and the base from $remote."
fetched_head=$(git -C "$work_dir/repo" rev-parse FETCH_HEAD 2>/dev/null) \
  || unmeasured "git fetched nothing for refs/pull/$PR_NUMBER/head."
[ "$fetched_head" = "$head_oid" ] \
  || unmeasured "refs/pull/$PR_NUMBER/head is $fetched_head but the API said the head was $head_oid: the PR moved between the two reads. Re-run."
got=$(git -C "$work_dir/repo" rev-list --count "$base_oid..$head_oid") \
  || unmeasured "git could not list the commits between the base and the head."
[ "$got" = "$expected" ] \
  || unmeasured "read $got commit(s) from git but the PR has $expected. Reading a prefix of the PR's commits and calling it clean is the failure this guard must not have."
git -C "$work_dir/repo" log --reverse --format=%B "$base_oid..$head_oid" > "$work_dir/commit_messages" \
  || unmeasured "git could not read the commit messages."

rc=0
PR_BODY_FILE="$work_dir/pr_body" \
  COMMIT_MESSAGES_FILE="$work_dir/commit_messages" \
  CLOSING_ISSUES_FILE="$work_dir/closing_issues" \
  python3 "$script_dir/check-negated-closing-references.py" || rc=$?
case "$rc" in
  0|1|2) exit "$rc" ;;
  *) unmeasured "check-negated-closing-references.py exited $rc, which is none of its statuses (0 clean, 1 a finding, 2 unmeasured)." ;;
esac
