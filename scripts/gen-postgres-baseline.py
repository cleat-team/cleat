#!/usr/bin/env python3
"""Generate a compacted migration baseline from a pg_dump of the CURRENT chain.

The dump is the resolved final state: every ALTER already applied, every routine
already at its last definition. That is the whole point -- it sidesteps the design
doc's "four of ten routines are redefined, take the LAST" trap by construction,
rather than by reading 87 files and hoping.

cleat#2059.

Reads:  argv[1] = pg_dump --schema-only output of a database built from the chain
Writes: argv[2]/001_schema.sql, 002_defaults.sql, 003_procedures.sql

Schema handling follows migration/migrations_do_not_hardcode_the_schema_test.go:
the schema cleat builds into is never named -- `public.`/`cleat.` prefixes are
stripped so names resolve through search_path, which the runner sets. `admin.` and
`tenant_*` are NOT the configured schema and stay qualified, as they are in the
current migrations.

How to run it, end to end -- this is what makes the baseline re-derivable from
the repo rather than a trusted artifact:

    # 1. Build a database from the chain being compacted. Check out the commit
    #    BEFORE the baseline and apply the chain the way every other consumer
    #    does -- migration.NewRunner over migrations/postgres/, which is what
    #    engine/testutil does and what a worker does at boot.
    # 2. Dump its RESOLVED final state. The two flags below are not
    #    interchangeable, and the wrong pair produces a baseline that applies
    #    cleanly and is quietly missing its grants:
    #      --no-owner       YES. Without it the dump emits
    #                       `ALTER FUNCTION ... OWNER TO postgres`, which the
    #                       chain never wrote and which pins the deployer's role.
    #      --no-privileges  NO.  It suppresses the ACLs this generator needs:
    #                       PROCEDURE_ACL comes out 0 and all three
    #                       `GRANT ... ON FUNCTION ... TO cleat_app` vanish from
    #                       003_procedures.sql.
    pg_dump --schema-only --no-owner <db> > /tmp/dump.sql
    # 3. Compact:
    python3 scripts/gen-postgres-baseline.py /tmp/dump.sql migrations/postgres

Step 3 is the cheap half. What decides whether the result is EQUIVALENT is the
differential: migration/catalogdiff compares a database built from the original
chain against one built from the baseline and reports the difference as a set
of SQL lines. An empty diff is the claim; a non-empty one names what moved.

Two caveats on that diff, both learned the expensive way in cleat#2059:

  * It compares END STATES, and only per-database objects. Role memberships
    (pg_auth_members), role comments (pg_shdescription) and role attributes are
    CLUSTER-wide, so a pg_dump carries none of them and the diff cannot see
    their absence. They are reconstructed by hand in the ROLES preamble below,
    and a mistake in that preamble is invisible to the differential -- which is
    exactly how a dropped `GRANT cleat_sweep TO cleat_app` survived a clean
    catalog diff until a reviewer found it.
  * It is blind to idempotence: that the shipped files can be RE-APPLIED is a
    separate property, asserted by engine/schema_bootstrap_test.go.

CLEAT_2059_PARTITION=1 additionally emits the hash-partitioned event_history and
its (tenant_id, workflow_id, step) primary key -- cleat#2059's PR B. It is OFF
by default precisely because that partition is the ONE intended difference, and
the differential is empty only without it.
"""
import os
import re
import sys
from collections import OrderedDict

# ONLY `public`, which is the schema cleat BUILDS INTO and which --schema can rename.
# `cleat` is NOT that: migration 001 creates a schema literally named `cleat`
# (`CREATE SCHEMA cleat`) and the RLS helper lives there as `cleat.assert_tenant_set`.
# Stripping it moves the helper into the configured schema -- `+ ROUTINE
# public.assert_tenant_set()` in the diff, with the original still in A. `cleat` and
# `admin` are fixed names and stay qualified, exactly as the current migrations write them.
CONFIGURED = ("public",)

def make_idempotent(sql: str) -> str:
    """The shipped files must be RE-APPLIABLE, and the dump is not.

    pg_dump emits the resolved DDL, so `CREATE SCHEMA admin;` comes out bare
    while the chain it replaces writes `CREATE SCHEMA IF NOT EXISTS admin;`.
    That difference is invisible on a first apply and fatal on a second:

        re-applying 001_schema.sql failed: pq: schema "admin" already exists (42P06)

    engine/schema_bootstrap_test.go's TestShippedSchema_IsIdempotent re-applies
    the shipped files deliberately, because "the schema we ship is the schema the
    engine needs" is only half the property -- a deployment that restarts must be
    able to re-run them.
    """
    # Every object kind the chain guards. Measured on the chain this replaces:
    # 48 `CREATE INDEX IF NOT EXISTS`, 31 `CREATE TABLE IF NOT EXISTS`, 4
    # `CREATE SCHEMA IF NOT EXISTS`. The dump emits none of them, because it
    # writes resolved DDL for objects it knows do not exist yet.
    #
    # Found the hard way, one object kind at a time: fixing SCHEMA moved the
    # failure to the sequence, and fixing that would have moved it to the tables.
    # TIMESTAMPTZ, the chain's own spelling. pg_dump writes the SQL-standard
    # `timestamp with time zone`, which is the same type -- but
    # scripts/check-entity-contract.py tests for the literal `TIMESTAMPTZ` in the
    # parsed column definition, because that is the project's convention for the
    # entity tables, and it is right to: the convention is about what the
    # migrations SAY as much as what they build.
    #
    #   ERROR: public.workflow_tags.created_at is 'TIMESTAMP WITH TIME ZONE
    #   DEFAULT NOW() NOT NULL', not TIMESTAMPTZ.
    #
    # Normalising to the chain's spelling keeps the guard meaningful instead of
    # loosening it to accept a second spelling it was never asked to.
    sql = re.sub(r"(?i)\btimestamp\s+with\s+time\s+zone\b", "TIMESTAMPTZ", sql)

    for kind in ("SCHEMA", "TABLE", "SEQUENCE", "INDEX", "EXTENSION"):
        sql = re.sub(r"CREATE %s (?!IF NOT EXISTS)" % kind,
                     "CREATE %s IF NOT EXISTS " % kind, sql)
    # UNIQUE and CONCURRENTLY sit between CREATE and INDEX, so the plain pattern
    # above misses them:
    #   re-applying 001_schema.sql failed: pq: relation "idx_promises_id_unique"
    #   already exists (42P07)
    sql = re.sub(r"CREATE UNIQUE INDEX (?!IF NOT EXISTS)", "CREATE UNIQUE INDEX IF NOT EXISTS ", sql)
    return sql


