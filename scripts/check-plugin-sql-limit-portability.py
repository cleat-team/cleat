#!/usr/bin/env python3
"""No plugin SQL sends a literal LIMIT to SQL Server.

cleat#2206. SQL Server has no LIMIT clause; a query built with one 500s there
outright -- "Incorrect syntax near 'LIMIT'". #2191 and #2198 already fixed
this class at several list endpoints. #2206's own audit found it missed
twice more: plugins/jobqueue/routes.go's handleListJobs and
plugins/blobstore/routes.go's handleList both built their row limit as a
literal `LIMIT $N`, sent through plugin.Rebind to every dialect including
MSSQL, unguarded.

TWO SAFE PATTERNS EXIST IN THIS CODEBASE, and this guard recognizes both:

  1. plugin.LimitClause(placeholder, dialect) -- returns "LIMIT $N" on
     Postgres/MySQL and "OFFSET 0 ROWS FETCH NEXT $N ROWS ONLY" on MSSQL. A
     query built this way never has the literal word LIMIT in Go source at
     all; it is constructed at runtime.

  2. plugin.Query{Default: "...LIMIT...", MySQL: "...LIMIT...", MSSQL:
     "...TOP.../OFFSET.../FETCH..."} -- a struct with a non-empty MSSQL arm
     that spells the row limit MSSQL's own way. plugin.Query.For(dialect)
     picks the matching field.

THE BUG IS A THIRD SHAPE: a literal LIMIT in a plain query string (or in a
plugin.Query{} struct with NO MSSQL field, which Query.For falls back to
Default for) that reaches plugin.Rebind/db.Query/db.Exec with no per-dialect
branch at all. jobqueue/background.go's queryPendingJobs carries this
guard's own cleat#1133 postmortem in its doc comment: two SEPARATE prior
fixes (cleat#1134, cleat#1141) repaired a plugin.Query{} struct eight lines
above a bare `LIMIT 10` literal that was never part of any struct and never
part of either fix -- "a check has to anchor on where SQL is EXECUTED, not
on where a dialect table is DECLARED." This guard's plugin.Query{} handling
therefore does not stand alone: every LIMIT occurrence is checked, whether
or not it sits inside a Query{} literal.

ONE PLUGIN IS ALLOWLISTED: pgvector. It has no MSSQL migration at all
(plugins/pgvector/plugin.go's Migrations() declares only a PostgreSQL Up
arm) and pgvector_embeddings' search query uses PostgreSQL-only syntax
(<=>, ::vector) unconditionally -- there is no dialect to branch for. A
literal LIMIT there cannot reach MSSQL because nothing about the plugin can.

WHAT THIS GUARD DOES AND DOES NOT COVER:

  * IN-TREE plugins only, non-test Go files under plugins/.
  * It finds LIMIT as a SQL keyword -- word-bounded, UPPERCASE only. Every
    SQL keyword in this codebase's queries is written uppercase (grep
    confirms it: the one lowercase "limit" outside Go identifiers is a JSON
    error string, `{"error":"rate limit exceeded"}`, not SQL). Case-
    insensitive matching was the first version of this pattern and it
    flagged the Go variable `limit` itself in its own self-test failure --
    matching an identifier, not a keyword. Uppercase-only is not a
    convenience; it is what keeps a scan of source text from confusing a
    variable named limit/limitStr with the SQL keyword LIMIT.
  * It does not distinguish a LIMIT that is itself unreachable code from
    one that runs -- that needs real dialect-reachability analysis, which
    this is not.
  * A plugin.Query{} struct whose MSSQL field itself contains LIMIT is
    flagged too: MSSQL has no LIMIT clause regardless of which field it is
    written into.

Usage:
  scripts/check-plugin-sql-limit-portability.py
  scripts/check-plugin-sql-limit-portability.py --self-test
"""

from __future__ import annotations

import argparse
import re
import subprocess
import sys

LIMIT_WORD = re.compile(r"\bLIMIT\b")

QUERY_OPEN = re.compile(r"plugin\.Query\{")


