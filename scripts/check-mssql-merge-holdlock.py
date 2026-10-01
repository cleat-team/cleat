#!/usr/bin/env python3
"""check-mssql-merge-holdlock.py -- cleat#2904.

SQL Server's `MERGE` under READ COMMITTED can evaluate `WHEN NOT MATCHED`
true on two concurrent statements against the same not-yet-existing key --
neither sees the other's uncommitted insert, so both attempt the INSERT
branch and one raises a duplicate-key error. `WITH (HOLDLOCK)` (optionally
paired with `UPDLOCK`) takes a serializable range lock on the target that
closes the window. This is a documented SQL Server hazard, not a
cleat-specific one; see engine/mssql_events.go's doc comment above its own
`MERGE ... WITH (HOLDLOCK)` for the first place this was fixed (cleat#986-
adjacent) and plugins/eventstore/queries.go for the second (cleat#2268).

cleat#2903/#2904: cleat-review's own enumeration while reviewing the
kvstore fix found seven more MSSQL `MERGE` statements without `HOLDLOCK`,
across engine/ and two other plugins -- and it was itself incomplete
(re-derived here: it missed a fifth blobstore statement, both
eventtriggers statements, and ratelimiter's, 13 real candidates against
the 9 it named). #2903's own review argued for a GUARD rather than five
individual fixes, because per-statement reachability -- whether a given
statement is ever raced by two writers on a brand-new key -- is a separate
question from "does this statement carry HOLDLOCK", and a probabilistic
concurrency test reads as a pass on hardware where the race window happens
to be narrow (0/800 on one reviewer's SQL Server, 28/800 on another's, same
commit). The deterministic, machine-independent check is structural: does
the statement text carry HOLDLOCK. This guard is that check.

RULE. Every MSSQL `MERGE` statement in tracked source (a backtick-quoted Go
string literal reaching a `*MSSQLStore` method or a `plugin.Query{}.MSSQL`
field, or a `MERGE` in a `.sql` migration) must contain `HOLDLOCK` --
textually, inside the same statement -- unless it is recorded below with a
non-empty reason and a valid status.

Deliberately NOT fixing any of the 13 candidates here: reachability is
unassessed for all of them (per cleat#2904's filing), and asserting EXEMPT
without having actually checked would be worse than the gap this guard
closes -- an EXEMPT entry is read as "checked, and safe", not as "assumed
safe to ship the guard". Every current candidate is DEFERRED, not EXEMPT.

Statuses, same distinction and the same reason check-mssql-tenant-scoped-
reads.py makes it: an EXEMPT and a DEFERRED entry look identical as "this
guard won't flag it", which is indistinguishable from "complete" at a
glance unless each says which it is.

  - EXEMPT: this statement genuinely cannot be raced by two concurrent
    writers on a brand-new key (e.g. it only ever runs inside a wider lock,
    or the key can never be fresh), established by someone having actually
    checked the call path.
  - DEFERRED: this statement IS a real candidate and HOLDLOCK has not been
    added yet -- tracked debt, not a decision.

STALENESS. An allowlist/deferred entry naming a declaration that no longer
exists, or that already carries HOLDLOCK, is a FAILURE, not a silent pass
-- the same rule check-mssql-tenant-scoped-reads.py and scripts/check-
skips.sh already apply to their own ledgers. An allowlist that can only
grow is a ratchet in the wrong direction.

KEYING. A MERGE statement is keyed by (file, enclosing top-level
declaration name) -- the `var NAME = plugin.Query{...}` or
`func (...) NAME(...)` it lives inside -- not by line number, because an
unrelated edit elsewhere in the same file would otherwise silently shift
the line and make a correct allowlist entry go stale for the wrong reason.
A `.sql` migration file has no such declaration, so it is keyed by (file,
target table name) instead; migrations are append-only by this project's
convention (see CLAUDE.md on `CREATE OR REPLACE` routines), so a line
number there would be stable too, but the table name needs no caveat.

Exit codes: 0 clean, 1 a violation (unallowed candidate or stale entry)
was found, 2 the check could not run (UNMEASURED).
"""
import os
import re
import subprocess
import sys

# ---------------------------------------------------------------------------
# ALLOWLIST (EXEMPT only -- see module docstring for why DEFERRED lives in
# the companion TSV instead of here)
# ---------------------------------------------------------------------------
EXEMPT = "exempt"
DEFERRED = "deferred"

