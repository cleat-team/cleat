#!/usr/bin/env bash
# Two migration files in one dialect must not share a version number, and a
# migration that states its own number in its header must state the right one.
#
# WHY THIS IS A GUARD AND NOT A CONVENTION. migration/runner.go derives the
# version from the filename prefix -- strconv.Atoi(parts[0]) -- and
# schema_migrations has `version` as its PRIMARY KEY, so two files collapse to
# one row. Runner.Run reads the applied set ONCE, before the loop, so:
#
#   * a fresh database applies both files and records one row, because the
#     insert is ON CONFLICT DO NOTHING / INSERT IGNORE. This is why CI is
#     green: CI builds fresh databases.
#   * a database that already recorded that version skips the other file
#     FOREVER. The run logs "applied <the one it did apply>", prints ok, and
#     exits 0.
#
# Measured on live PostgreSQL for cleat#1071 / #1073, varying only the
# migration directory:
#
#   pre-#1055 db -> develop with both 051 files   reclaim_count: FALSE   50,51
#   pre-#1055 db -> renumbered tree               reclaim_count: true    50,51,52
#
# WHY A TREE CHECK RATHER THAN A DATABASE ONE. The symptom is
# `column "..." does not exist` on a developer's machine, and CLAUDE.md's
# documented response -- recreate your test databases -- WORKS, because a fresh
# database applies both files. The correct routine advice is also what makes
# this invisible, so the check has to fire on the files.
#
# Usage: scripts/check-migration-versions.sh [--self-test]
#
# --self-test builds a throwaway tree containing a known duplicate and asserts
# this script REPORTS it. That is a known-positive, not a negative control, and
# it is the one that matters here: on a clean tree "no duplicates found" and
# "the check does not work" are the same output. See CLAUDE.md, "a check can
# tell you whether it is consistent with itself; it cannot tell you what it is
# not looking at".
#
# Re-derive by hand:
#   for d in postgres mysql mssql; do
#     git ls-files "migrations/$d/*.sql" | sed "s#.*/##;s/_.*//" | sort | uniq -d
#   done
#
# ---------------------------------------------------------------------------
# THE SECOND CHECK: a header that names a migration number names its own.
#
# The number is picked at PUSH time, not when the file is written -- that is
# this repo's rule, because the next free number is a property of
# origin/develop and two open PRs are handed the same one. So a file is
# routinely renamed after its header is written, and the header does not move
# with it.
#
# All three dialects of event_payload_encoding shipped that way: the files are
# 067/061/065 and every one of them opened `-- cleat migration 064`. One
# number, written once, correct in none of the three places it ended up.
#
# WHY IT IS WORTH A GUARD RATHER THAN A CAREFUL AUTHOR. A stale header is not
# inert. Diagnosing a failing local run means asking which migration a database
# recorded, and the answer is a number; a header claiming a different one sends
# the reader to a file that does not exist, or worse, to the wrong file in
# another dialect. A number that appears in two places is a number that will
# come to disagree with itself -- the same reason this file checks filenames
# rather than trusting them.
#
# Only LINE 1 is read, and only the two self-referential forms that occur:
#
#   -- cleat migration 064 (mysql): ...      -> 064     77 of 130 files
#   -- cleat consolidated schema (001)       -> 001
#
# A citation of ANOTHER migration is not a self-reference and must not be
# flagged; line 1 is a title, so restricting to it is what separates the two.
# Files whose line 1 states no number are not checked, and that is deliberate:
# requiring a header would be a different change, made to 53 files that are not
# wrong.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# duplicates_of -- reads FILENAMES on stdin, prints each version appearing twice.
#
# Takes a list rather than a directory on purpose. The real run feeds it
# `git ls-files`, never a filesystem walk: `.claude/worktrees/` and scratch
# checkouts hold whole second copies of migrations/, and a walking guard
# attributes another session's files to this repo -- getting MORE likely to
# misfire as the working tree gets messier, which is backwards for a guard.
# CLAUDE.md records that under the permissive-guard section. Taking a list also
# means --self-test drives this exact function rather than a parallel one.
duplicates_of() {
  sed 's#.*/##' | grep '\.sql$' | sed 's/_.*//' | grep -E '^[0-9]+$' | sort | uniq -d
}

# stated_number -- reads ONE header line on stdin, prints the migration number
# it claims for itself, or nothing.
#
# Pure and line-oriented for the same reason duplicates_of takes a list:
# --self-test drives this exact function on synthetic lines rather than a
# parallel reimplementation of it.
stated_number() {
  # [[:space:]] and not \b: \b is a GNU extension, and BSD sed matches nothing
  # rather than erroring -- so on a Mac the pattern silently reads no header at
  # all and every file passes. Caught by the known-positive below on the first
  # run, which is the whole argument for having one.
  sed -nE \
    -e 's/^-- cleat migration 0*([0-9]+)[[:space:]].*/\1/p' \
    -e 's/^-- cleat migration 0*([0-9]+)[[:space:]]*$/\1/p' \
    -e 's/^-- cleat [^(]*\(0*([0-9]+)\)[[:space:]]*$/\1/p' \
    | head -1
}

