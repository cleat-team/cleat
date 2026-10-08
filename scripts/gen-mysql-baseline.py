#!/usr/bin/env python3
"""Generate a compacted migration baseline from a mysqldump of the CURRENT chain.

The dump is the RESOLVED final state: every ALTER already applied, every routine
already at its last definition, every index already where its migration put it.
That sidesteps the "four of ten routines are redefined, take the LAST" trap by
construction rather than by reading 70 files and hoping.

cleat#2422, MySQL half (cleat#2433). Mirrors scripts/gen-postgres-baseline.py
deliberately, including the 002 refusal, which is the one defect from the
PostgreSQL half that is worth carrying across as a warning rather than
re-learning.

Reads:  argv[1] = `mysqldump --no-data --routines --triggers --skip-comments` of
                  a database built from the chain being compacted
Writes: argv[2]/001_schema.sql, 002_defaults.sql, 003_procedures.sql

How to run it, end to end -- this is what makes the baseline re-derivable from
the repo rather than a trusted artifact:

    # 1. Build a database from the chain. Apply it the way every other consumer
    #    does -- migration.NewRunner over migrations/mysql/, which is what a
    #    worker does at boot and what engine/testutil does in tests.
    #      CREATE DATABASE cleat_src CHARACTER SET utf8mb4;
    #      (then let the runner apply all 70 files)
    # 2. Dump its resolved state. The flags are part of the procedure:
    #      --no-data       YES. A catalog dump carries no rows, so 002 cannot be
    #                      generated from it -- see the refusal at the bottom.
    #      --skip-comments YES. mysqldump's own header carries the server
    #                      version and the dump TIME, which would make the
    #                      generator's output nondeterministic.
    #      --no-tablespaces, --single-transaction  YES, both.
    mysqldump -uroot -p"$PW" --no-data --routines --triggers --skip-comments \
              --no-tablespaces --single-transaction cleat_src > /tmp/dump.sql
    # 3. Compact:
    python3 scripts/gen-mysql-baseline.py /tmp/dump.sql migrations/mysql

Step 3 is the cheap half. What decides whether the result is EQUIVALENT is the
differential: migration/catalogdiff compares a database built from the chain
against one built from the baseline. MySQL's acceptance is STRICTER than
PostgreSQL's -- there is no intended delta here, MySQL stays unpartitioned and
single-tenant, so the expected diff is EXACTLY EMPTY with no carve-out
(acceptance 2026-09-26, section 1).

Three things this generator rewrites, each of which it ASSERTS it matched. A
rewrite that silently matches nothing emits a baseline that applies cleanly and
is quietly wrong, which is the failure mode this whole file exists to avoid:

  1. DEFINER. The chain contains NO `DEFINER=` anywhere -- measured, 0 across
     all 70 files, comment-stripped. mysqldump nonetheless EMITS one, because
     MySQL stores an implicit definer (the creating account) for every routine
     and every trigger. Measured on a real build: both objects came back as
     `root@%`. Left in, the baseline pins an account that need not exist on the
     target, and fails to apply there.
     It appears in TWO syntactic forms, which is why one check is not enough:
         CREATE DEFINER=`root`@`%` PROCEDURE ...
         /*!50003 CREATE*/ /*!50017 DEFINER=`root`@`%`*/ /*!50003 TRIGGER ...
     Both are stripped, and the output is asserted to contain neither.

  2. The builder's session settings. mysqldump bakes its own `sql_mode`,
     `collation_connection`, `character_set_client` and `TIME_ZONE` into the
     file. None is cleat's intent; all are properties of whichever container
     ran the dump. Acceptance section 4.1 asks whether a baseline depends on a
     session setting the builder happens to supply -- for a dump-shaped
     generator the answer is yes, and these are where.

  3. Idempotence shape. mysqldump emits `DROP TABLE IF EXISTS t; CREATE TABLE t`,
     which is destructive-then-create. The chain is `CREATE TABLE IF NOT
     EXISTS`, which is a no-op on an existing database. The baseline keeps the
     chain's shape, because re-appliability is asserted separately
     (acceptance 1.6) and a DROP/CREATE baseline cannot satisfy it on a
     non-empty database.

What it does NOT rewrite: the explicit `COLLATE=utf8mb4_0900_ai_ci` that
mysqldump puts on EVERY table. That clause is GENERATOR-INTRODUCED, in the same
family as the `DEFINER` above -- measured, comment-stripped:

	(against origin/develop, which is the chain)
	COLLATE in migrations/mysql/*.sql                 1   (one file: 103)
	COLLATE in a raw dump / in this generated 001    29   (29 tables, all of them)

So the chain names a collation in one place and the baseline names one
everywhere. It is kept because it reproduces what the LIVE database reports and
because it is utf8mb4's default on MySQL 8 -- not because a check would catch
its absence.

**AND IT IS UNVERIFIED, WHICH THE FIRST VERSION OF THIS PARAGRAPH GOT WRONG.**
It said a baseline without the collation "would differ from the chain and the
differential would name it". **The differential cannot name it.** catalogdiff has
zero references to collation on any dialect, and its MySQL columns query selects
`column_name, column_type, is_nullable, column_default` -- no `COLLATION_NAME`,
and `COLUMN_TYPE` does not embed it, so the property is not in the comparison at
all. A decision justified by a check that cannot see it reads as verified and is
not, which is worse than no justification.

	grep -rn -i collat migration/catalogdiff/     # no output, measured before cleat#3121
	git show origin/develop:migrations/mysql/*.sql | grep -ci collate   # 1

**Note the measurement trap this paragraph walked into first**, because it is
the one this project keeps meeting: run the grep over `migrations/mysql/*.sql`
in a checkout that has already been compacted and it counts the GENERATED file,
not the chain -- it reported 30, of which 29 were this generator's own output.
Measure the chain against `origin/develop`, and print what each side is.

**SUPERSEDED 2026-10-08 by cleat#3121** (cleat#2882), which added exactly the
comparison this paragraph said was missing: catalogdiff's column snapshot now
carries `Collation` on every dialect, and `Diff` reports a change in it --
`migration/catalogdiff/a_collation_change_is_reported_test.go` is the
known-positive. The grep above is history, not a live claim: re-run it today
and it returns matches, where it returned none when this paragraph was
written. This baseline's collation choice IS now checked by the differential;
everything above this line records the investigation that found it was not,
before that fix landed.
"""

