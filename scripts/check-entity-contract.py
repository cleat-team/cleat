#!/usr/bin/env python3
"""One contract for cleat's non-workflow entities (cleat#1702).

An entity here is a row an operator creates, that outlives every run using it,
and whose idle state is normal rather than evidence it should be removed. They
diverged: four spellings of "retired" (one inverted), updated_at on two of
thirteen, created_at missing from the two newest.

WHAT THIS ENFORCES, per the design approved 2026-09-16:

    created_at              TIMESTAMPTZ
    updated_at              TIMESTAMPTZ
    disabled_at             TIMESTAMPTZ   -- the single retirement spelling
    no legacy retirement    -- none of revoked_at, deprecated, enabled

Existing members convert one at a time, so a grandfather list records which
(table, clause) pairs are not enforced yet. It may only shrink.

WHY TOTAL COVERAGE RATHER THAN A MEMBER LIST. Membership is a semantic
judgement and is NOT derivable from column shape: admin.tenant_egress_allow is
structurally identical to public.workflow_routing. So every table found in the
migrations must be classified, and an unclassified one is an error. A list that
only names members cannot tell "not a member" from "nobody looked" -- and that
distinction is not theoretical here, see the header of entity-contract.tsv.

EXIT STATUS, which is deliberately three-valued:

    0   every enforced clause holds
    1   a contract violation, or an unclassified table
    2   the scan could not establish what it was measuring

2 is separate from 1 on purpose. A guard that cannot parse its input must not
be able to report the same thing as a clean tree -- "zero members found" agrees
with every schema, conforming or not.
"""

import argparse
import os
import re
import sys

CLAUSES = ("created_at", "updated_at", "disabled_at", "no-legacy-retirement")
LEGACY_RETIREMENT = ("revoked_at", "deprecated", "enabled")
CLASSES = ("member", "exempt", "not-an-entity")

# The grandfather list may only shrink. This ceiling lives here rather than in
# the list because a plain allowlist has an obvious cheat: the cheapest way to
# make the guard pass is to add a line, and a pass bought that way looks exactly
# like conforming. Lowering this as members convert is the intended edit;
# raising it is a deliberate change to the guard, in a diff someone reads.
#
# 23 of 40 (10 members x 4 clauses) on 2026-09-16, derived rather than typed.
GRANDFATHER_CEILING = 23


def strip_sql_comments(src):
    """Remove /* */ and -- comments.

    Without this a header that quotes a CREATE TABLE counts as a definition --
    the same "a text search cannot tell a thing from a sentence about the
    thing" trap CLAUDE.md records for migration routines.
    """
    src = re.sub(r"/\*.*?\*/", "", src, flags=re.S)
    return "\n".join(re.sub(r"--.*$", "", line) for line in src.split("\n"))


def matching_paren(src, open_idx):
    """Index of the ) matching the ( at open_idx, or None."""
    depth = 0
    i = open_idx
    while i < len(src):
        if src[i] == "(":
            depth += 1
        elif src[i] == ")":
            depth -= 1
            if depth == 0:
                return i
        i += 1
    return None


def parse_tables(migrations_dir):
    """{qualified_table: {column: type_text}} across every .sql in order.

    Tracks ALTER TABLE ADD/DROP COLUMN as well as CREATE TABLE, because a
    column added by a later migration is part of the table and a CREATE-only
    reading would miss it. Returns (tables, errors).
    """
    tables = {}
    errors = []
    files = sorted(
        os.path.join(migrations_dir, f)
        for f in os.listdir(migrations_dir)
        if f.endswith(".sql")
    )
    if not files:
        errors.append("no .sql files in %s" % migrations_dir)
        return tables, errors

    for path in files:
        src = strip_sql_comments(open(path, encoding="utf-8").read())

        for m in re.finditer(
            r"CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([A-Za-z0-9_.\"]+)\s*\(",
            src,
            re.I,
        ):
            name = qualify(m.group(1))
            close = matching_paren(src, m.end() - 1)
            if close is None:
                errors.append(
                    "%s: unbalanced parentheses in CREATE TABLE %s"
                    % (os.path.basename(path), name)
                )
                continue
            tables.setdefault(name, {})
            for col, typ in split_columns(src[m.end():close]):
                tables[name][col] = typ

        for m in re.finditer(
            r"ALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?([A-Za-z0-9_.\"]+)\s+"
            r"ADD\s+COLUMN\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-z_][a-z0-9_]*)\s+([A-Za-z0-9_ ]*)",
            src,
            re.I,
        ):
            name = qualify(m.group(1))
            if name in tables:
                tables[name][m.group(2).lower()] = m.group(3).strip().upper()

        for m in re.finditer(
            r"ALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?([A-Za-z0-9_.\"]+)\s+"
            r"DROP\s+COLUMN\s+(?:IF\s+EXISTS\s+)?([a-z_][a-z0-9_]*)",
            src,
            re.I,
        ):
            name = qualify(m.group(1))
            if name in tables:
                tables[name].pop(m.group(2).lower(), None)

    return tables, errors


