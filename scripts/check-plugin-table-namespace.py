#!/usr/bin/env python3
"""No two plugins may declare the same table name.

cleat#1288. Plugin migrations create their tables unqualified, in one flat
namespace, with CREATE TABLE IF NOT EXISTS. There is no per-plugin schema and
no name prefix, and several of the names are generic enough that a second
plugin would plausibly choose the same one -- kv_store, schedules, task_queue,
feature_flags, audit_events.

WHAT A COLLISION DOES, measured on postgres:16 in the issue. Plugin one owns
kv_store; plugin two declares its own shape under the same name:

    CREATE TABLE IF NOT EXISTS kv_store (owner text, payload jsonb, ...)
      -> "CREATE TABLE"       reported as success; it is a no-op
    columns -> tenant_id, k, v                plugin one's shape survives
    SELECT owner FROM kv_store -> ERROR: column "owner" does not exist

IF NOT EXISTS checks the NAME, not the shape. So the migration succeeds,
plugin_migrations records the version as applied and it never re-runs, the
plugin registers routes and background loops against a table it does not
recognise, and the failure surfaces at first query as a column error with
nothing connecting it to the cause. Since #1280 it can also inherit the other
plugin's RLS policy.

WHAT THIS GUARD DOES AND DOES NOT COVER, stated because a green guard is
otherwise read as coverage:

  * IN-TREE plugins only. It makes "no two plugins in this repository claim
    one name" a property CI checks rather than a fact someone measured once.
  * A THIRD-PARTY plugin colliding with a built-in is NOT covered and cannot
    be, from the tree. That needs either a schema per plugin (#1287, and it
    gives uninstall a meaning it currently lacks) or a migration-time column-set
    comparison. Neither is this script.

Usage:
  scripts/check-plugin-table-namespace.py
  scripts/check-plugin-table-namespace.py --self-test
"""

from __future__ import annotations

import argparse
import collections
import re
import subprocess
import sys

# CREATE TABLE [IF NOT EXISTS] <name>. The name may be qualified, and a
# qualified name is kept whole: `plugin_x.kv_store` and `kv_store` are
# different names and only the second is in the flat namespace this is about.
TABLE = re.compile(
    r"CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-zA-Z_][a-zA-Z0-9_.]*)", re.I
)


def strip_comments(src: str) -> str:
    """Remove // and /* */ comments, preserving line counts and the contents
    of string/backtick literals (which may contain // or /* and must not be
    treated as comments -- a DDL string can quote an http:// URL).

    cleat#2373: `CREATE\\s+TABLE\\s+...` matches prose too, and this tree has
    comments reading "at CREATE TABLE time" and "CREATE TABLE time with
    ...". Both yield a table named `time`, and two plugins each writing one
    of those sentences report a collision that does not exist. Ported from
    scripts/check-no-raw-rebind.py's strip_comments rather than reimplemented,
    since a hand-written character scanner that gets string/backtick handling
    right is not something to write twice.
    """
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


def declarations(files: dict[str, str]) -> dict[str, set[str]]:
    """table name -> the set of plugins declaring it.

    `files` maps path to contents, so --self-test drives this exact function on
    synthetic input rather than a reimplementation of it.

    THE PLUGIN IS THE PATH SEGMENT, NOT THE FILE. One plugin declares the same
    table once per dialect in one file, so a count of DECLARATIONS reports 3 for
    every table on a clean tree. The issue's own re-derive command says
    "anything with a count above one is a live collision" and would therefore
    fire on all 29. The predicate is two distinct PLUGINS, and that distinction
    is the whole of this function.
    """
    owner: dict[str, set[str]] = collections.defaultdict(set)
    for path, src in files.items():
        parts = path.split("/")
        if len(parts) < 2 or parts[0] != "plugins":
            continue
        plugin = parts[1]
        for m in TABLE.finditer(strip_comments(src)):
            owner[m.group(1)].add(plugin)
    return dict(owner)