import os
import re
import sys

# What this generator can say about 002 without overwriting it. See the refusal
# next to where it is written -- inherited from the PostgreSQL generator, where
# an unconditional write replaced the real 002 on 2026-09-26 and the catalog
# diff reported EMPTY, because a missing ROW is invisible to a structural diff.
PLACEHOLDER_002 = (
    "-- cleat MySQL default data (002)\n"
    "-- HAND-ASSEMBLED: a catalog dump carries no rows. cleat#2433.\n"
)

# mysqldump's session preamble and footer. Removed wholesale: every statement
# in them sets a session variable the builder happened to have, and the
# `@OLD_*` restores at the end have nothing to restore in a fresh apply.
# mysqldump writes the terminator both as `*/;` and as `*/ ;` -- the per-routine
# block uses the spaced form, so a pattern that insists on `*/;` silently leaves
# every one of those in place.
SET_STMT = re.compile(r"^[ \t]*/\*!\d+\s+SET\b.*?\*/[ \t]*;?[ \t]*$", re.M | re.S)
SET_STMT_INLINE = re.compile(r"\s*/\*!\d+\s+SET\b.*?\*/;", re.S)

# `DROP TABLE IF EXISTS \`t\`;` immediately before its CREATE -- replaced by the
# chain's IF NOT EXISTS form.
DROP_TABLE = re.compile(r"DROP\s+TABLE\s+IF\s+EXISTS\s+`([^`]+)`\s*;\s*", re.I)

