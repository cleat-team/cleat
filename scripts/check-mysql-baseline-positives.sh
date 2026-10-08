#!/usr/bin/env bash
#
# Known-positives for the MySQL baseline's supplementary checks.
#
# WHAT THIS IS FOR. migration/the_mysql_baseline_keeps_what_catalogdiff_cannot_see_test.go
# asserts four properties that migration/catalogdiff is STRUCTURALLY BLIND to --
# it reads no MySQL triggers, no rows, and no collation on any dialect. A check
# in that position has no natural way to be observed failing, which is the state
# this repo calls "a condition that never decides anything": it returns the
# reassuring answer forever and nothing distinguishes that from correctness.
#
# So each check has a mutation here that is KNOWN to be broken, and the script
# requires the RIGHT SUBTEST to fail -- not merely the test to fail. That
# distinction is not pedantic and this script's own history proves it: the first
# collation case changed a COLLATE on a column that carries a foreign key, the
# BUILD failed, the test failed, and the case looked green while the collation
# check had never run. A mutation that fails for a different reason than the
# check it targets demonstrates nothing about that check.
#
# It therefore greps for the subtest name, and a run that fails at the TOP level
# with no subtest is reported as a failure of the case, not a pass.
#
# Usage:  CLEAT_TEST_MYSQL=... scripts/check-mysql-baseline-positives.sh
# Exits 0 when every case fires the subtest it names, non-zero otherwise.
# The tree is restored BY CONTENT after every case, and verified.

set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MIG="$REPO/migrations/mysql"
BAK="$(mktemp -d)"
TEST='TestTheMySQLBaselineKeepsWhatCatalogdiffCannotSee'

# Invoked indirectly, through the `trap cleanup EXIT` below -- which the linter
# cannot see, so it reports the definition as dead. Both codes are listed because
# they are one check under two names: 0.11 reports SC2329 (function never
# invoked), older builds report SC2317 (command appears unreachable), and an
# SC2329-only suppression passed locally while failing on the older one.
# scripts/benchcmp.sh#46 carries the same note for the same reason.
# (No line above may begin with the linter's own name -- it would be parsed as a
# directive rather than read as prose. SC1073.)
# shellcheck disable=SC2329,SC2317
cleanup() {
  cp "$BAK"/001_schema.sql "$BAK"/002_defaults.sql "$BAK"/003_procedures.sql "$MIG"/ 2>/dev/null
  rm -rf "$BAK"
}
trap cleanup EXIT

cp "$MIG"/001_schema.sql "$MIG"/002_defaults.sql "$MIG"/003_procedures.sql "$BAK"/

fail=0
cases=0
pass=0

# run_case NAME SUBTEST -- mutate, run, require that subtest to fail, restore.
# The mutation is applied by the caller before this is invoked; this only
# judges and restores.
judge() {
  local name="$1" want="$2"
  cases=$((cases + 1))
  local out
  out="$(cd "$REPO" && go test ./migration/ -count=1 -run "$TEST" 2>&1)"

  if ! grep -q -- '--- FAIL' <<<"$out"; then
    echo "  $name: DID NOT FAIL. A case that cannot fire is not a known-positive."
    fail=1
  elif ! grep -q -- "--- FAIL: $TEST/$want" <<<"$out"; then
    # The test failed, but not the subtest this case targets. Usually the build
    # broke instead -- which proves something about the schema and nothing about
    # the check.
    echo "  $name: failed, but NOT in the subtest it targets ($want)."
    echo "       A top-level failure is a broken BUILD, not a fired check:"
    grep -E -- '^\s+--- FAIL' <<<"$out" | head -4 | sed 's/^/       /'
    fail=1
  else
    echo "  $name: OK -- $want fired"
    pass=$((pass + 1))
  fi

  cp "$BAK"/001_schema.sql "$BAK"/002_defaults.sql "$BAK"/003_procedures.sql "$MIG"/
  if ! diff -q "$BAK/001_schema.sql" "$MIG/001_schema.sql" >/dev/null ||
     ! diff -q "$BAK/002_defaults.sql" "$MIG/002_defaults.sql" >/dev/null ||
     ! diff -q "$BAK/003_procedures.sql" "$MIG/003_procedures.sql" >/dev/null; then
    echo "  $name: ** RESTORE FAILED -- the tree is dirty, stopping **"
    exit 2
  fi
}

echo "MySQL baseline supplementary known-positives ($TEST)"
echo