if [ "${1:-}" = "--self-test" ]; then
  fails=0
  check() { # <label> <want> <files...>
    local label="$1" want="$2"; shift 2
    local got
    got="$(printf '%s\n' "$@" | duplicates_of | tr '\n' ' ')"
    if [ "$got" != "$want" ]; then
      echo "SELF-TEST FAIL: $label reported '$got', want '$want'" >&2
      fails=$((fails + 1))
    fi
  }

  # The known-positive, and the case that matters: on a clean tree "no
  # duplicates found" and "the check does not work" are the same output, so a
  # negative control alone would pass for a guard that reports nothing ever.
  check "a tree with a duplicate 002" "002 " \
    migrations/postgres/001_first.sql \
    migrations/postgres/002_second.sql \
    migrations/postgres/002_also_second.sql

  # the real collision this was written for, in its real shape
  check "the cleat#1071 collision" "051 " \
    migrations/postgres/050_the_idempotency_write_needs_the_tenant.sql \
    migrations/postgres/051_a_reclaim_is_counted_on_the_row_it_happened_to.sql \
    migrations/postgres/051_an_idempotency_key_is_scoped_to_one_definition.sql

  # known-negative: a clean tree must report nothing, or the guard fails every
  # PR and gets deleted
  check "a clean tree" "" \
    migrations/postgres/001_first.sql \
    migrations/postgres/002_second.sql \
    migrations/postgres/003_third.sql

  # unnumbered files are not migrations and must not both read as version 0
  check "two unnumbered files" "" \
    migrations/postgres/001_first.sql \
    migrations/postgres/README.sql \
    migrations/postgres/notes.sql

  # a path that merely CONTAINS a duplicate-looking name elsewhere must not
  # count -- the guard is per dialect and the real run feeds one dialect at a
  # time, so this pins that the basename is what is read
  check "same number in two dialects" "" \
    migrations/postgres/051_a.sql

  hdr() { # <label> <want> <line>
    local label="$1" want="$2" line="$3" got
    got="$(printf '%s\n' "$line" | stated_number)"
    if [ "$got" != "$want" ]; then
      echo "SELF-TEST FAIL: $label read '$got', want '$want'" >&2
      fails=$((fails + 1))
    fi
  }

  # known-positive: the real header, in the real shape that went wrong
  hdr "a migration header"  "64"  "-- cleat migration 064 (mysql): record how request/response were encoded."
  hdr "a consolidated header" "1" "-- cleat consolidated schema (001)"
  hdr "leading zeros stripped" "5" "-- cleat application role (005)"

  # known-negatives. The first is the one that makes this check usable at all:
  # a header CITING another migration is not claiming to be it, and a guard
  # that cannot tell those apart fires on correct cross-references and is
  # deleted within the week.
  hdr "a citation of another migration" "" "-- See migration 043 for why this is split."
  hdr "a banner line"                   "" "-- ==================================================="
  hdr "a title with no number"          "" "-- cleat: claim terminating workflows"
  hdr "a number that is not parenthesised" "" "-- cleat schema 067 notes"

  if [ "$fails" -gt 0 ]; then
    echo "SELF-TEST: $fails case(s) failed" >&2
    exit 1
  fi
  echo "SELF-TEST: 12 cases pass (five known-positive, seven known-negative)"
  exit 0
fi

cd "$REPO_ROOT" || exit 1

status=0
dupes_found=0
for dialect in postgres mysql mssql; do
  dir="migrations/$dialect"
  [ -d "$dir" ] || continue
  dupes="$(git ls-files "$dir/*.sql" | duplicates_of)"
  [ -n "$dupes" ] || continue
  status=1
  dupes_found=1
  while read -r v; do
    [ -n "$v" ] || continue
    echo "ERROR: $dialect has two migrations numbered $v:" >&2
    git ls-files "$dir/*.sql" | sed 's#.*/##' | grep "^${v}_" | sed 's/^/    /' >&2
  done <<<"$dupes"
done

mismatches=0
for dialect in postgres mysql mssql; do
  dir="migrations/$dialect"
  [ -d "$dir" ] || continue
  while read -r f; do
    [ -n "$f" ] || continue
    base="$(basename "$f")"
    filenum="$(printf '%s' "$base" | sed 's/_.*//' | sed 's/^0*//')"
    [ -n "$filenum" ] || continue
    stated="$(head -1 "$f" | stated_number)"
    [ -n "$stated" ] || continue
    if [ "$stated" != "$filenum" ]; then
      mismatches=$((mismatches + 1))
      status=1
      echo "ERROR: $dialect/$base opens by calling itself migration $stated:" >&2
      head -1 "$f" | sed 's/^/    /' >&2
    fi
  done <<<"$(git ls-files "$dir/*.sql")"
done

if [ "$mismatches" -ne 0 ]; then
  cat >&2 <<'EOF'

A migration's number is picked at push time, so a file gets renamed after its
header is written and the header does not follow. Set the header to the number
in the filename -- the filename is what migration/runner.go reads, so it is the
one that cannot be wrong.

EOF
fi

# Gated on dupes_found, not on status. Written as `status -ne 0` it printed the
# renumbering advice under a HEADER error, telling the reader to go and rename a
# file over a one-line comment -- an over-report of exactly the kind CLAUDE.md
# describes as the cheaper sibling of a false green, and still a wrong answer
# someone acts on. Caught by reading the known-positive's full output rather
# than its exit status.
if [ "$dupes_found" -ne 0 ]; then
  cat >&2 <<'EOF'

Two files with one version number collapse to a single schema_migrations row.
A fresh database applies both, so CI and a recreated test database are green;
a database that already recorded that version never receives the other file,
silently, with the migration reporting ok.

Renumber the file that landed LAST -- `git log --diff-filter=A --format=%cI -1
-- <path>` for each, and move the later one to the next free number in that
dialect. Check the migration is safe to apply twice before moving it: a
database that recorded the shared version will run the renamed file.
EOF
fi

if [ "$status" -ne 0 ]; then
  exit 1
fi

echo "OK: no dialect has two migrations sharing a version number, and every"
echo "    header that names a migration number names its own."