def find_query_struct_spans(src: str) -> list[tuple[int, int, int]]:
    """(struct_start, body_start, body_end) character offsets for every
    plugin.Query{...} literal in `src`, found by counting brace depth rather
    than matching a closing brace with a regex. struct_start is where
    "plugin.Query{" itself begins -- the exemption check below walks
    backward from it to find the comment block immediately preceding the
    "var NAME = plugin.Query{" line.

    A regex closer anchored on "the next line that is just a closing brace"
    (this guard's first version) pairs a ONE-LINE plugin.Query{...} literal
    -- one whose own "}" sits on the same line as its fields, with no
    following newline before it -- with some LATER, unrelated standalone
    "}", swallowing everything in between (including any bare LIMIT out
    there) into what looks like "covered by a Query{} struct". No one-line
    literal exists in this tree today, so it never fired, but cleat-review
    found it by inspection while reviewing #2256: it is a real gap, not a
    hypothetical one. cleat#2257.

    Depth-counting closes on the FIRST brace that brings the count back to
    zero, whatever line it is on, so a one-liner, a multi-liner, and an
    indented close (plugins/datadogexport/background.go nests four of these
    inside an outer struct literal, each closing on its own "\\t},") are all
    handled the same way, with no assumption about where the closer sits.
    """
    spans = []
    for m in QUERY_OPEN.finditer(src):
        depth = 1
        i = m.end()  # just past the opening "{"
        while i < len(src) and depth > 0:
            if src[i] == "{":
                depth += 1
            elif src[i] == "}":
                depth -= 1
            i += 1
        if depth == 0:
            spans.append((m.start(), m.end(), i - 1))
        # depth > 0 here means an unterminated literal (unbalanced source);
        # nothing to pair it with, so it is left uncovered rather than
        # guessed at -- any LIMIT in it still gets caught by the bare-LIMIT
        # pass below.
    return spans

# Directories with no MSSQL story at all. See the module docstring for why
# pgvector is here. Keep this list short and explained; a plugin belongs on
# it only when it genuinely cannot reach MSSQL, not when fixing it is
# inconvenient.
SINGLE_DIALECT_ALLOWLIST = {"pgvector"}

# A plugin.Query{} struct can mark its own absent MSSQL arm as deliberate by
# carrying this marker in the comment block immediately above its "var NAME
# = plugin.Query{" line -- cleat#2821/#2866. blobstore's staleWorkflowRefs
# established the underlying pattern first (a Go-level dialect branch before
# the caller ever reaches Query.For, so Default is provably never reached on
# MSSQL) without needing this marker, because it carries no LIMIT; this
# guard has no way to see a Go-level branch in the CALLER, only the struct
# itself, so a struct that DOES contain LIMIT and relies on the same pattern
# has to say so where this guard can see it. A marker is deliberately
# easier to add than to satisfy by accident -- it is prose, not syntax, so
# a reviewer reads the claim it makes (that the caller branches around
# Query.For for this dialect) rather than a guard inferring it.
MSSQL_EXEMPT_MARKER = "MSSQL ARM INTENTIONALLY ABSENT"


def has_exempt_marker(src: str, struct_start: int) -> bool:
    """True if MSSQL_EXEMPT_MARKER appears in the contiguous "//" comment
    block immediately above the line containing `struct_start` -- i.e. the
    comment attached to this specific "var NAME = plugin.Query{" line, not
    some other comment earlier in the file. Walks upward one line at a time
    and stops at the first line that is not a "//" comment (a blank line
    separating this struct from an unrelated comment block above it, or
    code), so a marker on a DIFFERENT struct's comment never exempts this
    one.
    """
    lines = src[:struct_start].split("\n")
    # lines[-1] is the partial "var NAME = plugin.Query{" line up to
    # struct_start; walk upward through FULL preceding lines.
    i = len(lines) - 2
    while i >= 0:
        line = lines[i].strip()
        if not line.startswith("//"):
            break
        if MSSQL_EXEMPT_MARKER in line:
            return True
        i -= 1
    return False