ALLOWLIST = {
    # Deliberately empty: reachability has not been assessed for any
    # current candidate (cleat#2904's own filing), so nothing has been
    # checked carefully enough to call EXEMPT. Every candidate is in
    # mssql-merge-holdlock-deferred.tsv instead. An EXEMPT entry belongs
    # here only once someone has actually traced a statement's call path
    # and confirmed it cannot be raced by two writers on a fresh key.
}


def _load_deferred_from_file():
    """Deferred entries live in a companion TSV so the count of what's left
    is a `wc -l`, not a scroll through this file -- see
    mssql-merge-holdlock-deferred.tsv alongside this script."""
    path = os.path.join(os.path.dirname(__file__), "mssql-merge-holdlock-deferred.tsv")
    entries = {}
    try:
        with open(path, encoding="utf-8") as f:
            for lineno, line in enumerate(f, 1):
                line = line.rstrip("\n")
                if not line or line.startswith("#"):
                    continue
                parts = line.split("\t")
                if len(parts) != 3:
                    print(f"UNMEASURED: {path}:{lineno}: expected 3 tab-separated "
                          f"fields (file, key, reason), got {len(parts)}")
                    sys.exit(2)
                file_, key, reason = parts
                entries[(file_, key)] = (DEFERRED, reason)
    except FileNotFoundError:
        print(f"UNMEASURED: {path} not found")
        sys.exit(2)
    return entries


# ---------------------------------------------------------------------------
# Extraction
# ---------------------------------------------------------------------------
# Comment/string-tracking strip, shared shape with check-mssql-tenant-
# scoped-reads.py and check-no-raw-rebind.py: prose can quote "MERGE" or
# "HOLDLOCK" without either being a real statement (cleat#1.1's "a text
# search cannot tell a thing from a sentence about the thing").

def strip_go_comments(src):
    out = []
    i, n = 0, len(src)
    in_string = None
    while i < n:
        c = src[i]
        if in_string:
            out.append(c)
            if in_string == '"' and c == "\\" and i + 1 < n:
                out.append(src[i + 1])
                i += 2
                continue
            if c == in_string:
                in_string = None
            i += 1
            continue
        if c in ('"', "`"):
            in_string = c
            out.append(c)
            i += 1
            continue
        if c == "/" and i + 1 < n and src[i + 1] == "/":
            j = src.find("\n", i)
            if j == -1:
                i = n
            else:
                out.append("\n")
                i = j + 1
            continue
        if c == "/" and i + 1 < n and src[i + 1] == "*":
            j = src.find("*/", i + 2)
            if j == -1:
                i = n
            else:
                out.append("\n" * src[i:j + 2].count("\n"))
                i = j + 2
            continue
        out.append(c)
        i += 1
    return "".join(out)


def strip_sql_comments(src):
    out = []
    i, n = 0, len(src)
    in_string = False
    while i < n:
        c = src[i]
        if in_string:
            out.append(c)
            if c == "'":
                in_string = False
            i += 1
            continue
        if c == "'":
            in_string = True
            out.append(c)
            i += 1
            continue
        if c == "-" and i + 1 < n and src[i + 1] == "-":
            j = src.find("\n", i)
            if j == -1:
                i = n
            else:
                out.append("\n")
                i = j + 1
            continue
        if c == "/" and i + 1 < n and src[i + 1] == "*":
            j = src.find("*/", i + 2)
            if j == -1:
                i = n
            else:
                out.append("\n" * src[i:j + 2].count("\n"))
                i = j + 2
            continue
        out.append(c)
        i += 1
    return "".join(out)


# Pair first, filter after (CLAUDE.md: a length-bounded regex applied
# DURING pairing can resynchronise on the wrong backtick and fabricate a
# literal out of the Go source between two unrelated strings). No bound
# inside the pairing regex itself.
BACKTICK_RE = re.compile(r"`([^`]*)`", re.S)

# A MERGE statement in this codebase always reaches a WHEN (NOT) MATCHED
# clause -- that confirms the keyword is T-SQL MERGE syntax rather than a
# stray English word ("This was a MERGE, for upsert semantics" in a
# comment -- already excluded by strip_go_comments, but belt and braces,
# since a bare `MERGE` with no WHEN clause is not a statement this guard
# can evaluate anyway).
MERGE_START_RE = re.compile(r"(?i)(?<![A-Za-z0-9_])MERGE(?![A-Za-z0-9_])")
WHEN_MATCHED_RE = re.compile(r"(?i)WHEN\s+(NOT\s+)?MATCHED")
HOLDLOCK_RE = re.compile(r"(?i)\bHOLDLOCK\b")

