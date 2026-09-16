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
    """Remove SQL comments, leaving string literals intact.

    Walks the source rather than running two regexes over it. The regex version
    modelled comments and not strings, which is the same "a tool applied to a
    format it does not model" fault this guard exists to catch, and it had two
    demonstrated failure modes (cleat#1719, raised by a peer session):

    1. A `--` inside a string literal in a CREATE TABLE body dropped every
       column after it, silently. `DEFAULT 'see migration 064 -- nothing to
       sync'` removed the following `disabled_at` and reported no error. That is
       the worst possible shape for cleat#1702's conversion, which adds
       `disabled_at` to thirteen tables: the guard would report "table X is
       missing disabled_at", blaming the schema for a fault in the parser.

    2. A `/*` inside a string literal -- or inside a `--` line comment, which
       offers no protection, because the old code applied the `/* */` rule
       FIRST over the whole file with re.S, before the per-line `--` rule ran --
       swallowed everything to the next `*/`.

    The second was one unrelated edit away from firing. `migrations/postgres/072`
    line 18 and `migrations/mysql/070` line 62 both contain a `/*` inside a line
    comment, inert only because neither file contains a `*/`. Appending one
    ordinary block comment to 072 took its stripped length from 375 characters
    to 18. A defect armed by an edit to an unrelated part of an unrelated file
    arrives with nothing connecting it to its cause.

    Newlines are preserved so that anything downstream reasoning in lines still
    can. Dollar-quoted bodies are handled because Postgres migrations use them
    for routine definitions.
    """
    out = []
    i = 0
    n = len(src)
    while i < n:
        ch = src[i]

        if ch == "'":
            out.append(ch)
            i += 1
            while i < n:
                if src[i] == "'":
                    if i + 1 < n and src[i + 1] == "'":   # '' is an escaped quote
                        out.append("''")
                        i += 2
                        continue
                    out.append("'")
                    i += 1
                    break
                out.append(src[i])
                i += 1
            continue

        if ch == "$":
            tag = re.match(r"\$[A-Za-z_][A-Za-z0-9_]*\$|\$\$", src[i:])
            if tag:
                body_end = src.find(tag.group(0), i + len(tag.group(0)))
                if body_end == -1:
                    out.append(src[i:])
                    break
                stop = body_end + len(tag.group(0))
                out.append(src[i:stop])
                i = stop
                continue

        if src.startswith("--", i):
            while i < n and src[i] != "\n":
                i += 1
            continue

        if src.startswith("/*", i):
            # Postgres block comments nest; SQL Server's do not. Counting is
            # correct for Postgres and harmless elsewhere, since an unnested
            # comment closes at depth 1 either way.
            depth = 1
            i += 2
            while i < n and depth:
                if src.startswith("/*", i):
                    depth += 1
                    i += 2
                elif src.startswith("*/", i):
                    depth -= 1
                    i += 2
                else:
                    if src[i] == "\n":
                        out.append("\n")
                    i += 1
            continue

        out.append(ch)
        i += 1

    return "".join(out)


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
            CREATE_TABLE_RE,
            src,
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