def qualify(raw):
    raw = raw.strip().strip('"')
    if "." in raw:
        schema, _, name = raw.partition(".")
        return "%s.%s" % (schema.strip('"'), name.strip('"'))
    return "public.%s" % raw


def split_columns(body):
    """(name, type) for each top-level column definition in a CREATE TABLE body.

    Splits on top-level commas only: a comma inside NUMERIC(10, 2) or inside a
    CHECK (...) is not a column boundary.
    """
    out = []
    depth = 0
    current = ""
    for ch in body:
        if ch == "(":
            depth += 1
        elif ch == ")":
            depth -= 1
        if ch == "," and depth == 0:
            out.append(current)
            current = ""
        else:
            current += ch
    out.append(current)

    cols = []
    for piece in out:
        tokens = piece.strip().split()
        if not tokens:
            continue
        if not re.match(r"^[a-z_][a-z0-9_]*$", tokens[0], re.I):
            continue
        if tokens[0].upper() in (
            "PRIMARY", "FOREIGN", "UNIQUE", "CHECK", "CONSTRAINT", "EXCLUDE", "LIKE",
        ):
            continue
        cols.append((tokens[0].lower(), " ".join(tokens[1:]).upper()))
    return cols


def read_tsv(path, ncols):
    """Rows of a tab-separated data file, comments and blanks skipped.

    Rejects alignment padding. A file written with runs of tabs to line the
    columns up reads correctly here -- empty fields are dropped -- and WRONGLY
    under `awk -F'\t' '$2=="member"'`, which sees a padding tab as field 2 and
    returns nothing. Silent disagreement between two readers of one file is the
    trap this whole guard exists to prevent, so it is refused at the source
    rather than documented. It cost a vacuous mutation while this guard's own
    falsification was being written.
    """
    rows = []
    for lineno, line in enumerate(open(path, encoding="utf-8"), 1):
        line = line.rstrip("\n")
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        if "\t\t" in line:
            raise ValueError(
                "%s:%d: consecutive tabs (alignment padding). Use exactly one "
                "tab between fields, or awk reads a different file than this "
                "does: %r" % (path, lineno, line))
        fields = [f.strip() for f in line.split("\t") if f.strip() != ""]
        if len(fields) < ncols:
            raise ValueError("%s:%d: expected %d fields, got %d: %r"
                             % (path, lineno, ncols, len(fields), line))
        rows.append(fields)
    return rows