# Top-level Go declarations: a `func` (any or no receiver) or a
# `var NAME = ...` statement, up to (but not including) the next top-level
# `func`/`var`, or end of file. This is the same shape as check-mssql-
# tenant-scoped-reads.py's METHOD_RE, generalised to also match the
# `var upsertX = plugin.Query{...}` shape blobstore/kvstore/ratelimiter/
# eventtriggers use (none of them have a *MSSQLStore receiver).
DECL_RE = re.compile(r"^(func\b.*?|var\s+\w+\s*=.*?)(?=\n(?:func|var)\b|\Z)", re.M | re.S)
FUNC_NAME_RE = re.compile(r"^func\s*(?:\([^)]*\)\s*)?(\w+)\s*\(")
VAR_NAME_RE = re.compile(r"^var\s+(\w+)\s*=")


def _merge_statements_in_text(text):
    """Yields (holdlock: bool, statement_first_line: str) for every T-SQL
    MERGE statement found directly in `text` (not inside backticks -- the
    caller decides whether `text` is already a Go string literal's
    contents or a whole .sql file)."""
    for m in MERGE_START_RE.finditer(text):
        tail = text[m.start():]
        if not WHEN_MATCHED_RE.search(tail):
            continue
        stmt_end = tail.find(";")
        stmt = tail if stmt_end == -1 else tail[:stmt_end + 1]
        yield bool(HOLDLOCK_RE.search(stmt)), stmt.strip().splitlines()[0][:80]


def find_go_declarations(text):
    """Yields (decl_name, [(holdlock, stmt_head), ...]) for every top-level
    declaration in this (already comment-stripped) Go source that contains
    at least one MERGE statement inside a backtick literal."""
    for dm in DECL_RE.finditer(text):
        body = dm.group(1)
        fm = FUNC_NAME_RE.match(body)
        vm = VAR_NAME_RE.match(body)
        name = fm.group(1) if fm else (vm.group(1) if vm else None)
        if name is None:
            continue
        merges = []
        for bm in BACKTICK_RE.finditer(body):
            merges.extend(_merge_statements_in_text(bm.group(1)))
        if merges:
            yield name, merges


TABLE_NAME_RE = re.compile(r"(?i)^MERGE\s+(?:INTO\s+)?([A-Za-z0-9_.\[\]]+)")


def find_sql_declarations(text):
    """Yields (table_name, [(holdlock, stmt_head)]) for every MERGE
    statement found directly in a (already comment-stripped) .sql file."""
    for holdlock, stmt_head in _merge_statements_in_text(text):
        tm = TABLE_NAME_RE.match(stmt_head)
        table = tm.group(1) if tm else "<unparsed-target>"
        yield table, [(holdlock, stmt_head)]


# ---------------------------------------------------------------------------
# Self-test
# ---------------------------------------------------------------------------

