#!/usr/bin/env bash
#
# Detect code in a main package that nothing reachable from main() calls.
#
# This is the third dead-code check in scripts/, and it exists because the other
# two structurally cannot see one shape:
#
#   check-test-only-code.sh  staticcheck U1000, -tests=false. Catches "only tests
#                            call it" -- but U1000 does not report EXPORTED
#                            identifiers, on the theory that a library's public
#                            API may have callers outside the module.
#   check-dead-exports.sh    a grep for exported symbols in "internal" package
#                            roots whose name appears in no other file.
#
# U1000's theory is right for a library and meaningless for `package main`:
# nothing can import a main package, so "exported" carries no information there.
# An exported, test-only function in cmd/ is therefore invisible to both.
#
# That is not hypothetical. Issue #829 records StartAPIServer -- exported, in
# package main, called only by two tests, and the sole caller of registerRoutes,
# which is why seven API routes were unreachable from the shipped binary. The
# check that exists to find exactly this said "OK: no new test-only code".
#
# Measured on this tree, 2026-09-06, with a matched pair in cmd/cleat-worker:
#
#   func probeUnexportedTestOnly()   ->  REPORTED by check-test-only-code.sh
#   func ProbeExportedTestOnly()     ->  invisible to it, reported by this
#
# Same shape, same package, same test-only caller; only exportedness differs.
#
# METHOD. `deadcode` computes reachability from main() rather than syntactic
# use, so exportedness never enters into it. It reports across the whole
# program, which for one command is ~400 findings dominated by cleat's library
# packages and its dependencies -- the other two checks own that ground. This
# script keeps only findings in cmd/'s own files, which is 14 across every
# command as of this commit, and small enough that the baseline can be read.
#
# The baseline is keyed on "<dir><TAB><symbol>", not the raw line, so moving a
# function down a file does not churn it -- same convention as its siblings.
set -euo pipefail

cd "$(dirname "$0")/.."

DEADCODE="golang.org/x/tools/cmd/deadcode@v0.47.0"   # pinned: findings differ by version
TOOLDIR="${TMPDIR:-/tmp}/cleat-deadcode-tools"
BASELINE="scripts/unreachable-main-baseline.txt"
UPDATE=0
[ "${1:-}" = "--update" ] && UPDATE=1

mkdir -p "$TOOLDIR"
if [ ! -x "$TOOLDIR/deadcode" ]; then
  GOBIN="$TOOLDIR" go install "$DEADCODE" >&2 || {
    echo "ERROR: could not install $DEADCODE" >&2; exit 1; }
fi