def find_bug_lines(path: str, src: str) -> list[str]:
    """Lines in `src` (from `path`) that carry an unguarded literal LIMIT.

    Two passes over the same text, not one: a plugin.Query{} struct is
    checked for a missing/empty MSSQL arm as a whole (a LIMIT in its Default
    or MySQL field is fine precisely because MSSQL has its own field), and
    every OTHER LIMIT occurrence -- outside any Query{} struct -- is flagged
    unconditionally, because nothing branches it by dialect at all. Line
    numbers are computed from character offsets so the tool's own output
    points at a real line, not just a struct.
    """
    plugin = path.split("/")[1] if path.startswith("plugins/") else ""
    if plugin in SINGLE_DIALECT_ALLOWLIST:
        return []

    bug_lines: set[int] = set()

    def line_of(offset: int) -> int:
        return src.count("\n", 0, offset) + 1

    covered = []  # (start, end) character spans belonging to some Query{}
    for struct_start, body_start, body_end in find_query_struct_spans(src):
        body = src[body_start:body_end]
        covered.append((body_start, body_end))
        has_mssql = re.search(r'MSSQL:\s*`[^`]+`', body) is not None
        if not has_mssql:
            if has_exempt_marker(src, struct_start):
                continue
            for lm in LIMIT_WORD.finditer(body):
                bug_lines.add(line_of(body_start + lm.start()))
        else:
            # Even WITH an MSSQL arm, LIMIT inside that arm itself is wrong
            # on its own terms -- MSSQL has no LIMIT clause regardless of
            # which struct field carries it.
            for mssql_m in re.finditer(r'MSSQL:\s*`([^`]*)`', body):
                for lm in LIMIT_WORD.finditer(mssql_m.group(1)):
                    bug_lines.add(line_of(body_start + mssql_m.start(1) + lm.start()))

    def inside_covered(offset: int) -> bool:
        return any(s <= offset < e for s, e in covered)

    for lm in LIMIT_WORD.finditer(src):
        if inside_covered(lm.start()):
            continue
        # A comment mentioning LIMIT (documenting the very bug this guard
        # exists to catch, as several already-fixed sites do) is prose, not
        # SQL. A text search cannot tell a comment from code in general, but
        # this codebase's comments are always "//"-prefixed, one line at a
        # time -- check the line, not the file.
        line_start = src.rfind("\n", 0, lm.start()) + 1
        line = src[line_start : src.find("\n", lm.start())]
        stripped = line.strip()
        if stripped.startswith("//"):
            continue
        bug_lines.add(line_of(lm.start()))

    return [f"{path}:{n}" for n in sorted(bug_lines)]


def tracked_plugin_sources() -> dict[str, str]:
    """Every non-test Go file under plugins/, by git rather than a walk.

    git ls-files, not rglob: .claude/worktrees/ holds whole second copies of
    the repo, and a walking guard attributes another session's files to this
    one. CLAUDE.md records that under permissive guards.
    """
    out = subprocess.run(
        ["git", "ls-files", "plugins/"], capture_output=True, text=True, check=True
    ).stdout.split()
    files = {}
    for p in out:
        if p.endswith(".go") and not p.endswith("_test.go"):
            try:
                files[p] = open(p, encoding="utf-8", errors="replace").read()
            except OSError:
                continue
    return files