def run_self_test():
    ok = True

    # 1. Known positive: a plugin.Query var with an MSSQL MERGE and no
    # HOLDLOCK.
    positive_src = (
        "package widget\n\n"
        "var upsertWidget = plugin.Query{\n"
        "\tMSSQL: `MERGE widgets AS target\n"
        "USING (VALUES ($1)) AS source (id)\n"
        "ON target.id = source.id\n"
        "WHEN NOT MATCHED THEN INSERT (id) VALUES (source.id);`,\n"
        "}\n"
    )
    found = dict(find_go_declarations(strip_go_comments(positive_src)))
    if not found.get("upsertWidget") or found["upsertWidget"][0][0] is not False:
        print("SELF-TEST FAILED: a var-shaped MERGE with no HOLDLOCK was not flagged.")
        ok = False

    # 2. Known negative: same shape, WITH (HOLDLOCK).
    negative_src = (
        "package widget\n\n"
        "var upsertWidgetSafe = plugin.Query{\n"
        "\tMSSQL: `MERGE widgets WITH (HOLDLOCK) AS target\n"
        "USING (VALUES ($1)) AS source (id)\n"
        "ON target.id = source.id\n"
        "WHEN NOT MATCHED THEN INSERT (id) VALUES (source.id);`,\n"
        "}\n"
    )
    found2 = dict(find_go_declarations(strip_go_comments(negative_src)))
    if found2.get("upsertWidgetSafe") and found2["upsertWidgetSafe"][0][0] is not True:
        print("SELF-TEST FAILED: a HOLDLOCK MERGE was reported as missing it.")
        ok = False

    # 3. Known positive: a func-shaped (ExecContext) MERGE, the
    # *MSSQLStore engine/ shape.
    func_src = (
        "package engine\n\n"
        "func (s *MSSQLStore) DoThing(ctx context.Context) error {\n"
        "\t_, err := s.db.ExecContext(ctx, `MERGE things AS target\n"
        "USING (VALUES ($1)) AS source (id)\n"
        "ON target.id = source.id\n"
        "WHEN NOT MATCHED THEN INSERT (id) VALUES (source.id);`)\n"
        "\treturn err\n"
        "}\n"
    )
    found3 = dict(find_go_declarations(strip_go_comments(func_src)))
    if not found3.get("DoThing") or found3["DoThing"][0][0] is not False:
        print("SELF-TEST FAILED: a func-shaped MERGE with no HOLDLOCK was not flagged.")
        ok = False

    # 4. Comments/prose must not trigger a false positive -- the same trap
    # check-mssql-tenant-scoped-reads.py guards against, applied to this
    # guard's own signal words. mssql_signals_promises.go's real comment:
    # "This was a MERGE, for upsert semantics" -- past tense, no WHEN
    # clause, and (separately) inside a comment.
    comment_src = (
        "package engine\n\n"
        "// This was a MERGE, for upsert semantics, replaced by an INSERT.\n"
        "// A row a MERGE WHEN MATCHED would have matched is handled below.\n"
        "func (s *MSSQLStore) NoLongerMerges(ctx context.Context) error {\n"
        "\treturn nil\n"
        "}\n"
    )
    found4 = dict(find_go_declarations(strip_go_comments(comment_src)))
    if found4.get("NoLongerMerges"):
        print("SELF-TEST FAILED: a comment merely mentioning MERGE/WHEN MATCHED "
              "was read as a real statement.")
        ok = False

    # 5. A bare MERGE with no WHEN (NOT) MATCHED is not T-SQL MERGE syntax
    # this guard can evaluate (e.g. a different dialect, or truncated text)
    # and must not be flagged.
    bare_src = (
        "package engine\n\n"
        "func (s *MSSQLStore) Unrelated(ctx context.Context) error {\n"
        "\t_, err := s.db.ExecContext(ctx, `SELECT 'MERGE' AS label`)\n"
        "\treturn err\n"
        "}\n"
    )
    found5 = dict(find_go_declarations(strip_go_comments(bare_src)))
    if found5.get("Unrelated"):
        print("SELF-TEST FAILED: a bare MERGE with no WHEN (NOT) MATCHED clause "
              "was flagged as a real MERGE statement.")
        ok = False

    # 6. .sql extraction: a migration-shaped MERGE, keyed by table name,
    # with comments stripped so a `-- MERGE` mention doesn't count.
    sql_src = (
        "-- MERGE used to be written as 'plain'; see the real statement below.\n"
        "MERGE admin.some_table AS t\n"
        "USING (SELECT 1) AS s ON t.id = s.id\n"
        "WHEN NOT MATCHED THEN INSERT (id) VALUES (s.id);\n"
    )
    found6 = dict(find_sql_declarations(strip_sql_comments(sql_src)))
    if not found6.get("admin.some_table") or found6["admin.some_table"][0][0] is not False:
        print("SELF-TEST FAILED: a .sql MERGE with no HOLDLOCK, keyed by table "
              "name, was not flagged (or the leading -- comment line confused "
              "the table-name match).")
        ok = False

    # 7. Allowlist shape: every ALLOWLIST entry needs a non-empty reason
    # and a valid status. (ALLOWLIST is empty today -- see its own comment
    # -- so this exercises the validator against a synthetic entry rather
    # than a real one, the same way case 5 above exercises extraction
    # against synthetic source.)
    synthetic_allowlist = {("f.go", "X"): (EXEMPT, ""), ("f.go", "Y"): ("bogus", "reason")}
    for key, (status, reason) in synthetic_allowlist.items():
        bad = status not in (EXEMPT, DEFERRED) or not reason.strip()
        if not bad:
            print(f"SELF-TEST FAILED: {key} should have been rejected by the "
                  f"allowlist-shape validator but wasn't")
            ok = False

    if ok:
        print("self-test: OK (7/7)")
    return ok


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