# The two syntactic forms of a dump-introduced definer.
DEFINER_PLAIN = re.compile(r"\bDEFINER\s*=\s*`[^`]*`\s*@\s*`[^`]*`\s*", re.I)
DEFINER_COND = re.compile(r"/\*!\d+\s+DEFINER\s*=\s*`[^`]*`\s*@\s*`[^`]*`\s*\*/", re.I)

DELIMITER = "DELIMITER ;;"

# Tables that belong to the RUNNER, not to cleat's schema.
#
# The dump is of a database the migration runner built, so it contains the
# runner's own bookkeeping. `schema_migrations` is created by
# migration.Runner.ensureMigrationsTable and by NO migration in the chain
# (verified: zero references across all 70 files). Emitting it into the
# baseline is not merely untidy -- engine/check-entity-contract.py reads the
# shipped files to enumerate every table cleat defines, and reported it as
# "defined in mysql and classified nowhere". A baseline that declares the
# runner's own state table is claiming a fact about cleat's schema that is not
# one.
EXCLUDED_TABLES = {"schema_migrations"}

# mysqldump quotes every identifier; the chain quotes none (0 backticked
# CREATE TABLE across all 70 files, against 30 in a raw dump). The difference
# is not cosmetic: engine/a_test_that_drops_a_migrated_object_restores_it_test.go
# extracts object names with
#     CREATE ... (?:IF NOT EXISTS )?([A-Za-z_][A-Za-z0-9_.]*)
# which cannot match a backtick. Against ``CREATE TABLE IF NOT EXISTS `x` `` it
# backtracks the optional group away and captures IF as the table name -- so the
# guard reported a table called `IF` that a test "drops". Restoring the chain's
# unquoted form fixes that at the source, rather than by loosening the guard.
QUOTED_IDENT = re.compile(r"`([^`]+)`")

# Which kind of object, and its name -- needed to emit the DROP that makes the
# file re-appliable. Read AFTER unquoting, so the name is bare.
OBJ_KIND_NAME = re.compile(
    r"\bCREATE\s+(PROCEDURE|FUNCTION|TRIGGER|EVENT)\s+([A-Za-z_][A-Za-z0-9_]*)",
    re.I)

