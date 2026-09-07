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
  local d found=0
  for d in cmd/*/; do
    ls "$d"*.go >/dev/null 2>&1 || continue
    # -test=false: this asks what the SHIPPED binary reaches. Including tests
    # would make every test-only symbol reachable, which is the whole defect.
    "$TOOLDIR/deadcode" -test=false "./$d" 2>/dev/null | grep '^cmd/' || true
    found=1
  done
  [ "$found" = 1 ] || { echo "ERROR: no cmd/ packages found to scan" >&2; exit 1; }
}

# "<dir>\t<symbol>", deduped and sorted. Line numbers deliberately dropped.
normalise() {
  sed 's|^\(cmd/[^/]*\)/[^:]*:[0-9]*:[0-9]*: unreachable func: |\1\t|' |
    LC_ALL=C sort -u
}

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

echo "OK: no new unreachable-from-main code ($(grep -c . "$BASELINE") known entries)."
