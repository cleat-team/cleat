#!/usr/bin/env bash
#
# go.work's `go` directive must be >= the `go` directive of every module it
# uses. The go command enforces this, but only by refusing to build, and the
# refusal surfaces as a failure of whatever job happened to run `go build`
# first -- with seven more jobs failing downstream from it for reasons that
# each look like something else.
#
# WHY THIS EXISTS. Dependabot raises the `go` directive in member go.mod files
# and has no updater for go.work, so every Go bump that moves the floor lands
# with the workspace one version behind. cleat#1708: PR #1607 bumped five
# modules to 1.26.0, go.work stayed at 1.25.11, and `Build` failed before
# compiling a line:
#
#   go: module . listed in go.work file requires go >= 1.26.0,
#       but go.work lists go 1.25.11
#
# This check turns that into one sentence naming the two files, so the next
# occurrence is legible from the job name rather than from reading a build log.
#
# WHY IT FAILS WHEN IT FINDS NOTHING. A guard that scans for members and finds
# zero reports "clean" on exactly the broken tree it exists to catch -- a
# renamed go.work, a moved use block, a parse this script does not handle. So
# the member count is itself an assertion: fewer than MIN_MODULES is a failure
# of the check, reported as such and distinguished from a failure of the tree.
# scripts/check-required-contexts.py --self-test is the precedent.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORKFILE="${1:-$ROOT/go.work}"

# The tree has five members today. A floor rather than an equality so that
# adding a module does not fail the guard, while losing all of them does.
MIN_MODULES=3

# go_directive FILE -- print the `go` directive, or nothing if there is none.
# Anchored to the start of the line: `toolchain go1.26.0` and a `go` inside a
# require block must not match.
go_directive() {
  sed -n 's/^go[[:space:]]\{1,\}\([0-9][0-9.]*\)[[:space:]]*$/\1/p' "$1" | head -1
}

# ver_ge A B -- true when version A >= version B. Compares numerically
# field by field, so 1.25.11 > 1.25.2 (which a string comparison gets wrong,
# and 1.25.11 is the exact version this was written against).
ver_ge() {
  local a b i
  IFS=. read -r -a a <<< "$1"
  IFS=. read -r -a b <<< "$2"
  for i in 0 1 2; do
    local av="${a[i]:-0}" bv="${b[i]:-0}"
    if [ "$av" -gt "$bv" ]; then return 0; fi
    if [ "$av" -lt "$bv" ]; then return 1; fi
  done
  return 0
}

check_workspace() {
  local workfile="$1" workdir work_go members=() behind=() m mod mod_go failed=0

  if [ ! -f "$workfile" ]; then
    echo "CHECK FAILED: no go.work at $workfile -- this guard could not run." >&2
    return 2
  fi
  workdir="$(cd "$(dirname "$workfile")" && pwd)"

  work_go="$(go_directive "$workfile")"
  if [ -z "$work_go" ]; then
    echo "CHECK FAILED: no 'go' directive in $workfile -- this guard could not run." >&2
    return 2
  fi

  # The use block: both `use ( ... )` and single-line `use ./dir` forms.
  while IFS= read -r m; do
    [ -n "$m" ] && members+=("$m")
  done < <(sed -n -e 's|^[[:space:]]*use[[:space:]]\{1,\}\([^([:space:]][^[:space:]]*\).*|\1|p' \
                  -e '/^[[:space:]]*use[[:space:]]*(/,/^[[:space:]]*)/{
                        s|^[[:space:]]*\([^)([:space:]][^[:space:]]*\)[[:space:]]*$|\1|p
                      }' "$workfile")

  if [ "${#members[@]}" -lt "$MIN_MODULES" ]; then
    echo "CHECK FAILED: found ${#members[@]} module(s) in $workfile, expected at least $MIN_MODULES." >&2
    echo "  The use block did not parse. This is a failure of this guard, not of the tree," >&2
    echo "  and it is reported rather than passed because a zero-member scan agrees with" >&2
    echo "  every workspace, broken or not." >&2
    return 2
  fi

  for m in "${members[@]}"; do
    mod="$workdir/${m#./}/go.mod"
    if [ ! -f "$mod" ]; then
      echo "CHECK FAILED: $workfile uses $m, which has no go.mod at $mod." >&2
      failed=1
      continue
    fi
    mod_go="$(go_directive "$mod")"
    if [ -z "$mod_go" ]; then
      echo "CHECK FAILED: no 'go' directive in $mod." >&2
      failed=1
      continue
    fi
    if ! ver_ge "$work_go" "$mod_go"; then
      behind+=("  ${m#./}/go.mod requires go $mod_go")
      failed=1
    fi
  done

  if [ "${#behind[@]}" -gt 0 ]; then
    echo "go.work is behind ${#behind[@]} of the ${#members[@]} modules it uses:" >&2
    printf '%s\n' "${behind[@]}" >&2
    echo "  $(basename "$workfile") declares go $work_go" >&2
    echo "" >&2
    echo "  Raise the 'go' directive in $workfile to at least the highest of those." >&2
    echo "  Nothing in this workspace builds until you do: 'go build' refuses before" >&2
    echo "  compiling, so the first symptom is every Go job failing at once rather" >&2
    echo "  than one job naming this file." >&2
  fi

  [ "$failed" -eq 0 ] || return 1
  echo "go.work declares go $work_go, >= all ${#members[@]} member modules. OK"
  return 0
}

