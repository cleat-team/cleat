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
# malformed repo name and flagged it.
#
# A repo-name charset can't produce THAT false positive: GitHub repo and
# owner names are letters, digits, `_`, `.` and `-`. But cleat#2800's first
# fix used exactly that charset alone -- `[A-Za-z0-9_.-]+` -- with no leading
# punctuation allowed, and that swung the other way: cleat-review found it
# turned `Fixes **cleat#12**.` and `Fixes [cleat#12](url).` from correctly
# FLAGGED (the old class matched "**cleat" / "[cleat" as its one-char-plus
# "name", by accident) into a false NEGATIVE, because a name class that
# cannot START with `*` or `[` fails to match at all right after the
# required whitespace, and there is no second starting position to retry
# from. `cleat#12` wrapped in Markdown emphasis or a link still links
# nothing on GitHub; the wrapping is not a repo name and must not make the
# reference pass.
#
# The first fix (above) allowed, but did not require, a run of Markdown
# emphasis/link-opening punctuation (`*_[(`) before the repo-name charset.
# That was an ENUMERATED list, and cleat-review found the next miss within
# the hour: `Fixes "cleat#12".` and `Fixes 'cleat#12'.` -- quoted rather than
# emphasized -- both flag on develop and both PASSED under the `*_[(`
# version, because `"` and `'` are not in that list. Every fix so far had
# moved the miss to the next wrapper character rather than closing the
# class, which is the tell that the class needs a different SHAPE, not one
# more character added to it.
#
# The actual rule is not "which punctuation is allowed before the name" --
# it's what GitHub itself does: `#N` links only when nothing but whitespace
# precedes it, or when a slash-bearing `owner/repo` does. ANY other run of
# non-whitespace, non-`/` characters between the keyword and the name --
# whatever it is made of -- already breaks the link, so the class should
# accept ANY such run rather than enumerate the ones seen so far:
#
#   [[:space:]]+[^[:space:]#/]*[A-Za-z0-9_.-]#[0-9]+
#
# A prefix class, not a wrapper list: any number of non-space/non-hash/
# non-slash characters, followed by exactly one repo-name character
# immediately before `#N` (so there is always a "name" for the message to
# quote). `(#2003)` and `**#12**` still pass: every character between the
# keyword and `#` is punctuation, so there is no repo-name character to sit
# immediately before `#`, and the alternative fails at every backtrack --
# same reasoning as `owner/repo#N` passing (the `/` is excluded from the
# prefix class, so a namechar can never be adjacent to `#` across it).
# `**cleat#12**`, `[cleat#12](url)`, `"cleat#12"` and `'cleat#12'` all
# match: whatever wraps `cleat` is absorbed by the open prefix class, `t`
# (the last letter of `cleat`) sits immediately before `#12`, and the
# alternative succeeds. See --self-test below for the fixture pinning all
# of it, including the case that discriminates a prefix class from a
# wrapper list -- a form no prior version of this comment anticipated.
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

# The pattern this check applies. Named, and passed to check_body as an argument,
# so the self-test can exercise the ERROR path with a deliberately invalid one
# (cleat#3103): a pattern this grep cannot compile has to be a visible UNMEASURED,
# and a self-test that could only ever call the real pattern could not show that.
BAD_REF_PATTERN='\b(close[sd]?|fix(e[sd])?|resolve[sd]?):?[[:space:]]+[^[:space:]#/]*[A-Za-z0-9_.-]#[0-9]+'

