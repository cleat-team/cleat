#!/usr/bin/env bash
#
# Nothing refuses a large tracked blob on SIZE, and enumerating paths cannot
# fix that -- two `.gitignore` rules have each closed one instance and
# neither would have caught the other.
#
# `.gitignore:195` gained `bin/` on 2026-08-09, after 332MB of tracked build
# products. cleat#1838 then wrote `go build -o .bin/cleat` and committed
# 62,095,458 bytes straight past that rule, because `bin/` does not match
# `.bin/` -- one character off a rule that was itself correct. #1858 added
# `.bin/` too. There are now two names, and the next output directory will
# pick a third: the mechanism that failed is not "the list was incomplete",
# it is that enumerating paths cannot catch a path nobody has thought of
# yet. cleat#1859.
#
# THRESHOLD: 25 MiB (26,214,400 bytes), chosen against a CLEAN tree -- the
# 62MB accident is gone (#1858) before this threshold was picked, which is
# deliberate. A threshold chosen on a tree that already contains the
# violation it exists to catch tends to get picked around the violation.
# 25 MiB admits both of this repo's deliberate oversized fixtures with
# room (19,300,914 bytes each,
# tests/plugin-harness/testdata/pythonworkflow/call_all_plugins.wasm and
# its .component.wasm sibling) and would have refused the 62MB binary by
# more than 2x.
#
# TREE, NOT DIFF. `git ls-tree -r -l HEAD` walks every tracked blob, not
# only what one push changed. That is more expensive in principle and
# costs nothing that matters in practice -- measured at 31ms over 2490
# files on this checkout, 2026-09-18 (`time git ls-tree -r -l HEAD`). The
# diff form is cheaper and is exactly the shape that let the 62MB blob
# sit on develop for over a week: nothing re-examines a commit once it
# has landed, so a diff-scoped check only ever sees the push that
# introduces a violation and is blind to everything after.
#
# ALLOWLIST, WITH A REASON: scripts/allowed-large-blobs.txt, "path<TAB>reason"
# lines. Starts EMPTY -- both of today's oversized files clear the
# threshold on their own, so nothing needs an exemption yet, which is the
# useful state to start from. An entry with no stated reason is how a
# threshold becomes a baseline and a baseline becomes silence, the same
# rule scripts/check-migration-versions.sh and check-test-only-code.sh's
# baseline both carry. A STALE entry -- allowlisted but no longer present,
# or no longer over the threshold -- is reported and fails the run, so the
# allowlist cannot silently cover a blob that moved or shrank.
#
# Usage: scripts/check-tracked-blob-size.sh [--self-test]
#
# --self-test drives the functions below on synthetic ls-tree-shaped input,
# never real git state. A guard that has only ever been observed to pass on
# a clean tree is indistinguishable from one that cannot fail -- this one
# would, by construction, on every run until someone thought to check.
# CLAUDE.md: "a check can tell you whether it is consistent with itself; it
# cannot tell you what it is not looking at."

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
THRESHOLD=26214400 # 25 MiB
ALLOWLIST="$REPO_ROOT/scripts/allowed-large-blobs.txt"

# oversized_of -- reads `git ls-tree -r -l` lines on stdin, prints
# "size<TAB>path" for every entry AT OR OVER THRESHOLD.
#
# Splits on the TAB `git ls-tree -l` puts between the fixed-width metadata
# and the path, so a path containing a space is read whole rather than
# truncated at the first one. The size field is whitespace-padded and
# right-justified within the metadata half, so it is read as the LAST
# whitespace-separated token of that half rather than a fixed column --
# `split(..., /[[:space:]]+/)` rather than a byte offset, because the width
# git uses is not documented and this must not depend on it.
#
# A non-blob entry (a submodule's `commit` type) prints "-" in the size
# column; `size + 0` reads that as 0 in awk, so it can never clear a
# positive threshold and needs no separate exclusion.
#
# A stream rather than a `git` call of its own, so --self-test drives this
# exact function on synthetic lines instead of a parallel reimplementation
# that could disagree with it.
oversized_of() {
  awk -F'\t' -v thresh="$THRESHOLD" '
    NF < 2 { next }
    {
      n = split($1, f, /[[:space:]]+/)
      size = f[n] + 0
      if (size >= thresh) print f[n] "\t" $2
    }
  '
}