# Kept quoted if the bare form would be a MySQL reserved word. Deliberately
# short and NOT hand-trusted: the generator prints any identifier it leaves
# quoted, and the build is the arbiter -- an unquoted reserved word fails at
# apply time rather than silently.
RESERVED_WORDS = {
    "ADD", "ALL", "ALTER", "ANALYZE", "AND", "AS", "ASC", "BEFORE", "BETWEEN",
    "BOTH", "BY", "CALL", "CASCADE", "CASE", "CHANGE", "CHARACTER", "CHECK",
    "COLLATE", "COLUMN", "CONDITION", "CONSTRAINT", "CONTINUE", "CONVERT",
    "CREATE", "CROSS", "CURRENT_DATE", "CURRENT_TIME", "CURRENT_TIMESTAMP",
    "CURRENT_USER", "CURSOR", "DATABASE", "DATABASES", "DEC", "DECIMAL",
    "DECLARE", "DEFAULT", "DELAYED", "DELETE", "DESC", "DESCRIBE",
    "DETERMINISTIC", "DISTINCT", "DISTINCTROW", "DIV", "DOUBLE", "DROP",
    "DUAL", "EACH", "ELSE", "ELSEIF", "ENCLOSED", "ESCAPED", "EXISTS", "EXIT",
    "EXPLAIN", "FALSE", "FETCH", "FLOAT", "FOR", "FORCE", "FOREIGN", "FROM",
    "FULLTEXT", "GENERATED", "GET", "GRANT", "GROUP", "GROUPS", "HAVING",
    "HIGH_PRIORITY", "IF", "IGNORE", "IN", "INDEX", "INFILE", "INNER", "INOUT",
    "INSENSITIVE", "INSERT", "INTERVAL", "INTO", "IS", "ITERATE", "JOIN",
    "KEY", "KEYS", "KILL", "LAG", "LAST_VALUE", "LATERAL", "LEAD", "LEADING",
    "LEAVE", "LEFT", "LIKE", "LIMIT", "LINEAR", "LINES", "LOAD", "LOCALTIME",
    "LOCALTIMESTAMP", "LOCK", "LONG", "LOOP", "LOW_PRIORITY", "MATCH",
    "MAXVALUE", "MODIFIES", "NATURAL", "NOT", "NO_WRITE_TO_BINLOG", "NTH_VALUE",
    "NTILE", "NULL", "NUMERIC", "OF", "ON", "OPTIMIZE", "OPTION",
    "OPTIONALLY", "OR", "ORDER", "OUT", "OUTER", "OUTFILE", "OVER",
    "PARTITION", "PERCENT_RANK", "PRECISION", "PRIMARY", "PROCEDURE", "PURGE",
    "RANGE", "RANK", "READ", "READS", "REAL", "RECURSIVE", "REFERENCES",
    "REGEXP", "RELEASE", "RENAME", "REPEAT", "REPLACE", "REQUIRE",
    "RESTRICT", "RETURN", "REVOKE", "RIGHT", "RLIKE", "ROW", "ROWS",
    "ROW_NUMBER", "SCHEMA", "SCHEMAS", "SELECT", "SENSITIVE", "SEPARATOR",
    "SET", "SHOW", "SMALLINT", "SPATIAL", "SPECIFIC", "SQL", "SQLEXCEPTION",
    "SQLSTATE", "SQLWARNING", "SQL_BIG_RESULT", "SQL_CALC_FOUND_ROWS",
    "SQL_SMALL_RESULT", "SSL", "STARTING", "STORED", "STRAIGHT_JOIN", "SYSTEM",
    "TABLE", "TERMINATED", "THEN", "TO", "TRAILING", "TRIGGER", "TRUE",
    "UNDO", "UNION", "UNIQUE", "UNLOCK", "UNSIGNED", "UPDATE", "USAGE", "USE",
    "USING", "UTC_DATE", "UTC_TIME", "UTC_TIMESTAMP", "VALUES", "VARBINARY",
    "VARCHAR", "VARCHARACTER", "VARYING", "VIRTUAL", "WHEN", "WHERE", "WHILE",
    "WINDOW", "WITH", "WRITE", "XOR", "ZEROFILL",
}


def unquote_identifiers(text):
    """Drop mysqldump's identifier quoting where the chain would not have it.

    Returns (text, unquoted_count, still_quoted_names). The third is reported
    rather than swallowed: a name left quoted is one the caller should look at,
    not a success.
    """
    unquoted = 0
    kept = []

    def repl(m):
        nonlocal unquoted
        name = m.group(1)
        if name.upper() in RESERVED_WORDS:
            kept.append(name)
            return m.group(0)
        unquoted += 1
        return name

    return QUOTED_IDENT.sub(repl, text), unquoted, sorted(set(kept))

# MySQL's version-gated comment: /*!50003 sql */ means "run `sql` on 5.0.3+".
# mysqldump wraps object DDL in it.
VERSION_COMMENT = re.compile(r"/\*!\d*\s*(.*?)\*/", re.S)