scan() {
  local d found=0 out rc
  for d in cmd/*/; do
    ls "$d"*.go >/dev/null 2>&1 || continue
    # -test=false: this asks what the SHIPPED binary reaches. Including tests
    # would make every test-only symbol reachable, which is the whole defect.
    #
    # deadcode's OWN exit status is captured rather than piped away. Sending it
    # straight into a pipe discards it -- a pipeline reports only the LAST
    # command's status, grep exits 1 on no-match which is the normal case here,
    # and `|| true` then swallowed both. A deadcode that failed on ONE command
    # therefore contributed nothing and was indistinguishable from a command
    # with no findings, which seven of the nine legitimately are.
    #
    # The whole-scan emptiness guard below cannot see that: the other commands
    # still report, so `raw` is non-empty and the scan looks healthy while a
    # command's worth of unreachable code has silently dropped out. Measured
    # 2026-09-17, all nine commands scan clean, so this is a latent hole being
    # closed rather than a live failure being fixed.
    out="$("$TOOLDIR/deadcode" -test=false "./$d" 2>&1)" && rc=0 || rc=$?
    if [ "$rc" -ne 0 ]; then
      echo "ERROR: deadcode failed on ./$d (exit $rc). This is a broken scan," >&2
      echo "not a clean command -- its findings would otherwise be silently" >&2
      echo "absent while the other commands kept the overall scan looking fine." >&2
      printf '%s\n' "$out" | head -20 | sed 's/^/  /' >&2
      exit 1
    fi
    # Anchored on the finding shape, not just on "cmd/", so that a stray
    # diagnostic cannot be mistaken for a finding now that stderr is merged in.
    # If deadcode's format ever changes this matches nothing, and the emptiness
    # guard below turns that into a loud failure rather than a clean scan.
    printf '%s\n' "$out" | grep '^cmd/.*: unreachable func: ' || true
    found=1
  done
  [ "$found" = 1 ] || { echo "ERROR: no cmd/ packages found to scan" >&2; exit 1; }
}

# "<dir>\t<symbol>", deduped and sorted. Line numbers deliberately dropped.
normalise() {
  sed 's|^\(cmd/[^/]*\)/[^:]*:[0-9]*:[0-9]*: unreachable func: |\1\t|' |
    LC_ALL=C sort -u
}

# self_test drives THE REAL scan() and normalise() against a fixture whose
# answer is known independently of anything this script computes.
#
# WHY (cleat#1743). This guard regenerates its baseline from its own scan:
# `--update` writes whatever the scan says. Reviewing that diff tells you the
# diff is consistent with the scan; it cannot tell you the scan is right, and a
# systematic error would be reproduced faithfully with every total balancing.
#
# The scan here is a real reachability analysis rather than a text search, and
# it was measured correct on the tree before this fixture was written -- the
# sibling check-dead-exports.sh had a genuine scan bug and this one does not.
# That is exactly why the fixture is worth having: "it looks right today" is the
# state a guard is in immediately before it stops being right, and nothing else
# in the tree would notice.
#
# EACH CASE MUST BE ABLE TO FAIL ON ITS OWN.
#   onlyCalledFromTest    the guard's whole reason to exist (cleat#829,
#                         StartAPIServer): a symbol reachable ONLY from a test
#                         is unreachable in the shipped binary. Set -test=true
#                         and this one vanishes from the expected set.
#
#                         NOT "drop -test=false", which is what this comment
#                         said first and is a no-op -- false is deadcode's
#                         DEFAULT, so removing the flag mutates nothing and the
#                         self-test stayed green. A falsification that changes
#                         no behaviour reads exactly like a case the test cannot
#                         catch; measured against deadcode -help rather than
#                         assumed the second time.
#   UnreachableExported   exportedness must not matter. staticcheck's U1000 skips
#                         exported identifiers, which is the hole this script was
#                         written to cover; if it ever starts mattering here, the
#                         script has silently become its own sibling.
#   unreachableUnexported the ordinary case.
#   reachableHelper       the NEGATIVE control, and without it a scan that
#                         reported EVERY function would pass the other three.
self_test() {
  local tmp expected got
  tmp="$(mktemp -d)" || return 1
  mkdir -p "$tmp/cmd/probecmd"

  cat > "$tmp/go.mod" <<'FIXTURE_MOD'
module probe

go 1.25
FIXTURE_MOD

  cat > "$tmp/cmd/probecmd/main.go" <<'FIXTURE_MAIN'
package main

func main() { reachableHelper() }

func reachableHelper() {}

func UnreachableExported() {}

func unreachableUnexported() {}

func onlyCalledFromTest() {}
FIXTURE_MAIN

  cat > "$tmp/cmd/probecmd/main_test.go" <<'FIXTURE_TEST'
package main

import "testing"

func TestOnlyCaller(t *testing.T) { onlyCalledFromTest() }
FIXTURE_TEST

  expected="$(printf '%s\n' \
    'cmd/probecmd	UnreachableExported' \
    'cmd/probecmd	onlyCalledFromTest' \
    'cmd/probecmd	unreachableUnexported' | LC_ALL=C sort)"

  # scan() iterates cmd/*/ relative to the CWD, so running it from the fixture
  # points it at the fixture and nothing else. A subshell keeps the cd local.
  got="$( cd "$tmp" && scan | normalise )" || { rm -rf "$tmp"; echo "self-test: scan failed" >&2; return 1; }
  got="$(printf '%s\n' "$got" | grep -v '^$' | LC_ALL=C sort || true)"
  rm -rf "$tmp"

  if [ "$got" != "$expected" ]; then
    echo "ERROR: check-unreachable-main.sh self-test FAILED." >&2
    echo "The scan does not give the right answer on a fixture whose answer is" >&2
    echo "known, so nothing it says about cmd/ can be trusted -- and --update" >&2
    echo "would write that wrong answer into the baseline with the totals" >&2
    echo "balancing and the diff looking reasonable." >&2
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

# Runs on EVERY invocation, including --update, rather than behind a flag CI has
# to remember to call -- a self-test nobody runs is a comment. It costs one
# deadcode pass over a five-function fixture, and --update is precisely the path
# that must not write a baseline from a broken scan.
self_test || exit 1

raw="$(scan)"

# A tool that reports nothing is indistinguishable from a clean tree, and this
# repo has shipped that false green before. cmd/cleat-worker alone has hundreds
# of whole-program findings, so an empty raw scan means deadcode did not run.
if [ -z "$raw" ]; then
  echo "ERROR: deadcode reported nothing at all. That is a broken scan, not a" >&2
  echo "clean tree -- it always reports library findings even when cmd/ is clean." >&2
  exit 1
fi

current="$(printf '%s\n' "$raw" | normalise)"

if [ "$UPDATE" = 1 ]; then
  printf '%s\n' "$current" > "$BASELINE"
  echo "Wrote $(printf '%s\n' "$current" | grep -c . ) entries to $BASELINE"
  exit 0
fi

[ -f "$BASELINE" ] || { echo "ERROR: $BASELINE missing" >&2; exit 1; }

new="$(LC_ALL=C comm -23 <(printf '%s\n' "$current") <(LC_ALL=C sort -u "$BASELINE"))"

if [ -n "$new" ]; then
  echo "ERROR: new code in a main package that main() cannot reach:" >&2
  echo "" >&2
  printf '  %s\n' "$new" >&2
  echo "" >&2
  echo "Either wire it into the command, delete it, or -- if it is genuinely" >&2
  echo "meant to be unreachable -- add it with" >&2
  echo "  scripts/check-unreachable-main.sh --update" >&2
  echo "and say why in the commit message." >&2
  exit 1
fi

stale="$(LC_ALL=C comm -13 <(printf '%s\n' "$current") <(LC_ALL=C sort -u "$BASELINE"))"
if [ -n "$stale" ]; then
  echo "ERROR: $BASELINE lists entries the scan no longer reports:" >&2
  echo "" >&2
  printf '  %s\n' "$stale" >&2
  echo "" >&2
  echo "A baseline is a ratchet: it may shrink, never silently hold. Each line" >&2
  echo "above is a standing claim that main() cannot reach a symbol, and the" >&2
  echo "scan now disagrees -- normally because somebody wired it up, which is" >&2
  echo "this guard working and deserves to be recorded as such." >&2
  echo "" >&2
  echo "Worker.compactionLoop was exactly that: defined and never started, found" >&2
  echo "here, fixed in setup.go, and still listed as unreachable afterwards" >&2
  echo "because nothing compared the baseline against the scan (cleat#1743)." >&2
  echo "" >&2
  echo "Re-derive it with" >&2
  echo "  scripts/check-unreachable-main.sh --update" >&2
  echo "and say in the commit message what made each line reachable." >&2
  exit 1
fi

echo "OK: no new unreachable-from-main code ($(grep -c . "$BASELINE") known entries, none stale)."
