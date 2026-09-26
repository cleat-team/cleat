#!/usr/bin/env python3
"""Verify that the committed PostgreSQL baseline is exactly what the generator emits.

**Why this is not shaped like the SQL Server mode, and cannot be.** WS-2's
`scripts/gen-mssql-baseline -mode=verify` builds a database from the committed
baseline, regenerates, and compares -- which works because its emitter is a pure
function from the live catalogue to the three files: such a function is a **fixed
point of its own input**, so self-application verifies it. `gen-postgres-baseline.py`
is not that. It is a **one-shot transform from a `pg_dump` of the pre-rebaseline
chain**, as its own docstring says, so regenerating from its own output is not
defined -- it fails outright, at the partition anchor:

    close = body.index("\\n);", decl)      # gen-postgres-baseline.py
    ValueError: substring not found

because a database built from the committed baseline already has a partitioned
`event_history` and its dump closes `)\\nPARTITION BY HASH (tenant_id);`. So this
mode verifies the artifact against **the thing it claims to come from**: it builds a
database from the PRIOR REBASELINE's chain, dumps that, regenerates, and requires
byte-identity. That is the acceptance procedure, automated.

It exists because two guards in `engine/` were narrowed on the strength of the claim
"this baseline is generated, so enumerating the policy list in it is not the
hand-written list those guards forbid". That is a claim about the PROCESS, and until
this mode existed nothing checked it of the ARTIFACT: a hand-edit to
`001_schema.sql` or `003_procedures.sql` would have been undetectable, and the
guards' justification would have been resting on a property the tree did not have.

**Three checks, and the third is not redundant with the second.** The two GENERATED
files must be byte-identical to a fresh emit. `002_defaults.sql` is hand-assembled -- a
`pg_dump --schema-only` carries no rows, so the generator writes a placeholder instead --
and it is asserted twice: that it is NOT that placeholder, and that the rows it exists to
seed are really in a database built from the COMMITTED baseline. The second of those is
not decoration. "Not the placeholder" catches total loss and says nothing about a single
deleted row, and it is exactly that case the mode's own failure text describes -- *a
missing ROW is not a structural difference*, so no catalog diff can see it. Found by
cleat-review, which measured this file and showed its only row query was the
empty-database precondition.

**The seed-row queries are safe from RLS by two independent properties, and only one of
them is load-bearing.** None of the three tables `002` seeds is RLS-scoped -- that is the
substantive one, and it is why those queries can read a row at all. The CI DSN is ALSO a
superuser, which bypasses row-level security unconditionally, and that is a coincidence of
the credential rather than a property of the check. The distinction matters for whoever
adds the fourth assertion: **if it names an RLS-scoped table, the mode starts depending on
being run as a superuser**, passing here and failing wherever the DSN is an ordinary role.

    ./scripts/verify-postgres-baseline.py --dsn postgres://... \\
        --prior-rebaseline 8f91b43a8e2259332c5bdbd2c47db15261927d10

Exit status carries three outcomes, not two, because "could not measure" and "the
artifact is wrong" send you to different places (CLAUDE.md):

    0   the committed baseline is exactly what the generator produces
    1   a finding about the TREE: a committed file is not a fresh emit, a seed row it
        exists to insert is missing, or the committed files do not APPLY.
    2   could not establish what was being measured -- the target database was not
        empty, the prior tree could not be fetched, the server version cannot produce
        a comparable dump, or no client was available. A failure of the CHECK.

**Choose 1 over 2 whenever the unexpected thing is in what is being measured.** The
temptation, on any surprise, is to file it under "could not measure" and stop -- but
that is a statement about the INSTRUMENT, and it sends the reader to fix the harness.
A defect in the artifact is a statement about the TREE and must be 1. Measured while
building this mode: two half-statement deletions from 002 left a dangling `ON CONFLICT`,
so the committed file stopped applying, and the first version reported rc=2. The
detection was right and the bucket was wrong -- an unappliable shipped file is the
worst possible thing to route away from the tree.

**The DSN's database must be EMPTY, and it is asserted rather than documented.**
This mode BUILDS INTO the database it is given, so pointing it at a shared one
compares the generator's output for that database against the committed baseline for
a different one.

**search_path must be pinned to what the runner sets, and `psql` does not pin it.**
The routines carry `SET search_path FROM CURRENT`, which freezes whatever is current
AT CREATION TIME onto the function. Applying the migrations under psql's default
`"$user", public` freezes that instead, the regenerated dump then reads
`SET search_path TO '$user', 'public'`, and the generator's `un_freeze_schema` refuses
because it cannot put the deferral back. `migration/runner.go` pins `public, pg_temp`;
this mode does the same, per file.

**PostgreSQL-only: the roles this baseline creates are CLUSTER-wide, so run this step
FIRST in its job.** The scratch database is not the boundary people assume it is:
`cleat_app`, `cleat_sweep`, `cleat_dispatcher` and the `cleat_tenant_%` loop are
cluster-scoped, which is the same fact that made `pg_auth_members` invisible to the
per-database catalog diff during the rebaseline (cleat#2416). SQL Server has no
equivalent exposure because its roles are database-scoped, so this ordering rule is
PostgreSQL's alone -- and a reader who reorders the step needs to know why it was
first, which is what this paragraph is for.

**The prior-rebaseline SHA must be the FULL 40 characters.** An abbreviated SHA does
not resolve here: `git fetch --depth 1 origin 8f91b43a` returns
`fatal: couldn't find remote ref 8f91b43a`, an exact-string miss rather than a prefix
resolve. The convention doc's own citation is abbreviated, so this is asserted below
rather than left to bite the next reader.
"""