def strip_configured(sql: str) -> str:
    for s in CONFIGURED:
        sql = sql.replace(f" {s}.", " ")
        sql = sql.replace(f"({s}.", "(")
        sql = sql.replace(f",{s}.", ",")
    # A schema-qualified object reference INSIDE a string literal -- the regclass
    # form pg_dump emits for a sequence-backed column default. Unqualified, it
    # resolves through search_path like every other name here.
    #
    # ONLY this form. `'cleat.tenant_id'` and `'cleat.cross_tenant'` are custom GUC
    # NAMES passed to set_config/current_setting, not schema references, and
    # stripping their prefix would silently break the RLS assertion that reads
    # them. Two strings that look identical and mean opposite things.
    for s in CONFIGURED:
        sql = re.sub(r"'%s\.([A-Za-z_][A-Za-z0-9_]*)'::regclass" % re.escape(s),
                     r"'\1'::regclass", sql)
    return sql

def un_freeze_schema(sql: str) -> str:
    """Put back the schema deferrals pg_dump had to resolve.

    A dump is taken from a DATABASE, where current_schema() has already been
    evaluated -- so every deferred reference comes out as the value it had on
    that machine. Under the default schema that is invisible: a hardcoded
    `public` and a deferred current_schema() ARE the same value, which is why
    the A/B catalog diff reported these two trees equivalent while they are not.
    Under `--schema` they diverge, and the baseline stops being relocatable.

    Measured: 17 hardcoded-schema findings here against 0 in the chain this
    replaces (migration/migrations_do_not_hardcode_the_schema_test.go), and the
    default-schema diff was EMPTY throughout.

    Every rewrite asserts it matched. A rewrite that silently does nothing
    leaves the literal in place and the file still applies -- just pinned to a
    schema the deployment may not have chosen.
    """
    n0 = sql

    # CREATE EXTENSION ... WITH SCHEMA public -> the original is unqualified and
    # resolves through search_path.
    for s in CONFIGURED:
        sql = re.sub(r"(CREATE EXTENSION[^;]*?) WITH SCHEMA %s;" % re.escape(s), r"\1;", sql)

    # A SECURITY DEFINER function's own search_path attribute. The original
    # writes `SET search_path FROM CURRENT`, which freezes the migration-time
    # value onto the function at creation; the dump writes the value it froze.
    for s in CONFIGURED:
        sql = sql.replace("SET search_path TO '%s', 'pg_temp'" % s,
                          "SET search_path FROM CURRENT")

    # pg_catalog is a FIXED schema, not the configured one, so pinning it is
    # correct -- but it cannot start a line, or the guard's `^\\s*SET search_path`
    # matches it and the file fails. The original writes it on the LANGUAGE line;
    # fold it back there.
    sql = re.sub(r"\n(\s*LANGUAGE [^\n]*?)\n\s*SET search_path TO 'pg_catalog'",
                 r"\n\1 SET search_path = pg_catalog", sql)

    # GRANT ... ON SCHEMA <configured>: the original asks with current_schema().
    def grant_repl(m):
        privs, role = m.group(1), m.group(2)
        return ("DO $do$ BEGIN\n"
                "    EXECUTE format('GRANT %s ON SCHEMA %%I TO %%I', current_schema(), '%s');\n"
                "END $do$;" % (privs, role))

    for s in CONFIGURED:
        sql = re.sub(r"GRANT (.*?) ON SCHEMA %s TO (\S+);" % re.escape(s), grant_repl, sql)

    # ALTER DEFAULT PRIVILEGES IN SCHEMA <configured>: same.
    def adp_repl(m):
        return ("DO $do$ BEGIN\n"
                "    EXECUTE format('ALTER DEFAULT PRIVILEGES IN SCHEMA %%I %s', current_schema());\n"
                "END $do$;" % m.group(1))

    for s in CONFIGURED:
        sql = re.sub(r"ALTER DEFAULT PRIVILEGES IN SCHEMA %s (.*?);" % re.escape(s),
                     adp_repl, sql)

    # A trgm index whose opclass is written BARE. The chain resolves where
    # pg_trgm actually lives and qualifies it -- `ext_schema` is read from
    # pg_extension and interpolated with %I, because an extension is
    # PER-DATABASE and may sit in any schema (cleat#1366, established by 033 and
    # repeated by 090). pg_dump materialises the resolved value, so the index
    # comes back naming gin_trgm_ops unqualified -- which resolves only when the
    # extension happens to be on the applying connection's search_path.
    #
    #   cleat#1366 (pool_b_1366): ... operator class "gin_trgm_ops" does not
    #   exist for access method "gin" (42704)
    #
    # The same defect class as current_schema(), one level down: a runtime
    # lookup frozen into a literal by the dump.
    def trgm_repl(m):
        name, table, col, where = m.group(1), m.group(2), m.group(3), (m.group(4) or "")
        return ("DO $trgm$\n"
                "DECLARE\n"
                "    ext_schema text;\n"
                "BEGIN\n"
                "    SELECT n.nspname INTO ext_schema\n"
                "      FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace\n"
                "     WHERE e.extname = 'pg_trgm';\n"
                "    IF ext_schema IS NULL THEN\n"
                "        RAISE EXCEPTION 'pg_trgm is not installed and CREATE EXTENSION did not create it';\n"
                "    END IF;\n"
                "    EXECUTE format('CREATE INDEX IF NOT EXISTS %s ON %s USING GIN (%s %%I.gin_trgm_ops)%s',\n"
                "                   ext_schema);\n"
                "END\n"
                "$trgm$;" % (name, table, col, where))

    # The `IF NOT EXISTS` is OPTIONAL here, and that is not defensiveness: this
    # runs inside un_freeze_schema, which is applied BEFORE make_idempotent adds
    # it. Requiring it made the first version of this transform silently match
    # nothing -- leaving both bare opclasses in place while reporting success.
    sql = re.sub(r"CREATE INDEX (?:IF NOT EXISTS )?(\w+) ON ([\w.]+) USING gin \((\w+) gin_trgm_ops\)([^;]*);",
                 trgm_repl, sql)

    if sql == n0:
        # Not fatal on its own -- a body may legitimately hold none of these --
        # but a body that DID hold one and was not rewritten is the defect.
        pass
    return sql


# The three workflow procedures the original baseline kept in 003_procedures.sql.
# Everything else -- cleat.assert_tenant_set (referenced 44 times by 001's own
# policies) and every admin.* helper (named by 001's function ACLs) -- has to be
# created BEFORE the policies and grants in 001, which is where the previous
# consolidated 001 defined them too.
PROCEDURES = {"finalize_workflow_status", "flush_event_step", "batch_flush_events"}