# An identifier in any of the three dialects: plain, "quoted", [bracketed]
# (SQL Server) or `backticked` (MySQL).
#
# DEFENSIVE, NOT A FIX -- and this is written down because the opposite was
# claimed first. The prediction was that cleat#1702's plain-and-quoted pattern
# would match nothing in migrations/mssql/, find zero tables and report clean:
# the zero-members trap again. Measured, that is FALSE. Reverting this constant
# to the old pattern still parses all 24 SQL Server tables, because no CREATE
# TABLE in this repo quotes its identifier at all:
#
#   for d in postgres mysql mssql; do
#     grep -rhcE 'CREATE[[:space:]]+TABLE[^(]*[][`"]' migrations/$d/*.sql
#   done
#   # 0, 0, 0 on 2026-09-16
#
# It is kept because all three quotings are legal and a scan that cannot read
# one parses to nothing rather than failing -- but it earns no credit for a bug
# it does not currently prevent.
#
# THAT ZERO NEEDED A SECOND MEASUREMENT TO MEAN ANYTHING, and the first command
# published here could not make it. `grep -E 'CREATE[[:space:]]+TABLE[^(]*[][`"]'`
# is LINE-ANCHORED, so it scores 0 on a file that does exactly what it looks
# for:
#
#     CREATE TABLE            -> grep: 0     CREATE TABLE [dbo].[workers] (...);
#       [dbo].[workers]                      -> grep: 1
#       (id INT);
#
# Both define [dbo].[workers]. So "0 quoted identifiers" and "0 quoted
# identifiers I could see" rendered identically, and only a separate check --
# no CREATE TABLE in the tree puts its name on a later line, 0 across all three
# dialects -- turns the first into an answer. Raised by a peer session scanning
# the same tree with a statement-aware parser; the conclusion held, the
# instrument did not deserve to be believed on its own.
#
# Re-derive with something that spans newlines and strips comments:
#
#     python3 - <<'EOF'
#     import glob, re
#     pat = re.compile(r"CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([^\s(]+)", re.I)
#     for d in ("postgres", "mysql", "mssql"):
#         q = 0
#         for f in glob.glob("migrations/%s/*.sql" % d):
#             src = re.sub(r"/\*.*?\*/", "", open(f).read(), flags=re.S)
#             src = "\n".join(re.sub(r"--.*$", "", l) for l in src.split("\n"))
#             q += sum(1 for m in pat.finditer(src) if re.search(r'[\[\]`"]', m.group(1)))
#         print(d, "quoted identifiers:", q)
#     EOF
#     # 0, 0, 0 on 2026-09-16; reports 2 on a fixture holding one same-line and
#     # one continuation-line bracketed definition, which is its positive control.
#
# The self-test locks the behaviour in and IS a control rather than a hope:
# dropping the bracket and backtick alternatives from this constant fails four
# cases, each with "parsed 0 tables" -- the zero-members signature. Verified by
# doing it.
IDENT = r'(?:\[[^\]]+\]|`[^`]+`|"[^"]+"|[A-Za-z0-9_]+)'
CREATE_TABLE_RE = re.compile(
    r"CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?((?:%s\.)?%s)\s*\(" % (IDENT, IDENT),
    re.I,
)

# SQL Server's admin schema is Postgres's admin schema; dbo is its public.
SCHEMA_ALIASES = {"dbo": "public"}


def split_identifier(raw):
    """['schema', 'name'] or ['name'], with any quoting removed."""
    parts = [next(p for p in t if p) for t in
             re.findall(r"\[([^\]]+)\]|`([^`]+)`|\"([^\"]+)\"|([A-Za-z0-9_]+)", raw)]
    return parts


def qualify(raw):
    parts = split_identifier(raw.strip())
    if len(parts) >= 2:
        schema = parts[0].lower()
        return "%s.%s" % (SCHEMA_ALIASES.get(schema, schema), parts[1])
    return "public.%s" % parts[0]


def bare(qualified):
    """The table name without its schema.

    Cross-dialect membership is compared on this, because MySQL CANNOT express
    the schema: it writes `CREATE TABLE IF NOT EXISTS tenants` where Postgres
    and SQL Server write `admin.tenants`, and `grep -c 'admin\.'
    migrations/mysql/*.sql` returns 0. Comparing qualified names reports twelve
    differences -- the same six tables in both directions -- and every one is
    spurious, which would bury the one real difference.

    Sound only while bare names are unique, which the registry asserts.
    """
    return qualified.split(".", 1)[1]


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