def tracked_plugin_sources() -> dict[str, str]:
    """Every non-test Go file under plugins/, by git rather than a walk.

    git ls-files, not rglob: .claude/worktrees/ holds whole second copies of the
    repo, and a walking guard attributes another session's files to this one --
    getting MORE likely to misfire as the working tree gets messier, which is
    backwards. CLAUDE.md records that under permissive guards.

    NOT just */migrations.go, and this is not hypothetical: pgvector declares
    its migrations inline in plugin.go, so a filename-keyed scan misses
    pgvector_collections and pgvector_embeddings. The issue's census said 27 for
    exactly that reason; it is 29. Anchor to where the DDL lives, not to what
    the file is called.
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

    def check(label: str, files: dict[str, str], want_dupes: set[str]):
        got = {t for t, p in declarations(files).items() if len(p) > 1}
        if got != want_dupes:
            failures.append(f"{label}: reported {sorted(got)}, want {sorted(want_dupes)}")

    # KNOWN-POSITIVE: two plugins, one name. This is the whole subject, and on a
    # clean tree "no collisions" and "the scan is broken" are the same output.
    check(
        "two plugins claiming kv_store",
        {
            "plugins/kvstore/migrations.go": "CREATE TABLE IF NOT EXISTS kv_store (k text)",
            "plugins/other/migrations.go": "CREATE TABLE IF NOT EXISTS kv_store (owner text)",
        },
        {"kv_store"},
    )

    # KNOWN-NEGATIVE, and the one the issue's own command gets wrong: ONE plugin
    # declaring the same table once per dialect is not a collision.
    check(
        "one plugin, three dialects",
        {
            "plugins/kvstore/migrations.go": (
                "CREATE TABLE IF NOT EXISTS kv_store (k text)\n"
                "CREATE TABLE IF NOT EXISTS kv_store (k varchar(255))\n"
                "CREATE TABLE IF NOT EXISTS kv_store (k nvarchar(255))\n"
            )
        },
        set(),
    )

    # KNOWN-NEGATIVE: a qualified name is not in the flat namespace, so two
    # plugins using their own schemas do not collide.
    check(
        "qualified names in different schemas",
        {
            "plugins/a/migrations.go": "CREATE TABLE plugin_a.kv_store (k text)",
            "plugins/b/migrations.go": "CREATE TABLE plugin_b.kv_store (k text)",
        },
        set(),
    )

    # KNOWN-POSITIVE for the inline case: DDL outside migrations.go still counts.
    check(
        "a table declared in plugin.go, not migrations.go",
        {
            "plugins/pgvector/plugin.go": "CREATE TABLE IF NOT EXISTS shared_name (id uuid)",
            "plugins/other/migrations.go": "CREATE TABLE IF NOT EXISTS shared_name (id uuid)",
        },
        {"shared_name"},
    )

    # Vacuity: a scan that sees nothing agrees with everything.
    if declarations({"plugins/a/migrations.go": "-- no DDL here\n"}):
        failures.append("invented a declaration in a file with none")

    # KNOWN-NEGATIVE, cleat#2373: two plugins, each with a comment containing
    # "CREATE TABLE <word>" in prose, must not report a collision on <word>.
    # This is the exact shape of the two real comments this issue was filed
    # against (oauthprovider's "CREATE TABLE time with ..." and tenantquota's
    # "at CREATE TABLE time, ..."), reduced to the minimum that reproduces it.
    check(
        "a comment mentioning CREATE TABLE in two plugins",
        {
            "plugins/a/migrations.go": (
                "// unless one is given at CREATE TABLE time, and v1 gave none --\n"
                "CREATE TABLE IF NOT EXISTS kv_store (k text)"
            ),
            "plugins/b/migrations.go": (
                "// CREATE TABLE time with \"exceeds the maximum key length\"\n"
                "CREATE TABLE IF NOT EXISTS other_table (k text)"
            ),
        },
        set(),
    )

    if failures:
        for f in failures:
            print(f"SELF-TEST FAIL: {f}", file=sys.stderr)
        return 1
    print("self-test passed: 6 cases (two known-positive, four known-negative)")
    return 0


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--self-test", action="store_true")
    args = ap.parse_args()
    if args.self_test:
        return self_test()

    files = tracked_plugin_sources()
    owner = declarations(files)

    # Both vacuity guards matter. Zero files and zero names look identical to a
    # clean tree from the outside.
    if not files:
        print("ERROR: found no non-test Go files under plugins/. This scanned nothing; "
              "it did not pass.", file=sys.stderr)
        return 2
    if not owner:
        print(f"ERROR: scanned {len(files)} plugin sources and found no CREATE TABLE at all. "
              "The pattern is broken, not the tree -- these plugins do declare tables.",
              file=sys.stderr)
        return 2

    dupes = {t: sorted(p) for t, p in owner.items() if len(p) > 1}
    if dupes:
        print("ERROR: these table names are declared by more than one plugin:", file=sys.stderr)
        for t, plugins in sorted(dupes.items()):
            print(f"    {t}: {', '.join(plugins)}", file=sys.stderr)
        print(
            "\nCREATE TABLE IF NOT EXISTS checks the NAME, not the shape. The second "
            "plugin's migration becomes a silent no-op, is recorded as applied so it "
            "never re-runs, and the plugin then runs against the first plugin's table -- "
            "surfacing much later as 'column does not exist' with nothing pointing here. "
            "Rename one, or qualify both with a per-plugin schema (cleat#1288, #1287).",
            file=sys.stderr,
        )
        return 1

    print(f"OK: {len(owner)} plugin table names across {len(files)} sources, "
          f"each claimed by exactly one plugin.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