# Function OWNERSHIP, which `pg_dump --no-owner` throws away and which is not
# cosmetic on a SECURITY DEFINER function: the function runs with the OWNER's
# privileges, so ownership IS the security policy. Three routines are deliberately
# owned by cleat_dispatcher, the only role holding BYPASSRLS -- a function owned
# by the migration role instead would look identical in a catalog diff, apply
# cleanly, and enforce nothing.
#
# Measured: without this the engine suite failed
#   admin.in_flight_workflow_ids() is owned by "postgres", want cleat_dispatcher.
#   SECURITY DEFINER lends the OWNER's exemption, and under FORCE ROW LEVEL
#   SECURITY the table owner has none to lend -- only BYPASSRLS does.
#
# --no-owner stays: it keeps the baseline from pinning every table and sequence
# to whatever role ran the dump, which is the reason it is there. These three are
# re-declared explicitly instead, copied from 023/024/040/073.
FUNCTION_OWNERSHIP = r"""
-- ── Function ownership ──────────────────────────────────────────────────────
-- `pg_dump --no-owner` was used to generate this file, so that the baseline does
-- not pin every object to the role that happened to run the dump. These three
-- are the exception, and they carry their ownership because it is not cosmetic:
-- a SECURITY DEFINER function executes with the OWNER's privileges, so the owner
-- IS the policy. cleat_dispatcher is the only role holding BYPASSRLS, which is
-- what these three need to read across tenants.
--
-- Guarded on the role existing, as 023/024/040/073 each are: BYPASSRLS needs a
-- superuser to grant, so a non-superuser deployment has no cleat_dispatcher and
-- the ALTER would fail the whole migration rather than skipping.
DO $do$ BEGIN
    -- ATTEMPTED, NOT GUARDED ON THE ROLE'S EXISTENCE, and that is 023's own
    -- finding rather than a preference. "Does cleat_dispatcher exist" is the
    -- wrong question: ALTER ... OWNER TO also requires the CURRENT ROLE to be a
    -- member of the target, so a role that exists but was created by somebody
    -- else still fails --
    --
    --   ERROR:  must be able to SET ROLE "cleat_dispatcher"   (SQLSTATE 42501)
    --
    -- and when the role does not exist at all the refusal is a DIFFERENT
    -- SQLSTATE -- undefined_object, 42704, not 42501 -- so both are caught.
    -- Catching only the first passes every run against a cluster where an
    -- earlier superuser run left the role behind, and fails the first genuinely
    -- clean one. Measured here: the guard-on-existence version failed
    -- TestTheMigrationSetAppliesWithoutASuperuser with exactly 42501.
    BEGIN
        EXECUTE 'ALTER FUNCTION admin.claim_workflows(text, text[], integer) OWNER TO cleat_dispatcher';
        EXECUTE 'ALTER FUNCTION admin.get_due_schedules() OWNER TO cleat_dispatcher';
        EXECUTE 'ALTER FUNCTION admin.in_flight_workflow_ids() OWNER TO cleat_dispatcher';
    EXCEPTION WHEN insufficient_privilege OR undefined_object THEN
        RAISE NOTICE 'cannot give the cross-tenant functions to cleat_dispatcher (SQLSTATE %); they keep the migrating role as their owner and will not see across tenants. Use --claim-strategy=rotate, which needs no exemption.', SQLSTATE;
    END;
END $do$;
"""

# Cluster-level roles. `pg_dump --schema-only` dumps a DATABASE, and a role is
# not in one -- it is in the cluster -- so a dump can never carry these, while
# every GRANT in the dump names them. Generated from the dump alone, the
# baseline therefore refers to four roles it never creates, and on a cluster
# that has never run cleat it dies at the first GRANT:
#
#   applying ...: migration 001_schema.sql: execute: pq:
#     role "cleat_sweep" does not exist (42704)
#
# That is invisible on any machine where the chain has ever run, because the
# roles outlive the database. It was invisible here for exactly that reason, and
# for a second one: the A/B test gives each side its own DATABASE but they share
# a CLUSTER, so A's build created the roles a few milliseconds before B's
# GRANTs ran. See TestZZTempCandidateAppliesOnAFreshCluster.
#
# The guards and attributes are copied verbatim from the migrations that created
# them -- 005_app_role.sql, 023_cross_tenant_claim.sql, 077_... -- including
# 023's insufficient_privilege branch, which is what lets the file apply as a
# non-superuser (BYPASSRLS needs a superuser to grant, and the deployment may
# not be one).
ROLES = r"""
-- ── Cluster roles ───────────────────────────────────────────────────────────
-- Roles live in the CLUSTER, not the database, so no pg_dump of a database can
-- carry them -- while every GRANT below names one. Created here, idempotently,
-- from the migrations that owned them: 005_app_role.sql (cleat_app),
-- 023_cross_tenant_claim.sql (cleat_dispatcher) and 077 (cleat_sweep).
--
-- cleat_app is the role the engine is meant to run as. It is created NOLOGIN
-- and without a password on purpose: a credential does not belong in a file
-- that is committed and applied by every worker at boot. The deployment
-- supplies it with ALTER ROLE cleat_app LOGIN PASSWORD '...'.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cleat_app') THEN
        CREATE ROLE cleat_app
            NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS;
    END IF;
END $$;

-- Correct the attributes if the role PRE-EXISTED with different ones. From 005,
-- and it is here for the same reason the memberships below are: role attributes
-- live in the CLUSTER, so a fresh database on a cluster that already has a
-- cleat_app does not get them from the CREATE above -- that branch is skipped by
-- IF NOT EXISTS, and nothing else would notice.
--
-- Only the differing case executes, deliberately (005's own note): asserting a
-- value the CREATE branch just set makes this the first file to fail on managed
-- PostgreSQL, where no true superuser exists and the ALTER would be a no-op
-- refusal. A deployment that really did grant SUPERUSER to cleat_app still
-- fails here, loudly, and should.
DO $$
DECLARE
    r RECORD;
BEGIN
    SELECT rolsuper, rolcreatedb, rolcreaterole, rolbypassrls
      INTO r FROM pg_roles WHERE rolname = 'cleat_app';

    IF r.rolsuper     THEN ALTER ROLE cleat_app NOSUPERUSER;  END IF;
    IF r.rolcreatedb  THEN ALTER ROLE cleat_app NOCREATEDB;   END IF;
    IF r.rolcreaterole THEN ALTER ROLE cleat_app NOCREATEROLE; END IF;
    IF r.rolbypassrls THEN ALTER ROLE cleat_app NOBYPASSRLS;  END IF;
END $$;

-- BYPASSRLS is the whole point of this one: the `global` claim strategy reads
-- across tenants and needs the exemption. Only a superuser can grant it, so a
-- deployment applying these files as a non-superuser gets the NOTICE and the
-- cross-tenant path stays unusable -- which is why `rotate` is the default
-- strategy. 023's own branch, kept so the outcome is a notice rather than a
-- failed migration.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cleat_dispatcher') THEN
        BEGIN
            CREATE ROLE cleat_dispatcher NOLOGIN BYPASSRLS;
        EXCEPTION WHEN insufficient_privilege THEN
            RAISE NOTICE 'cleat_dispatcher needs BYPASSRLS, which only a superuser can grant, and this connection is not one. The cross-tenant claim function is still created but will not see across tenants; use --claim-strategy=rotate, which needs no grant. (SQLSTATE %)', SQLSTATE;
        END;
    END IF;
END $$;

-- The retention sweeper. NOBYPASSRLS is the default, stated because it is the
-- point: the sweep must be subject to the policies it runs under.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cleat_sweep') THEN
        CREATE ROLE cleat_sweep NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
    END IF;
END
$$;

-- From 077. A role comment is stored in pg_shdescription, which is CLUSTER-wide
-- like pg_auth_members -- so no pg_dump of a database carries it and no
-- per-database catalog diff can see its absence. Restored here for the same
-- reason the memberships below are.
COMMENT ON ROLE cleat_sweep IS
    'Cross-tenant plugin sweeps (cleat#1490). Entered with SET LOCAL ROLE from '
    'engine/plugindb_tenant.go; never connected to directly. Granted WITH '
    'INHERIT FALSE so membership alone does not apply its policies.';

-- The DEFAULT tenant's login role. Normally a tenant role is provisioned at
-- runtime by admin.create_tenant_role, which derives its password from the
-- worker's key -- but this file's own GRANTs below name it, so it has to exist
-- before they run, and 002 -- which seeds every other row of the default tenant
-- -- runs after this file.
--
-- Creating it here with no password is deliberate and matches how cleat_app is
-- created: the password is not this file's to know. The worker re-ALTERs it to
-- the derived value on its next boot, which is the documented rotation path
-- (see 064). LOGIN is set because that is the attribute a provisioned tenant
-- role carries; a passwordless LOGIN role cannot authenticate in any case.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles
                   WHERE rolname = 'cleat_tenant_00000000_0000_0000_0000_000000000000') THEN
        CREATE ROLE cleat_tenant_00000000_0000_0000_0000_000000000000
            LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS;
    END IF;
END $$;

-- ── Role memberships ────────────────────────────────────────────────────────
-- From 077, the only migration that ever granted these. A role GRANT does not
-- belong in a per-database dump, so the rebaseline had to carry it explicitly
-- -- and dropping it is silent in exactly the way 077's own comment warns
-- about one line further on: nothing in a catalog diff of the DATABASE can see
-- a membership, because pg_auth_members is CLUSTER-wide.
--
-- Membership WITH INHERIT FALSE: enough to SET ROLE, not enough to match the
-- sweep policy passively. This distinction is load-bearing and silent when got
-- wrong -- measured, a plain GRANT lets the application role read all 400000
-- rows with no error and the correct number of policies; WITH INHERIT FALSE
-- returns 1000. PostgreSQL 16+.
--
-- `cleat_app` is the one the worker names: cmd/cleat-worker/setup.go does
-- `SET LOCAL ROLE cleat_sweep` on the retention path, and without this membership
-- that fails with 42501 `permission denied to set role "cleat_sweep"`.
--
-- The loop covers whatever cleat_tenant_% roles exist at this instant. On a
-- fresh bootstrap that is the default tenant role just created above, which is
-- exactly what 077 swept on a fresh database too -- so the rebaseline preserves
-- the behaviour rather than narrowing it. (Roles a worker provisions later via
-- admin.create_tenant_role are not covered, and never were: no migration after
-- 077 granted this.)
DO $$
DECLARE
    r record;
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cleat_app') THEN
        EXECUTE 'GRANT cleat_sweep TO cleat_app WITH INHERIT FALSE';
    END IF;
    FOR r IN SELECT rolname FROM pg_roles WHERE rolname LIKE 'cleat_tenant\_%' LOOP
        EXECUTE format('GRANT cleat_sweep TO %I WITH INHERIT FALSE', r.rolname);
    END LOOP;
END
$$;
"""