def check(migrations_dir, registry_path, grandfather_path, sibling_dirs=()):
    """Returns (exit_code, lines).

    migrations_dir is the reference dialect (Postgres): the contract's clauses
    are checked there, because TIMESTAMPTZ is a Postgres spelling.

    sibling_dirs are the other dialects. Only MEMBERSHIP is checked against
    them -- a table defined only in MySQL or SQL Server must still be
    classified, or total coverage has a hole exactly where nobody is looking.
    """
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
    # 1b. Coverage extends to the other dialects, compared on BARE names.
    #
    # Scoped to membership on purpose. The clause checks stay single-dialect
    # because TIMESTAMPTZ is Postgres's spelling; asking SQL Server for it would
    # fail on a schema that is correct.
    bare_to_qualified = {}
    for t in classified:
        b = bare(t)
        if b in bare_to_qualified:
            return 2, ["SCAN FAILED: %s classifies both %s and %s, whose bare "
                       "names collide. Cross-dialect membership is compared on "
                       "the bare name because MySQL cannot express a schema, so "
                       "a collision makes that comparison ambiguous."
                       % (registry_path, bare_to_qualified[b], t)]
        bare_to_qualified[b] = t

    sibling_counts = []
    seen_bare = {bare(t) for t in tables}
    for sib in sibling_dirs:
        if not os.path.isdir(sib):
            return 2, ["SCAN FAILED: %s is not a directory. A sibling dialect "
                       "that cannot be read must not look like one with nothing "
                       "in it." % sib]
        sib_tables, sib_errors = parse_tables(sib)
        if sib_errors:
            return 2, ["SCAN FAILED (%s): %s" % (os.path.basename(sib), e)
                       for e in sib_errors]
        if not sib_tables:
            return 2, ["SCAN FAILED: parsed 0 tables from %s." % sib]
        sibling_counts.append((os.path.basename(sib), len(sib_tables)))
        seen_bare |= {bare(t) for t in sib_tables}

        unknown = sorted(t for t in sib_tables if bare(t) not in bare_to_qualified)
        if unknown:
            failures.append(
                "these tables are defined in %s and are classified nowhere:\n%s"
                "  A table that exists in only one dialect is invisible to a\n"
                "  Postgres-only scan rather than unclassified, which is the one\n"
                "  place total coverage could still have a hole."
                % (os.path.basename(sib), "".join("    %s\n" % t for t in unknown))
            )

    # Staleness is judged against EVERY dialect, not the reference one.
    #
    # Written against Postgres alone -- which is how it shipped in cleat#1702,
    # when Postgres was the only dialect read -- a table that legitimately
    # exists in one dialect only reads as "no longer exists". Classifying
    # admin.rls_predicate_form correctly produced exactly that, so the guard
    # refused the fix for the hole it had just reported.
    stale = sorted(t for t in classified if bare(t) not in seen_bare)
    if stale:
        failures.append(
            "these are classified in %s but exist in no dialect's migrations:\n%s"
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
    out.append("tables parsed: %d (%s); members: %d; grandfathered pairs: %d"
               % (len(tables),
                  ", ".join("%s %d" % (n, c) for n, c in sibling_counts) or "postgres only",
                  len(members), len(gf)))
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
    ap.add_argument("--sibling", action="append", default=None,
                    help="another dialect's migrations; membership only. "
                         "Repeatable. Defaults to mysql and mssql.")
    ap.add_argument("--self-test", action="store_true")
    args = ap.parse_args()

    if args.self_test:
        sys.exit(self_test())

    siblings = args.sibling
    if siblings is None:
        siblings = [os.path.join(root, "migrations", d) for d in ("mysql", "mssql")]

    code, lines = check(args.migrations, args.registry, args.grandfathered, siblings)
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
     CONFORMING, "public.widgets\tmember\npublic.ghosts\tmember\n", 1,
     "exist in no dialect"),
    ("grandfathering a non-member",
     CONFORMING, "public.widgets\tmember\n", 1, "not a member"),
    ("every clause grandfathered enforces nothing",
     CONFORMING, "public.widgets\tmember\n", 2, "enforced nothing"),
    ("a migrations directory with no SQL at all",
     None, "public.widgets\tmember\n", 2, "no .sql files"),

    # A string literal is not a comment. Both of these dropped or destroyed
    # columns before cleat#1719, and the first did it SILENTLY -- the guard
    # reported the table as missing disabled_at, blaming the schema for a fault
    # in its own parser. That is the exact column cleat#1702 adds to thirteen
    # tables, so it would have surfaced under the conversion migrations.
    ("a -- inside a string literal does not eat the columns after it",
     CONFORMING.replace(
         "    widget_id   UUID PRIMARY KEY,",
         "    widget_id   UUID PRIMARY KEY,\n"
         "    note        TEXT NOT NULL DEFAULT 'see migration 064 -- nothing to sync',"),
     "public.widgets\tmember\n", 0, None),

    # Inert in the tree today only because no file containing a `/*` inside a
    # line comment also contains a `*/`. Appending one ordinary block comment to
    # migrations/postgres/072 took its stripped length from 375 chars to 18.
    ("a /* inside a string literal does not swallow the file",
     CONFORMING.replace(
         "    widget_id   UUID PRIMARY KEY,",
         "    widget_id   UUID PRIMARY KEY,\n"
         "    note        TEXT NOT NULL DEFAULT 'engine/*.go',")
     + "\n/* an ordinary block comment, elsewhere in the file */\n",
     "public.widgets\tmember\n", 0, None),
]