# --self-test builds a workspace this check MUST reject, and one it MUST
# accept, and fails if either verdict is wrong. Without it, "OK" on the real
# tree is equally consistent with a check that cannot fail at all -- which is
# the failure mode this whole file is written against.
self_test() {
  local tmp rc status=0
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  mkdir -p "$tmp/a" "$tmp/b" "$tmp/c"
  printf 'module a\n\ngo 1.26.0\n' > "$tmp/a/go.mod"
  printf 'module b\n\ngo 1.25.11\n' > "$tmp/b/go.mod"
  printf 'module c\n\ngo 1.25.11\n' > "$tmp/c/go.mod"

  # KNOWN-POSITIVE: workspace below module a. Must be rejected with rc=1.
  printf 'go 1.25.11\n\nuse (\n\t./a\n\t./b\n\t./c\n)\n' > "$tmp/go.work"
  rc=0; check_workspace "$tmp/go.work" >/dev/null 2>&1 || rc=$?
  if [ "$rc" -ne 1 ]; then
    echo "SELF-TEST FAILED: a workspace behind a member returned rc=$rc, expected 1." >&2
    status=1
  fi

  # NEGATIVE CONTROL: workspace at the floor. Must be accepted with rc=0.
  printf 'go 1.26.0\n\nuse (\n\t./a\n\t./b\n\t./c\n)\n' > "$tmp/go.work"
  rc=0; check_workspace "$tmp/go.work" >/dev/null 2>&1 || rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "SELF-TEST FAILED: a workspace at the floor returned rc=$rc, expected 0." >&2
    status=1
  fi

  # The version comparison is numeric, not lexical: 1.25.11 vs 1.25.2 is the
  # pair that distinguishes them, and 1.25.11 is what this tree actually ran.
  printf 'module b\n\ngo 1.25.2\n' > "$tmp/b/go.mod"
  printf 'go 1.25.11\n\nuse (\n\t./a\n\t./b\n\t./c\n)\n' > "$tmp/go.work"
  rc=0; check_workspace "$tmp/go.work" >/dev/null 2>&1 || rc=$?
  if [ "$rc" -ne 1 ]; then
    echo "SELF-TEST FAILED: expected rejection on module a (1.26.0), got rc=$rc." >&2
    status=1
  fi
  printf 'module a\n\ngo 1.25.2\n' > "$tmp/a/go.mod"
  rc=0; check_workspace "$tmp/go.work" >/dev/null 2>&1 || rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "SELF-TEST FAILED: 1.25.11 >= 1.25.2 compared lexically, rc=$rc." >&2
    status=1
  fi

  # UNPARSEABLE USE BLOCK: must be rc=2, a failure of the check, NOT a pass.
  printf 'go 1.26.0\n\nuse ./a\n' > "$tmp/go.work"
  rc=0; check_workspace "$tmp/go.work" >/dev/null 2>&1 || rc=$?
  if [ "$rc" -ne 2 ]; then
    echo "SELF-TEST FAILED: a one-member workspace returned rc=$rc, expected 2." >&2
    status=1
  fi

  if [ "$status" -eq 0 ]; then
    echo "self-test: 5 cases, all verdicts as expected. OK"
  fi
  return "$status"
}

if [ "${1:-}" = "--self-test" ]; then
  self_test
  exit $?
fi

check_workspace "$WORKFILE"
