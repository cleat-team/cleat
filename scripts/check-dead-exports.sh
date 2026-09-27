#!/usr/bin/env bash
#
# Detect code that has zero callers anywhere -- not production, not tests.
#
# scripts/check-test-only-code.sh (which this script sits beside) runs
# staticcheck's U1000 with -tests=false, so anything an exported symbol's own
# _test.go file is the only caller of shows up as unused once that file is
# excluded from the analysis. That catches the "only tests call it" shape.
#
# It does NOT catch the "nothing calls it, including tests" shape, and it
# cannot: U1000 does not report exported identifiers in non-main packages at
# all, on the theory that a library's public API may be used by callers
# outside the scanned module. That theory is correct for cleat/ and
# pluginapi/, which genuinely are public API for external consumers. It is
# false for engine/, plugin/, wasm/, and friends: nothing outside this repo
# imports them, so an unreferenced exported symbol there is not "public API
# nobody has adopted yet", it is dead code that happens to start with a
# capital letter. engine/batch_flush.go (NewBatchFlusher, all-exported, all
# 0.0% coverage) and plugin/audit.go (NewAuditLog and 10 more, same story)
# both survived under check-test-only-code.sh's baseline for exactly this
# reason -- U1000 never saw them as findings to baseline in the first place.
#
# This script closes that gap with a blunter, complementary method: for each
# exported top-level func/method declared in an explicitly "internal" set of
# package roots (below), grep the whole tree for the bare identifier. If it
# appears in no file other than the one that declares it, nothing anywhere
# -- production or test -- ever spells its name, and it is reported.
#
# False-positive sources and how they're handled:
#
#   * Public API surface. cleat/, pluginapi/, crates/, python-sdk/, and
#     packages/ are deliberately excluded from the scanned roots below --
#     they exist specifically to be called from outside this repo, so an
#     internal grep finding nothing is the expected, correct state, not a
#     defect.
#
#   * Interface satisfaction with no textual call site. A method that exists
#     only to satisfy a standard interface (error, fmt.Stringer, io.Reader,
#     json.Marshaler, http.Handler, sql.Scanner, ...) can be invoked by the
#     runtime without any source line ever writing `.MethodName(`. Those
#     names are allowlisted in COMMON_INTERFACE_METHODS below and skipped
#     unconditionally -- they are common enough, and false-positive-prone
#     enough, that per-entry baseline review would not be worth much.
#
#   * Name collisions. A method named e.g. Deploy in one package is
#     indistinguishable, to a bare `grep -w`, from an unrelated Deploy
#     defined and called somewhere else in the tree. That direction of error
#     is a false NEGATIVE (a truly dead method hides behind an unrelated
#     same-named call) and is accepted: this script is intentionally
#     conservative about what it flags, per the same false-positive
#     discipline as check-test-only-code.sh.
#
#   * Build tags -- NOT a blind spot here, unlike check-test-only-code.sh's
#     staticcheck-based scan. scripts/finddeadexports.go extracts
#     declarations with go/parser.ParseFile directly, which parses a file's
#     syntax unconditionally and does not evaluate //go:build constraints at
#     all -- so engine/backend_wasmtime.go's declarations are seen and
#     grepped for the same as everything else, with or without
#     CGO_ENABLED=1. The same is true of the cross-reference grep, which is
#     plain text search over every .go file regardless of what would
#     actually compile together. A file that flat-out fails to parse (rare;
#     would mean a genuine syntax error, not a missing build tag) is skipped
#     with a warning on stderr rather than silently mis-scanned.
#
# Usage:
#   scripts/check-dead-exports.sh              # fail on entries not in the baseline
#   scripts/check-dead-exports.sh --update     # rewrite the baseline
#
# RUN --update AGAINST THE FINISHED DIFF, NOT WHILE STILL EDITING. cleat#1116
# (cleat/queue_store.go) ran it mid-edit and got a wrong split: four methods
# that are genuinely test-only landed in deadexports-baseline.txt (the
# zero-callers-even-in-tests list) instead of exported-test-only-baseline.txt,
# because at that moment in the edit the caller graph the scan saw was not
# the one that shipped. CI's fresh scan against the final tree disagreed with
# the committed baseline and failed. Re-running --update against the finished
# tree produced the correct split. The tool has no way to warn about this
# itself -- it can only see the tree it is pointed at, not whether more edits
# are coming.
#
# AND `git add` THE NEW FILES FIRST, for the same reason one layer down: the
# cross-reference enumerates with `git ls-files` (see below), so an UNTRACKED
# .go file does not exist as far as this scan is concerned and its calls into
# everything else are invisible. cleat#1116's PR 3 hit exactly this. A new,
# unstaged cmd/cleatctl/queue.go called four QueueStore methods and one new
# one; --update left all four in exported-test-only-baseline.txt and ADDED the
# fifth to deadexports-baseline.txt -- the zero-callers-even-in-tests list --
# for a method being called one directory over. `git add` and re-run gave the
# right answer: the four came out and the fifth was never added.
#
# The dangerous part is that this failure is silent and reads as a real
# finding, so the obvious response is to accept the new baseline line. The
# self-test below `git add`s its own fixture precisely because the scan reads
# the index rather than the working tree.
#
# The baseline (scripts/deadexports-baseline.txt) exists for the same reason
# check-test-only-code.sh's does: there may be a backlog the day this lands.
# New entries fail the build; every baseline entry needs a reason recorded
# where it's added (commit message), same discipline as the sibling check.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT" || exit 1

