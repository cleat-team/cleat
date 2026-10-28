#!/usr/bin/env bash
#
# Runs the REAL `Check stream trailers` step of
# .github/workflows/stream-trailer-check.yml -- the text, extracted from the YAML
# -- against throwaway repositories, and asserts what it says and how it exits.
#
# Exercising a copy would certify the copy. The step is ~200 lines of bash inside
# a workflow, and the way it has been tested until now is by hand, once, on the
# PR that wrote it (cleat#3488 is the first change with a known-positive in
# CI for it).
#
# The cases are the ones the check exists to tell apart:
#   * a good commit passes;
#   * a commit whose trailer git will not parse FAILS (the unreleased case);
#   * the SAME commit reachable from a vX.Y.Z tag is a notice that names it and
#     the tag, and passes -- it cannot be amended (cleat#3488);
#   * a prerelease or non-semver tag does NOT widen that exemption;
#   * a released finding does not hide an unreleased one in the same range;
#   * an empty range is UNMEASURED (exit 2), not a pass.
#
# Exit 0 all cases held, 1 a case did not.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORKFLOW="${STREAM_TRAILER_WORKFLOW:-$ROOT/.github/workflows/stream-trailer-check.yml}"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# Extract the step's run: text. Anchored on the step NAME, not on a line number.
python3 - "$WORKFLOW" "$WORK/step.sh" <<'PY' || { echo "UNMEASURED: could not extract the step from the workflow" >&2; exit 2; }
import sys, yaml
doc = yaml.safe_load(open(sys.argv[1]))
steps = doc["jobs"]["stream-trailer"]["steps"]
hits = [s for s in steps if s.get("name") == "Check stream trailers"]
if len(hits) != 1 or not hits[0].get("run"):
    sys.exit(1)
open(sys.argv[2], "w").write(hits[0]["run"])
PY
[ -s "$WORK/step.sh" ] || { echo "UNMEASURED: extracted step is empty" >&2; exit 2; }

fail=0
nth=0
OUT=""
RC=0

new_repo() {
  nth=$((nth + 1))
  R="$WORK/repo$nth"
  git init -q "$R"
  git -C "$R" config user.email t@example.com
  git -C "$R" config user.name t
  # A base commit the convention-start exemption cannot apply to... it predates
  # nothing: every commit below is dated after 2026-09-17.
  GIT_COMMITTER_DATE="2026-10-01T00:00:00Z" GIT_AUTHOR_DATE="2026-10-01T00:00:00Z" \
    git -C "$R" commit -q --allow-empty -m "base"
}

commit() { # commit <message>
  GIT_COMMITTER_DATE="2026-10-02T00:00:00Z" GIT_AUTHOR_DATE="2026-10-02T00:00:00Z" \
    git -C "$R" commit -q --allow-empty -m "$1"
}

GOOD=$'good\n\nbody\n\nClaude-Stream: WS-cleat-3\nSigned-off-by: t <t@example.com>'
# What GitHub's squash made of #3464 on release/v0.5.0: the trailer, a blank
# line, then the appended block.
MALFORMED=$'squashed\n\nbody\n\nClaude-Stream: WS-cleat\n\nSigned-off-by: t <t@example.com>\nCo-authored-by: t <t@example.com>'
MISSING=$'no trailer here\n\nSigned-off-by: t <t@example.com>'

run_step() { # run_step <base> <head>
  OUT="$(cd "$R" && BASE="$1" HEAD="$2" PR_AUTHOR=human bash -e "$WORK/step.sh" 2>&1)"
  RC=$?
}