import argparse
import os
import re
import shutil
import subprocess
import sys
import tempfile
import urllib.parse

GENERATED = ("001_schema.sql", "003_procedures.sql")
BASELINE = ("001_schema.sql", "002_defaults.sql", "003_procedures.sql")

# The rows 002 exists to seed. Asserted by NAME, not by count: a count of one passes
# on the wrong row, and a count of zero is the failure this exists to catch. These are
# the three the file inserts -- the default org, the default tenant, and the
# tenant_roles row the tenant_roles loop derives from them.
SEED_ROWS = (
    ("admin.orgs", "org_id = '00000000-0000-0000-0000-000000000000'"),
    ("admin.tenants", "tenant_id = '00000000-0000-0000-0000-000000000000'"),
    ("admin.tenant_roles", "role_name = 'cleat_tenant_00000000_0000_0000_0000_000000000000'"),
)


def replace_db(dsn, db):
    """The same DSN pointed at a different database, for the maintenance connection."""
    u = urllib.parse.urlparse(dsn)
    return urllib.parse.urlunparse(u._replace(path="/" + db))

# The generator's PLACEHOLDER_002, which it writes into an empty outdir and refuses
# to write over anything else. Compared against the COMMITTED 002: if they match, the
# seed rows are gone and no structural diff could have seen it.
PLACEHOLDER_002 = (
    "-- cleat consolidated defaults (002)\n"
    "-- HAND-ASSEMBLED: a catalog dump carries no rows. cleat#2059.\n"
)