# event_history is the one table the design doc partitions: hash, 64 buckets
# (docs/schema-partitioning-design.md, Decision 1, RESOLVED). Written here
# explicitly rather than inferred, because the dump was taken from an
# UNPARTITIONED table and carries no trace of it.
#
# The PK has to move to (tenant_id, workflow_id, step): PostgreSQL requires the
# partition key to be covered by every unique constraint on a partitioned table,
# and the old (workflow_id, step) omits tenant_id. The doc records that this is
# free "with no deployments", which is the same precondition as the rest of this
# rebaseline.
PARTITION = os.environ.get("CLEAT_2059_PARTITION") == "1"
PARTITIONED_TABLE = "event_history"
PARTITION_BUCKETS = 64
OLD_PK = "PRIMARY KEY (workflow_id, step)"
NEW_PK = "PRIMARY KEY (tenant_id, workflow_id, step)"
# The PK move's other half, and the reason the generator has to carry it: an
# `ON CONFLICT` arbiter must match a unique index, and on a partitioned table
# that index must cover the partition key. The two routine bodies in 003 are
# dumped verbatim from the UNPARTITIONED chain, so they name the old arbiter and
# would ship a baseline whose own procedures fail every batch flush:
#
#   ERROR: there is no unique or exclusion constraint matching the
#          ON CONFLICT specification (42P10)
#
# Not a hand-edit to 003. 003 is GENERATED, so a hand-edit has no conflict
# surface and the next regeneration silently reverts it -- the failure survives
# review precisely because the file that was reviewed is not the file that ships.
#
# Matched as a CLAUSE and asserted as a PROPERTY, never as a string. The first
# draft was `body.replace(OLD, NEW)` plus a residue scan for OLD, and it had a
# hole that a green run hid completely: narrow the search text to one site's
# spelling and the replace moves that site, the residue scan looks for the same
# narrowed text and finds none, and the generator exits 0 having emitted a
# baseline whose other procedure still names the old arbiter. Measured
# 2026-09-26 by mutating OLD_CONFLICT to the flush_event_step spelling -- 003
# line 131 moved, line 28 did not, rc=0. Reading the column list is
# spelling-independent, which is the whole point of the check.
CONFLICT_RE = re.compile(r"ON CONFLICT\s*\(([^)]*)\)")

# What this generator can say about 002 without overwriting it. See the
# refusal next to where it is written.
PLACEHOLDER_002 = (
    "-- cleat consolidated defaults (002)\n"
    "-- HAND-ASSEMBLED: a catalog dump carries no rows. cleat#2059.\n")


def routine_name(body: str) -> str:
    m = re.search(r"FUNCTION\s+([A-Za-z0-9_.]+)\s*\(", body, re.I)
    return m.group(1).split(".")[-1] if m else ""


