#!/usr/bin/env bash
# Two migration files in one dialect must not share a version number.
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

  if [ "$fails" -gt 0 ]; then
    echo "SELF-TEST: $fails case(s) failed" >&2
    exit 1
  fi
  echo "SELF-TEST: 5 cases pass (two known-positive, three known-negative)"
  exit 0
fi

cd "$REPO_ROOT" || exit 1

status=0
for dialect in postgres mysql mssql; do
  dir="migrations/$dialect"
  [ -d "$dir" ] || continue
  dupes="$(git ls-files "$dir/*.sql" | duplicates_of)"
  [ -n "$dupes" ] || continue
  status=1
  while read -r v; do
    [ -n "$v" ] || continue
    echo "ERROR: $dialect has two migrations numbered $v:" >&2
    git ls-files "$dir/*.sql" | sed 's#.*/##' | grep "^${v}_" | sed 's/^/    /' >&2
  done <<<"$dupes"
done

if [ "$status" -ne 0 ]; then
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
  exit 1
fi

echo "OK: no dialect has two migrations sharing a version number."