# --no-owner is REQUIRED and --no-privileges must NOT be passed. Omitting the first
# puts `ALTER FUNCTION ... OWNER TO postgres` into 003; passing the second drops the
# `GRANT ... ON FUNCTION ... TO cleat_app` statements the baseline exists to carry.
# Both measured on cleat#2416.
PG_DUMP_FLAGS = ["--schema-only", "--no-owner"]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--dsn", required=True, help="an EMPTY database this mode may build into")
    ap.add_argument("--prior-rebaseline", required=True,
                    help="FULL 40-char SHA of the commit whose chain the committed "
                         "baseline was generated from; see the module docstring")
    ap.add_argument("--committed", default="migrations/postgres")
    ap.add_argument("--generator", default="scripts/gen-postgres-baseline.py")
    ap.add_argument("--schema", default="public",
                    help="the schema the runner would build into; see the docstring")
    ap.add_argument("--pg-container", default=None,
                    help="container id running PostgreSQL; preferred, because a "
                         "client whose major matches the server's is what makes "
                         "byte-identity meaningful")
    ap.add_argument("--expect-server-version", default=None,
                    help="major version, e.g. 16; a mismatch exits 2 rather than 1")
    args = ap.parse_args()

    if not re.fullmatch(r"[0-9a-f]{40}", args.prior_rebaseline):
        fail(
            f"--prior-rebaseline {args.prior_rebaseline!r} is not a full 40-character SHA.\n"
            "  An abbreviated SHA does not resolve: `git fetch --depth 1 origin 8f91b43a`\n"
            "  returns `fatal: couldn't find remote ref 8f91b43a` -- an exact-string miss,\n"
            "  not a prefix resolve. Full SHAs only.", 2)

    for name in GENERATED + ("002_defaults.sql",):
        if not os.path.exists(os.path.join(args.committed, name)):
            fail(f"{os.path.join(args.committed, name)} does not exist -- "
                 "is --committed right?", 2)

    # Printed in full, and printed even on success. "identical 001_schema.sql" reads
    # as "the artifact is what the generator produces" without saying FROM WHAT, and
    # that is half the evidence: a reader six months on has to be able to judge
    # whether the pin still names the right tree. An abbreviated SHA would not let
    # them look it up.
    print(f"prior rebaseline: {args.prior_rebaseline}")

    # ---- pick a client ----------------------------------------------------
    container = args.pg_container or find_container()
    if container:
        print(f"using the client inside container {container[:12]}")
    else:
        for prog in ("psql", "pg_dump"):
            if shutil.which(prog) is None:
                fail(f"{prog} is not on PATH and no postgres container was found.\n"
                     "  Pass --pg-container <id> for the container running PostgreSQL.", 2)
        print("using the host's psql/pg_dump -- byte-identity needs a matching major")

    user = urllib.parse.unquote(urllib.parse.urlparse(args.dsn).username or "postgres")
    dbname = (urllib.parse.urlparse(args.dsn).path or "/postgres").lstrip("/") or "postgres"

    def psql(sql=None, stdin=None, db=None):
        """psql against the target database, or another when `db` is given.

        `db` exists for the seed-row assertions, which need a database holding the
        COMMITTED baseline rather than the prior chain -- see check_seed_rows().
        """
        target = db or dbname
        if container:
            cmd = ["docker", "exec", "-i", container, "psql", "-U", user, "-d", target]
        else:
            cmd = ["psql", args.dsn if target == dbname else replace_db(args.dsn, target)]
        cmd += ["-v", "ON_ERROR_STOP=1", "-q"]
        if sql is not None:
            cmd += ["-tAc", sql]
        if stdin is not None:
            cmd += ["-f", "-"]
        return subprocess.run(cmd, input=stdin, capture_output=True, text=True)

    def admin_sql(sql):
        """Run one statement on the maintenance database, for CREATE/DROP DATABASE."""
        if container:
            cmd = ["docker", "exec", "-i", container, "psql", "-U", user, "-d", "postgres"]
        else:
            cmd = ["psql", replace_db(args.dsn, "postgres")]
        return subprocess.run(cmd + ["-v", "ON_ERROR_STOP=1", "-q", "-tAc", sql],
                              capture_output=True, text=True)

    # ---- precondition: the target database is empty -----------------------
    r = psql(sql=(
        "SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace "
        "WHERE n.nspname NOT IN ('pg_catalog','information_schema','pg_toast') "
        "AND c.relkind IN ('r','p','v','m','S','f')"))
    if r.returncode != 0:
        fail(f"could not reach the database -- check --dsn:\n{r.stderr.strip()}", 2)
    n = (r.stdout or "").strip()
    if not n.isdigit():
        fail(f"could not read the relation count; psql said {n!r}", 2)
    if int(n) != 0:
        fail(f"the target database is NOT empty ({n} relation(s)).\n"
             "  This mode BUILDS INTO the database it is given, so a shared database\n"
             "  compares one database's output against another's baseline and reports\n"
             "  a difference that means nothing. Give it a freshly created one.", 2)

    r = psql(sql="SHOW server_version")
    server_version = (r.stdout or "").strip()
    if args.expect_server_version:
        if server_version.split(".")[0] != args.expect_server_version:
            fail(f"server version is {server_version!r}, expected major "
                 f"{args.expect_server_version!r}.\n"
                 "  REGENERATION MISMATCH CAUSED BY SERVER VERSION. A pg_dump from a\n"
                 "  different major is not byte-identical, so this comparison would\n"
                 "  report a difference about the server and not about the tree.\n"
                 "  This is a failure of the CHECK, not of the baseline.", 2)
    print(f"target database is empty; server_version {server_version}")

    tmp = tempfile.mkdtemp(prefix="verify-postgres-baseline-")
    try:
        # ---- the prior tree -------------------------------------------------
        # The generator is a one-shot transform of THIS tree's dump, so this is the
        # only input that can verify the committed files. Fetched rather than
        # required in history: the job checks out shallow, and a depth-1 fetch of one
        # tree is ~3s and 18M.
        old = os.path.join(tmp, "old")
        os.makedirs(old)
        r = subprocess.run(["git", "fetch", "--depth", "1", "origin", args.prior_rebaseline],
                           capture_output=True, text=True)
        if r.returncode != 0:
            fail(f"could not fetch {args.prior_rebaseline}:\n{r.stderr.strip()}", 2)
        p = subprocess.Popen(["git", "archive", args.prior_rebaseline, "migrations/postgres"],
                             stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        t = subprocess.run(["tar", "-x", "-C", old], stdin=p.stdout,
                           capture_output=True, text=True)
        p.wait()
        if p.returncode != 0 or t.returncode != 0:
            fail("could not extract migrations/postgres from the prior tree", 2)
        chain = sorted(x for x in os.listdir(os.path.join(old, "migrations", "postgres"))
                       if x.endswith(".sql"))
        if len(chain) < 30:
            fail(f"the prior tree yielded only {len(chain)} migration file(s); that is "
                 "not a pre-rebaseline chain -- is --prior-rebaseline the right commit?", 2)
        # No abbreviated SHA here: the full one is printed once, above, and a short
        # form appearing a second time invites someone to copy that one.
        print(f"prior chain: {len(chain)} file(s)")

        # ---- build it, with the runner's search_path ------------------------
        pin = f"SET search_path = {args.schema}, pg_temp;\n"
        for name in chain:
            with open(os.path.join(old, "migrations", "postgres", name)) as fh:
                r = psql(stdin=pin + fh.read())
            if r.returncode != 0:
                fail(f"applying {name} failed:\n{r.stderr.strip()}", 2)
        print(f"applied {len(chain)} file(s) (search_path = {args.schema}, pg_temp)")

        # ---- regenerate -----------------------------------------------------
        dump = os.path.join(tmp, "dump.sql")
        cmd = (["docker", "exec", "-i", container, "pg_dump"] if container
               else ["pg_dump", "-d", args.dsn])
        cmd += ["-U", user, "-d", dbname] if container else []
        r = subprocess.run(cmd + PG_DUMP_FLAGS, capture_output=True, text=True)
        if r.returncode != 0:
            fail(f"pg_dump failed:\n{r.stderr.strip()}", 2)
        with open(dump, "w") as fh:
            fh.write(r.stdout)

        outdir = os.path.join(tmp, "out")
        os.makedirs(outdir)
        r = subprocess.run([sys.executable, args.generator, dump, outdir],
                           capture_output=True, text=True)
        if r.returncode != 0:
            fail(f"the generator failed:\n{r.stdout.strip()}\n{r.stderr.strip()}", 2)

        # ---- compare ---------------------------------------------------------
        differing = 0
        for name in GENERATED:
            path = os.path.join(args.committed, name)
            have = open(path, "rb").read()
            want = open(os.path.join(outdir, name), "rb").read()
            if have == want:
                print(f"identical   {path}")
                continue
            differing += 1
            print(f"DIFFERS     {path}")
            print("            the committed file is not what the generator produces from")
            print(f"            {args.prior_rebaseline[:8]}'s chain. Either it was hand-edited, or the")
            print("            generator changed without a regeneration.")
            print(f"            {first_difference(have, want)}")
            if not args.expect_server_version:
                print(f"            (if this dump came from PostgreSQL {server_version} and the")
                print("             baseline was cut on a different major, the difference is the")
                print("             server version -- pass --expect-server-version to make that")
                print("             case its own failure)")

        # ---- 002 is hand-assembled, checked rather than skipped --------------
        path002 = os.path.join(args.committed, "002_defaults.sql")
        if open(path002).read().strip() == PLACEHOLDER_002.strip():
            differing += 1
            print(f"DIFFERS     {path002}")
            print("            the committed 002 is the GENERATOR'S PLACEHOLDER, not the")
            print("            hand-assembled file. Its seed rows are gone, and no schema")
            print("            diff can see that: a missing ROW is not a structural")
            print("            difference. Restore it from git history.")
        else:
            print(f"hand-made   {path002} (not the generator's placeholder)")

        # ---- and the assertions must still be about everything 002 seeds -------
        # SEED_ROWS is complete today -- every `INSERT INTO` in the file, including the
        # one inside the DO $$ ... FOR t IN SELECT ... LOOP. But nothing TIES the list
        # to the file: if 002 gains a fourth seed, this mode stays green and silently
        # stops covering it, which is precisely the decay this whole mode exists to
        # prevent. So derive the set from the committed file and require them to match.
        # WHAT THIS DOES NOT SEE, so it is not read as stronger than it is: it counts
        # statements that NAME a table, so a seed written as a function call is
        # invisible here AND to the row assertions below. That is not hypothetical in
        # this file -- 002's own comment records that it used to call
        # admin.create_tenant_role(tenant_id), the call raised, an EXCEPTION handler
        # swallowed the raise, and the row silently never appeared. This check has that
        # same shape one level out.
        #
        # Strip comments FIRST. The whole file is prose describing what it used to do,
        # so a comment mentioning `INSERT INTO` would otherwise be counted as a seed,
        # and the mode would red-light a tree where every row is present and correct.
        # A guard that goes red the first time someone does the right thing is worse
        # than no guard: it teaches the next person to ignore it. Measured when this
        # was added -- 3 statements raw, 3 after stripping, so the defect was LATENT
        # rather than live, which is exactly why nothing had caught it. `--` alone is
        # sufficient here: 002 carries no /* */ block.
        #
        # Fold BOTH sides. `seeded` was lowercased and `declared` was not, so a
        # capitalised entry would report UNASSERTED and STALE for the same table on a
        # correct tree -- latent only because all three entries happen to be lowercase.
        declared = {t.lower() for t, _ in SEED_ROWS}
        src002 = re.sub(r"--[^\n]*", "", open(path002).read())
        seeded = {t.lower() for t in re.findall(
            r"INSERT\s+INTO\s+([A-Za-z_]+\.[A-Za-z_]+)", src002, re.I)}
        if seeded != declared:
            differing += 1
            for t in sorted(seeded - declared):
                print(f"UNASSERTED  {path002} seeds {t}, which no row assertion covers")
                print("            Add it to SEED_ROWS, or this mode goes green without")
                print("            checking the row that file inserts.")
            for t in sorted(declared - seeded):
                print(f"STALE       SEED_ROWS asserts {t}, which {path002} no longer seeds")
        else:
            print(f"seed tables {len(seeded)} in {path002}, all asserted")

        # ---- and 002's rows, BY EFFECT ---------------------------------------
        # The comparison above catches 002 being replaced by the generator's
        # placeholder and NOTHING ELSE. An emptied file, or one row deleted from an
        # otherwise intact file, also differs from the placeholder -- so it prints
        # `hand-made` and passes. That is the gap cleat-review found on this PR, and it
        # is exactly the failure the message above describes ("a missing ROW is not a
        # structural difference") for a check that only covered the total-loss form.
        #
        # So assert the ROWS, in a database built from the COMMITTED baseline rather
        # than from the prior chain -- the prior chain's seed is not the artifact. This
        # is a third, narrower check rather than a widening of the second, which is the
        # shape this mode already uses for 002.
        rows_db = dbname + "_seedrows"
        admin_sql(f'DROP DATABASE IF EXISTS "{rows_db}"')
        r = admin_sql(f'CREATE DATABASE "{rows_db}"')
        if r.returncode != 0:
            fail(f"could not create {rows_db} for the row assertions:\n{r.stderr.strip()}", 2)
        try:
            # An unappliable committed file is a FINDING about the artifact, not a
            # failure to measure -- it is the shipped files that are the subject here.
            # Exit 1 rather than 2, or the reader is sent to the check when the tree is
            # what is broken. Measured: a two-line deletion from 002 left a dangling
            # `ON CONFLICT`, the apply died on syntax, and the first version of this
            # reported it as rc=2.
            applied = True
            for name in BASELINE:
                with open(os.path.join(args.committed, name)) as fh:
                    r = psql(stdin=pin + fh.read(), db=rows_db)
                if r.returncode != 0:
                    applied = False
                    differing += 1
                    print(f"UNAPPLIABLE {os.path.join(args.committed, name)}")
                    print("            a database built from the committed baseline cannot")
                    print("            apply this file, so it does not install and its seed")
                    print("            rows cannot be checked.")
                    # The FIRST line naming an ERROR -- not the last line of stderr.
                    # For a psql syntax error the last line is the caret (^), which
                    # carries no line number, and taking it discards the `LINE n:` and
                    # the `psql:<stdin>:102: ERROR: ...` above it. A syntax error is the
                    # one thing this branch exists to diagnose, so printing the caret is
                    # the bucket being right and its contents pointing nowhere. Measured:
                    # last[-1] printed `^`, and the reader needs line 102.
                    lines = [l for l in r.stderr.strip().splitlines() if l.strip()]
                    hits = [l for l in lines if "ERROR:" in l]
                    for l in (hits[:1] or lines):
                        print(f"            {l}")
                    break
            for table, where in (SEED_ROWS if applied else ()):
                r = psql(sql=f"SELECT count(*) FROM {table} WHERE {where}", db=rows_db)
                if r.returncode != 0:
                    fail(f"could not read {table}: {r.stderr.strip()}", 2)
                n = (r.stdout or "").strip()
                if not n.isdigit():
                    fail(f"reading {table} returned {n!r}", 2)
                if int(n) == 0:
                    differing += 1
                    print(f"MISSING     {path002}: {table} has no row where {where}")
                    print("            the seed 002 exists to insert is gone. No schema diff")
                    print("            can see this: a missing ROW is not a structural")
                    print("            difference, which is why it is asserted here.")
                else:
                    print(f"seeded      {table} WHERE {where}")
        finally:
            admin_sql(f'DROP DATABASE IF EXISTS "{rows_db}"')

        if differing:
            # "finding(s)", NOT "file(s)". `differing` counts findings: an emptied 002 is
            # ONE file with three missing rows, and the sentence a reader quotes said
            # "3 file(s) are not what this tree claims". The status was right and the
            # noun was wrong.
            fail(f"{differing} finding(s) against the committed baseline", 1)
        print("verify: the committed baseline is exactly what the generator produces,")
        print(f"        and the {len(SEED_ROWS)} rows its hand-assembled 002 seeds are present")
        return 0
    finally:
        shutil.rmtree(tmp, ignore_errors=True)


def find_container():
    """Locate the postgres service container when one was not named.

    By PUBLISHED PORT, not image digest: the digest is pinned in six places that
    Lint requires to move together, so matching on it would add a seventh site that
    nothing checks and would go stale on the next bump.
    """
    r = subprocess.run(["docker", "ps", "-q", "--filter", "publish=5432"],
                       capture_output=True, text=True)
    ids = [x for x in (r.stdout or "").split() if x]
    return ids[0] if len(ids) == 1 else None


def first_difference(have, want):
    """Name the first differing line, so a failure points at something to look at."""
    hl, wl = have.split(b"\n"), want.split(b"\n")
    for i in range(min(len(hl), len(wl))):
        if hl[i] != wl[i]:
            return ("first difference at line %d:\n"
                    "              committed: %s\n"
                    "              generated: %s" % (i + 1, trunc(hl[i]), trunc(wl[i])))
    return ("the shorter file ends first (committed %d line(s), generated %d)"
            % (len(hl), len(wl)))


def trunc(b):
    s = b.decode("utf-8", "replace")
    return s[:120] + "..." if len(s) > 120 else s


def fail(msg, code):
    # Flush stdout FIRST. Redirected to a pipe it is block-buffered while stderr is not,
    # so without this the summary is written before the diagnostic it summarises and
    # appears ABOVE it in a CI log -- the conclusion before the evidence. Measured
    # fixing the UNAPPLIABLE path: the exit message landed after the stdout block.
    sys.stdout.flush()
    print(f"verify-postgres-baseline: {msg}", file=sys.stderr)
    sys.exit(code)


if __name__ == "__main__":
    sys.exit(main())
