#!/usr/bin/env python3
"""check-no-raw-rebind.py -- cleat#2259.

plugin.Rebind is the identity for MySQL now: the $N -> ? rewrite (and the
arg reorder MySQL's positional ? binding needs) happens only inside
plugin.RebindArgs. A caller that hands Rebind's output straight to a raw
*sql.DB/*sql.Tx/*sql.Conn sends literal "$1" text to a MySQL server -- a
syntax error, not a silent mis-bind, but silent until someone actually runs
it against MySQL.

Finding these by eye undercounts them. The first pass at this fix searched
for "DialectMySQL" appearing near "plugin.Rebind(" and missed every site
built from a `dialect` loop variable instead -- 8 of the 19 files this
guard now protects were found only by re-deriving the call sites
exhaustively, by receiver type rather than by nearby text. This guard does
the same: it does not look for "MySQL" anywhere, only for the call SHAPE.

RULE. The call shape `plugin.Rebind(` (or the bare `Rebind(` inside
package plugin itself, which spells it unqualified) is forbidden:
  - in every _test.go file in the repository, and
  - in every non-test .go file OUTSIDE plugins/**.

Everywhere else that wants a rebound statement should use one of:
  - a plugin.PluginDB/PluginTx (p.db, tx, ...) -- Rebind is redundant
    there and already safe, since RebindArgs runs inside the adapter.
  - plugins/plugintest.ExecRebound/QueryRebound/QueryRowRebound, for a raw
    handle in a test (see that package's doc comment for why it lives
    there and not in engine/testutil).
  - plugin.RebindArgs directly, for a raw handle in production code
    (cmd/cleatctl/dialect.go's rebindArgs is the existing example).

EXCEPTIONS. An explicit allowlist of the handful of files that define or
directly test Rebind's own behaviour -- not a directory heuristic, because
"outside plugins/**" already IS the directory heuristic and the exception
to it is narrow and enumerable.

Exit codes: 0 clean, 1 a violation was found, 2 the check could not run.
"""
import re
import subprocess
import sys

ALLOWED_FILES = {
    "plugin/query.go",
    "plugin/query_test.go",
    "plugin/query_dialect_test.go",
    "plugin/rebind_literal_test.go",
}


def strip_comments(src):
    """Remove // and /* */ comments, preserving line counts and the
    contents of string/backtick literals (which may contain // or /* and
    must not be treated as comments -- and which may themselves quote
    "plugin.Rebind(" in prose, which must not be treated as a call)."""
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


QUALIFIED = re.compile(r"\bplugin\.Rebind\(")
BARE = re.compile(r"(?<![\w.])Rebind\(")
FUNC_DEF = re.compile(r"^\s*func\s+Rebind\(", re.M)


def violations_in(path, text):
    """Return the 1-indexed line numbers where the call shape appears,
    for the given repo-relative path and its (unstripped) source text."""
    stripped = strip_comments(text)
    # Any file directly inside plugin/ (not plugins/) spells its own
    # package's function unqualified.
    is_plugin_pkg_file = path.startswith("plugin/")
    pat = BARE if is_plugin_pkg_file else QUALIFIED

    def_lines = set()
    for m in FUNC_DEF.finditer(stripped):
        def_lines.add(stripped.count("\n", 0, m.start()) + 1)

    lines = []
    for m in pat.finditer(stripped):
        line = stripped.count("\n", 0, m.start()) + 1
        if line in def_lines:
            continue
        lines.append(line)
    return lines


def is_forbidden_path(path):
    if path in ALLOWED_FILES:
        return False
    if path.endswith("_test.go"):
        return True
    if path.startswith("plugins/"):
        return False
    return True


def flagged(path, text):
    """The exact decision main() makes for one file: eligible path AND the
    call shape present. Self-test cases drive this, not violations_in
    directly, so a bug in is_forbidden_path is exercised too."""
    if not is_forbidden_path(path):
        return []
    return violations_in(path, text)


def run_self_test():
    """A known-positive proving the guard catches the shape this fix
    exists for, not a shape that happens to mention MySQL. This is the
    exact pattern the first pass at cleat#2259 missed: a `dialect` loop
    variable, no "MySQL" text anywhere nearby, assigned to a local before
    being handed to a raw driver method on a later line."""
    positive = (
        "package widget_test\n\n"
        "func probe(fixtureDB *sql.Conn, dialect plugin.Dialect) {\n"
        "\tstmt := plugin.Rebind(`SELECT 1 FROM t WHERE id = $1`, dialect)\n"
        "\tfixtureDB.ExecContext(ctx, stmt, 1)\n"
        "}\n"
    )
    found = flagged("plugins/widget/a_probe_test.go", positive)
    if not found:
        print("SELF-TEST FAILED: the loop-variable / assign-then-use shape "
              "was not caught. This is the exact case the first pass at "
              "cleat#2259 missed by keying on \"DialectMySQL\" appearing "
              "nearby -- the guard must not repeat that mistake.")
        return False

    negative_safe_plugin_db = (
        "package widget\n\n"
        "func (p *Plugin) probe(ctx context.Context) {\n"
        "\tp.db.Exec(ctx, plugin.Rebind(`DELETE FROM t WHERE id = $1`, p.dialect), 1)\n"
        "}\n"
    )
    if flagged("plugins/widget/backend.go", negative_safe_plugin_db):
        print("SELF-TEST FAILED: a plugins/** non-test file was flagged; "
              "those redundant Rebind calls are safe by design (RebindArgs "
              "runs inside the adapter) and must not be flagged.")
        return False

    negative_comment = (
        "package widget_test\n\n"
        "// see plugin.Rebind(query, dialect) for the rewrite this used to do.\n"
        "func probe() {}\n"
    )
    if flagged("plugins/widget/a_probe_test.go", negative_comment):
        print("SELF-TEST FAILED: a comment merely mentioning plugin.Rebind( "
              "was flagged as a call.")
        return False

    print("self-test: OK (3/3)")
    return True


def main():
    if "--self-test" in sys.argv:
        sys.exit(0 if run_self_test() else 1)

    try:
        out = subprocess.run(
            ["git", "ls-files", "*.go"],
            capture_output=True, text=True, check=True,
        )
    except Exception as e:
        print(f"UNMEASURED: could not list tracked .go files: {e}")
        sys.exit(2)

    files = out.stdout.splitlines()
    if not files:
        print("UNMEASURED: git ls-files returned no .go files -- "
              "not run from inside the repository?")
        sys.exit(2)

    problems = []
    for path in files:
        if not is_forbidden_path(path):
            continue
        try:
            text = open(path, encoding="utf-8").read()
        except Exception as e:
            print(f"UNMEASURED: could not read {path}: {e}")
            sys.exit(2)
        if "Rebind(" not in text:
            continue
        for line in violations_in(path, text):
            problems.append((path, line))

    if problems:
        print("plugin.Rebind( called directly outside plugins/** or inside "
              "a _test.go file. Rebind is the identity for MySQL -- use "
              "plugins/plugintest.ExecRebound/QueryRebound/QueryRowRebound "
              "for a raw handle in a test, plugin.RebindArgs directly in "
              "production code, or route through a plugin.PluginDB/PluginTx "
              "(cleat#2259):\n")
        for path, line in problems:
            print(f"  {path}:{line}")
        sys.exit(1)

    print("OK: no raw plugin.Rebind( call sites outside plugins/** or in a _test.go file.")
    sys.exit(0)


if __name__ == "__main__":
    main()