def self_test() -> int:
    failures: list[str] = []

    def check(label: str, path: str, src: str, want: list[str]):
        got = find_bug_lines(path, src)
        if got != want:
            failures.append(f"{label}: got {got}, want {want}")

    # KNOWN-POSITIVE: the exact shape #2206 found and fixed -- a bare literal
    # LIMIT, no Query{} struct, no dialect branch anywhere.
    check(
        "bare literal LIMIT, unguarded",
        "plugins/widget/routes.go",
        'query := `SELECT * FROM widgets WHERE tenant_id = $1 ORDER BY id LIMIT $2`\n'
        'rows, err := p.db.Query(ctx, plugin.Rebind(query, p.dialect), tid, limit)\n',
        ["plugins/widget/routes.go:1"],
    )

    # KNOWN-NEGATIVE, and the one the first version of this pattern got
    # wrong: a Go identifier that merely CONTAINS "limit" -- limit,
    # limitStr, argLimit -- is not the SQL keyword. Case-insensitive
    # matching flagged this line, which is real, ordinary code
    # (plugins/jobqueue/routes.go, plugins/blobstore/routes.go, and every
    # other list endpoint in this codebase has exactly this shape) with no
    # SQL in it at all.
    check(
        "a Go identifier containing 'limit' is not the keyword",
        "plugins/widget/routes.go",
        'limitStr := r.URL.Query().Get("limit")\n'
        "limit := 50\n"
        "if v, err := strconv.Atoi(limitStr); err == nil && v > 0 {\n"
        "\tlimit = v\n"
        "}\n",
        [],
    )

    # KNOWN-POSITIVE: a plugin.Query{} struct with Default/MySQL LIMIT arms
    # but no MSSQL arm -- Query.For falls back to Default, carrying the
    # literal through.
    check(
        "plugin.Query{} with no MSSQL arm",
        "plugins/widget/queries.go",
        "var q = plugin.Query{\n"
        "\tDefault: `SELECT * FROM widgets ORDER BY id LIMIT 10`,\n"
        "\tMySQL:   `SELECT * FROM widgets ORDER BY id LIMIT 10`,\n"
        "}\n",
        ["plugins/widget/queries.go:2", "plugins/widget/queries.go:3"],
    )

    # KNOWN-POSITIVE: LIMIT written into the MSSQL arm itself -- wrong on its
    # own terms, regardless of Default's LIMIT being fine (MSSQL has its own
    # arm, so Default's is never reached on that dialect).
    check(
        "LIMIT inside the MSSQL arm itself",
        "plugins/widget/queries.go",
        "var q = plugin.Query{\n"
        "\tDefault: `SELECT * FROM widgets ORDER BY id LIMIT 10`,\n"
        "\tMSSQL:   `SELECT TOP 10 * FROM widgets ORDER BY id LIMIT 10`,\n"
        "}\n",
        ["plugins/widget/queries.go:3"],
    )

    # KNOWN-POSITIVE -- cleat#2821/#2866: a struct missing its MSSQL arm is
    # still flagged with NO marker present, even when the reason would be a
    # good one if stated. The marker is required, not inferred.
    check(
        "no MSSQL arm, no exemption marker -- still flagged",
        "plugins/widget/queries.go",
        "// The caller branches on dialect before ever calling .For() on this.\n"
        "var q = plugin.Query{\n"
        "\tDefault: `SELECT * FROM widgets ORDER BY id LIMIT 10`,\n"
        "}\n",
        ["plugins/widget/queries.go:3"],
    )

    # KNOWN-NEGATIVE -- the exemption itself: the marker on the comment line
    # immediately above the struct's own "var ... = plugin.Query{" silences
    # it.
    check(
        "no MSSQL arm, exemption marker present -- not flagged",
        "plugins/widget/queries.go",
        "// MSSQL ARM INTENTIONALLY ABSENT: the caller dialect-branches before\n"
        "// ever calling .For() on this struct; see claimOldestUnprocessedEventMSSQL.\n"
        "var q = plugin.Query{\n"
        "\tDefault: `SELECT * FROM widgets ORDER BY id LIMIT 10`,\n"
        "}\n",
        [],
    )

    # KNOWN-NEGATIVE: the marker has to be on THIS struct's own comment
    # block, not merely present somewhere earlier in the file. A blank line
    # breaks the contiguous block, so an unrelated struct right below an
    # exempted one does NOT inherit the exemption.
    check(
        "a marker on an earlier, unrelated struct does not exempt this one",
        "plugins/widget/queries.go",
        "// MSSQL ARM INTENTIONALLY ABSENT: see other struct.\n"
        "var other = plugin.Query{\n"
        "\tDefault: `SELECT * FROM other_table ORDER BY id`,\n"
        "}\n"
        "\n"
        "var q = plugin.Query{\n"
        "\tDefault: `SELECT * FROM widgets ORDER BY id LIMIT 10`,\n"
        "}\n",
        ["plugins/widget/queries.go:7"],
    )

    # KNOWN-POSITIVE, and the exact gap cleat-review found reviewing #2256
    # (cleat#2257): a ONE-LINE plugin.Query{} literal -- its own closing "}"
    # on the same line as its fields, no newline before it -- followed by an
    # unrelated bare LIMIT later in the file. The first version of this
    # guard paired braces with a regex anchored on "the next line that is
    # just a closing brace", so a one-liner's "}" (mid-line, not matched)
    # made the pairing skip past it and grab the NEXT standalone "}" instead
    # -- here, the end of an unrelated function -- swallowing the real LIMIT
    # in between into what looked like "covered by a Query{} struct with an
    # MSSQL arm". Depth-counting closes each Query{} on its own matching
    # brace regardless of what line it is on, so the one-liner covers only
    # itself and the later LIMIT is still checked as the bare literal it is.
    check(
        "a one-line plugin.Query{} literal does not swallow a later LIMIT",
        "plugins/widget/queries.go",
        "var q = plugin.Query{Default: `SELECT * FROM widgets ORDER BY id LIMIT 10`, "
        "MSSQL: `SELECT TOP 10 * FROM widgets ORDER BY id`}\n"
        "\n"
        "func other() {\n"
        "\tquery := `SELECT * FROM other_table ORDER BY id LIMIT $1`\n"
        "\t_ = query\n"
        "}\n",
        ["plugins/widget/queries.go:4"],
    )

    # KNOWN-NEGATIVE: the jobqueue/background.go shape -- a complete
    # plugin.Query{} struct with a real, non-empty MSSQL arm using TOP.
    check(
        "plugin.Query{} with a real MSSQL arm",
        "plugins/widget/queries.go",
        "var q = plugin.Query{\n"
        "\tDefault: `SELECT * FROM widgets ORDER BY id LIMIT 10`,\n"
        "\tMySQL:   `SELECT * FROM widgets ORDER BY id LIMIT 10`,\n"
        "\tMSSQL:   `SELECT TOP 10 * FROM widgets ORDER BY id`,\n"
        "}\n",
        [],
    )

    # KNOWN-NEGATIVE: plugin.LimitClause -- no literal LIMIT in source at
    # all.
    check(
        "plugin.LimitClause, the safe pattern",
        "plugins/widget/routes.go",
        'query += " " + plugin.LimitClause(fmt.Sprintf("$%d", argIdx), p.dialect)\n',
        [],
    )

    # KNOWN-NEGATIVE: a comment documenting the bug this guard exists to
    # catch (several already-fixed sites read exactly like this) is not SQL.
    check(
        "a comment mentioning LIMIT",
        "plugins/widget/routes.go",
        '// plugin.LimitClause, not a literal "LIMIT $N": SQL Server has no LIMIT.\n'
        'query += " " + plugin.LimitClause(fmt.Sprintf("$%d", argIdx), p.dialect)\n',
        [],
    )

    # KNOWN-NEGATIVE: the pgvector allowlist -- a bare literal LIMIT in a
    # plugin with no MSSQL story at all.
    check(
        "pgvector is allowlisted",
        "plugins/pgvector/host_functions.go",
        'query := fmt.Sprintf(`SELECT * FROM pgvector_embeddings ORDER BY embedding <=> \'%s\' LIMIT $3`, lit)\n',
        [],
    )

    # A DOCUMENTED GAP, not a known-negative: uppercase-only matching means a
    # lowercase `limit` written as the SQL keyword is NOT caught. Every SQL
    # keyword in this tree's queries is written uppercase today (see the
    # module docstring's grep), so this has not yet been a real miss -- but
    # it is a real limitation, not an absence of one, and this case exists so
    # that stays visible and tested rather than silently true. If this case
    # ever starts FAILING (find_bug_lines finding the lowercase site), that
    # means uppercase-only just got weaker, not stronger -- read the diff
    # before deciding that is progress.
    if find_bug_lines(
        "plugins/widget/routes.go",
        'query := `select * from widgets order by id limit $2`\n',
    ):
        failures.append(
            "documented-gap case started passing: uppercase-only now catches "
            "lowercase SQL, which contradicts this script's own module docstring"
        )

    # Vacuity: a scan that sees nothing agrees with everything.
    if find_bug_lines("plugins/widget/routes.go", "// no SQL here at all\n"):
        failures.append("invented a finding in a file with no LIMIT anywhere")

    if failures:
        for f in failures:
            print(f"SELF-TEST FAIL: {f}", file=sys.stderr)
        return 1
    print("self-test passed: 14 cases (six known-positive, six known-negative, "
          "one documented gap, one vacuity)")
    return 0


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--self-test", action="store_true")
    args = ap.parse_args()
    if args.self_test:
        return self_test()

    files = tracked_plugin_sources()
    if not files:
        print("ERROR: found no non-test Go files under plugins/. This scanned nothing; "
              "it did not pass.", file=sys.stderr)
        return 2

    findings: list[str] = []
    for path, src in sorted(files.items()):
        findings.extend(find_bug_lines(path, src))

    if findings:
        print("ERROR: these lines send a literal LIMIT to a query that reaches SQL Server "
              "unguarded:", file=sys.stderr)
        for f in findings:
            print(f"    {f}", file=sys.stderr)
        print(
            "\nSQL Server has no LIMIT clause; this 500s there outright. Use "
            "plugin.LimitClause(placeholder, dialect) for a placeholder-bound row limit, "
            "or a plugin.Query{} struct with a non-empty MSSQL arm spelling it as TOP or "
            "OFFSET/FETCH. cleat#2206.",
            file=sys.stderr,
        )
        return 1

    print(f"OK: scanned {len(files)} plugin sources, no unguarded literal LIMIT found.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