# Absolute, because self_test runs the scan from a fixture directory that is
# not under the repo. A relative "scripts/finddeadexports.go" resolves against
# the CWD and would silently fail there -- measured while writing this: the
# copy reported "0 findings" and the diff read as "nothing is dead any more".
FINDER="$REPO_ROOT/scripts/finddeadexports.go"

# The baseline key is <file><TAB>func <Recv.Name> -- deliberately NO line number.
#
# It had one, and that made the guard fail on edits that changed nothing about
# what it measures: adding 18 lines anywhere above a baselined entry shifted its
# line number, the exact-line comparison below saw a string it had never seen,
# and it reported a function that had been dead and baselined for weeks as a
# brand-new dead export. That happened on the very PR that introduced this
# script -- 12 monitoring/prometheus Metrics methods, all already in the
# baseline, all flagged because unrelated edits moved them down the file.
#
# scripts/check-skips.sh and scripts/check-test-only-code.sh both key on the
# enclosing function for this reason, and say so; this script mirrors them, and
# now actually does. A baseline that churns on unrelated edits trains people to
# regenerate it without reading it, which is how a guard stops guarding.
BASELINE="scripts/deadexports-baseline.txt"

# The second question's list, same key and same relative style. Separate file
# rather than a third column on the one above: the two conditions are mutually
# exclusive, so no entry ever appears in both, and one file per question keeps
# each diff about one claim.
TESTONLY_BASELINE="scripts/exported-test-only-baseline.txt"

# Package roots scanned for exported declarations. Deliberately excludes:
#   cleat/        -- "Public Go API" (CLAUDE.md); has its own go.mod
#   pluginapi/    -- "Public re-exports for external plugin authors"
#   crates/       -- Rust + Java SDKs, not Go
#   python-sdk/   -- Python SDK, not Go
#   packages/     -- AssemblyScript SDK, not Go
#   examples/     -- example workflows, not library code with callers to find
#   tests/        -- integration suites; their exported helpers are consumed
#                    by other files under tests/, which this script does not
#                    special-case, and false positives there are cheap to
#                    hit given the suite layout -- left out rather than
#                    fought over
#   web/          -- Svelte, not Go
#   benchmarks/   -- comparative benchmark harnesses, own go.mod files
ROOTS=(engine wasm wasmrw plugin auth migration monitoring internal cmd plugins)