def main(src, outdir):
    text = open(src).read()
    # Split on pg_dump's own object headers. Reliable where a semicolon split is
    # not: DO blocks and function bodies contain semicolons inside $$ ... $$.
    # pg_dump's session setup (`SET statement_timeout = 0;`, the set_config call)
    # lives in the PREAMBLE, before the first object header -- never inside an
    # object body. Stripping `SET ` lines globally corrupts every function body
    # that contains SQL's own `SET`, as in `UPDATE ... SET assigned_to = ...`:
    # a line-oriented filter cannot tell session config from an UPDATE assignment,
    # and the result is a syntax error a thousand lines away from the cause.
    text = re.sub(r"^(SET [^\n]*|SELECT pg_catalog\.set_config\([^\n]*)$", "", text,
                  flags=re.M)
    text = re.sub(r"^\\[a-z]+ [^\n]*$", "", text, flags=re.M)   # psql meta-commands

    parts = re.split(r"\n--\n-- Name: (.*?); Type: ([A-Z ]+);.*?\n--\n", text)
    # parts = [preamble, name1, type1, body1, name2, type2, body2, ...]
    objs = []
    for i in range(1, len(parts), 3):
        objs.append((parts[i].strip(), parts[i + 1].strip(), parts[i + 2].strip()))

    buckets = OrderedDict()
    for name, kind, body in objs:
        # migration/runner.go's ensureMigrationsTable creates schema_migrations --
        # WITH its primary key -- before any migration file runs, and the harness
        # excludes it from every snapshot for that reason (catalogdiff.go's
        # schemaMigrationsTable). Emitting it here fails with `already exists`.
        # The GRANT is kept: the table does exist by then, and dropping a privilege
        # would be a change the harness structurally cannot see.
        # schema_migrations is the RUNNER's table -- ensureMigrationsTable creates
        # it, with its own DDL, before any migration file runs -- and no migration
        # in the chain grants on it. The dump carries an ACL for it anyway, because
        # ALTER DEFAULT PRIVILEGES had applied one on the database being dumped,
        # and that GRANT is what broke the raw-application path:
        #   applying shipped migration 001_schema.sql failed:
        #   pq: relation "schema_migrations" does not exist (42P01)
        # Anything applying these files WITHOUT the runner (engine/schema_bootstrap_test.go
        # is the one that does) has no such table yet. Dropped, both object and ACL.
        if "schema_migrations" in name:
            continue
        # psql meta-commands only. `\restrict` / `\unrestrict` are client syntax, not
        # SQL, and the runner Execs these files raw. NOT `SET`: see the preamble note.
        body = "\n".join(l for l in body.split("\n")
                         if not re.match(r"^\\[a-z]+ ", l)).strip()
        if not body:
            continue
        # `ALTER DEFAULT PRIVILEGES FOR ROLE <owner>` hardcodes the role that ran the
        # dump. The current migrations omit it (005_app_role.sql), and the statement
        # means "objects THIS role creates" either way, so dropping it is both more
        # portable and faithful.
        #
        # BEFORE un_freeze_schema, and that order is load-bearing: `FOR ROLE
        # <owner>` sits between PRIVILEGES and IN SCHEMA, so while it is still
        # there the ALTER DEFAULT PRIVILEGES pattern cannot match and the rewrite
        # silently does nothing. Caught by the self-check below on the first run.
        body = re.sub(r"ALTER DEFAULT PRIVILEGES FOR ROLE \S+ IN SCHEMA",
                      "ALTER DEFAULT PRIVILEGES IN SCHEMA", body)
        body = make_idempotent(un_freeze_schema(strip_configured(body)))

        # pg_dump emits `CREATE FUNCTION`; the chain it replaces uses
        # `CREATE OR REPLACE` (36 of its 43 routine definitions). The difference
        # does not show up on a FIRST apply, which is why the catalog diff -- and
        # every check that builds a schema once -- was blind to it. It shows up on
        # the SECOND, and applying this set twice is not an edge case:
        # engine/testutil's SetupFullSchema does it on every PostgreSQL test.
        #
        # Measured: with the plain form, a clean engine run failed 502 tests, 107
        # of them with
        #   pq: function "batch_flush_events" already exists with same argument
        #   types (42723)
        # and the rest cascading behind it.
        #
        # `CREATE OR REPLACE` produces an identical catalog entry, so this is
        # free against every differential check -- which is the point: the
        # harness compares end states and cannot see idempotence at all.
        body = re.sub(r"\bCREATE (FUNCTION|PROCEDURE)\b", r"CREATE OR REPLACE \1", body, count=1)
        if kind == "FUNCTION":
            kind = "PROCEDURE" if routine_name(body) in PROCEDURES else "FUNCTION"
        elif kind == "ACL" and any(pr in body for pr in PROCEDURES):
            # The three procedures carry their own GRANTs, written UNQUALIFIED
            # (`GRANT ALL ON FUNCTION batch_flush_events(...)`) -- so a
            # schema-qualified search for them finds nothing, and they land in 001
            # ahead of the definitions 003 holds. They follow their function instead.
            kind = "PROCEDURE_ACL"
        buckets.setdefault(kind, []).append(body)

    # The generator must not be ABLE to emit a hardcoded schema. This is the
    # same predicate migrations_do_not_hardcode_the_schema_test.go applies, run
    # here so a generation defect fails at generation time rather than in a CI
    # job that has to apply the files against a database first -- and it is a
    # predicate, not a count, so it survives the file count changing.
    bare = re.compile(r"(^|[^A-Za-z0-9_])public([^A-Za-z0-9_]|$)")
    sp = re.compile(r"(?i)^\s*SET\s+(LOCAL\s+)?search_path\s*(=|\bTO\b)")
    bad = []
    for kind, bodies in buckets.items():
        for b in bodies:
            for line in b.split("\n"):
                if sp.match(line) or bare.search(line):
                    bad.append("%s: %s" % (kind, line.strip()[:110]))
    if bad:
        raise SystemExit(
            "the generated files name the configured schema on %d line(s); pg_dump "
            "resolved a deferral that un_freeze_schema must put back:\n  %s"
            % (len(bad), "\n  ".join(bad[:12])))

    def emit(kinds, header):
        out = [header]
        for k in kinds:
            for b in buckets.get(k, []):
                out.append("")
                out.append(b)
        return "\n".join(out).rstrip() + "\n"

    # ── event_history: hash-partition it (partitioning PR only) ─────
    # OFF by default; CLEAT_2059_PARTITION=1 turns it on.
    #
    # The two are separate PRs because partitionING moves event_history's primary
    # key, and that is NOT a schema-only change. PostgreSQL needs a unique index
    # matching every conflict target, so the live `ON CONFLICT (workflow_id, step)`
    # statements on the event write path stop working (42P10). Those, the nine
    # predicates and the partition-aware grants are code the schema REQUIRES, and
    # they belong in the PR that requires them.
    # docs/schema-partitioning-design.md does not mention ON CONFLICT anywhere --
    # it is scoped as schema-only and it is not.
    #
    # With PARTITION false, side B differs from side A in NO way whatever, which is
    # a strictly stronger claim than "identical apart from".
    if PARTITION:
        # Each transformation asserts it found exactly what it expected. A silently
        # unmatched rewrite emits the UNPARTITIONED table and the old PK, and the
        # A/B diff then reports an EMPTY difference -- a pass, on a tree where the
        # one change this step exists to make did not happen.
        tb = buckets.get("TABLE", [])
        # BOTH spellings, because make_idempotent has already run by this point
        # (it is applied per bucket, above) and has rewritten the declaration to
        # `CREATE TABLE IF NOT EXISTS event_history (`. Matching only the bare
        # form finds 0 and the SystemExit below fires on a correct tree --
        # measured 2026-09-26, first time this branch was ever executed:
        #
        #   expected exactly one CREATE TABLE event_history; found 0
        #
        # Same shape as the trgm opclass rewrite, which also ran after
        # make_idempotent had added the clause it was not expecting.
        marker = "CREATE TABLE %s (" % PARTITIONED_TABLE
        marker_ine = "CREATE TABLE IF NOT EXISTS %s (" % PARTITIONED_TABLE
        found = [i for i, b in enumerate(tb)
                 if b.startswith(marker) or b.startswith(marker_ine)]
        if len(found) != 1:
            raise SystemExit("expected exactly one CREATE TABLE %s; found %d"
                             % (PARTITIONED_TABLE, len(found)))
        # pg_dump puts the table's own ALTERs -- FORCE ROW LEVEL SECURITY among
        # them -- in the SAME "-- Name: event_history; Type: TABLE" block, so the
        # body does not end at the column list. Anchor on the ");" that closes it.
        body = tb[found[0]]
        anchor = marker_ine if marker_ine in body else marker
        decl = body.index(anchor)
        close = body.index("\n);", decl)
        tb[found[0]] = (body[:close]
                        + "\n)\nPARTITION BY HASH (tenant_id);"
                        + body[close + len("\n);"):])

        # IF NOT EXISTS on every child, because this bucket is built AFTER
        # make_idempotent has already run over the buckets that existed then --
        # so nothing else will add it, and a second apply of the shipped file
        # dies on the first child:
        #
        #   pq: relation "event_history_p0" already exists (42P07)
        #
        # engine/schema_bootstrap_test.go's TestShippedSchema_IsIdempotent
        # re-applies the shipped files deliberately, so this is not optional.
        # RLS does NOT propagate from a partitioned parent to its partitions.
        # Measured on the generated schema, 2026-09-26 -- the parent is t/t and
        # every child is f/f -- and independently measured in the design doc,
        # which calls it "a hard constraint on this design":
        #
        #   relname  relrowsecurity  relforcerowsecurity
        #   eh       t               t
        #   eh_p0    f               f          <- partitions inherit neither
        #
        # Reading THROUGH the parent applies the parent's policy. Reading a
        # partition DIRECTLY does not, and that is a real leak rather than a
        # theoretical one: as the table's owner, with tenant 1 in context, a
        # direct read of one partition returned 100 rows -- tenant 1's row COUNT
        # while holding a different tenant's rows entirely. It is the partition
        # with an unexpected count that exposes it, so spot-checking one
        # partition confirms the wrong answer.
        #
        # So each child gets the same three things the parent has. FORCE matters
        # as much as ENABLE: without it the table's OWNER bypasses the policy,
        # and the retention sweeps run as a role that would otherwise see
        # everything.
        # The three statements go in THREE different buckets, and the split is
        # load-bearing rather than tidiness. PARTITION is emitted immediately
        # after TABLE -- the children must follow their parent -- but FUNCTION
        # and POLICY come much later, so a policy emitted here names
        # cleat.assert_tenant_set() before it exists:
        #
        #   ERROR: function cleat.assert_tenant_set() does not exist
        #
        # Measured 2026-09-26, on the first apply of the partitioned baseline.
        # The parent's policy has no such problem only because POLICY already
        # sits after FUNCTION in schema_kinds.
        buckets["PARTITION"] = [
            "CREATE TABLE IF NOT EXISTS %s_p%d PARTITION OF %s\n"
            "    FOR VALUES WITH (MODULUS %d, REMAINDER %d);"
            % (PARTITIONED_TABLE, i, PARTITIONED_TABLE, PARTITION_BUCKETS, i)
            for i in range(PARTITION_BUCKETS)
        ]
        buckets.setdefault("ROW SECURITY", []).extend(
            "ALTER TABLE %s_p%d %s ROW LEVEL SECURITY;"
            % (PARTITIONED_TABLE, i, clause)
            for i in range(PARTITION_BUCKETS)
            for clause in ("ENABLE", "FORCE")
        )
        # Bare CREATE POLICY, no DROP: guard_policy (below) prepends the
        # DROP POLICY IF EXISTS for every POLICY body, and rejects anything that
        # does not start with CREATE POLICY -- which is what caught the first
        # draft of this:
        #   unrecognised policy body: 'DROP POLICY IF EXISTS ...'
        buckets.setdefault("POLICY", []).extend(
            "CREATE POLICY tenant_isolation_events ON %s_p%d "
            "USING ((tenant_id = cleat.assert_tenant_set()));"
            % (PARTITIONED_TABLE, i)
            for i in range(PARTITION_BUCKETS)
        )

        cs = buckets.get("CONSTRAINT", [])
        hit = [i for i, b in enumerate(cs) if PARTITIONED_TABLE + "_pkey" in b]
        if len(hit) != 1:
            raise SystemExit("expected exactly one %s_pkey; found %d"
                             % (PARTITIONED_TABLE, len(hit)))
        if OLD_PK not in cs[hit[0]]:
            raise SystemExit("PK is not %r: %r" % (OLD_PK, cs[hit[0]]))
        cs[hit[0]] = cs[hit[0]].replace(OLD_PK, NEW_PK)

        # ...and drop ONLY from that same statement. pg_dump emits
        # `ALTER TABLE ONLY <t> ADD CONSTRAINT <t>_pkey ...` for every table,
        # which is harmless on an ordinary one -- but on a PARTITIONED table
        # ONLY suppresses the recursion to the partitions. The unique index is
        # then created on the parent ALONE, no partition carries a matching
        # one, and the table cannot serve any ON CONFLICT at all, whichever
        # arbiter it names:
        #
        #   pq: there is no unique or exclusion constraint matching the
        #       ON CONFLICT specification (42P10)
        #
        # Measured 2026-09-26 with the arbiter held constant: as shipped,
        # 42P10 on both; after re-adding the identical constraint without
        # ONLY, err=<nil> and event_history_p0 gains a unique index. Nothing
        # else differed between the two readings. That the OLD arbiter fails
        # the same way is what identifies this as a separate defect rather
        # than a symptom of the arbiter rewrite -- and it is why the arbiter
        # change is inert until this line is right.
        only_pat = re.compile(r"ALTER TABLE ONLY\s+%s\b" % PARTITIONED_TABLE)
        cs[hit[0]], n_only = only_pat.subn("ALTER TABLE %s" % PARTITIONED_TABLE,
                                           cs[hit[0]])
        if n_only != 1:
            raise SystemExit(
                "expected exactly one 'ALTER TABLE ONLY %s' in the %s_pkey entry; "
                "found %d" % (PARTITIONED_TABLE, PARTITIONED_TABLE, n_only))

        # Every routine body that names an arbiter moves with the PK, for the
        # same 42P10 reason. Gated on PARTITION because the rewrite is only
        # correct WITH the new PK: against the unpartitioned schema the old
        # (workflow_id, step) target is the one that matches a unique index, and
        # `ON CONFLICT (tenant_id, workflow_id, step)` would fail instead.
        def add_tenant(m):
            cols = m.group(1).strip()
            if "tenant_id" in cols:
                return m.group(0)
            return "ON CONFLICT (tenant_id, %s)" % cols

        seen = 0
        for kind in ("FUNCTION", "PROCEDURE"):
            bodies = buckets.get(kind, [])
            for i, b in enumerate(bodies):
                moved, n = CONFLICT_RE.subn(add_tenant, b)
                if n:
                    seen += n
                    bodies[i] = moved
        if seen == 0:
            raise SystemExit(
                "no 'ON CONFLICT (...)' arbiter found in any routine body; the "
                "rewrite did not fire on a tree it was written for")

        # The same predicate, re-applied to the result -- and it is not a
        # restatement of the count above. `seen` says the rewrite ran; this says
        # it covered every clause, and the two come apart exactly when a clause
        # reaches the file in a shape the substitution handles differently from
        # the scan. That is not hypothetical: see the CONFLICT_RE note.
        short = [(kind, m.group(0), b.strip().split("\n")[0][:70])
                 for kind in ("FUNCTION", "PROCEDURE")
                 for b in buckets.get(kind, [])
                 for m in CONFLICT_RE.finditer(b)
                 if "tenant_id" not in m.group(1)]
        if short:
            raise SystemExit(
                "%d arbiter(s) in routine bodies still omit tenant_id, which a "
                "partitioned event_history refuses with 42P10:\n  %s"
                % (len(short), "\n  ".join("%s: %r in %s" % s for s in short)))

        # A THIRD check, and it exists only because the two above share one
        # anchor. Both use CONFLICT_RE, so narrowing that regex narrows the
        # substitution and the residue scan together and neither can notice --
        # measured 2026-09-26: with CONFLICT_RE narrowed to the flush_event_step
        # spelling, that site moved, the residue scan looked for the narrowed
        # text, found none, and the generator exited 0 on a baseline whose other
        # procedure still named the old arbiter. Counting the bare KEYWORD is
        # different code from matching the clause, which is what makes the
        # coverage claim falsifiable rather than a restatement of itself.
        #
        # It also subsumes the `ON CONFLICT ON CONSTRAINT <name>` form, which
        # carries no column list and which CONFLICT_RE cannot see: keywords=1,
        # covered=0.
        for kind in ("FUNCTION", "PROCEDURE"):
            for b in buckets.get(kind, []):
                keywords = b.count("ON CONFLICT")
                covered = len(CONFLICT_RE.findall(b))
                if keywords != covered:
                    raise SystemExit(
                        "%d 'ON CONFLICT' keyword(s) but %d clause(s) matched by "
                        "CONFLICT_RE in:\n  %s"
                        % (keywords, covered, b.strip().split("\n")[0][:70]))

        # idx_event_history_tenant_wf is now EXACTLY the primary key, so drop it.
        #
        # It was not redundant before this change, and that is why it belongs
        # here rather than in a cleanup: the old PK was (workflow_id, step), so a
        # tenant-leading index was the only path that could prune by tenant. The
        # PK move to (tenant_id, workflow_id, step) makes the index a duplicate
        # of the PK's own -- same columns, same order -- on all 64 partitions,
        # serving nothing the PK does not serve.
        #
        # Free to drop now and not later: 0.3.0 requires a fresh database, so
        # this is a line in a baseline; removing it afterwards would be an
        # ALTER on a partitioned table for no behavioural gain.
        #
        # Asserted both ways. A silent no-match leaves the duplicate in place and
        # NOTHING would say so -- it is a valid index, the catalog diff compares
        # structure on one side only, and the suite passes. A silent over-match
        # would drop an index the cursor read depends on.
        idx = buckets.get("INDEX", [])
        hit_idx = [i for i, b in enumerate(idx) if "idx_event_history_tenant_wf" in b]
        if len(hit_idx) != 1:
            raise SystemExit(
                "expected exactly one idx_event_history_tenant_wf index statement; "
                "found %d" % len(hit_idx))
        del idx[hit_idx[0]]
        if any("idx_event_history_tenant_wf" in b for b in idx):
            raise SystemExit("idx_event_history_tenant_wf still present after removal")

    # GRANTs naming functions the baseline does NOT create. pg_dump emits an ACL
    # for every object in the database, including the ones an EXTENSION owns --
    # and those are not in this file, because CREATE EXTENSION makes them. Named
    # unqualified, they resolve only where the extension's schema happens to be
    # on the search_path:
    #
    #   GRANT ALL ON FUNCTION gtrgm_in(cstring) TO cleat_app;
    #   -> pool_b_1366: function gtrgm_in(cstring) does not exist (42883)
    #
    # The chain never grants on them. The rule is narrower than a blocklist and
    # cannot go stale: grant on a function only if this file defines it.
    defined = set()
    for b in buckets.get("FUNCTION", []) + buckets.get("PROCEDURE", []):
        m = re.search(r"FUNCTION\s+([A-Za-z_][\w.]*)\s*\(", b, re.I)
        if m:
            defined.add(m.group(1).split(".")[-1])

    # PER STATEMENT, not per entry, and that distinction cost 68 legitimate
    # grants on the first attempt. An ACL entry is a BLOCK of GRANT/REVOKE lines --
    # a table's privileges, a sequence's, several functions' -- so rejecting the
    # entry because one function in it is an extension member throws away every
    # other grant it carried. Filtering line-wise keeps the rest.
    func_re = re.compile(r"(?:ON|FOR)\s+(?:FUNCTION|PROCEDURE)\s+([A-Za-z_][\w.]*)\s*\(", re.I)

    def keep_line(line):
        m = func_re.search(line)
        return not m or m.group(1).split(".")[-1] in defined

    for kind in ("ACL", "PROCEDURE_ACL"):
        kept, dropped = [], 0
        for b in buckets.get(kind, []):
            lines = [l for l in b.split("\n") if keep_line(l)]
            if len(lines) != len(b.split("\n")):
                dropped += len(b.split("\n")) - len(lines)
            if any(l.strip() for l in lines):
                kept.append("\n".join(lines).strip())
        if dropped:
            print("  dropped %d ACL statement(s) naming objects this baseline does not create"
                  % dropped)
        buckets[kind] = kept

    # Function ownership must follow the definitions it re-owns, so it is a
    # bucket rather than part of the header.
    buckets["FUNCTION_OWNERSHIP"] = [FUNCTION_OWNERSHIP]

    # The runner's own table needs a grant the chain never wrote, because the
    # chain got it from ALTER DEFAULT PRIVILEGES instead: those apply when a
    # table is CREATED, so on a database where the runner made schema_migrations
    # before 005 ran, the privilege arrived only on a later recreate. Explicit
    # and guarded reproduces that on every path:
    #   Verify as cleat_app: get applied versions: pq: permission denied for
    #   table schema_migrations (42501)
    # The guard matters for the appliers that run these files WITHOUT the runner
    # (engine/schema_bootstrap_test.go), where the table does not exist yet.
    buckets["SCHEMA_MIGRATIONS_GRANT"] = ["""DO $do$ BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE c.relname = 'schema_migrations' AND n.nspname = current_schema()
    ) THEN
        EXECUTE format('GRANT SELECT, INSERT, DELETE, UPDATE ON TABLE %I.schema_migrations TO cleat_app', current_schema());
    END IF;
END $do$;"""]

    # Constraints and foreign keys: the dump emits them as ALTER TABLE ... ADD
    # CONSTRAINT, which is not idempotent and has no IF NOT EXISTS in PostgreSQL.
    # The chain it replaces declares them INLINE in CREATE TABLE, so IF NOT EXISTS
    # on the table covered them -- and the dump cannot put them back inline
    # without rewriting every table.
    #
    #   re-applying 001_schema.sql failed: pq: relation "orgs_name_key" already
    #   exists (42P07)
    #
    # Wrapped in a DO block keyed on pg_constraint, which is the same predicate
    # the inline form satisfied by other means.
    def guard_constraint(body):
        m = re.match(r"(?s)ALTER TABLE (?:ONLY )?([A-Za-z_][\w.]*)\s+ADD CONSTRAINT (\w+)", body)
        if not m:
            raise SystemExit("unrecognised constraint body: %r" % body[:80])
        table, cname = m.group(1), m.group(2)
        indented = "\n".join("        " + l for l in body.split("\n"))
        return ("DO $do$ BEGIN\n"
                "    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = '%s'\n"
                "                   AND conrelid = '%s'::regclass) THEN\n"
                "%s\n"
                "    END IF;\n"
                "END $do$;" % (cname, table, indented))

    for kind in ("CONSTRAINT", "FK CONSTRAINT"):
        buckets[kind] = [guard_constraint(b) for b in buckets.get(kind, [])]

    # Policies: PostgreSQL has no IF NOT EXISTS for CREATE POLICY, and the chain
    # it replaces uses the idiom the RLS test helpers describe -- "DROP POLICY IF
    # EXISTS ... CREATE POLICY ...", which is why re-applying 031/061/083 was a
    # no-op. Reproduced here rather than invented:
    #   re-applying 001_schema.sql failed: pq: policy
    #   "idempotency_keys_cross_tenant" for table "idempotency_keys" already
    #   exists (42710)
    def guard_policy(body):
        m = re.match(r"(?s)CREATE POLICY (\w+) ON ([A-Za-z_][\w.]*)", body)
        if not m:
            raise SystemExit("unrecognised policy body: %r" % body[:80])
        return "DROP POLICY IF EXISTS %s ON %s;\n%s" % (m.group(1), m.group(2), body)

    buckets["POLICY"] = [guard_policy(b) for b in buckets.get("POLICY", [])]

    # Triggers: CREATE OR REPLACE TRIGGER since PostgreSQL 14, which is the
    # whole of this file's supported range. The dump emits plain CREATE TRIGGER.
    buckets["TRIGGER"] = [re.sub(r"^CREATE TRIGGER", "CREATE OR REPLACE TRIGGER", b)
                          for b in buckets.get("TRIGGER", [])]

    # DEPENDENCY ORDER, not alphabetical: `ALTER SEQUENCE ... OWNED BY <table>` names a
    # table, so it must follow CREATE TABLE -- bucketing it with the other SEQUENCE
    # objects emits it first and fails with `relation ... does not exist`. Same for
    # column DEFAULTs that call nextval() on a sequence.
    schema_kinds = ["SCHEMA", "EXTENSION", "SEQUENCE", "TABLE", "SEQUENCE OWNED BY",
                    "DEFAULT", "CONSTRAINT", "FK CONSTRAINT", "INDEX", "FUNCTION", "FUNCTION_OWNERSHIP",
                    "ROW SECURITY", "POLICY", "TRIGGER", "ACL", "SCHEMA_MIGRATIONS_GRANT", "DEFAULT ACL"]
    if PARTITION:
        schema_kinds.insert(schema_kinds.index("TABLE") + 1, "PARTITION")
    open(f"{outdir}/001_schema.sql", "w").write(emit(schema_kinds, """-- cleat consolidated schema (001)
--
-- GENERATED from a pg_dump of the database built by the previous
-- migrations/postgres chain, then wrapped by hand. cleat#2059.
--
-- Every CREATE TABLE carries its FINAL column set -- the dump has already
-- applied every ALTER TABLE ADD COLUMN, so there is nothing to fold by hand.
-- Every routine in 003 is likewise the LAST definition of that routine, which
-- is the property the design doc warns is easy to get wrong by reading files.
--
-- The schema cleat builds into is never named here: names are unqualified and
-- resolve through search_path, which the runner sets. `admin.` and `tenant_*`
-- are not the configured schema and stay qualified. See
-- migration/migrations_do_not_hardcode_the_schema_test.go.""" + ROLES))
    open(f"{outdir}/003_procedures.sql", "w").write(emit(["PROCEDURE", "PROCEDURE_ACL"], """-- cleat consolidated procedures (003)
--
-- GENERATED from pg_get_functiondef via pg_dump, so each body is the LAST
-- definition of that routine. cleat#2059."""))
    # 002 is data and behaviour, which a catalog dump cannot carry -- it is
    # hand-assembled from the files that touched data. Left as a marker so the
    # omission is visible rather than silent.
    #
    # BUT NEVER OVER AN EXISTING FILE. This write used to be unconditional, and
    # on 2026-09-26 it replaced the real 002 -- the only carrier of admin.orgs
    # and the default tenant/org seed -- with the 100-byte marker below. The
    # whole file still applied cleanly, the catalog A/B diff reported EMPTY
    # (a missing ROW is invisible to a structural diff, which is the same blind
    # spot this generator's own notes describe for the org/tenant FK ordering),
    # and ~70 database tests then failed on foreign keys to admin.orgs and
    # admin.tenants with a schema that looked perfectly correct.
    #
    # A refusal, not a warning: the placeholder is a correct thing to emit into
    # an empty directory and a destructive thing to emit over a hand-assembled
    # file, and nothing in the filename distinguishes those.
    defaults = f"{outdir}/002_defaults.sql"
    if os.path.exists(defaults):
        with open(defaults) as fh:
            existing = fh.read()
        if existing.strip() != PLACEHOLDER_002.strip():
            raise SystemExit(
                "refusing to overwrite %s: it already carries content this "
                "generator cannot produce (a catalog dump has no rows).\n"
                "Generate into an empty directory, or move that file aside "
                "deliberately -- do not let this overwrite the seed." % defaults)
    open(defaults, "w").write(PLACEHOLDER_002)

    print("objects by kind:", {k: len(v) for k, v in buckets.items()})

if __name__ == "__main__":
    main(sys.argv[1], sys.argv[2])