# Cross-dialect cases (cleat#1719). Each carries a sibling schema as well.
SIBLING_CASES = [
    ("a SQL Server-only table classified nowhere",
     "CREATE TABLE [admin].[gizmos] (\n  gizmo_id UNIQUEIDENTIFIER PRIMARY KEY\n);\n",
     "public.widgets\tmember\n", 1, "classified nowhere"),

    ("a SQL Server-only table that IS classified",
     "CREATE TABLE [admin].[gizmos] (\n  gizmo_id UNIQUEIDENTIFIER PRIMARY KEY\n);\n",
     "public.widgets\tmember\nadmin.gizmos\tnot-an-entity\n", 0, None),

    # The whole reason membership is keyed on the bare name. MySQL writes the
    # admin tables unqualified; a qualified comparison calls every one of them
    # missing. If this case ever goes red, the bare-name keying has regressed
    # and the guard will be reporting spurious gaps rather than real ones.
    ("MySQL's unqualified spelling of an admin table is not a difference",
     "CREATE TABLE IF NOT EXISTS `gizmos` (\n  gizmo_id CHAR(36) PRIMARY KEY\n);\n",
     "public.widgets\tmember\nadmin.gizmos\tnot-an-entity\n", 0, None),

    # A comment is not a definition. The census that prompted this issue
    # counted two of them as tables.
    ("a commented-out CREATE TABLE in a sibling is not a table",
     "-- CREATE TABLE [admin].[ghosts] ( id INT );\n"
     "CREATE TABLE [admin].[gizmos] (\n  gizmo_id UNIQUEIDENTIFIER PRIMARY KEY\n);\n",
     "public.widgets\tmember\nadmin.gizmos\tnot-an-entity\n", 0, None),

    ("a sibling dialect with no tables at all",
     "-- nothing here\n",
     "public.widgets\tmember\n", 2, "parsed 0 tables"),
]


def self_test():
    import shutil
    import tempfile

    fails = 0
    ran = 0
    cases = ([(n, sc, r, wc, wt, None) for n, sc, r, wc, wt in SELF_TEST_CASES] +
             [(n, CONFORMING, r, wc, wt, sib) for n, sib, r, wc, wt in SIBLING_CASES])
    for name, schema, registry, want_code, want_text, sibling in cases:
        ran += 1
        tmp = tempfile.mkdtemp()
        try:
            mig = os.path.join(tmp, "migrations")
            os.makedirs(mig)
            siblings = ()
            if sibling is not None:
                sibdir = os.path.join(tmp, "mssql")
                os.makedirs(sibdir)
                with open(os.path.join(sibdir, "001_sibling.sql"), "w") as fh:
                    fh.write(sibling)
                siblings = (sibdir,)
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

            code, lines = check(mig, reg, gf, siblings)
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