def check(migrations_dir, registry_path, grandfather_path):
    """Returns (exit_code, lines)."""
    out = []

    tables, parse_errors = parse_tables(migrations_dir)
    if parse_errors:
        return 2, ["SCAN FAILED: " + e for e in parse_errors]
    if not tables:
        return 2, ["SCAN FAILED: parsed 0 tables from %s. A scan that finds "
                   "nothing agrees with every schema." % migrations_dir]

    empty = sorted(t for t, cols in tables.items() if not cols)
    if empty:
        return 2, ["SCAN FAILED: parsed these tables with zero columns, which "
                   "means the body was not read: " + ", ".join(empty)]

    try:
        registry = read_tsv(registry_path, 2)
        grandfathered = read_tsv(grandfather_path, 2)
    except (OSError, ValueError) as exc:
        return 2, ["SCAN FAILED: %s" % exc]

    classified = {}
    for row in registry:
        table, klass = row[0], row[1]
        if klass not in CLASSES:
            return 2, ["SCAN FAILED: %s: unknown class %r for %s (expected one "
                       "of %s)" % (registry_path, klass, table, ", ".join(CLASSES))]
        if table in classified:
            return 2, ["SCAN FAILED: %s: %s classified twice" % (registry_path, table)]
        classified[table] = klass

    failures = []

    # 1. Total coverage, both directions.
    unclassified = sorted(set(tables) - set(classified))
    if unclassified:
        failures.append(
            "these tables exist in the migrations and are not classified in %s:\n%s\n"
            "  Add each as member, exempt or not-an-entity. An unclassified table\n"
            "  is an error rather than a default, because \"not a member\" and\n"
            "  \"nobody looked\" are the same silence otherwise."
            % (registry_path, "".join("    %s\n" % t for t in unclassified))
        )
    stale = sorted(set(classified) - set(tables))
    if stale:
        failures.append(
            "these are classified in %s but no longer exist in the migrations:\n%s"
            % (registry_path, "".join("    %s\n" % t for t in stale))
        )

    members = sorted(t for t, k in classified.items() if k == "member")
    if not members:
        return 2, ["SCAN FAILED: the registry classifies 0 members. Enforcing a "
                   "contract over an empty set passes on every schema."]

    # 2. The grandfather list may only shrink, and must name real pairs.
    gf = set()
    for row in grandfathered:
        table, clause = row[0], row[1]
        if clause not in CLAUSES:
            return 2, ["SCAN FAILED: %s: unknown clause %r (expected one of %s)"
                       % (grandfather_path, clause, ", ".join(CLAUSES))]
        if table not in members:
            failures.append(
                "%s grandfathers %s for %r, but it is not a member. A pair that\n"
                "  matches no member silently excuses nothing and hides a rename."
                % (grandfather_path, table, clause)
            )
        gf.add((table, clause))

    # 3. The contract, per member, per clause.
    for table in members:
        if table not in tables:
            # Already reported as stale above. Indexing here would raise, and a
            # guard that crashes on a bad registry gives a traceback instead of
            # the finding -- which the self-test caught on its first run.
            continue
        cols = tables[table]
        for clause in CLAUSES:
            if (table, clause) in gf:
                continue
            if clause == "no-legacy-retirement":
                present = [c for c in LEGACY_RETIREMENT if c in cols]
                if present:
                    failures.append(
                        "%s carries %s. The contract spells retirement one way,\n"
                        "  disabled_at TIMESTAMPTZ. `deprecated` and `enabled` are the\n"
                        "  same idea at opposite polarity, which is the actual hazard."
                        % (table, " and ".join(present))
                    )
            elif clause not in cols:
                failures.append("%s has no %s column." % (table, clause))
            elif "TIMESTAMPTZ" not in cols[clause]:
                failures.append(
                    "%s.%s is %r, not TIMESTAMPTZ."
                    % (table, clause, cols[clause].strip())
                )

    if len(gf) > GRANDFATHER_CEILING:
        failures.append(
            "%s holds %d pairs, over the ceiling of %d. This list may only\n"
            "  shrink: adding to it is the cheapest way to make this guard pass,\n"
            "  and a pass bought that way is indistinguishable from conforming.\n"
            "  If a member genuinely regressed, fix the member."
            % (grandfather_path, len(gf), GRANDFATHER_CEILING)
        )

    enforced = len(members) * len(CLAUSES) - len(gf)
    out.append("tables parsed: %d; members: %d; grandfathered pairs: %d"
               % (len(tables), len(members), len(gf)))
    out.append("clauses enforced this run: %d of %d"
               % (enforced, len(members) * len(CLAUSES)))

    # Vacuity is checked BEFORE the ordinary failures, and the order is not
    # cosmetic. Exit 2 means "the scan did not establish what it was measuring",
    # and a run that enforced nothing is exactly that whatever else is also
    # wrong. Written the other way round -- which is how this shipped first --
    # over-grandfathering the whole tree tripped the ceiling and reported 1, so
    # the one status that means "this told you nothing" was unreachable in the
    # case it exists for. Found by falsifying, not by reading.
    if enforced == 0:
        return 2, out + ["", "SCAN FAILED: every clause of every member is "
                             "grandfathered, so this run enforced nothing. That "
                             "agrees with every schema, conforming or not."]

    if failures:
        out.append("")
        for f in failures:
            out.append("ERROR: %s" % f)
        return 1, out

    out.append("OK: the entity contract holds for every clause not grandfathered.")
    return 0, out


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    ap.add_argument("--migrations", default=os.path.join(root, "migrations", "postgres"))
    ap.add_argument("--registry", default=os.path.join(root, "scripts", "entity-contract.tsv"))
    ap.add_argument("--grandfathered",
                    default=os.path.join(root, "scripts", "entity-contract-grandfathered.tsv"))
    ap.add_argument("--self-test", action="store_true")
    args = ap.parse_args()

    if args.self_test:
        sys.exit(self_test())

    code, lines = check(args.migrations, args.registry, args.grandfathered)
    for line in lines:
        print(line, file=sys.stderr if code else sys.stdout)
    sys.exit(code)


# --------------------------------------------------------------------------
# Self-test: a KNOWN-POSITIVE per clause, not a negative control.
#
# Passing on today's tree proves nothing. Today's tree is non-conforming by
# construction -- every member is grandfathered for at least one clause -- so a
# guard that passes on it is either working or carried entirely by the
# grandfather list, and the two are indistinguishable from a green run.
#
# So each case below is a schema already known to be wrong in exactly one way,
# and each must go red SEPARATELY. A single "it failed" is not enough: a guard
# that reported the same failure for every input would satisfy that.
# --------------------------------------------------------------------------