# allowlisted_paths -- prints every path named in the allowlist, one per
# line, empty output if the file is absent or empty. Comments (#) and blank
# lines are skipped so the file can carry a header.
allowlisted_paths() {
  local file="$1"
  [ -f "$file" ] || return 0
  awk -F'\t' '/^[[:space:]]*#/ || NF < 2 { next } { print $1 }' "$file"
}

# reason_for -- prints the allowlist's reason for exactly one path, or
# nothing if that path is not listed.
reason_for() {
  local path="$1" file="$2"
  [ -f "$file" ] || return 0
  awk -F'\t' -v p="$path" '/^[[:space:]]*#/ || NF < 2 { next } $1 == p { print $2 }' "$file"
}

if [ "${1:-}" = "--self-test" ]; then
  fails=0

  check_oversized() { # <label> <want (size TAB path per line, or "")> <input lines...>
    local label="$1" want="$2"; shift 2
    local got
    got="$(printf '%s\n' "$@" | oversized_of)"
    if [ "$got" != "$want" ]; then
      echo "SELF-TEST FAIL: $label" >&2
      echo "  want: $(printf '%q' "$want")" >&2
      echo "  got:  $(printf '%q' "$got")" >&2
      fails=$((fails + 1))
    fi
  }

  # The known-positive: this is the case that matters. On a clean tree, "no
  # oversized blobs found" and "the check does not work" print identically,
  # so the negative controls below are not sufficient on their own.
  check_oversized "a blob well over threshold" \
    "$(printf '62095458\t.bin/cleat')" \
    "$(printf '100644 blob abc123      62095458\t.bin/cleat')"

  # known-negative: this repo's real deliberate fixtures, under threshold,
  # must not be reported
  check_oversized "the real 19.3MB fixtures, under threshold" "" \
    "$(printf '100644 blob abc      19300914\ttests/plugin-harness/testdata/pythonworkflow/call_all_plugins.wasm')" \
    "$(printf '100644 blob def      19300914\ttests/plugin-harness/testdata/pythonworkflow/call_all_plugins.wasm.component.wasm')"

  # boundary: AT threshold counts, one byte under does not. ">=" not ">" --
  # get this backwards and the threshold silently admits its own edge.
  check_oversized "exactly at threshold" \
    "$(printf '26214400\tat.bin')" \
    "$(printf '100644 blob a      26214400\tat.bin')"
  check_oversized "one byte under threshold" "" \
    "$(printf '100644 blob a      26214399\tunder.bin')"

  # a small tree entirely under threshold must report nothing
  check_oversized "a clean, small tree" "" \
    "$(printf '100644 blob a           66\t.claude/settings.local.json')" \
    "$(printf '100644 blob b         1347\t.devcontainer/devcontainer.json')"

  # a path containing a space must be read whole, not truncated at the
  # first whitespace run after the TAB
  check_oversized "a path containing a space" \
    "$(printf '30000000\tbin/a big file.bin')" \
    "$(printf '100644 blob a      30000000\tbin/a big file.bin')"

  # a submodule ("commit") entry prints "-" for size and must not crash the
  # numeric comparison or be reported
  check_oversized "a submodule entry" "" \
    "$(printf '160000 commit a                 -\tvendor/some-submodule')"

  # multiple oversized entries in one run, to prove the function does not
  # stop at the first match
  check_oversized "two oversized entries in one tree" \
    "$(printf '30000000\ta.bin\n40000000\tb.bin')" \
    "$(printf '100644 blob a      30000000\ta.bin')" \
    "$(printf '100644 blob b      40000000\tb.bin')"

  # --- allowlist parsing ---

  ALIST="$(mktemp)"
  trap 'rm -f "$ALIST"' EXIT
  printf '# comment line, skipped\n' > "$ALIST"
  printf 'tests/fixtures/big.bin\ta deliberate fixture, see cleat#0000\n' >> "$ALIST"
  printf '\n' >> "$ALIST" # blank line, skipped

  check_list() { # <label> <want> <got>
    local label="$1" want="$2" got="$3"
    if [ "$got" != "$want" ]; then
      echo "SELF-TEST FAIL: $label: want '$want', got '$got'" >&2
      fails=$((fails + 1))
    fi
  }

  check_list "allowlisted_paths skips comments and blanks" \
    "tests/fixtures/big.bin" "$(allowlisted_paths "$ALIST")"
  check_list "reason_for a listed path" \
    "a deliberate fixture, see cleat#0000" "$(reason_for "tests/fixtures/big.bin" "$ALIST")"
  check_list "reason_for an unlisted path" \
    "" "$(reason_for "not/listed.bin" "$ALIST")"
  check_list "allowlisted_paths on a missing file" \
    "" "$(allowlisted_paths "$REPO_ROOT/scripts/no-such-allowlist-file.txt")"

  if [ "$fails" -gt 0 ]; then
    echo "SELF-TEST: $fails case(s) failed" >&2
    exit 1
  fi
  echo "SELF-TEST: 13 cases pass"
  exit 0