def unwrap_version_comments(text):
    """Turn `/*!NNNNN sql */` into plain `sql`. Returns (text, count).

    THIS IS LOAD-BEARING AND ITS ABSENCE IS INVISIBLE. migration/runner.go's
    splitSQL treats ANY `/* ... */` as a comment and replaces it with a single
    space -- it has no special case for the `/*!` form. mysqldump writes the
    trigger as

        /*!50003 CREATE*/ /*!50003 TRIGGER `t` BEFORE UPDATE … END */;;

    so the splitter reduces the entire statement to whitespace, `flush()`
    discards it as a fragment that is "only comments and whitespace", and the
    baseline applies CLEANLY with no trigger at all.

    Nothing reported it until cleat#2882 taught catalogdiff to read MySQL
    triggers too (originally found: acceptance section 0) -- before that, the
    differential was empty, the file applied, and the count of statements was
    nobody's assertion. The chain never uses this form -- it
    writes `CREATE TRIGGER … END//` with its own DELIMITER -- so the version
    comment is purely a dump artifact, and unwrapping restores the chain's
    shape.
    """
    n = 0

    def repl(m):
        nonlocal n
        n += 1
        return " " + m.group(1) + " "

    return VERSION_COMMENT.sub(repl, text), n


def die(msg):
    raise SystemExit(msg)


def strip_session(text):
    """Drop the builder's session settings, as whole statements only.

    Statement-anchored on purpose. An inline sub would also delete the
    `/*!50003 SET ... */` lines that sit *inside* a routine block, which is
    where mysqldump parks the per-routine sql_mode -- and deleting those is
    desired -- but it would equally eat a conditional comment that is part of
    an object's own text. Counted and reported either way.
    """
    n_before = len(SET_STMT.findall(text))
    out = SET_STMT.sub("", text)
    return out, n_before


def split_objects(text):
    """Split a dump into (head, [object blocks]).

    Splitting on mysqldump's own `DELIMITER ;;` is reliable where a semicolon
    split is not: routine and trigger bodies contain semicolons, and the whole
    reason mysqldump emits a DELIMITER directive is to say so. The head holds
    the preamble and the CREATE TABLEs; each later chunk holds exactly one
    routine or trigger, terminated by `DELIMITER ;`.
    """
    if DELIMITER not in text:
        die("no 'DELIMITER ;;' in the dump: this does not look like a "
            "mysqldump --routines --triggers output")
    segments = text.split(DELIMITER)
    head_parts = [segments[0]]
    objects = []
    for chunk in segments[1:]:
        body, sep, after = chunk.partition("DELIMITER ;")
        if not sep:
            die("unterminated object block: a 'DELIMITER ;;' with no "
                "matching 'DELIMITER ;'")
        objects.append(body.strip())
        # NOT discarded. mysqldump emits a table's triggers IMMEDIATELY AFTER
        # that table, so the file is tables, an object, more tables, another
        # object -- this generator's first version dropped `after`, and lost
        # 11 of 30 tables. The count assertion below is what caught it.
        head_parts.append(after)
    return "\n".join(head_parts), objects


def strip_definers(text):
    """Remove both forms of the dump-introduced definer. Returns (text, count)."""
    text, n1 = DEFINER_COND.subn("", text)
    text, n2 = DEFINER_PLAIN.subn("", text)
    return text, n1 + n2


def split_tables(head):
    """Return the list of CREATE TABLE statements in a dump's head section."""
    # The head is preamble + `DROP TABLE IF EXISTS t;` + `CREATE TABLE t (...);`
    # Repeatedly, in mysqldump's alphabetical order.
    stmts = []
    for m in re.finditer(r"CREATE\s+TABLE\s+`([^`]+)`", head, re.I):
        start = m.start()
        end = head.find("\n) ENGINE", start)
        if end == -1:
            die("no '\\n) ENGINE' after CREATE TABLE `%s` -- the dump's table "
                "form is not what this generator models" % m.group(1))
        end = head.find(";", end)
        if end == -1:
            die("unterminated CREATE TABLE `%s`" % m.group(1))
        stmts.append((m.group(1), head[start:end + 1]))
    return stmts