CONFORMING = """
CREATE TABLE public.widgets (
    widget_id   UUID PRIMARY KEY,
    tenant_id   UUID NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    disabled_at TIMESTAMPTZ,
    CHECK (widget_id IS NOT NULL)
);
"""

SELF_TEST_CASES = [
    ("a conforming entity passes", CONFORMING, "public.widgets\tmember\n", 0, None),
    ("missing created_at",
     CONFORMING.replace("    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),\n", ""),
     "public.widgets\tmember\n", 1, "has no created_at"),
    ("missing updated_at",
     CONFORMING.replace("    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),\n", ""),
     "public.widgets\tmember\n", 1, "has no updated_at"),
    ("missing disabled_at",
     CONFORMING.replace("    disabled_at TIMESTAMPTZ,\n", ""),
     "public.widgets\tmember\n", 1, "has no disabled_at"),
    ("legacy retirement spelling `enabled`",
     CONFORMING.replace("    disabled_at TIMESTAMPTZ,",
                        "    disabled_at TIMESTAMPTZ,\n    enabled BOOLEAN NOT NULL DEFAULT true,"),
     "public.widgets\tmember\n", 1, "carries enabled"),
    ("legacy retirement spelling `deprecated`",
     CONFORMING.replace("    disabled_at TIMESTAMPTZ,",
                        "    disabled_at TIMESTAMPTZ,\n    deprecated BOOLEAN NOT NULL DEFAULT false,"),
     "public.widgets\tmember\n", 1, "carries deprecated"),
    ("disabled_at with the wrong type",
     CONFORMING.replace("    disabled_at TIMESTAMPTZ,", "    disabled_at BOOLEAN,"),
     "public.widgets\tmember\n", 1, "not TIMESTAMPTZ"),
    ("an unclassified table",
     CONFORMING + "\nCREATE TABLE public.gadgets (gadget_id UUID PRIMARY KEY);\n",
     "public.widgets\tmember\n", 1, "not classified"),
    ("a registry naming a table that no longer exists",
     CONFORMING, "public.widgets\tmember\npublic.ghosts\tmember\n", 1, "no longer exist"),
    ("grandfathering a non-member",
     CONFORMING, "public.widgets\tmember\n", 1, "not a member"),
    ("every clause grandfathered enforces nothing",
     CONFORMING, "public.widgets\tmember\n", 2, "enforced nothing"),
    ("a migrations directory with no SQL at all",
     None, "public.widgets\tmember\n", 2, "no .sql files"),
]


def self_test():
    import shutil
    import tempfile

    fails = 0
    ran = 0
    for name, schema, registry, want_code, want_text in SELF_TEST_CASES:
        ran += 1
        tmp = tempfile.mkdtemp()
        try:
            mig = os.path.join(tmp, "migrations")
            os.makedirs(mig)
            if schema is not None:
                with open(os.path.join(mig, "001_fixture.sql"), "w") as fh:
                    fh.write(schema)

            reg = os.path.join(tmp, "registry.tsv")
            with open(reg, "w") as fh:
                fh.write(registry)

            gf = os.path.join(tmp, "grandfathered.tsv")
            with open(gf, "w") as fh:
                if name == "grandfathering a non-member":
                    fh.write("public.nonesuch\tupdated_at\n")
                elif name == "every clause grandfathered enforces nothing":
                    for clause in CLAUSES:
                        fh.write("public.widgets\t%s\n" % clause)

            code, lines = check(mig, reg, gf)
            blob = "\n".join(lines)

            if code != want_code:
                print("SELF-TEST FAIL [%s]: exit %d, want %d\n%s"
                      % (name, code, want_code, blob), file=sys.stderr)
                fails += 1
            elif want_text and want_text not in blob:
                # Right status for the wrong reason is still a failure: it is
                # how a guard passes a case it never actually examined.
                print("SELF-TEST FAIL [%s]: exit %d as expected, but the output "
                      "never says %r\n%s" % (name, code, want_text, blob),
                      file=sys.stderr)
                fails += 1
        finally:
            shutil.rmtree(tmp, ignore_errors=True)

    if fails:
        print("SELF-TEST: %d of %d cases failed." % (fails, ran), file=sys.stderr)
        return 1
    print("OK: self-test passed, all %d known-positive cases classified correctly." % ran)
    return 0


if __name__ == "__main__":
    main()