# Method names that can be invoked by the runtime (interface dispatch,
# encoding/json, encoding/sql, net/http, ...) with no source line anywhere
# ever writing `.Name(`. Skipped unconditionally, not baselined -- see the
# "Interface satisfaction" note above.
COMMON_INTERFACE_METHODS='^(Error|String|Unwrap|Is|As|Format|GoString|MarshalJSON|UnmarshalJSON|MarshalText|UnmarshalText|MarshalBinary|UnmarshalBinary|MarshalYAML|UnmarshalYAML|ServeHTTP|Read|Write|ReadFrom|WriteTo|Close|Len|Less|Swap|Scan|Value|Sort|Cap|Seek|Lock|Unlock|RLock|RUnlock|Done|Err|Deadline|Value|Visit|Walk|Init|Reset)$'

if [ "${1:-}" != "--update" ] && [ ! -f "$BASELINE" ]; then
  echo "ERROR: $BASELINE is missing. Generate it with:" >&2
  echo "  scripts/check-dead-exports.sh --update" >&2
  exit 1
fi

# is_code_use decides whether a matching line REFERENCES the name in code, as
# opposed to merely spelling it in a comment or inside a string literal.
#
# WHY THIS EXISTS, and it is the defect cleat#1743 was filed to look for. The
# same-file branch below has excluded comment lines since the script was
# written -- its comment explains exactly why, because a Go doc comment repeats
# its own declaration's name and counted as a caller. That fix was applied to
# ONE of the two branches. The other-file branch asked only whether some other
# file CONTAINED the word, so a name written in prose in any other file marked
# the declaration used, silently and permanently.
#
# Measured on develop before this change: WithWasmCumulativeAllocationMax has
# zero callers -- its three matches in the tree are its own doc comment, its own
# declaration, and a STRING KEY in engine/engine_option_reachability_test.go
# describing why it is unreachable. A test table documenting a function as
# unreachable was the thing keeping it out of the report.
#
# Strings are stripped BEFORE comments, not after: a `//` inside a string
# literal would otherwise truncate the line and hide real code to its left.
# That ordering is the "pair first, filter after" rule in CLAUDE.md.
#
# The residual error is deliberately biased. Stripping too MUCH can drop a real
# call and report a live function as dead -- which fails loudly, as a new
# finding a human reads. Stripping too LITTLE counts prose as a call and marks a
# dead function used -- which is silent, and is the bug being fixed. When this
# heuristic is wrong it should be wrong in the loud direction.
# A fourth argument, skip_tests, makes the filter ignore callers in _test.go
# files. That is the only difference between this guard's two questions -- "does
# anything call it" and "does anything OUTSIDE A TEST call it" -- so they share
# one grep and one filter rather than being two scans that can drift.
code_use_filter() {
  awk -v name="$1" -v decl_file="$2" -v decl_line="$3" -v skip_tests="${4:-0}" '
    {
      # split "file:line:content" on the FIRST two colons only; content keeps
      # any colons of its own.
      i = index($0, ":");            f = substr($0, 1, i-1); rest = substr($0, i+1)
      j = index(rest, ":");          ln = substr(rest, 1, j-1); c = substr(rest, j+1)
      if (f == decl_file && ln == decl_line) next    # the declaration itself
      if (skip_tests == 1 && f ~ /_test\.go$/) next  # a test is not a caller
      gsub(/"[^"]*"/, "", c)                         # strings first...
      gsub(/`[^`]*`/, "", c)
      sub(/\/\/.*/, "", c)                           # ...then comments
      if (c ~ ("(^|[^A-Za-z0-9_])" name "([^A-Za-z0-9_]|$)")) { print "USED"; exit }
    }'
}

# scan emits one finding per line, "<file>\tfunc <Label>", for the given roots.
#
# Extracted into a function so that self_test can drive THE REAL SCAN rather
# than a copy of it. A self-test that reimplements the logic it checks agrees
# with itself by construction, which is the failure this whole change is about.
scan() {
  local decls stderr_f
  decls="$(mktemp)"; stderr_f="$(mktemp)"

  if ! go run "$FINDER" "$@" > "$decls" 2>"$stderr_f"; then
    echo "ERROR: finddeadexports.go failed:" >&2
    cat "$stderr_f" >&2
    rm -f "$decls" "$stderr_f"
    return 2
  fi

  if [ ! -s "$decls" ]; then
    echo "ERROR: finddeadexports.go produced no declarations at all." >&2
    echo "That almost certainly means the scan is broken (wrong roots, parser" >&2
    echo "failure on everything) rather than that there are zero exported" >&2
    echo "top-level funcs/methods in $*. Treating that as a clean" >&2
    echo "scan would be a vacuous pass -- exactly what this guard exists to" >&2
    echo "refuse. stderr from the scan:" >&2
    cat "$stderr_f" >&2
    rm -f "$decls" "$stderr_f"
    return 2
  fi

  # One `git ls-files` for the whole scan rather than one per symbol: the list
  # does not change while scan() runs, and there are ~1500 declarations.
  local tracked
  tracked="$(mktemp)"
  git ls-files -z '*.go' > "$tracked"
  if [ ! -s "$tracked" ]; then
    echo "ERROR: git ls-files '*.go' produced nothing in $(pwd)." >&2
    echo "The scan reads the INDEX (cleat#1783), so an empty list means every" >&2
    echo "declaration would look unused and the whole baseline would be" >&2
    echo "rewritten as dead. Refusing rather than reporting that." >&2
    rm -f "$decls" "$stderr_f" "$tracked"
    return 2
  fi

  local out="" file line recv name matches label
  while IFS=$'\t' read -r file line recv name; do
    if [[ "$name" =~ $COMMON_INTERFACE_METHODS ]]; then
      continue
    fi

    # THE INDEX, NOT THE WORKING DIRECTORY (cleat#1783). This used to be
    # `grep -rnw --include='*.go' . `, which walks whatever is on disk -- and
    # this repo routinely holds whole additional copies of itself under
    # .claude/worktrees/. A copy of a declaration in another session's scratch
    # checkout has a different path, so code_use_filter's one exclusion (the
    # declaration's own file:line) does not cover it; the copy's `func Name(`
    # is neither comment nor string, so it survives both gsubs and matches the
    # identifier regex. The symbol reads as used.
    #
    # Both directions exist and only one is loud. A BASELINED symbol that looks
    # used stops being reported and the staleness half fails, which is how
    # cleat#1783 was found. A symbol that is genuinely dead and NOT yet
    # baselined also looks used -- and is silently never reported. That is the
    # direction worth fixing: the guard goes quiet in proportion to how messy
    # the checkout is.
    #
    # Measured twice on the same commit: in a pristine worktree the two file
    # sets agreed exactly (1481 = 1481, difference 0) and the guard was
    # correct; in a checkout carrying .claude/worktrees/ it was not. An answer
    # that depends on what happens to be beside the repo cannot be reproduced
    # from the commit, which is worse than being consistently wrong.
    #
    # -n rather than -l: every branch reasons about LINES, so a comment or
    # string is treated identically wherever it lives. -w so Deploy does not
    # match Deployment. -H because the file list arrives through xargs, and a
    # final chunk of ONE file makes grep omit the filename prefix that every
    # branch of code_use_filter parses. -z/-0 so a path containing whitespace
    # is one argument rather than two.
    matches="$(xargs -0 grep -Hnw -- "$name" < "$tracked" 2>/dev/null)"

    label="$name"
    if [ "$recv" != "-" ]; then
      label="${recv}.${name}"
    fi

    # TWO PREDICATES OVER ONE GREP, and the classification is mutually
    # exclusive by construction:
    #
    #   dead      nothing calls it, tests included
    #   testonly  something calls it, and everything that does is a test
    #
    # That exclusivity is what keeps the two baselines from overlapping. A
    # zero-caller symbol is the STRONGER finding, so it is reported as dead and
    # never appears in the test-only list -- otherwise every entry in
    # deadexports-baseline.txt would also need an entry in the other file, and
    # two files asserting the same thing drift apart.
    if [ -n "$(printf '%s\n' "$matches" | code_use_filter "$name" "$file" "$line" 0)" ]; then
      if [ -n "$(printf '%s\n' "$matches" | code_use_filter "$name" "$file" "$line" 1)" ]; then
        continue                                  # a real, non-test caller
      fi
      out="${out}${file}	func ${label}	testonly"$'\n'
      continue
    fi
    out="${out}${file}	func ${label}	dead"$'\n'
  done < "$decls"

  rm -f "$decls" "$stderr_f" "$tracked"
  printf '%s' "$out" | grep -v '^$' | LC_ALL=C sort -u || true
}


# self_test drives THE REAL scan() against a fixture whose correct answer is
# known independently of anything this script computes.
#
# WHY IT EXISTS (cleat#1743). This guard regenerates its own baseline from its
# own scan: `--update` writes whatever scan() says, so the baseline agrees with
# the scan by construction, correct or not. Reviewing the diff tells you the
# diff is consistent with the scan; it cannot tell you the scan is right. Until
# this fixture there was nothing in the tree asserting that scan() answers
# correctly for ANY input, and a systematic error would have been reproduced
# faithfully every time with all the totals balancing.
#
# It is not hypothetical. On develop this scan counted a name appearing in a
# COMMENT or inside a STRING LITERAL in any other file as a caller. Two of the
# functions it was hiding were hidden by files whose entire purpose is to record
# that they are unreachable -- engine_option_reachability_test.go's table entry
# for WithWasmCumulativeAllocationMax, and every_metric_has_a_feeder_test.go's
# entry for RecordWasmCompileDuration, which reads "NEEDS A CALL SITE". One
# guard's documentation was silencing another guard.
#
# EVERY CASE BELOW MUST BE ABLE TO FAIL ON ITS OWN. Cases that are merely
# consistent with the fix prove nothing: cases 5 and 6 are the ones that redden
# against the pre-fix scan, and cases 2 and 3 are what stop a scan that reports
# EVERYTHING as dead from passing the other four. A fixture set where each
# member is rescued by another member is decoration -- which is a lesson this
# repo paid for elsewhere this week.
self_test() {
  local tmp expected got
  tmp="$(mktemp -d)" || return 1
  mkdir -p "$tmp/pkg"

  cat > "$tmp/pkg/a.go" <<'FIXTURE_A'
package pkg

// DeadNoRefs is spelled nowhere else in the fixture.
func DeadNoRefs() {}

// LiveFromOtherFile is called from b.go.
func LiveFromOtherFile() {}

// LiveFromSameFile is called by sameFileCaller below.
func LiveFromSameFile() {}

// DeadOnlyInOwnDocComment is named by this doc comment and nowhere else.
// A Go doc comment repeats its own declaration's name, so a scan that counts
// its own documentation as a reference reports nothing as dead, ever.
func DeadOnlyInOwnDocComment() {}

// DeadOnlyInCommentElsewhere is named only by a comment in b.go.
func DeadOnlyInCommentElsewhere() {}

// DeadOnlyInStringElsewhere is named only inside a string literal in b.go.
func DeadOnlyInStringElsewhere() {}

// TestOnlyCaller is called from a_test.go and from nowhere else. It is the
// known-positive for the second predicate: a scan that answers only the
// "zero callers anywhere" question reports nothing for this, because the test
// IS a caller.
func TestOnlyCaller() {}

// LiveFromTestAndCode is called from a_test.go AND from b.go. It is the
// control that stops a scan which reports EVERYTHING as test-only from
// passing: without it, classifying every symbol as testonly satisfies the
// TestOnlyCaller case.
func LiveFromTestAndCode() {}

func sameFileCaller() { LiveFromSameFile() }
FIXTURE_A

  cat > "$tmp/pkg/b.go" <<'FIXTURE_B'
package pkg

// DeadOnlyInCommentElsewhere is deliberately named here in prose, and never
// called. A comment is not a caller.
//
// Neither is a string: "DeadOnlyInStringElsewhere" appears below as data.
var notACall = map[string]string{
	"DeadOnlyInStringElsewhere": "documented as unreachable; still not a call",
}

func otherFileCaller() { LiveFromOtherFile(); LiveFromTestAndCode() }

var _ = notACall
FIXTURE_B

  # A _test.go file is not a source of DECLARATIONS (finddeadexports.go skips
  # them) but it is a source of REFERENCES, which is the whole point of the
  # second predicate.
  cat > "$tmp/pkg/a_test.go" <<'FIXTURE_TEST'
package pkg

import "testing"

func TestBoth(t *testing.T) {
	TestOnlyCaller()
	LiveFromTestAndCode()
}
FIXTURE_TEST

  # cleat#1783's case: a whole copy of the package in a scratch directory, of
  # the kind .claude/worktrees/ holds. It is deliberately NOT added to the
  # fixture's index -- that is what makes it a copy rather than a second real
  # file, and it mirrors reality, where .gitignore line 127 excludes
  # .claude/worktrees/ so git never tracks it.
  #
  # Every DeadNoRefs-style symbol below is declared again in here. If the scan
  # reads the working directory instead of the index, each of those copies is a
  # `func Name(` line in a file with a different path -- not the declaration's
  # own file:line, not a comment, not a string -- so it is counted as a caller
  # and the four dead symbols vanish from the report. The expectation is
  # therefore unchanged BY the copy, which is the whole assertion.
  mkdir -p "$tmp/.claude/worktrees/scratch/pkg"
  cp "$tmp/pkg/a.go" "$tmp/.claude/worktrees/scratch/pkg/a.go"
  cp "$tmp/pkg/b.go" "$tmp/.claude/worktrees/scratch/pkg/b.go"

  # The scan reads the index, so the fixture has to have one. git init rather
  # than a repo/non-repo branch in scan(): one code path, and the self-test
  # then exercises the real file selection instead of a special case of it.
  # `git add pkg` and not `git add .` -- adding everything would track the copy
  # above and make it a legitimate second declaration.
  git -C "$tmp" init -q 2>/dev/null || { echo "self-test: git init failed" >&2; rm -rf "$tmp"; return 1; }
  git -C "$tmp" add pkg || { echo "self-test: git add failed" >&2; rm -rf "$tmp"; return 1; }

  expected="$(printf '%s\n' \
    'pkg/a.go	func DeadNoRefs	dead' \
    'pkg/a.go	func DeadOnlyInCommentElsewhere	dead' \
    'pkg/a.go	func DeadOnlyInOwnDocComment	dead' \
    'pkg/a.go	func DeadOnlyInStringElsewhere	dead' \
    'pkg/a.go	func TestOnlyCaller	testonly' | LC_ALL=C sort)"

  # scan() greps "." from the CWD, so running it from the fixture points both
  # halves -- declarations and references -- at the fixture and nothing else.
  got="$(cd "$tmp" && scan pkg)" || { rm -rf "$tmp"; echo "self-test: scan failed" >&2; return 1; }
  got="$(printf '%s\n' "$got" | grep -v '^$' | LC_ALL=C sort || true)"
  rm -rf "$tmp"

  if [ "$got" != "$expected" ]; then
    echo "ERROR: check-dead-exports.sh self-test FAILED." >&2
    echo "The scan does not give the right answer on a fixture whose answer is" >&2
    echo "known, so nothing it says about the real tree can be trusted -- and" >&2
    echo "--update would write the wrong baseline without anything looking odd." >&2
    echo >&2
    echo "  expected:" >&2; printf '%s\n' "$expected" | sed 's/^/    /' >&2
    echo "  got:" >&2;      printf '%s\n' "$got"      | sed 's/^/    /' >&2
    return 1
  fi
  return 0
}

if [ "${1:-}" = "--self-test" ]; then
  self_test || exit 1
  echo "self-test passed"
  exit 0
fi

# The self-test runs on EVERY invocation, including --update, rather than behind
# a flag that CI has to remember to call. A self-test nobody runs is a comment.
# It costs about a second against a scan that takes four minutes, and --update
# is precisely the path that must not write a baseline from a broken scan.
self_test || exit 1

findings="$(scan "${ROOTS[@]}")" || exit 2

# The scan's third column is the condition. Split here and drop it, so each
# baseline file keeps the two-column "<file>\tfunc <Label>" shape it already
# has -- adding a column would have rewritten every line of
# deadexports-baseline.txt for no gain and made the diff unreadable.
dead_findings="$(printf '%s\n' "$findings" | awk -F'\t' '$3=="dead"     {print $1"\t"$2}')"
testonly_findings="$(printf '%s\n' "$findings" | awk -F'\t' '$3=="testonly" {print $1"\t"$2}')"

if [ "${1:-}" = "--update" ]; then
  printf '%s\n' "$dead_findings"     | grep -v '^$' > "$BASELINE" || true
  printf '%s\n' "$testonly_findings" | grep -v '^$' > "$TESTONLY_BASELINE" || true
  echo "Wrote $(grep -c . "$BASELINE" || true) entries to $BASELINE"
  echo "Wrote $(grep -c . "$TESTONLY_BASELINE" || true) entries to $TESTONLY_BASELINE"
  exit 0
fi

# gate compares one condition's findings against one baseline, both directions:
# new findings fail, and baseline lines the scan no longer reports fail too.
#
# One function rather than two copies, because the second copy is where the
# staleness half gets forgotten -- and a baseline that can only grow stale is
# the defect cleat#1743 was filed for.
gate() {
  local found="$1" baseline="$2" headline="$3" advice="$4" new stale
  new="$(printf '%s\n' "$found" | grep -Fxv -f "$baseline" || true)"
  new="$(printf '%s' "$new" | grep -v '^$' || true)"
  if [ -n "$new" ]; then
    echo "ERROR: $headline" >&2
    echo >&2
    printf '%s\n' "$new" | sed 's/^/  /' >&2
    echo >&2
    printf '%s\n' "$advice" >&2
    return 1
  fi

  stale="$(LC_ALL=C comm -23 <(LC_ALL=C sort -u "$baseline") \
                             <(printf '%s\n' "$found" | grep -v '^$' | LC_ALL=C sort -u))"
  if [ -n "$(printf '%s' "$stale" | tr -d '[:space:]')" ]; then
    echo "ERROR: $baseline lists entries the scan no longer reports:" >&2
    echo >&2
    printf '%s\n' "$stale" | sed 's/^/  /' >&2
    echo >&2
    echo "A baseline is a ratchet: it may shrink, never silently hold. Each line" >&2
    echo "above is a standing claim the scan now disagrees with -- because the" >&2
    echo "symbol was wired up, deleted, or the scan itself changed. Left alone" >&2
    echo "the file accumulates false claims that read as authoritative: on" >&2
    echo "develop before cleat#1743, 10 of 16 entries were stale, one named a" >&2
    echo "function that no longer existed, and the guard reported all 16 as" >&2
    echo "'known entries' on every green run." >&2
    echo >&2
    echo "Re-derive both baselines with:" >&2
    echo "  scripts/check-dead-exports.sh --update" >&2
    return 1
  fi
  return 0
}

rc=0
gate "$dead_findings" "$BASELINE" \
  "exported code with zero callers anywhere in the tree (not even tests):" \
  "Either wire it into production, delete it, or -- if it is genuinely
meant as public API not yet adopted -- move it under cleat/ or
pluginapi/ (the packages this script treats as public surface), or
add it to $BASELINE with a reason in the commit message via
  scripts/check-dead-exports.sh --update" || rc=1

gate "$testonly_findings" "$TESTONLY_BASELINE" \
  "exported code whose ONLY callers are tests (cleat#1795):" \
  "This is not reported by either guard that exists to catch dead code, which
is why it has its own list. scripts/check-test-only-code.sh runs staticcheck
U1000, and U1000 does not report exported identifiers in non-main packages at
all; the zero-caller question above sees the test as a caller and moves on. So
a symbol here is tested, passing, and wired to nothing -- the shape
cmd/cleatctl/revokeapikey.go:22 describes for auth.TenantStore.RevokeAPIKey,
which was noticed, written about, given a CLI command, and is STILL uncalled
because the command issues its own SQL instead.

Either give it a production caller, delete it, or add it to
$TESTONLY_BASELINE via
  scripts/check-dead-exports.sh --update" || rc=1

[ "$rc" -eq 0 ] || exit 1

echo "OK: no new dead exports ($(grep -c . "$BASELINE" || true) known) and no new"
echo "    test-only exports ($(grep -c . "$TESTONLY_BASELINE" || true) known), none stale."
