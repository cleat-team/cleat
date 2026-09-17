#!/usr/bin/env bash
#
# Run the `cleat build` command each example's README documents, and require it
# to succeed. cleat#1814.
#
# WHY THIS EXISTS
#
# Measured 2026-09-17: 18 example directories, 10 whose README documents a
# `cleat build …`, and ZERO built by any workflow. Every `cleat build` string in
# .github/workflows/ is inside a comment. So the commands we tell users to run
# were the commands nothing ran.
#
# That was load-bearing, twice in one day. Two independent implementations of the
# Python determinism gate both refused `examples/python-langchain`'s documented
# command, producing 24 false positives from a test harness (cleat#1813). One was
# caught by running the command by hand; the other (cleat#1809) reached 51
# passing checks with one failure, and that failure was an unrelated lint. Remove
# that lint and it merges.
#
# A green CI run was evidence that the unit tests pass. It was not evidence that
# the product does what the documentation says.
#
# DERIVED, NOT LISTED
#
# The set comes from the READMEs, not from a list in this file. A hand-maintained
# list is the thing that goes stale silently -- ci.yml:810 already carries that
# lesson about a shellcheck file list that had been missing four scripts for
# months. Add an example with a documented build and it is covered; nobody has to
# remember this script.
#
# EXCLUSIONS ARE NAMED, WITH AN ISSUE
#
# An example whose documented command is known broken is listed in
# KNOWN_BROKEN with the issue that tracks it. That keeps this job green and
# honest at once: the exclusion is visible, it names why, and when the issue is
# fixed the entry is deleted and the example is covered. A job that silently
# skipped what it could not build would report the same green as one that built
# everything.
set -uo pipefail

cd "$(git rev-parse --show-toplevel)" || exit 2

# Examples whose documented command is known broken, as "dir<TAB>reason" lines.
#
# NOT an associative array: macOS ships bash 3.2, which has none, and a script
# the author cannot run locally is how this job would come to be trusted without
# being exercised -- which is the failure this whole file exists to prevent.
KNOWN_BROKEN="\
examples/python-langchain\tcleat#1836 - a relative --entry is resolved against the SDK root
examples/python-hello\tcleat#1836 - a relative --entry is resolved against the SDK root"

known_broken_reason() {
  printf '%b\n' "$KNOWN_BROKEN" | while IFS="$(printf '\t')" read -r d reason; do
    [ "$d" = "$1" ] && printf '%s' "$reason"
  done
}

CLEAT_BIN="${CLEAT_BIN:-$(pwd)/.bin/cleat}"
if [[ ! -x "$CLEAT_BIN" ]]; then
  echo "UNMEASURED: no cleat binary at $CLEAT_BIN. Build it first:" >&2
  echo "  go build -o .bin/cleat ./cmd/cleat" >&2
  exit 2
fi

examined=0 built=0 failed=0 skipped=0
declare -a FAILURES=()

while IFS= read -r readme; do
  dir="$(dirname "$readme")"
  # The first documented `cleat build …` line, taken verbatim. A `$ ` prompt is
  # stripped; nothing else is rewritten, because rewriting the command would
  # test something other than what the README says.
  cmd="$(grep -m1 -oE '(^|\$ )cleat build[^`]*' "$readme" | sed 's/^\$ //' | sed 's/[[:space:]]*$//')"
  [[ -z "$cmd" ]] && continue
  examined=$((examined + 1))

  reason="$(known_broken_reason "$dir")"
  if [[ -n "$reason" ]]; then
    echo "SKIP  $dir"
    echo "      $reason"
    skipped=$((skipped + 1))
    continue
  fi

  # Run it from the example's own directory when the command names no path, and
  # from the repository root when it does -- which is how each README presents
  # it. Both are user-facing forms and both must work.
  if [[ "$cmd" == *"./examples/"* ]]; then
    workdir="."
  else
    workdir="$dir"
  fi

  out="$(mktemp -d)"
  run="${cmd/cleat build/$CLEAT_BIN build}"
  run="${run/-o \/tmp\/out/-o $out}"
  if (cd "$workdir" && eval "$run") >/tmp/bde.log 2>&1; then
    echo "OK    $dir"
    built=$((built + 1))
  else
    echo "FAIL  $dir"
    echo "      \$ $cmd"
    sed 's/^/      /' /tmp/bde.log | tail -8
    FAILURES+=("$dir")
    failed=$((failed + 1))
  fi
  rm -rf "$out"
done < <(git ls-files 'examples/*/README.md')

echo
echo "examples with a documented build: $examined   built: $built   skipped: $skipped   failed: $failed"

# A run that examined nothing is a broken check, not a clean tree. Ten READMEs
# document a build today; a scan finding none means the pattern or the path
# stopped matching, and "0 failures out of 0" is what that looks like from
# outside.
if (( examined < 8 )); then
  echo "UNMEASURED: found only $examined examples with a documented cleat build; expected at least 8." >&2
  echo "The scan stopped matching, so a clean result here would mean nothing." >&2
  exit 2
fi

# An exclusion that matches no example is a grant covering something that no
# longer exists -- the same arm every other guard in this repo carries.
while IFS="$(printf '\t')" read -r d _; do
  [ -z "$d" ] && continue
  if [[ ! -f "$d/README.md" ]]; then
    echo "STALE EXCLUSION: $d is listed in KNOWN_BROKEN but has no README.md; delete the entry." >&2
    exit 1
  fi
done < <(printf '%b\n' "$KNOWN_BROKEN")

if (( failed > 0 )); then
  echo >&2
  echo "These examples document a cleat build command that does not work:" >&2
  printf '  %s\n' "${FAILURES[@]}" >&2
  echo >&2
  echo "Fix the command, fix the build, or add the example to KNOWN_BROKEN with the" >&2
  echo "issue that tracks it. Do not delete the documented command." >&2
  exit 1
fi