expect() { # expect <label> <want rc> <substring...>
  local label="$1" want="$2"; shift 2
  local why=""
  [ "$RC" -eq "$want" ] || why="exit $RC, want $want"
  for sub in "$@"; do
    # Assert on the TEXT as well as the status: another mechanism can supply the
    # right status for the wrong reason (CLAUDE.md, "A condition that never
    # decides anything").
    case "$OUT" in *"$sub"*) ;; *) why="${why:+$why; }output lacks: $sub" ;; esac
  done
  if [ -z "$why" ]; then
    printf '  %-66s ok\n' "$label"
  else
    printf '  %-66s FAIL: %s\n' "$label" "$why"
    printf '%s\n' "$OUT" | sed 's/^/      | /' | head -25
    fail=$((fail + 1))
  fi
}

refute() { # refute <label> <substring> -- the output must NOT contain it
  case "$OUT" in
    *"$2"*) printf '  %-66s FAIL: output contains: %s\n' "$1" "$2"; fail=$((fail + 1)) ;;
    *) printf '  %-66s ok\n' "$1" ;;
  esac
}

head_of() { git -C "$R" rev-parse HEAD; }

echo "extracted $(wc -l < "$WORK/step.sh" | tr -d ' ') lines of the real step"

new_repo; base=$(head_of); commit "$GOOD"
run_step "$base" "$(head_of)"
expect "a well-formed trailer passes" 0 "All 1 checked commit(s) carry a valid Claude-Stream trailer"

new_repo; base=$(head_of); commit "$MALFORMED"
run_step "$base" "$(head_of)"
expect "KNOWN-POSITIVE: an unreleased unparseable trailer fails" 1 "does NOT parse as a trailer" "STREAM TRAILER CHECK"

new_repo; base=$(head_of); commit "$MISSING"
run_step "$base" "$(head_of)"
expect "an unreleased missing trailer fails" 1 "no Claude-Stream trailer"

new_repo; base=$(head_of); commit "$MALFORMED"; git -C "$R" tag v1.0.0
run_step "$base" "$(head_of)"
expect "the same commit under a vX.Y.Z tag is a notice, and passes" 0 \
  "reported as already published: 1" "ALREADY PUBLISHED" "published in v1.0.0" "does NOT parse as a trailer" \
  "::notice title=Stream trailer on an already-published commit::"
refute "...and does not print the failure banner" "STREAM TRAILER CHECK"
refute "...and does not claim every commit is valid" "All 1 checked commit(s) carry a valid"

new_repo; base=$(head_of); commit "$MISSING"; git -C "$R" tag v2.3.4
run_step "$base" "$(head_of)"
expect "a released commit with no trailer at all is a notice too" 0 "published in v2.3.4" "no Claude-Stream trailer"

new_repo; base=$(head_of); commit "$MALFORMED"; git -C "$R" tag v1.0.0-rc.1
run_step "$base" "$(head_of)"
expect "a PRERELEASE tag does not widen the exemption" 1 "STREAM TRAILER CHECK" "reported as already published: 0"

new_repo; base=$(head_of); commit "$MALFORMED"; git -C "$R" tag v1
run_step "$base" "$(head_of)"
expect "a non-semver v* tag does not widen it either" 1 "STREAM TRAILER CHECK"

new_repo; base=$(head_of); commit "$MALFORMED"
run_step "$base" "$(head_of)"
expect "no tags at all: strict, and says so" 1 "no vX.Y.Z tag is present"

new_repo; base=$(head_of); commit "$MALFORMED"; git -C "$R" tag v1.0.0; commit "$MISSING"
run_step "$base" "$(head_of)"
expect "a released finding does not hide an unreleased one" 1 \
  "reported as already published: 1" "STREAM TRAILER CHECK" "no Claude-Stream trailer" "published in v1.0.0"

new_repo; base=$(head_of)
run_step "$base" "$base"
expect "an empty range is UNMEASURED, not a pass" 2 "UNMEASURED: base..head contains no non-merge commits"

echo
if [ "$fail" -ne 0 ]; then
  echo "test-stream-trailer-check: $fail case(s) failed"
  exit 1
fi
echo "test-stream-trailer-check: all cases held"