def main(src, outdir):
    text = open(src).read()

    if "DEFINER" not in text:
        # Not a refusal -- a dump of a chain with no routines/triggers is a
        # legitimate input. But it must be SAID, because a generator whose
        # strip silently ran on nothing is indistinguishable from one whose
        # strip failed, and the assertion below would then pass vacuously.
        print("note: the dump contains no DEFINER token at all, so the strip "
              "below is vacuous on this input")

    head, objects = split_objects(text)

    # --- objects: triggers and routines -------------------------------------
    # Order is forced: session settings out FIRST (so they are deleted rather
    # than unwrapped into live `SET` statements), then the version comments
    # unwrapped, then the definer -- which mysqldump puts INSIDE a version
    # comment (`/*!50017 DEFINER=...*/`) and which only becomes matchable in
    # its plain form once the comment is unwrapped.
    proc_like, trig_like, other = [], [], []
    definers_stripped = 0
    unwrapped = 0
    unquoted_total = 0
    still_quoted = set()
    emitted_drops = []
    for obj in objects:
        obj, _ = strip_session(obj)
        obj, nu = unwrap_version_comments(obj)
        unwrapped += nu
        obj, n = strip_definers(obj)
        definers_stripped += n
        # The conditional-comment form hides the verb: mysqldump writes
        # `/*!50003 CREATE*/ /*!50017 DEFINER=…*/ /*!50003 TRIGGER …`, so the
        # verb and the noun are separated by a comment terminator and a naive
        # `CREATE TRIGGER` search finds nothing.
        flat = re.sub(r"/\*!\d+", "", obj)
        flat = flat.replace("*/", " ")
        if re.search(r"\bTRIGGER\b", flat, re.I):
            trig_like.append(obj)
        elif re.search(r"\b(PROCEDURE|FUNCTION|EVENT)\b", flat, re.I):
            proc_like.append(obj)
        else:
            other.append(obj)

    if other:
        die("could not classify %d object block(s); first 200 chars:\n%s"
            % (len(other), other[0][:200]))

    # RE-APPLIABLE, which is not the same property as "the runner will not
    # re-apply it". The chain drops each object before recreating it -- 12
    # `DROP <kind> IF EXISTS` statements across the shipped files, and the old
    # 003 opened with one -- and a baseline that omits them fails 1304,
    # "PROCEDURE finalize_workflow_status already exists", for any caller that
    # applies the FILE rather than going through migration.Runner.
    #
    # That distinction cost a CI cycle, and the reason is worth keeping: the
    # generator's own idempotence check applied the baseline through the runner,
    # where `schema_migrations` suppresses the second apply -- so it was
    # measuring the runner's bookkeeping, not the SQL's re-appliability. A test
    # that goes through the same indirection as the thing it is testing cannot
    # see this.
    procs_out = []
    for o in proc_like + trig_like:
        o, n, kept = unquote_identifiers(o)
        unquoted_total += n
        still_quoted.update(kept)
        m = OBJ_KIND_NAME.search(o)
        if not m:
            die("no 'CREATE <kind> <name>' in a routine/trigger body, so the "
                "DROP that makes the file re-appliable cannot be emitted:\n%s"
                % o[:200])
        kind, name = m.group(1).upper(), m.group(2)
        if kind == "TRIGGER":
            # MySQL has no DROP TRIGGER ... IF EXISTS before 8.0.19; the
            # shipped chain targets 8.0+, and engine tests apply this file
            # directly, so keep the guard but match the chain's spelling.
            drop = "DROP TRIGGER IF EXISTS %s;" % name
        else:
            drop = "DROP %s IF EXISTS %s;" % (kind, name)
        emitted_drops.append("%s %s" % (kind, name))
        procs_out.append(drop + "\n\n" + DELIMITER + "\n" + o + "\nDELIMITER ;")
    if len(emitted_drops) != len(proc_like) + len(trig_like):
        die("emitted %d DROP(s) for %d object(s)"
            % (len(emitted_drops), len(proc_like) + len(trig_like)))
    procedures = "\n\n".join(procs_out)

    # --- tables -------------------------------------------------------------
    head, session_stmts = strip_session(head)
    head, head_unwrapped = unwrap_version_comments(head)
    unwrapped += head_unwrapped
    tables = split_tables(head)
    if not tables:
        die("no CREATE TABLE found in the dump's head section")

    excluded = [n for n, _ in tables if n.lower() in EXCLUDED_TABLES]
    if len(excluded) != len(EXCLUDED_TABLES):
        die("expected to exclude %d runner-owned table(s) %s from the dump, "
            "found %d -- if the runner stopped creating one, say so here "
            "rather than silently emitting it"
            % (len(EXCLUDED_TABLES), sorted(EXCLUDED_TABLES), len(excluded)))
    tables = [(n, s) for n, s in tables if n.lower() not in EXCLUDED_TABLES]

    table_names = [n for n, _ in tables]
    if len(set(table_names)) != len(table_names):
        dupes = sorted({n for n in table_names if table_names.count(n) > 1})
        die("duplicate CREATE TABLE for: %s" % ", ".join(dupes))

    # Idempotence shape: the chain's IF NOT EXISTS, not the dump's DROP+CREATE.
    rendered = [
        re.sub(r"^CREATE\s+TABLE\s+", "CREATE TABLE IF NOT EXISTS ", stmt,
               flags=re.I)
        for _, stmt in tables
    ]
    for i, stmt in enumerate(rendered):
        rendered[i], n, kept = unquote_identifiers(stmt)
        unquoted_total += n
        still_quoted.update(kept)

    # ORDER-SAFE BY CONSTRUCTION, and this is the acceptance's preferred form
    # (section 1.7) rather than a workaround.
    #
    # mysqldump emits tables ALPHABETICALLY. The chain creates them in
    # DEPENDENCY order. So a dump's first table is `concurrency_keys`, which
    # carries a foreign key to `workflow_instances` -- created far later -- and
    # applying the baseline straight through fails 1824, "Failed to open the
    # referenced table". Measured, not anticipated.
    #
    # The alternative is a topological sort of the tables, which is more code
    # and fails on a cycle rather than tolerating one. This is the standard
    # answer: with `foreign_key_checks = 0` MySQL creates a foreign key without
    # requiring its target to exist yet, so the baseline's statement order
    # stops mattering. mysqldump relies on exactly this.
    #
    # IT IS WRITTEN OUT RATHER THAN INHERITED. mysqldump's own
    # `/*!40014 SET FOREIGN_KEY_CHECKS=0 */;` preamble is what strip_session
    # removes, and the first version of this generator removed it -- turning a
    # working dump into a baseline that could not apply. A session setting the
    # builder supplied is a hazard; this one is the file's own requirement, and
    # the difference is that this one is written here deliberately.
    schema = (
        "-- cleat MySQL consolidated schema (001)\n"
        "-- GENERATED by scripts/gen-mysql-baseline.py from a mysqldump of a\n"
        "-- database built from the numbered chain. Do not hand-edit: the next\n"
        "-- regeneration silently reverts it. Add a new numbered migration.\n"
        "--\n"
        "-- Tables carry their indexes inline, which is how a dump resolves 39\n"
        "-- CREATE INDEX and 111 ALTER TABLE statements in the chain.\n"
        "--\n"
        "-- CREATE TABLE IF NOT EXISTS guards idempotency, matching the chain's\n"
        "-- shape. A re-run is a no-op.\n"
        "--\n"
        "-- FOREIGN_KEY_CHECKS is off for the body and restored after it: a dump\n"
        "-- orders tables alphabetically, the chain orders them by dependency,\n"
        "-- and this makes the baseline correct in either order.\n\n"
        "SET FOREIGN_KEY_CHECKS = 0;\n\n"
        + "\n\n".join(rendered) +
        "\n\nSET FOREIGN_KEY_CHECKS = 1;\n")

    # --- assertions ---------------------------------------------------------
    # Each of these can fail, and each reports the number it decided on. A
    # clean result here is evidence only because the same checks are known to
    # fire -- see the known-positives in cleat#2433.
    out_all = schema + "\n" + procedures
    remaining = DEFINER_PLAIN.findall(out_all) + DEFINER_COND.findall(out_all)
    if remaining:
        die("DEFINER survived the strip (%d remaining); the baseline would pin "
            "the builder's account" % len(remaining))

    # A surviving version comment is a statement the runner will discard. This
    # is the check that would have caught the trigger, had it existed before
    # the unwrap -- see unwrap_version_comments for why its absence is silent.
    leftover_version = re.findall(r"/\*!\d", out_all)
    if leftover_version:
        die("%d version comment(s) survived; the runner's splitSQL discards "
            "/* ... */ wholesale, so these statements would vanish silently"
            % len(leftover_version))

    # ANCHORED TO LINE START, on purpose. The first version of this counted the
    # phrase anywhere in the output, and the generated file's own header
    # comment contains the sentence "CREATE TABLE IF NOT EXISTS guards
    # idempotency" -- so it reported 31 for 30 tables and refused to write.
    # A text search cannot tell a statement from prose ABOUT the statement, and
    # the fix is to ask the question at the position a statement occupies.
    n_create = len(re.findall(r"^CREATE TABLE IF NOT EXISTS", schema, re.M))
    if n_create != len(tables):
        die("emitted %d CREATE TABLE IF NOT EXISTS for %d tables"
            % (n_create, len(tables)))

    if session_stmts == 0 and "SET" in head:
        die("the session-setting strip matched nothing, but the head still "
            "mentions SET -- the pattern no longer models the dump")

    os.makedirs(outdir, exist_ok=True)
    open(f"{outdir}/001_schema.sql", "w").write(schema)
    open(f"{outdir}/003_procedures.sql", "w").write(
        "-- cleat MySQL stored procedures (003)\n"
        "-- GENERATED by scripts/gen-mysql-baseline.py. Do not hand-edit.\n"
        "--\n"
        "-- A DEFINER clause is stripped here and the absence is asserted:\n"
        "-- the chain declares none, and mysqldump adds one naming whichever\n"
        "-- account ran the dump. See the generator's docstring.\n\n"
        + procedures + "\n")

    # BUT NEVER OVER AN EXISTING FILE. Inherited from the PostgreSQL generator,
    # where this write was unconditional and replaced the real 002 -- the only
    # carrier of the default rows -- on 2026-09-26. The A/B diff reported EMPTY
    # (a missing row is invisible to a structural diff) and ~70 tests then
    # failed on foreign keys to rows that no longer existed.
    defaults = f"{outdir}/002_defaults.sql"
    if os.path.exists(defaults):
        with open(defaults) as fh:
            existing = fh.read()
        if existing.strip() != PLACEHOLDER_002.strip():
            raise SystemExit(
                "refusing to overwrite %s: it already carries content this "
                "generator cannot produce (a catalog dump carries no rows).\n"
                "Generate into an empty directory, or move that file aside "
                "deliberately -- do not let this overwrite the seed." % defaults)
    open(defaults, "w").write(PLACEHOLDER_002)

    print("tables:            %d" % len(tables))
    print("routines:          %d" % len(proc_like))
    print("triggers:          %d" % len(trig_like))
    print("DEFINER stripped:  %d" % definers_stripped)
    print("re-appliable DROPs emitted: %d" % len(emitted_drops))
    print("identifiers unquoted: %d" % unquoted_total)
    if still_quoted:
        print("  LEFT QUOTED (reserved word, check by building): %s" % ", ".join(still_quoted))
    print("version comments unwrapped: %d" % unwrapped)
    print("session stmts cut: %d" % session_stmts)
    print("wrote %s/001_schema.sql, 002_defaults.sql, 003_procedures.sql"
          % outdir)


if __name__ == "__main__":
    if len(sys.argv) != 3:
        raise SystemExit(__doc__)
    main(sys.argv[1], sys.argv[2])