# 1. The trigger. catalogdiff reads MySQL triggers too as of cleat#2882, but
#    this script is independent of whether that differential is ever re-run --
#    the generator's first version dropped the trigger silently, and this is
#    a check on the generator itself, not on catalogdiff's output.
python3 - "$MIG/003_procedures.sql" <<'PY'
import re
import sys
p = sys.argv[1]
s = open(p).read()
parts = s.split('DELIMITER ;;')
kept, removed = [parts[0]], 0
for c in parts[1:]:
    # `CREATE TRIGGER`, not the substring TRIGGER. The DROP that precedes the
    # NEXT object sits inside the PREVIOUS part, so a part can contain the word
    # "TRIGGER" while defining the procedure -- which made the first version of
    # this mutation find two blocks, assert, and never mutate anything.
    if re.search(r'\bCREATE\s+TRIGGER\b', c):
        removed += 1
        _, _, after = c.partition('DELIMITER ;')
        kept.append(after)
    else:
        kept.append('DELIMITER ;;' + c)
assert removed == 1, "expected exactly one CREATE TRIGGER block, found %d" % removed
out = ''.join(kept)
assert not re.search(r'\bCREATE\s+TRIGGER\b', out), "the trigger survived the mutation"
open(p, 'w').write(out)
PY
judge "trigger dropped from 003" "the_trigger_survives"

# 2. The seed rows. A structural diff cannot see a missing ROW at all.
printf -- '-- emptied for the known-positive\n' > "$MIG/002_defaults.sql"
judge "002 emptied" "both_seed_rows_are_present"

# 3. The ORDER the seeds depend on. Reversed, MySQL raises 1452 and INSERT
#    IGNORE downgrades it to a warning, so the statement reports success.
python3 - "$MIG/002_defaults.sql" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read()
o, t = s.index('INSERT INTO orgs'), s.index('INSERT IGNORE INTO tenants')
assert o < t, "the shipped 002 already has the tenant first; case is meaningless"
open(p, 'w').write(s[:o] + s[t:].strip() + '\n\n' + s[o:t].strip() + '\n')
PY
judge "seed order reversed" "the_tenant's_org_resolves"

# 4. Collation -- the class catalogdiff compares on NO dialect. The column is
#    deliberately UNCONSTRAINED: the first version of this case used one under a
#    foreign key, the build failed, and the case passed while the check had not
#    run.
python3 - "$MIG/001_schema.sql" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read()
old = '  key_text text NOT NULL,'
assert old in s, "declaration not found -- refusing to mutate blind"
open(p, 'w').write(s.replace(old, '  key_text text COLLATE utf8mb4_bin NOT NULL,', 1))
PY
judge "one column's collation changed" "every_column's_collation_is_the_default_or_a_declared_exception"

# 5. The descending index. cleat#2446's class: a DESC key silently becoming ASC
#    inserts the same rows, so no behavioural test distinguishes them.
python3 - "$MIG/001_schema.sql" <<'PY'
import re
import sys
p = sys.argv[1]
s = open(p).read()
# The declaration is inside the KEY, as `recorded_at DESC`.
# re.subn returns (new_string, count) -- in that order. The first version of
# this indexed it the other way round and compared a STRING to 1, so the case
# never mutated anything and reported DID NOT FAIL. Which is the battery
# working: a case that cannot fire is not a known-positive.
out, count = re.subn(r'(recorded_at)\s+DESC', r'\1', s)
assert count == 1, "expected exactly one 'recorded_at DESC', found %d" % count
open(p, 'w').write(out)
PY
judge "descending key dropped" "the_descending_index_is_still_descending"

# 6. An auto_increment column. `auto_increment` exists nowhere else the schema
#    can be read from: COLUMN_TYPE says `bigint` either way.
python3 - "$MIG/001_schema.sql" <<'PY'
import re
import sys
p = sys.argv[1]
s = open(p).read()
out, count = re.subn(r'\s+AUTO_INCREMENT\b', '', s, count=1, flags=re.I)
assert count == 1, "no AUTO_INCREMENT to remove"
open(p, 'w').write(out)
PY
judge "auto_increment dropped" "the_auto_increment_columns_are_still_auto_increment"

echo
if [ "$fail" -eq 0 ]; then
  echo "OK: $pass of $cases cases fired the subtest each names."
else
  echo "FAIL: some cases did not fire the check they target."
fi
exit "$fail"