def main():
    if "--self-test" in sys.argv:
        sys.exit(0 if run_self_test() else 1)

    for key, (status, reason) in ALLOWLIST.items():
        if status not in (EXEMPT, DEFERRED) or not reason.strip():
            print(f"UNMEASURED: malformed ALLOWLIST entry {key}: "
                  f"status={status!r} reason={reason!r}")
            sys.exit(2)

    deferred = _load_deferred_from_file()
    overlap = set(ALLOWLIST) & set(deferred)
    if overlap:
        print(f"UNMEASURED: {sorted(overlap)} appear in both the inline "
              f"ALLOWLIST and mssql-merge-holdlock-deferred.tsv")
        sys.exit(2)
    allowlist = dict(ALLOWLIST)
    allowlist.update(deferred)

    try:
        out = subprocess.run(
            ["git", "ls-files", "*.go", "*.sql"],
            capture_output=True, text=True, check=True,
        )
    except Exception as e:
        print(f"UNMEASURED: could not list tracked *.go/*.sql files: {e}")
        sys.exit(2)

    files = [f for f in out.stdout.splitlines() if not f.endswith("_test.go")]
    if not files:
        print("UNMEASURED: git ls-files returned no *.go/*.sql files -- "
              "not run from inside the repository?")
        sys.exit(2)

    vulnerable_seen = set()   # (file, key) with no HOLDLOCK
    seen_with_holdlock = set()  # (file, key) that already pass, for diagnostics
    for path in files:
        try:
            text = open(path, encoding="utf-8").read()
        except Exception as e:
            print(f"UNMEASURED: could not read {path}: {e}")
            sys.exit(2)
        if "MERGE" not in text and "merge" not in text:
            continue

        if path.endswith(".sql"):
            stripped = strip_sql_comments(text)
            decls = list(find_sql_declarations(stripped))
        else:
            stripped = strip_go_comments(text)
            decls = list(find_go_declarations(stripped))

        seen_keys_this_file = {}
        for name, merges in decls:
            if name in seen_keys_this_file:
                print(f"UNMEASURED: {path}: declaration key {name!r} is not "
                      f"unique in this file (collides between a statement at "
                      f"{seen_keys_this_file[name]!r} and another) -- the "
                      f"keying scheme needs a tiebreaker for this file")
                sys.exit(2)
            seen_keys_this_file[name] = merges
            for holdlock, _stmt_head in merges:
                if holdlock:
                    seen_with_holdlock.add((path, name))
                else:
                    vulnerable_seen.add((path, name))

    unallowed = sorted(vulnerable_seen - set(allowlist))
    stale = sorted(
        (k for k in allowlist if k not in vulnerable_seen),
    )

    problems = False
    if unallowed:
        problems = True
        print("The following MSSQL MERGE statements have no HOLDLOCK -- SQL "
              "Server can evaluate WHEN NOT MATCHED true on two concurrent "
              "writers racing the same not-yet-existing key (cleat#2903/"
              "#2904). Either add WITH (HOLDLOCK) to the target, matching "
              "engine/mssql_events.go's precedent, or add a justified entry "
              "to mssql-merge-holdlock-deferred.tsv (or, having actually "
              "checked reachability, ALLOWLIST as EXEMPT):\n")
        for path, name in unallowed:
            print(f"  {path}: {name}")

    if stale:
        problems = True
        print("\nThe following allowlist/deferred entries no longer match a "
              "candidate MERGE statement -- it was removed, renamed, fixed "
              "(HOLDLOCK added), or never matched in the first place. A "
              "stale entry is a ratchet in the wrong direction; remove it:\n")
        for path, name in stale:
            print(f"  {path}: {name}")

    if problems:
        sys.exit(1)

    n_exempt = sum(1 for s, _ in allowlist.values() if s == EXEMPT)
    n_deferred = sum(1 for s, _ in allowlist.values() if s == DEFERRED)
    print(f"OK: {len(vulnerable_seen) + len(seen_with_holdlock)} MSSQL MERGE "
          f"statements found; {len(seen_with_holdlock)} already carry "
          f"HOLDLOCK, {len(vulnerable_seen)} without it are all accounted "
          f"for ({n_exempt} exempt, {n_deferred} deferred, 0 "
          f"unaccounted-for).")
    sys.exit(0)


if __name__ == "__main__":
    main()