# Runs the check against $1 (a PR body) and prints the same report the real
# invocation would. $2 overrides the pattern and is used by the self-test alone.
# Returns 0 (nothing to flag), 1 (a bad reference found) or 2 (the pattern did not
# compile, so nothing was searched -- cleat#3103).
check_body() {
  local body="$1"
  local pattern="${2-$BAD_REF_PATTERN}"

  # Code is not a claim. GitHub links nothing inside a fenced block or an inline
  # code span, so `Closes cleat#N` written as code -- a PR QUOTING the bad form,
  # as this check's own PR did, and failed its own first run for it -- is inert
  # and must pass. Both are removed before matching; unclosed fences and
  # backticks are left alone, which errs toward flagging.
  #
  # A QUOTED CLAUSE IS THE SAME KIND OF THING (cleat#3095, and the shape this
  # check's own PR hit a second time). A body that DESCRIBES this defect has to
  # write `does not close cleat#N` down, and that sentence is usually quoted --
  # *"This procedure does not close cleat#2311; it only avoids triggering it."*
  # The keyword there is the quoted author's, not this body's, and flagging it
  # refuses a body for describing the thing the check is about.
  #
  # WHAT DECIDES IS THE KEYWORD'S POSITION, NOT THE QUOTE. A span is blanked only
  # when it contains the whole closing phrase, keyword included. `Fixes
  # "cleat#12".` quotes the REFERENCE while the keyword stays outside and remains
  # the body's own attempt, so it is still flagged -- which cleat#2807 R2's two
  # fixtures pin, and which is why blanking quoted spans outright would have been
  # wrong: it buys this false positive's repair by losing two true positives.
  #
  # KNOWN FALSE NEGATIVE, left in place deliberately (cleat#3102). Because this
  # rule keys on the QUOTE and not on quotation-versus-assertion, a whole phrase
  # scare-quoted is blanked:
  #
  #     The commit "fixes cleat#5" as required.      -- exit 0, missed
  #
  # cleat-review tried to build a NATURAL instance and could not: scare-quoting
  # normally keeps the keyword outside the span (`Fixes "cleat#5"`), which is
  # still flagged and is what the R2 fixtures pin. Recorded here because a known
  # false negative belongs beside the check rather than only in a review thread.
  #
  # MARKDOWN EMPHASIS IS NOT PART OF THE KEYWORD (cleat#3102). `_` is a word
  # character to `\b`, so `_Fixes cleat#5_` -- italic text, and the body's own
  # closing attempt -- had no boundary before `Fixes` and went unflagged. Two
  # things argue for normalising it HERE rather than in the pattern. One: an `_`
  # is an emphasis delimiter unless it is INTRA-WORD, which is markdown's own
  # rule -- so `_Fixes` loses its underscore while `a_Fixes` keeps it, being
  # neither emphasis nor a closing attempt. Two: the quoted-span rule below needs
  # the same view of what the keyword is, or a body QUOTING `_Fixes cleat#5_` is
  # refused, which is cleat#3095's false positive arriving again in the `_` form.
  #
  # THE RULE IS AN ALTERNATION, AND BOTH HALVES ARE LOAD-BEARING. It removes an
  # `_` that cannot OPEN emphasis AND one that cannot CLOSE it; leading-only would
  # leave `fixes_ cleat#5` unflagged. That is a real looseness -- markdown leaves a
  # lone closer literal, so the emphasised form it "fixes" is one the author did
  # not write -- and it is kept deliberately: this check's documented posture is
  # to err toward flagging, and a reader meeting `fixes_ cleat#5` reads a typo'd
  # closing attempt beside a reference that will not link. The fixture is here so
  # the half cannot be dropped silently; cleat-review measured that dropping it
  # left every other case green.
  #
  # Changing only the pattern cannot do the second one: the `_` there sits
  # immediately after the opening quote, where the blanker's lazy prefix cannot
  # reach it. Normalising first gives both rules a single answer.
  #
  # AND THE TWO RULES MUST AGREE ABOUT CASE. The grep below runs `-i`; this
  # blanker did not, so a QUOTED but CAPITALISED whole phrase was blanked by
  # neither and flagged by the checker -- measured on develop, which flags
  # `It says "Fixes cleat#12" as an example.` while the same sentence in lower
  # case passes. Pre-existing rather than this change's doing, and it is the same
  # shape as the paragraph above: two rules that must agree about what a closing
  # keyword IS, disagreeing. The new `_`-form makes it reachable a second way, so
  # the `/i` is added rather than the fixture adjusted around it.
  local prose
  prose=$(printf '%s\n' "$body" | perl -0pe '
    s/^[ \t]*```.*?^[ \t]*```[^\n]*$//gms;
    s/`[^`\n]*`//g;
    s/(?<![[:alnum:]])_|_(?![[:alnum:]])//g;
    s/(["\x27])(?:(?!\1).)*?\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\b(?:(?!\1).)*?\b[A-Za-z0-9_.-]*#[0-9]+(?:(?!\1).)*?\1/ /gsi;
  ')

  # grep exits 0 on a match, 1 on no match, and 2 or more on an ERROR -- an invalid
  # pattern among them. `if ! bad=$(... | grep ...)` folded all three into "the
  # branch is taken", so a pattern grep could not compile reported a CLEAN PR, with
  # the failure visible only on stderr inside a green step. Measured on develop with
  # one stray parenthesis, and again through a lookbehind repair: exit 0, with
  # `Fixes cleat#5.` passing. The comment that used to stand here said the no-match
  # case was taken explicitly "rather than swallowed with `|| true` ... (which would
  # also hide a grep that failed to run)" -- the intent was exactly right and the
  # construct reintroduced the hazard it names. This is a REQUIRED check, so a
  # pattern that cannot be applied is its own status and not a pass (cleat#3103).
  local bad rc
  bad=$(printf '%s\n' "$prose" | grep -oiE "$pattern") && rc=0 || rc=$?
  if [ "$rc" -gt 1 ]; then
    echo "UNMEASURED: grep could not apply this check's pattern (rc=$rc)."
    echo "            This is a failure of the CHECK, not a finding about the PR:"
    echo "            no body was searched, so nothing was measured."
    echo ""
    echo "            pattern: $pattern"
    return 2
  fi
  if [ "$rc" -eq 1 ]; then
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
    "bold cleat#N must still be caught (cleat#2807)|Fixes **cleat#12**.|1|cleat#12"
    "linked cleat#N must still be caught (cleat#2807)|Fixes [cleat#12](https://x).|1|cleat#12"
    "bold correct reference still passes (cleat#2807)|Fixes **#12**.|0|No closing keyword"
    "linked correct reference still passes (cleat#2807)|Fixes [#12](https://x).|0|No closing keyword"
    "double-quoted cleat#N must still be caught (cleat#2807 R2)|Fixes \"cleat#12\".|1|cleat#12"
    "single-quoted cleat#N must still be caught (cleat#2807 R2)|Fixes 'cleat#12'.|1|cleat#12"
    "a valid ref alongside a bad one, one keyword each|Resolves: cleat#7 and fixes #8|1|cleat#7"
    "a whole closing phrase inside quotes is inert (cleat#3095)|It says \"this procedure does not close cleat#2311\" and that is all.|0|No closing keyword"
    "the same phrase unquoted is still caught (cleat#3095)|Deliberately does not close cleat#2311.|1|cleat#2311"
    "a quoted phrase keeps its neighbours caught (cleat#3095)|See \"does not close cleat#2311\", and fixes cleat#9.|1|cleat#9"
    "an emphasised keyword must still be caught (cleat#3102)|_Fixes cleat#5_|1|cleat#5"
    "an intra-word underscore is not emphasis, and stays clean (cleat#3102)|a_Fixes cleat#5|0|No closing keyword"
    "an intra-word underscore must not be JOINED into a keyword (cleat#3102)|fix_es cleat#5|0|No closing keyword"
    "a trailing emphasis marker is stripped, and that half is pinned (cleat#3102)|fixes_ cleat#5|1|cleat#5"
    "a quoted emphasised phrase is inert, as quoted phrases are (cleat#3102)|It says \"_Fixes cleat#12_\" as an example.|0|No closing keyword"
    "a quoted capitalised phrase is inert too (cleat#3102, pre-existing)|It says \"Fixes cleat#12\" as an example.|0|No closing keyword"
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

  # cleat#3103: a pattern grep cannot compile must be UNMEASURED (2), never a pass.
  # Taken through check_body ITSELF, with the bad pattern as its argument, rather
  # than by repeating the pipeline here -- a copy of the construct would test the
  # copy, and this case exists precisely because the construct was the defect.
  local bad_pat_rc=0 bad_pat_out
  bad_pat_out=$(check_body "Fixes cleat#5." '(') || bad_pat_rc=$?
  if [ "$bad_pat_rc" != "2" ]; then
    echo "FAIL [an uncompilable pattern is UNMEASURED, not a pass (cleat#3103)]: exit=$bad_pat_rc want=2"
    failures=$((failures + 1))
  elif [[ "$bad_pat_out" != *"UNMEASURED"* ]]; then
    echo "FAIL [an uncompilable pattern is UNMEASURED, not a pass (cleat#3103)]: output lacks UNMEASURED:"
    printf '%s\n' "$bad_pat_out" | sed 's/^/    /'
    failures=$((failures + 1))
  else
    echo "ok   [an uncompilable pattern is UNMEASURED, not a pass (cleat#3103)]"
  fi

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