fi

cd "$REPO_ROOT" || exit 1

oversized="$(git ls-tree -r -l HEAD | oversized_of)"

status=0
new_count=0
allowed_count=0

if [ -n "$oversized" ]; then
  while IFS="$(printf '\t')" read -r size path; do
    [ -n "$path" ] || continue
    reason="$(reason_for "$path" "$ALLOWLIST")"
    if [ -n "$reason" ]; then
      allowed_count=$((allowed_count + 1))
      echo "ALLOWED: $path is $size bytes -- $reason"
    else
      new_count=$((new_count + 1))
      status=1
      echo "ERROR: $path is $size bytes, over the $THRESHOLD byte (25 MiB) threshold" >&2
      echo "  and not in $ALLOWLIST." >&2
    fi
  done <<<"$oversized"
fi

# A STALE exemption is not a pass. An allowlist entry naming a path that is
# no longer oversized -- deleted, moved, or shrunk -- is a grant covering
# nothing, exactly the ceiling-only-ratchet shape CLAUDE.md names under
# cleat#1746: it cannot be seen to be wrong because nothing it excuses is
# there to fail. Fail the run and name the stale entry rather than let it
# sit as silent cover for whatever lands at that path next.
stale_count=0
current_oversized_paths="$(printf '%s\n' "$oversized" | awk -F'\t' 'NF>=2{print $2}')"
while IFS= read -r listed; do
  [ -n "$listed" ] || continue
  if ! grep -qxF "$listed" <<<"$current_oversized_paths"; then
    stale_count=$((stale_count + 1))
    status=1
    echo "ERROR: $ALLOWLIST lists $listed, which is not over the threshold (or no longer" >&2
    echo "  exists). Delete the entry -- a stale exemption covers whatever lands at that" >&2
    echo "  path next, not the thing it was written for." >&2
  fi
done <<<"$(allowlisted_paths "$ALLOWLIST")"

if [ "$new_count" -gt 0 ]; then
  cat >&2 <<EOF

A blob this large should not be committed. If it is test fixture data that
must ship in the repo, add it to $ALLOWLIST as "path<TAB>reason with an
issue number" -- an entry with no reason is how a threshold becomes a
baseline and a baseline becomes silence. Otherwise: use a build output
directory that is already gitignored (bin/, .bin/), or add the actual
directory this file landed in to .gitignore, and untrack it:

  git rm --cached <path>
EOF
fi

if [ "$status" -ne 0 ]; then
  exit 1
fi

echo "OK: no tracked blob is over $THRESHOLD bytes (25 MiB) without a reason in"
echo "    $ALLOWLIST ($allowed_count allowed, $new_count new, $stale_count stale)."
