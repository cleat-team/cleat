#!/usr/bin/env python3
"""Seed a database with a PUBLISHED cleat-worker v0.4.0, then upgrade it to the
release candidate and run it.  cleat#3386, gate G4 of the 0.5.0 runbook.

    scripts/upgrade_from_v040.py seed   --owner-db DSN --state-dir DIR
    scripts/upgrade_from_v040.py verify --owner-db DSN --state-dir DIR --rc-worker PATH
    scripts/upgrade_from_v040.py --self-test

    --from-version vX.Y.Z (default v0.4.0) names the published release that seeds the database.
    The file keeps its v0.4.0 name because the 0.5.0 gate's job calls it by that path;
    D79 (0.6.0 supports an in-place upgrade from 0.5.0) is `--from-version v0.5.0
    --expect upgrade`, cleat#3735.

    --owner-db is a postgres:// or a sqlserver:// URL; the dialect is read from the scheme.  SQL Server
    is `--expect upgrade` only (the 0.5.0 refusal gate, --expect refuse, was only ever measured on PostgreSQL).

WHY THIS EXISTS.  tests/upgrade/ runs against a schema built fresh from
migrations/, never against a database a released worker wrote.  Whether a
database written by v0.4.0 can be migrated by the next release and then served
by it was a claim nothing measured.  Its first measurement (2026-10-26, before
any seeding) was already a finding: the RC's `--migrate-only` applied nothing,
because schema_migrations is keyed by version number alone and v0.4.0 had
already recorded 4, 5 and 6 for different files.

WHAT "SEEDED BY v0.4.0" MEANS HERE.  Everything that writes the database is a
published v0.4.0 artifact: the `cleat-worker` binary (or its ghcr image), the
`cleat` CLI that compiles the workflows, and the v0.4.0 Go SDK they link.  The
workflows are written to the HTTP API, never inserted with SQL, so the history,
checksums and lease rows are what a v0.4.0 worker produces.  The artifacts are
checked against the release's checksums.txt, which proves the download is what
was published, not that it is trustworthy.

EXIT STATUS, which is the contract and not decoration:
    0   passed
    1   a finding: the upgrade did something it should not
    2   UNMEASURED: a precondition failed, so nothing was learned.  This is a
        failure of the check, never a finding about cleat.
A check that measured nothing must not agree with a correct tree, and a finding
must not send the reader to debug the harness.

The verify half reports EVERY finding it can reach rather than stopping at the
first, because the first one (the RC applies no migrations) does not stop the RC
from starting, and what it does to the runs is the question the gate exists for.
"""
from __future__ import annotations

import argparse
import concurrent.futures
import hashlib
import json
import os
import platform
import re
import shutil
import signal
import socket
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.error
import urllib.request
import zipfile
from pathlib import Path

VERSION = "v0.4.0"
RELEASE_URL = f"https://github.com/cleat-team/cleat/releases/download/{VERSION}"
# The tag is vX.Y.Z, not X.Y.Z -- release.yml publishes vX.Y.Z, X.Y and latest.
IMAGE = f"ghcr.io/cleat-team/cleat-worker:{VERSION}"

# WHAT EACH PUBLISHED RELEASE IS, PINNED, so a re-pointed tag or a replaced asset cannot
# change what seeds the database without this table changing in a reviewed commit.  The
# artifacts are still checked against the release's own checksums.txt; these pins are a
# second, independent reading of the same fact, taken once (2026-11-13) from that file and
# from `docker buildx imagetools inspect`.  A release with no entry is not pinned.
#
# v0.5.0: the tag was re-pointed under D31 (fe052198 -> 8f9a59b4, a rebuilt dashboard bundle
# only).  The assets below are the ones the release run at 8f9a59b4 published; the run at
# fe052198 failed.  Both the tarball's binary and the image's binary embed the RE-POINTED
# bundle (index-DVc1oRS5.js, not the first commit's index-vw5X-b8v.js), measured with
# `strings` on 2026-11-13.  The image carries no revision label (the label steps run only on a
# real tag), so the digest is the identity.
PINS: dict[str, dict] = {
    "v0.5.0": {
        "sha256": {
            "cleat-worker_0.5.0_linux_amd64.tar.gz": "ebecedb556eae61a21b92261a241591da960ca12c98df63296da92650705c101",
            "cleat-worker_0.5.0_linux_arm64.tar.gz": "6ae189cee266d1e65662fd9a910ab3abec205b81705782fd24ef4b87855a1a13",
            "cleat_0.5.0_darwin_amd64.zip": "fc11282b564248ed56c81d8c4ad3ca6fa74a1e9edc38dd479f5f8408d0db6927",
            "cleat_0.5.0_darwin_arm64.zip": "353df0f03c2a0daa33dd9d050aed39da3b813f15503800546476326883565e6c",
            "cleat_0.5.0_linux_amd64.tar.gz": "b2c4ee30ab53c507fb2228a4af2e003790cc706453aef38b4c13c9315d1320da",
            "cleat_0.5.0_linux_arm64.tar.gz": "85d954232f27ec874affca8fde4c2b391b727b87e42b0daffd1df728a57a0c1f",
        },
        "image_digest": "sha256:bdc9968a8645a0796ad863a94e97144e9c99f729e241f73a44e33d037163aa7e",
        # Plugins whose migrations.go gained steps after v0.5.0 (`git diff v0.5.0 origin/develop --stat --
        # 'plugins/*/migrations*.go'`, 2026-11-13): after an in-place upgrade each must have recorded a HIGHER
        # step than the seeded database had.  The PostgreSQL arm of some of these steps is a no-op (they are
        # SQL Server and MySQL collation changes), but the step is still recorded in plugin_migrations, which
        # is what proves the plugin runner ran.
        "plugins_with_new_migrations": ["event-triggers", "jobqueue", "notifications", "scheduler"],
    },
}


def use_version(version: str) -> None:
    """Point every artifact name and URL at one published release."""
    global VERSION, RELEASE_URL, IMAGE
    need(re.fullmatch(r"v\d+\.\d+\.\d+", version) is not None, f"--from-version wants vX.Y.Z, got {version!r}")
    VERSION = version
    RELEASE_URL = f"https://github.com/cleat-team/cleat/releases/download/{VERSION}"
    digest = PINS.get(VERSION, {}).get("image_digest")
    IMAGE = (f"ghcr.io/cleat-team/cleat-worker@{digest}" if digest
             else f"ghcr.io/cleat-team/cleat-worker:{VERSION}")


def bare() -> str:
    """0.5.0 for v0.5.0: the form the release asset names use."""
    return VERSION[1:]
REPO = Path(__file__).resolve().parent.parent
WORKFLOWS = REPO / "scripts" / "upgrade-from-v040" / "testdata"
APP_ROLE_PASSWORD = "cleat-upgrade-test"

# Counts are knobs because the point is a spread of states, not a load test.
DEFAULT_COUNTS = {
    "ok": 120, "fail": 30, "deadletter": 10,
    "signal": 60, "timer": 40, "parent": 20,
    "spin": 8, "burst": 40,
}
TERMINAL = {"done", "failed", "dead_lettered", "cancelled", "terminated"}


class Unmeasured(Exception):
    """A precondition failed.  Exit 2: the check learned nothing."""


class Finding(Exception):
    pass


def log(msg: str) -> None:
    print(f"==> {msg}", flush=True)


def need(cond: bool, why: str) -> None:
    if not cond:
        raise Unmeasured(why)


# --------------------------------------------------------------------------
# small helpers
# --------------------------------------------------------------------------

def free_port() -> int:
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


def swap_database(dsn: str, name: str) -> str:
    """Replace the database component of a postgres URL."""
    base, _, query = dsn.partition("?")
    head, _, _ = base.rpartition("/")
    return f"{head}/{name}" + (f"?{query}" if query else "")


def app_dsn(owner: str) -> str:
    m = re.match(r"^(postgres(?:ql)?://)([^@]*@)(.*)$", owner)
    need(bool(m), "--owner-db must look like postgres://user:pass@host:port/db")
    return f"{m.group(1)}cleat_app:{APP_ROLE_PASSWORD}@{m.group(3)}"


def psql(dsn: str, sql: str, *, tuples: bool = True) -> str:
    if shutil.which("psql") is not None:
        base = ["psql"]
    else:
        # No client on this machine: borrow the one in the postgres image. The DSN
        # is a loopback address, hence host networking.
        need(shutil.which("docker") is not None,
             "neither psql nor docker is installed; psql provisions the cleat_app role")
        base = ["docker", "run", "--rm", "--network", "host", "postgres:16", "psql"]
    args = base + [dsn, "-v", "ON_ERROR_STOP=1", "-X", "-q"]
    if tuples:
        args += ["-At"]
    args += ["-c", sql]
    p = subprocess.run(args, capture_output=True, text=True)
    if p.returncode != 0:
        raise Unmeasured(f"psql failed ({sql[:60]!r}): {p.stderr.strip()[:300]}")
    return p.stdout.strip()


def psql_or_empty(dsn: str, sql: str) -> str:
    try:
        return psql(dsn, sql)
    except Unmeasured:
        return ""


# --------------------------------------------------------------------------
# dialects: PostgreSQL, and SQL Server (cleat#3735 PR 2)
#
# Everything in the seed and verify halves that is SQL goes through one of these, so the logic
# above them is written once.  The PostgreSQL methods are the statements this file always ran.
# --------------------------------------------------------------------------

MSSQLQ_SRC = "./scripts/upgrade-from-v040/sqlq"
_sqlq: Path | None = None


def sqlq_binary() -> Path:
    """The SQL Server and MySQL query helper (scripts/upgrade-from-v040/sqlq), built once per process.

    A Go program rather than sqlcmd for two reasons: the owner login is NOT exempt from row-level
    security on SQL Server, so a read of a tenant table needs the session's tenant_id set on the SAME
    connection as the query, and a CI runner is not promised a sqlcmd.
    """
    global _sqlq
    if _sqlq is None:
        need(shutil.which("go") is not None, "go is not installed; the SQL Server helper is a Go program")
        out = Path(tempfile.gettempdir()) / f"cleat-sqlq-{os.getpid()}"
        p = subprocess.run(["go", "build", "-o", str(out), MSSQLQ_SRC], capture_output=True, text=True, cwd=REPO)
        need(p.returncode == 0, f"could not build the SQL Server helper:\n{p.stderr.strip()[-400:]}")
        _sqlq = out
    return _sqlq


def sql_run(driver: str, dsn: str, sql: str, tenant: str | None = None, pre: str | None = None) -> str:
    args = [str(sqlq_binary()), "-driver", driver, "-dsn", dsn, "-sql", sql]
    if tenant:
        args += ["-tenant", tenant]
    if pre:
        args += ["-pre", pre]
    p = subprocess.run(args, capture_output=True, text=True)
    if p.returncode != 0:
        raise Unmeasured(f"{driver} query failed ({sql[:60]!r}): {p.stderr.strip()[:300]}")
    return p.stdout.strip()


def mssql_run(dsn: str, sql: str, tenant: str | None = None) -> str:
    return sql_run("sqlserver", dsn, sql, tenant)


class Postgres:
    name = "postgres"
    driver_args: list[str] = []
    bad_log = ("panic:", "does not exist")

    def sql(self, dsn: str, q: str, tenant: str | None = None) -> str:
        return psql(dsn, q)

    def sql_or_empty(self, dsn: str, q: str, tenant: str | None = None) -> str:
        return psql_or_empty(dsn, q)

    def app_dsn(self, owner: str) -> str:
        return app_dsn(owner)

    def swap_database(self, dsn: str, name: str) -> str:
        return swap_database(dsn, name)

    def provision_app_role(self, owner: str) -> None:
        psql(owner, f"ALTER ROLE cleat_app LOGIN PASSWORD '{APP_ROLE_PASSWORD}'")

    def create_database(self, owner: str, name: str) -> None:
        psql(owner, f"CREATE DATABASE {name}")

    def drop_database(self, owner: str, name: str) -> None:
        psql(owner, f"DROP DATABASE IF EXISTS {name}")

    # Where a run's status lives depends on the release that wrote the database: workflow_instances
    # up to v0.4.0, workflow_leases from v0.5.0 (the storage split, which left workflow_instances
    # without a status column).  Asked of the database, not assumed from the version string.
    def status_table(self, owner: str, tenant: str | None) -> str:
        return "workflow_leases" if psql(owner, "SELECT to_regclass('public.workflow_leases')").strip() else "workflow_instances"

    schema_versions_sql = "SELECT string_agg(version, ' ' ORDER BY version) FROM schema_migrations"
    plugin_steps_sql = "SELECT plugin_name||' '||max(version) FROM plugin_migrations GROUP BY plugin_name ORDER BY 1"
    # One line per run: its event rows, in step order.  event_history is the history a resumed run replays and
    # verifies, so a migration that rewrote, dropped or re-ordered a row would break a run that is merely PARKED,
    # which a "did the runs finish" check finds only if it finishes.  The digest covers EVERY column the seeding
    # release's table had, whole row by row, not a chosen few: the docs say the history is left as that release wrote
    # it, and a payload column rewritten on a run passes a digest of step, event_type and checksum alone.  The column
    # list is read from the seeded database and recorded in the manifest, so a column a later migration ADDS is not
    # compared and one it rewrites is.
    history_columns_sql = ("SELECT string_agg(column_name, ',' ORDER BY column_name) FROM information_schema.columns "
                           "WHERE table_schema = 'public' AND table_name = 'event_history'")

    def history_digest_sql(self, columns: str) -> str:
        cols = [c for c in columns.split(",") if c]
        need(bool(cols), "the seeded database's event_history column list is empty, so its history cannot be digested")
        row = ", ".join(f'"{c}"' for c in cols)
        return (f"SELECT workflow_id||' '||count(*)||' '||md5(string_agg(row({row})::text, ',' ORDER BY step, event_type, checksum)) "
                "FROM event_history GROUP BY workflow_id ORDER BY workflow_id")

    def status_snapshot_sql(self, table: str) -> str:
        return f"SELECT status, count(*) FROM {table} GROUP BY 1 ORDER BY 1"

    def queued_sql(self, table: str, ids: str) -> str:
        return (f"SELECT count(*) FROM {table} WHERE status = 'ready' AND assigned_to IS NULL AND id IN ({ids})")

    def tamper_checksums(self, owner: str, tenant: str | None, rid: str) -> int:
        rows = psql(owner, f"UPDATE event_history SET checksum = 'tampered-' || checksum "
                           f"WHERE workflow_id = '{rid}' RETURNING 1")
        return len(rows.splitlines())

    def fingerprint(self, dsn: str, tenant: str | None = None) -> dict[str, list[str]]:
        return schema_fingerprint(dsn)


class SQLServer:
    name = "mssql"
    driver_args = ["-driver", "mssql"]
    bad_log = ("panic:", "Invalid object name")

    def sql(self, dsn: str, q: str, tenant: str | None = None) -> str:
        return mssql_run(dsn, q, tenant)

    def sql_or_empty(self, dsn: str, q: str, tenant: str | None = None) -> str:
        try:
            return mssql_run(dsn, q, tenant)
        except Unmeasured:
            return ""

    def app_dsn(self, owner: str) -> str:
        m = re.match(r"^(sqlserver://)([^@]*@)(.*)$", owner)
        need(bool(m), "--owner-db must look like sqlserver://user:pass@host:port?database=db")
        return f"{m.group(1)}cleat_app:{APP_ROLE_PASSWORD}@{m.group(3)}"

    def swap_database(self, dsn: str, name: str) -> str:
        need(re.search(r"[?&]database=[^&]*", dsn) is not None, "--owner-db has no ?database=")
        return re.sub(r"([?&]database=)[^&]*", lambda mm: mm.group(1) + name, dsn)

    def provision_app_role(self, owner: str) -> None:
        # What deploy/mssql/900-app-role.sh does, minus its sqlcmd: a login, a user, and membership of the
        # role the migrations created.  The owner login stays the migrator.
        self.sql(owner, f"""
            IF NOT EXISTS (SELECT 1 FROM sys.server_principals WHERE name = N'cleat_app')
                CREATE LOGIN cleat_app WITH PASSWORD = N'{APP_ROLE_PASSWORD}', CHECK_POLICY = OFF;
            IF NOT EXISTS (SELECT 1 FROM sys.database_principals WHERE name = N'cleat_app')
                CREATE USER cleat_app FOR LOGIN cleat_app;
            ALTER ROLE cleat_app_role ADD MEMBER cleat_app;""")

    def create_database(self, owner: str, name: str) -> None:
        self.sql(owner, f"CREATE DATABASE [{name}]")

    def drop_database(self, owner: str, name: str) -> None:
        self.sql_or_empty(owner, f"IF DB_ID(N'{name}') IS NOT NULL BEGIN "
                                 f"ALTER DATABASE [{name}] SET SINGLE_USER WITH ROLLBACK IMMEDIATE; "
                                 f"DROP DATABASE [{name}] END")

    # No workflow_leases table exists on SQL Server at v0.5.0 (status stayed on workflow_instances).
    def status_table(self, owner: str, tenant: str | None) -> str:
        return ("workflow_leases" if self.sql(owner, "SELECT CASE WHEN OBJECT_ID(N'workflow_leases') IS NOT NULL "
                                                     "THEN 1 ELSE 0 END") == "1" else "workflow_instances")

    schema_versions_sql = ("SELECT STRING_AGG(CAST(version AS nvarchar(20)), ' ') WITHIN GROUP (ORDER BY version) "
                           "FROM schema_migrations")
    plugin_steps_sql = ("SELECT plugin_name + ' ' + CAST(MAX(version) AS varchar(10)) FROM plugin_migrations "
                        "GROUP BY plugin_name ORDER BY plugin_name")
    # event_history is row-level secured: this is read with the tenant's session context.  Every column the seeded
    # release's table had goes into the digest (see Postgres.history_columns_sql).
    history_columns_sql = ("SELECT STRING_AGG(CAST(column_name AS nvarchar(max)), ',') WITHIN GROUP (ORDER BY column_name) "
                           "FROM INFORMATION_SCHEMA.COLUMNS WHERE table_schema = 'dbo' AND table_name = 'event_history'")

    def history_digest_sql(self, columns: str) -> str:
        cols = [c for c in columns.split(",") if c]
        need(bool(cols), "the seeded database's event_history column list is empty, so its history cannot be digested")
        # An explicit collation on each: after the 0.6.0 migrations (D60) the columns do not all share one, and
        # concatenating across collations is error 4191.  It changes no byte the digest reads.
        row = " + '|' + ".join(f"ISNULL(CAST([{c}] AS nvarchar(max)) COLLATE Latin1_General_100_BIN2, N'~')" for c in cols)
        return ("SELECT workflow_id + ' ' + CAST(COUNT(*) AS varchar(20)) + ' ' + CONVERT(varchar(32), "
                f"HASHBYTES('MD5', STRING_AGG({row}, ',') WITHIN GROUP (ORDER BY step, event_type, checksum)), 2) "
                "FROM event_history GROUP BY workflow_id ORDER BY workflow_id")

    def status_snapshot_sql(self, table: str) -> str:
        return f"SELECT status, COUNT(*) FROM {table} GROUP BY status ORDER BY status"

    def queued_sql(self, table: str, ids: str) -> str:
        return (f"SELECT COUNT(*) FROM {table} WHERE status = 'ready' AND assigned_to IS NULL AND id IN ({ids})")

    def tamper_checksums(self, owner: str, tenant: str | None, rid: str) -> int:
        out = self.sql(owner, f"SET NOCOUNT ON; UPDATE event_history SET checksum = 'tampered-' + checksum "
                              f"WHERE workflow_id = '{rid}'; SELECT @@ROWCOUNT", tenant)
        return int(out or 0)

    def fingerprint(self, dsn: str, tenant: str | None = None) -> dict[str, list[str]]:
        """What the schema IS, as sorted lines per facet.  Collations are part of it on purpose: D60 changed several."""
        # Columns are returned as columns, not concatenated in SQL: the catalog views carry a different
        # collation from this database's literals, and a `+` between them is error 451.  sqlq joins with "|".
        facets = {
            "columns": "SELECT table_schema, table_name, column_name, data_type, is_nullable, character_maximum_length, "
                       "collation_name FROM INFORMATION_SCHEMA.COLUMNS",
            "indexes": "SELECT s.name, t.name, CASE WHEN kc.is_system_named = 1 THEN '(system-named)' ELSE i.name END, "
                       "i.type_desc, i.is_unique, i.filter_definition FROM sys.indexes i "
                       "JOIN sys.tables t ON t.object_id = i.object_id JOIN sys.schemas s ON s.schema_id = t.schema_id "
                       "LEFT JOIN sys.key_constraints kc ON kc.parent_object_id = i.object_id AND kc.unique_index_id = i.index_id "
                       "WHERE i.name IS NOT NULL",
            "functions": "SELECT s.name, o.name, CONVERT(varchar(32), HASHBYTES('MD5', ISNULL(OBJECT_DEFINITION(o.object_id), N'')), 2) "
                         "FROM sys.objects o JOIN sys.schemas s ON s.schema_id = o.schema_id "
                         "WHERE o.type IN ('P', 'FN', 'IF', 'TF', 'V') AND o.is_ms_shipped = 0",
            # A system-generated constraint name carries a hex suffix that differs per database; the
            # name is replaced by its kind so two databases built the same way read the same.
            "constraints": "SELECT s.name, t.name, CASE WHEN COALESCE(dc.is_system_named, cc.is_system_named, kc.is_system_named, "
                           "fk.is_system_named) = 1 THEN '(system-named)' ELSE c.name END, c.type, "
                           "COL_NAME(c.parent_object_id, dc.parent_column_id), OBJECT_DEFINITION(c.object_id) "
                           "FROM sys.objects c JOIN sys.tables t ON c.parent_object_id = t.object_id "
                           "JOIN sys.schemas s ON s.schema_id = t.schema_id "
                           "LEFT JOIN sys.default_constraints dc ON dc.object_id = c.object_id "
                           "LEFT JOIN sys.check_constraints cc ON cc.object_id = c.object_id "
                           "LEFT JOIN sys.key_constraints kc ON kc.object_id = c.object_id "
                           "LEFT JOIN sys.foreign_keys fk ON fk.object_id = c.object_id "
                           "WHERE c.type IN ('C', 'D', 'F', 'PK', 'UQ')",
            "triggers": "SELECT s.name, t.name, tr.name, CONVERT(varchar(32), HASHBYTES('MD5', "
                        "ISNULL(OBJECT_DEFINITION(tr.object_id), N'')), 2) FROM sys.triggers tr "
                        "JOIN sys.tables t ON tr.parent_id = t.object_id JOIN sys.schemas s ON s.schema_id = t.schema_id",
            "policies": "SELECT OBJECT_SCHEMA_NAME(p.object_id), p.name, OBJECT_SCHEMA_NAME(pr.target_object_id), "
                        "OBJECT_NAME(pr.target_object_id), pr.predicate_type_desc, p.is_enabled, "
                        "CONVERT(varchar(32), HASHBYTES('MD5', pr.predicate_definition), 2) FROM sys.security_policies p "
                        "JOIN sys.security_predicates pr ON pr.object_id = p.object_id",
        }
        return {k: sorted(self.sql(dsn, q, tenant).splitlines()) for k, q in facets.items()}


class MySQL:
    """MySQL is single-tenant by owner decision, so there is no row-level security, no tenant to set and no
    separate application login: the owner login is the application's too.  Its tenant is the default one
    the migrations create, and -generate-api-key mints the key for it.

    ITS DATA IS NOT IN THE DATABASE YOU NAME.  MySQL keeps a tenant's tables (workflow_instances,
    event_history, ...) in a database of their own, `cleat_<tenant id with underscores>`, so `-db .../cleat`
    holds the ledgers and the admin tables and the runs are in `cleat_00000000_0000_...` (measured 2026-11-13:
    348 rows in workflow_instances there, 0 in `cleat`).  A tenant argument therefore selects that database.

    THE PREFIX IS `cleat`, NOT THE NAME OF THE DATABASE IN THE DSN, and it is per SERVER: a seed against a database
    called mut_hist created `cleat_0000...` (log: "creating tenant database"), and found the one a previous seed had
    left there.  So this harness wants a database named `cleat` on a server nothing else has used."""
    name = "mysql"
    driver_args = ["-driver", "mysql"]
    bad_log = ("panic:", "doesn't exist")
    default_tenant = "00000000-0000-0000-0000-000000000000"
    # GROUP_CONCAT truncates at 1024 bytes unless the session raises this, and a truncated digest is a digest
    # of less than the history: it would read as equal for rows it never saw.
    PRE = "SET SESSION group_concat_max_len = 16777216"

    def tenant_dsn(self, dsn: str, tenant: str | None) -> str:
        if not tenant:
            return dsn
        return self.swap_database(dsn, f"cleat_{tenant.replace('-', '_')}")

    def sql(self, dsn: str, q: str, tenant: str | None = None) -> str:
        return sql_run("mysql", self.tenant_dsn(dsn, tenant), q, pre=self.PRE)

    def sql_or_empty(self, dsn: str, q: str, tenant: str | None = None) -> str:
        try:
            return self.sql(dsn, q, tenant)
        except Unmeasured:
            return ""

    def app_dsn(self, owner: str) -> str:
        need(re.match(r"^[^@]+@tcp\([^)]+\)/cleat(\?|$)", owner) is not None,
             "--owner-db must look like user:pass@tcp(host:port)/cleat?parseTime=true (the database must be named cleat: "
             "the worker names the tenant's database cleat_<tenant id> whatever the DSN says)")
        return owner

    def swap_database(self, dsn: str, name: str) -> str:
        return re.sub(r"(\)/)[^?]*", lambda mm: mm.group(1) + name, dsn, count=1)

    def provision_app_role(self, owner: str) -> None:
        pass

    def create_database(self, owner: str, name: str) -> None:
        self.sql(owner, f"CREATE DATABASE `{name}`")

    def drop_database(self, owner: str, name: str) -> None:
        self.sql_or_empty(owner, f"DROP DATABASE IF EXISTS `{name}`")

    def status_table(self, owner: str, tenant: str | None) -> str:
        return ("workflow_leases" if self.sql(owner, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() "
                                                     "AND table_name = 'workflow_leases'") != "0" else "workflow_instances")

    schema_versions_sql = "SELECT GROUP_CONCAT(version ORDER BY version SEPARATOR ' ') FROM schema_migrations"
    plugin_steps_sql = ("SELECT CONCAT(plugin_name, ' ', MAX(version)) FROM plugin_migrations "
                        "GROUP BY plugin_name ORDER BY plugin_name")
    # Every column the seeded release's table had goes into the digest (see Postgres.history_columns_sql).  Read through
    # the tenant schema, where the table is.
    history_columns_sql = ("SELECT GROUP_CONCAT(column_name ORDER BY column_name SEPARATOR ',') FROM information_schema.columns "
                           "WHERE table_schema = DATABASE() AND table_name = 'event_history'")

    def history_digest_sql(self, columns: str) -> str:
        cols = [c for c in columns.split(",") if c]
        need(bool(cols), "the seeded database's event_history column list is empty, so its history cannot be digested")
        row = ", '|', ".join(f"IFNULL(CAST(`{c}` AS CHAR), '~')" for c in cols)
        return ("SELECT CONCAT(workflow_id, ' ', COUNT(*), ' ', MD5(GROUP_CONCAT(CONCAT(" + row + ") "
                "ORDER BY step, event_type, checksum SEPARATOR ','))) FROM event_history GROUP BY workflow_id ORDER BY workflow_id")

    def status_snapshot_sql(self, table: str) -> str:
        return f"SELECT status, COUNT(*) FROM {table} GROUP BY status ORDER BY status"

    def queued_sql(self, table: str, ids: str) -> str:
        return (f"SELECT COUNT(*) FROM {table} WHERE status = 'ready' AND assigned_to IS NULL AND id IN ({ids})")

    def tamper_checksums(self, owner: str, tenant: str | None, rid: str) -> int:
        # An UPDATE through the query helper returns no rows, so the effect is read back.
        self.sql(owner, f"UPDATE event_history SET checksum = CONCAT('tampered-', checksum) WHERE workflow_id = '{rid}'", tenant)
        return int(self.sql(owner, f"SELECT COUNT(*) FROM event_history WHERE workflow_id = '{rid}' "
                                   f"AND checksum LIKE 'tampered-%'", tenant) or 0)

    def fingerprint(self, dsn: str, tenant: str | None = None) -> dict[str, list[str]]:
        """What the schema IS, as sorted lines per facet; collations are part of it (D60 changed several).

        Of the schema the DSN names, or of the tenant's schema when `tenant` is given: every query is about
        DATABASE(), and the DSN decides which that is."""
        dsn = self.tenant_dsn(dsn, tenant)
        facets = {
            "columns": "SELECT table_name, column_name, column_type, is_nullable, collation_name, IFNULL(column_default, ''), extra "
                       "FROM information_schema.columns WHERE table_schema = DATABASE()",
            "indexes": "SELECT table_name, index_name, non_unique, index_type, GROUP_CONCAT(column_name ORDER BY seq_in_index) "
                       "FROM information_schema.statistics WHERE table_schema = DATABASE() "
                       "GROUP BY table_name, index_name, non_unique, index_type",
            "functions": "SELECT routine_name, MD5(IFNULL(routine_definition, '')) FROM information_schema.routines "
                         "WHERE routine_schema = DATABASE()",
            "constraints": "SELECT table_name, constraint_name, constraint_type FROM information_schema.table_constraints "
                           "WHERE constraint_schema = DATABASE() "
                           "UNION ALL SELECT constraint_name, update_rule, delete_rule FROM information_schema.referential_constraints "
                           "WHERE constraint_schema = DATABASE() "
                           "UNION ALL SELECT constraint_name, MD5(check_clause), '' FROM information_schema.check_constraints "
                           "WHERE constraint_schema = DATABASE()",
            "triggers": "SELECT trigger_name, event_object_table, MD5(action_statement) FROM information_schema.triggers "
                        "WHERE trigger_schema = DATABASE()",
        }
        return {k: sorted(self.sql(dsn, q).splitlines()) for k, q in facets.items()}

    def extra_comparisons(self, owner: str, tenant: str, fresh_fp: dict[str, list[str]]) -> list[tuple[str, dict[str, list[str]]]]:
        """The tenant's own schema, which the worker made when it first served the tenant, must match a fresh
        release candidate's core schema too: it is where the runs live, and `--migrate-only` is the only thing that
        migrates it.  (Whether it is migrated at all is what this asks.)"""
        return [("the tenant's schema against a fresh release candidate's core schema", self.fingerprint(owner, tenant))]


def dialect_of(dsn: str):
    if dsn.startswith(("postgres://", "postgresql://")):
        return Postgres()
    if dsn.startswith("sqlserver://"):
        return SQLServer()
    if re.match(r"^[^@/]+@tcp\(", dsn):
        return MySQL()
    raise Unmeasured("--owner-db must be a postgres:// or sqlserver:// URL, or a MySQL user:pass@tcp(host:port)/db DSN")


def http(method: str, url: str, key: str | None = None, body: dict | None = None,
         timeout: float = 20.0) -> tuple[int, object]:
    data = None
    headers = {}
    if body is not None:
        data = json.dumps(body).encode()
        headers["Content-Type"] = "application/json"
    if key:
        headers["Authorization"] = f"Bearer {key}"
    req = urllib.request.Request(url, data=data, method=method, headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            raw = r.read()
            code = r.status
    except urllib.error.HTTPError as e:
        raw = e.read()
        code = e.code
    except (urllib.error.URLError, TimeoutError, ConnectionError) as e:
        return 0, str(e)
    try:
        return code, json.loads(raw) if raw else None
    except ValueError:
        return code, raw.decode(errors="replace")


# --------------------------------------------------------------------------
# the published v0.4.0 artifacts
# --------------------------------------------------------------------------

def sha256_of(path: Path) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def fetch(url: str, dest: Path) -> None:
    dest.parent.mkdir(parents=True, exist_ok=True)
    try:
        with urllib.request.urlopen(url, timeout=120) as r, open(dest, "wb") as f:
            shutil.copyfileobj(r, f)
    except (urllib.error.URLError, TimeoutError) as e:
        raise Unmeasured(f"could not download {url}: {e}")


def machine() -> tuple[str, str]:
    os_ = {"Linux": "linux", "Darwin": "darwin"}.get(platform.system())
    arch = {"x86_64": "amd64", "AMD64": "amd64", "arm64": "arm64", "aarch64": "arm64"}.get(platform.machine())
    need(os_ is not None and arch is not None, f"no {VERSION} artifact for {platform.system()}/{platform.machine()}")
    return os_, arch


def release_artifact(cache: Path, name: str) -> Path:
    """Download one release asset and verify it against checksums.txt."""
    sums = cache / "checksums.txt"
    if not sums.exists():
        fetch(f"{RELEASE_URL}/checksums.txt", sums)
    want = None
    for line in sums.read_text().splitlines():
        parts = line.split()
        if len(parts) == 2 and parts[1] == name:
            want = parts[0]
    need(want is not None, f"{name} is not listed in the {VERSION} checksums.txt")
    dest = cache / name
    if not dest.exists():
        fetch(f"{RELEASE_URL}/{name}", dest)
    got = sha256_of(dest)
    if got != want:
        dest.unlink()
        raise Unmeasured(f"{name}: sha256 {got} does not match checksums.txt {want}")
    pinned = PINS.get(VERSION, {}).get("sha256", {}).get(name)
    if PINS.get(VERSION) is not None and pinned is None:
        raise Unmeasured(f"{name} is not in this harness's pin table for {VERSION}; add its sha256 from a reviewed read of checksums.txt")
    if pinned is not None and got != pinned:
        dest.unlink()
        raise Unmeasured(f"{name}: sha256 {got} matches checksums.txt but not the pin {pinned} in this harness. "
                         f"The published {VERSION} asset changed since the pin was taken; that is a finding about the release, not about the upgrade")
    return dest


def unpack(archive: Path, member: str, out: Path) -> Path:
    out.mkdir(parents=True, exist_ok=True)
    if archive.suffix == ".zip":
        with zipfile.ZipFile(archive) as z:
            z.extract(member, out)
    else:
        with tarfile.open(archive) as t:
            t.extract(member, out, filter="data")
    path = out / member
    path.chmod(0o755)
    return path


class Published:
    """The published cleat-worker (VERSION), as a binary on Linux or its image elsewhere."""

    def __init__(self, cache: Path, mode: str, local_bin: Path | None = None):
        self.mode = mode
        self.cache = cache
        self.bin: Path | None = None
        os_, arch = machine()
        if local_bin is not None:
            # THE POSITIVE CONTROL, not a seed of v0.4.0: the harness's own "all
            # green" path can only be exercised by an upgrade that is trivially
            # fine, i.e. seeding with the same binary verify then runs.
            need(local_bin.exists() and os.access(local_bin, os.X_OK), f"--worker-bin {local_bin} is not an executable")
            self.mode = "binary"
            self.bin = local_bin
        elif mode == "binary":
            need(os_ == "linux", f"cleat-worker {VERSION} binaries are Linux only; use --worker-mode docker")
            tarball = release_artifact(cache, f"cleat-worker_{bare()}_linux_{arch}.tar.gz")
            self.bin = unpack(tarball, "cleat-worker", cache / "worker")
        else:
            need(shutil.which("docker") is not None, "docker is not installed")
            p = subprocess.run(["docker", "pull", "-q", IMAGE], capture_output=True, text=True)
            need(p.returncode == 0, f"could not pull {IMAGE}: {p.stderr.strip()[:200]}")

    def command(self, args: list[str], name: str | None = None, detach: bool = False) -> list[str]:
        if self.mode == "binary":
            return [str(self.bin)] + args
        # Host networking: the DSNs and --api-addr are loopback addresses.
        cmd = ["docker", "run", "--network", "host"]
        cmd += ["-d", "--name", name] if detach else ["--rm"]
        return cmd + [IMAGE] + args

    def run(self, args: list[str]) -> subprocess.CompletedProcess:
        return subprocess.run(self.command(args), capture_output=True, text=True)


def cleat_cli(cache: Path) -> Path:
    os_, arch = machine()
    if os_ == "darwin":
        return unpack(release_artifact(cache, f"cleat_{bare()}_darwin_{arch}.zip"), "cleat", cache / "cli")
    return unpack(release_artifact(cache, f"cleat_{bare()}_linux_{arch}.tar.gz"), "cleat", cache / "cli")


def build_workflows(cli: Path, work: Path) -> dict[str, Path]:
    """Compile the three seed workflows with the v0.4.0 cleat against the v0.4.0 SDK.

    A throwaway module, written here, rather than a go.mod in the tree: a
    go.mod requiring cleat v0.4.0 would be moved to v0.5.0 by the release's
    require-bump step (and checked by TestEveryRootRequireNamesTheSameVersion),
    and then this would stop compiling against the SDK that wrote the history.
    """
    need(shutil.which("go") is not None, "go is not installed; it compiles the seed workflows")
    mod = work / "mod"
    if mod.exists():
        shutil.rmtree(mod)
    mod.mkdir(parents=True)
    # GOWORK=off: the module is a standalone one this function writes, and the state
    # directory may sit inside a checkout that has a go.work (CI puts it in
    # $GITHUB_WORKSPACE). Go then finds that go.work and refuses -mod=mod, which
    # is how the first CI run ended UNMEASURED on the Linux path. Locally the state
    # directory was in /tmp, outside any workspace, so nothing showed it.
    env = dict(os.environ, GOFLAGS="-mod=mod", GOWORK="off")
    for cmd in (["go", "mod", "init", "seedwf"], ["go", "get", f"github.com/cleat-team/cleat/cleat@{VERSION}"]):
        p = subprocess.run(cmd, cwd=mod, env=env, capture_output=True, text=True)
        need(p.returncode == 0, f"{' '.join(cmd)}: {p.stderr.strip()[:300]}")
    out: dict[str, Path] = {}
    for name in ("seed-plain", "seed-waiter", "seed-parent"):
        shutil.copytree(WORKFLOWS / name, mod / name)
        dest = work / "out" / name
        p = subprocess.run([str(cli), "build", "--target", "go", "-o", str(dest), f"./{name}/"],
                           cwd=mod, env=env, capture_output=True, text=True)
        need(p.returncode == 0, f"cleat build {name}: {(p.stdout + p.stderr).strip()[-400:]}")
        wasm = dest / f"{name}.wasm"
        need(wasm.exists(), f"cleat build {name} produced no {wasm.name}")
        out[name] = wasm
    return out


# --------------------------------------------------------------------------
# a service that always fails, so a retry policy can really exhaust
# --------------------------------------------------------------------------

def start_failing_service() -> tuple[object, str]:
    """A loopback HTTP service that answers every call 503.

    A dead-lettered run is one whose last call ran out of retries
    (endedOnAnExhaustedCall), which needs a service that is reachable and keeps
    failing.  A missing service is a permanent error and ends the run "failed".
    """
    import http.server
    import threading

    class H(http.server.BaseHTTPRequestHandler):
        def do_POST(self):  # noqa: N802
            n = int(self.headers.get("Content-Length") or 0)
            if n:
                self.rfile.read(n)
            self.send_response(503)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"error":"seeded: always unavailable"}')

        do_GET = do_POST  # noqa: N815

        def log_message(self, *_):
            pass

    srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv, f"http://127.0.0.1:{srv.server_address[1]}"


# --------------------------------------------------------------------------
# running a worker
# --------------------------------------------------------------------------

class Worker:
    def __init__(self, argv: list[str], log_path: Path, api: str, container: str | None = None):
        self.argv = argv
        self.api = api
        self.container = container
        self.log_path = log_path
        self.log = open(log_path, "ab")
        self.proc = subprocess.Popen(argv, stdout=self.log, stderr=subprocess.STDOUT,
                                     start_new_session=True)

    def wait_healthy(self, budget: float = 90.0) -> None:
        end = time.monotonic() + budget
        while time.monotonic() < end:
            if self.proc.poll() is not None and self.container is None:
                raise Unmeasured(f"the worker exited ({self.proc.returncode}) before it answered:\n{self.tail()}")
            code, _ = http("GET", f"{self.api}/healthz", timeout=3)
            if code == 200:
                return
            time.sleep(1)
        raise Unmeasured(f"the worker never answered /healthz within {budget:.0f}s:\n{self.tail()}")

    def kill9(self) -> None:
        """SIGKILL, not SIGTERM: a graceful stop is the case that already works."""
        if self.container:
            subprocess.run(["docker", "kill", "--signal", "KILL", self.container], capture_output=True)
            subprocess.run(["docker", "rm", "-f", self.container], capture_output=True)
        else:
            try:
                os.killpg(self.proc.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
        try:
            self.proc.wait(timeout=20)
        except subprocess.TimeoutExpired:
            pass

    def stop(self) -> None:
        if self.container:
            subprocess.run(["docker", "rm", "-f", self.container], capture_output=True)
        elif self.proc.poll() is None:
            try:
                os.killpg(self.proc.pid, signal.SIGTERM)
                self.proc.wait(timeout=30)
            except (ProcessLookupError, subprocess.TimeoutExpired):
                self.kill9()

    def tail(self, n: int = 40) -> str:
        try:
            return "\n".join(self.log_path.read_text(errors="replace").splitlines()[-n:])
        except OSError:
            return ""


# --------------------------------------------------------------------------
# seed
# --------------------------------------------------------------------------

def provision(worker_run, owner: str, d) -> tuple[str, str]:
    """Create an org, a tenant and an API key; return (tenant, key)."""
    def field(out: str, label: str) -> str:
        m = re.search(rf"{label}:\s+(\S+)", out)
        return m.group(1) if m else ""

    if d.name == "mssql":
        # v0.5.0's -create-org and -create-tenant are PostgreSQL only ("auth: CreateOrg is not implemented for
        # mssql", measured 2026-11-13), so on SQL Server the rows are written with SQL, as the engine's own tests
        # do.  -generate-api-key does work there, and is what mints the key.  Fixed ids keep reruns readable.
        org, tenant = "7f1c0a52-3b4e-4d6a-9c1e-0d79a0000001", "7f1c0a52-3b4e-4d6a-9c1e-0d79a0000002"
        d.sql(owner, f"INSERT INTO admin.orgs (org_id, name) VALUES ('{org}', N'upgrade-seed-org'); "
                     f"INSERT INTO admin.tenants (tenant_id, name, display_name, org_id) "
                     f"VALUES ('{tenant}', N'upgrade-seed-tenant', N'upgrade-seed-tenant', '{org}')")
    elif d.name == "mysql":
        # MySQL is single-tenant: v0.5.0 refuses --create-tenant there, and the default tenant is the tenant.
        tenant = d.default_tenant
    else:
        org = field(worker_run(["-db", owner, "-create-org", "upgrade-seed-org"]), "Org ID")
        need(bool(org), "-create-org produced no org id")
        tenant = field(worker_run(["-db", owner, "-create-tenant", "upgrade-seed-tenant", "-org", org]), "Tenant ID")
        need(bool(tenant), "-create-tenant produced no tenant id")
    key = field(worker_run(["-db", owner, "-generate-api-key", tenant]), "Key")
    need(bool(key), "-generate-api-key produced no key")
    return tenant, key


def upload(api: str, key: str, name: str, wasm: Path) -> None:
    import base64
    body = {"name": name, "wasm_bytes_base64": base64.b64encode(wasm.read_bytes()).decode()}
    code, resp = http("POST", f"{api}/api/definitions", key, body, timeout=60)
    need(code in (200, 201), f"POST /api/definitions for {name} answered {code}: {str(resp)[:200]}")


def start_run(api: str, key: str, name: str, inp: dict) -> str:
    # 429 is the tenant's rate limit, which a burst of starts from the seed can hit (observed on MySQL, 2026-11-13); it
    # says how long to wait.
    for _ in range(8):
        code, resp = http("POST", f"{api}/api/workflows/{name}/start", key, {"input": inp})
        if code != 429:
            break
        time.sleep(min(5.0, (resp.get("retry_after_ms", 1000) if isinstance(resp, dict) else 1000) / 1000))
    if code not in (200, 201) or not isinstance(resp, dict) or not resp.get("id"):
        raise Unmeasured(f"start {name} {inp} answered {code}: {str(resp)[:200]}")
    return resp["id"]


def status_of(api: str, key: str, rid: str) -> str:
    code, resp = http("GET", f"{api}/api/workflows/{rid}", key, timeout=10)
    if code == 200 and isinstance(resp, dict):
        return str(resp.get("status", ""))
    return ""


def query_state(api: str, key: str, rid: str, k: str) -> str:
    code, resp = http("GET", f"{api}/api/workflows/{rid}/query?key={k}", key, timeout=10)
    if code == 200 and isinstance(resp, dict):
        v = resp.get("value", resp.get(k, ""))
        return v if isinstance(v, str) else json.dumps(v)
    return ""


def wait_for(pred, budget: float, what: str, step: float = 1.0):
    end = time.monotonic() + budget
    last = None
    while time.monotonic() < end:
        last = pred()
        if last:
            return last
        time.sleep(step)
    raise Unmeasured(f"timed out after {budget:.0f}s waiting for {what}")


def cmd_seed(a) -> int:
    counts = dict(DEFAULT_COUNTS)
    for kv in a.count or []:
        k, _, v = kv.partition("=")
        need(k in counts and v.isdigit(), f"--count wants one of {sorted(counts)} as name=N, got {kv!r}")
        counts[k] = int(v)
    state = Path(a.state_dir).resolve()
    state.mkdir(parents=True, exist_ok=True)
    cache = Path(a.cache_dir).resolve()
    owner = a.owner_db
    d = dialect_of(owner)
    app = d.app_dsn(owner)

    mode = a.worker_mode or ("binary" if platform.system() == "Linux" else "docker")
    local_bin = Path(a.worker_bin).resolve() if a.worker_bin else None
    if local_bin:
        mode = "binary"
        log(f"CONTROL RUN: seeding with {local_bin}, NOT the published {VERSION} worker; "
            "this says nothing about the upgrade")
    else:
        log(f"{VERSION} artifacts ({mode} mode)")
    v040 = Published(cache, mode, local_bin)
    cli = cleat_cli(cache)
    wasm = build_workflows(cli, state / "build")

    def one_shot(args: list[str]) -> str:
        args = d.driver_args + args
        p = v040.run(args)
        if p.returncode != 0:
            raise Unmeasured(f"{VERSION} worker {args[:3]} exited {p.returncode}: {(p.stdout + p.stderr).strip()[-400:]}")
        return p.stdout + p.stderr

    log(f"migrating the database with {VERSION} and provisioning the cleat_app role")
    one_shot(["-db", owner, "-migrate-only"])
    d.provision_app_role(owner)
    tenant, key = provision(one_shot, owner, d)

    port = free_port()
    api = f"http://127.0.0.1:{port}"
    container = f"cleat-upgrade-seed-{os.getpid()}" if mode == "docker" else None
    flaky, flaky_url = start_failing_service()
    common = d.driver_args + ["-db", app, "-migrate-db", owner, "-api-addr", f":{port}", "-concurrency", "4",
                              "-poll", "200ms", "-bench-svc-url", flaky_url]
    if container:
        # Attached, so the Popen is the container; --name lets kill9 find it.
        argv = ["docker", "run", "--network", "host", "--name", container, IMAGE] + common
    else:
        argv = v040.command(common)
    log(f"starting the {VERSION} worker on {api}")
    worker = Worker(argv, state / "seed-worker.log", api, container)
    try:
        worker.wait_healthy()
        for name, path in wasm.items():
            upload(api, key, name, path)

        timer_ms = a.timer_seconds * 1000
        runs: dict[str, list[dict]] = {k: [] for k in
                                       ("ok", "fail", "deadletter", "signal", "timer", "parent", "spin", "burst")}

        def submit(kind: str, name: str, inp: dict):
            return kind, inp, start_run(api, key, name, inp)

        jobs = []
        for i in range(counts["ok"]):
            jobs.append(("ok", "seed-plain", {"n": i, "mode": "ok"}))
        for i in range(counts["fail"]):
            jobs.append(("fail", "seed-plain", {"n": i, "mode": "fail"}))
        for i in range(counts["deadletter"]):
            jobs.append(("deadletter", "seed-plain", {"n": i, "mode": "deadletter"}))
        for i in range(counts["signal"]):
            jobs.append(("signal", "seed-waiter", {"n": i, "mode": "signal"}))
        for i in range(counts["timer"]):
            jobs.append(("timer", "seed-waiter", {"n": i, "mode": "timer", "timer_ms": timer_ms}))
        for i in range(counts["parent"]):
            jobs.append(("parent", "seed-parent", {"n": i}))

        log(f"starting {len(jobs)} runs")
        with concurrent.futures.ThreadPoolExecutor(max_workers=8) as ex:
            for kind, inp, rid in ex.map(lambda j: submit(*j), jobs):
                runs[kind].append({"id": rid, "input": inp})

        def settled() -> bool:
            for kind in ("ok", "fail", "deadletter"):
                for r in runs[kind]:
                    if status_of(api, key, r["id"]) not in TERMINAL:
                        return False
            for kind, want in (("signal", "waiting"), ("timer", "waiting"), ("parent", "awaiting-child")):
                for r in runs[kind]:
                    if query_state(api, key, r["id"], "stage") != want:
                        return False
            return True

        wait_for(settled, 180, "the finished runs to finish and the waiters to be waiting", step=2)

        # The children are runs of their own; record them so verify can signal them.
        for r in runs["parent"]:
            r["child"] = query_state(api, key, r["id"], "child")
            need(bool(r["child"]), f"parent {r['id']} reported no child run id")

        log("stopping the worker mid-population: a burst of runs, then SIGKILL")
        burst_jobs = [("spin", "seed-plain", {"n": i, "mode": "spin"}) for i in range(counts["spin"])]
        burst_jobs += [("burst", "seed-plain", {"n": i, "mode": "ok"}) for i in range(counts["burst"])]
        with concurrent.futures.ThreadPoolExecutor(max_workers=8) as ex:
            for kind, inp, rid in ex.map(lambda j: submit(*j), burst_jobs):
                runs[kind].append({"id": rid, "input": inp})
        # Kill the moment a step is RUNNING, not after a fixed half second: how long the spin runs take to be claimed
        # varies with the machine (a seed on MySQL under emulation found none running at 0.5 s, and was UNMEASURED).
        # The burst is still mostly unclaimed at that point; the need() below asks the database for both.
        # A SPIN run, specifically: it takes seconds, so it is still running when the kill lands.  Any running step
        # would do for the first moment and then finish (a seed waited on one, killed, and found none).
        spin_ids = ",".join(f"'{r['id']}'" for r in runs["spin"]) or "''"
        running_sql = (f"SELECT COUNT(*) FROM {d.status_table(owner, tenant)} WHERE status = 'running' "
                       f"AND id IN ({spin_ids})")
        stop_at = time.monotonic() + 30
        while time.monotonic() < stop_at and int(d.sql_or_empty(owner, running_sql, tenant) or 0) == 0:
            time.sleep(0.1)
        log(f"killing the worker {30 - (stop_at - time.monotonic()):.1f}s after the burst; running steps seen: "
            f"{d.sql_or_empty(owner, running_sql, tenant) or 'none'}")
        worker.kill9()
    finally:
        worker.stop()
        flaky.shutdown()

    # The table a run's status lives in is asked of the database, not assumed from the version string.
    status_table = d.status_table(owner, tenant)
    snapshot = d.sql(owner, d.status_snapshot_sql(status_table), tenant)
    log(f"statuses in the database when the {VERSION} worker stopped:")
    print(snapshot, flush=True)
    seen = {line.split("|")[0] for line in snapshot.splitlines()}
    snapshot_counts = {l.split("|")[0]: int(l.split("|")[1]) for l in snapshot.splitlines()}
    inflight = seen - TERMINAL
    need(bool(inflight), "no run was left in flight, so the RC has nothing to resume; the seed measured nothing")
    # A run parked or queued resumes by an ordinary claim.  One killed mid-execution
    # is reclaimed by the reaper after a lease expires, which is a different
    # path and the one a changed lease table most plausibly breaks.
    need("running" in seen, "no run was mid-execution when the worker was killed (statuses: "
         f"{sorted(seen)}), so the reclaim path is not covered; raise --count spin")
    need("dead_lettered" in seen, "no run was dead-lettered (statuses: "
         f"{sorted(seen)}), so that state is not covered")
    # The three in-flight classes D79 asks for, each shown present rather than assumed: a step
    # RUNNING when the worker died (above), a run SLEEPING on a timer or waiting on a signal (settled()
    # above waited for their stage), and a run QUEUED and never claimed.  The burst is submitted
    # 0.5 s before the kill, at --concurrency 4, so most of it has not been claimed; "most" is asked of the
    # database.
    burst_ids = ",".join(f"'{r['id']}'" for r in runs["burst"])
    queued = int(d.sql(owner, d.queued_sql(status_table, burst_ids), tenant) or 0) if runs["burst"] else 0
    need(queued > 0, "no burst run was left queued and unclaimed when the worker was killed, so a "
                     "run that has never been claimed is not covered; raise --count burst")
    log(f"in flight at the kill: running {snapshot_counts.get('running', 0)}, "
        f"waiting on a timer {len(runs['timer'])}, waiting on a signal {len(runs['signal'])}, "
        f"parents awaiting a child {len(runs['parent'])}, queued and unclaimed {queued}")

    # What each finished run's status WAS when the seeding worker stopped, read from the database, so verify
    # holds the upgrade to "nothing finished changed" rather than to an expectation about what the seeding
    # release should have produced.  (On SQL Server v0.5.0, 6 of 10 dead-letter-mode runs ended `failed`,
    # "service seed-flaky.Poison not configured: no endpoint registered", before any upgrade; on PostgreSQL
    # all 10 were dead-lettered.  That is a fact about the seeding release, recorded here, not a finding.)
    stopped = {}
    for line in d.sql(owner, f"SELECT id, status FROM {status_table}", tenant).splitlines():
        rid, _, st = line.partition("|")
        stopped[rid] = st
    status_by_run = {r["id"]: stopped.get(r["id"], "") for kind in ("ok", "fail", "deadletter") for r in runs[kind]}
    need(all(status_by_run.values()), "a finished run's status could not be read back from the database")
    log(f"dead-letter-mode runs at the stop: "
        f"{sum(1 for r in runs['deadletter'] if status_by_run[r['id']] == 'dead_lettered')} dead_lettered of "
        f"{len(runs['deadletter'])}")

    history_columns = d.sql_or_empty(owner, d.history_columns_sql, tenant)
    manifest = {
        "version": VERSION, "seeded_by": str(local_bin) if local_bin else f"published {VERSION}",
        "tenant": tenant, "api_key": key, "timer_seconds": a.timer_seconds, "status_by_run_at_stop": status_by_run,
        "stopped_at": time.time(), "counts": counts, "runs": runs,
        "status_at_stop": {l.split("|")[0]: int(l.split("|")[1]) for l in snapshot.splitlines()},
        "migrations_by_v040": d.sql(owner, d.schema_versions_sql),
        # Recorded for --expect upgrade; "" where the seeding release has no such table or column, which
        # the refuse-mode seed (v0.4.0) must not fail on.
        "plugin_steps_by_seed": d.sql_or_empty(owner, d.plugin_steps_sql),
        "history_columns_by_seed": history_columns,
        "history_digest_by_seed": (d.sql_or_empty(owner, d.history_digest_sql(history_columns), tenant)
                                   if history_columns else ""),
    }
    (state / "manifest.json").write_text(json.dumps(manifest, indent=1))
    log(f"seeded; manifest at {state / 'manifest.json'}")
    return 0


# --------------------------------------------------------------------------
# verify
# --------------------------------------------------------------------------

def schema_fingerprint(dsn: str) -> dict[str, list[str]]:
    """What the schema IS, as sorted lines per facet, so a diff names the difference."""
    facets = {
        "columns": "SELECT table_schema||'.'||table_name||'.'||column_name||' '||data_type||' '||is_nullable "
                   "FROM information_schema.columns WHERE table_schema NOT IN ('pg_catalog','information_schema')",
        "indexes": "SELECT schemaname||'.'||indexname FROM pg_indexes "
                   "WHERE schemaname NOT IN ('pg_catalog','information_schema')",
        "functions": "SELECT n.nspname||'.'||p.proname||' '||md5(pg_get_functiondef(p.oid)) FROM pg_proc p "
                     "JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname NOT IN ('pg_catalog','information_schema')",
    }
    # Constraints, triggers and RLS policies are part of "the schema": a change
    # that adds a foreign key or rewrites a tenant policy and leaves every column
    # alone must still read as a change.
    facets["constraints"] = (
        "SELECT n.nspname||'.'||c.relname||'.'||k.conname||' '||pg_get_constraintdef(k.oid) FROM pg_constraint k "
        "JOIN pg_class c ON c.oid=k.conrelid JOIN pg_namespace n ON n.oid=c.relnamespace "
        "WHERE n.nspname NOT IN ('pg_catalog','information_schema')")
    facets["triggers"] = (
        "SELECT n.nspname||'.'||c.relname||'.'||t.tgname||' '||md5(pg_get_triggerdef(t.oid)) FROM pg_trigger t "
        "JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace "
        "WHERE NOT t.tgisinternal AND n.nspname NOT IN ('pg_catalog','information_schema')")
    facets["policies"] = (
        "SELECT schemaname||'.'||tablename||'.'||policyname||' '||md5(coalesce(qual,'')||'|'||coalesce(with_check,'')) "
        "FROM pg_policies")
    return {k: sorted(psql(dsn, q).splitlines()) for k, q in facets.items()}


def parse_steps(text: str) -> dict[str, int]:
    out: dict[str, int] = {}
    for line in text.splitlines():
        name, _, ver = line.rpartition(" ")
        if name and ver.isdigit():
            out[name] = int(ver)
    return out


def judge_plugin_steps(before: str, after: str, plugins: list[str]) -> list[str]:
    """One message per plugin that did not record a higher migration step than the seeded database had."""
    b, a = parse_steps(before), parse_steps(after)
    msgs = []
    if not b:
        return ["the seeded database has no plugin_migrations rows, so nothing can be compared (UNMEASURED-PLUGINS)"]
    for name in plugins:
        if name not in a:
            msgs.append(f"plugin {name}: no plugin_migrations row after the upgrade")
        elif name not in b:
            continue  # first recorded by the upgrade: higher by construction
        elif a[name] <= b[name]:
            msgs.append(f"plugin {name}: step {b[name]} before the upgrade and {a[name]} after; its new migration did not run")
    return msgs


def judge_history(before: str, after: str) -> list[str]:
    """One message per run whose event rows differ, or are gone, after the migration."""
    b = dict(l.split(" ", 1) for l in before.splitlines() if " " in l)
    a = dict(l.split(" ", 1) for l in after.splitlines() if " " in l)
    msgs = []
    if not b:
        return ["the seeded database has no event_history rows to compare (UNMEASURED-HISTORY)"]
    for rid, digest in b.items():
        if rid not in a:
            msgs.append(f"run {rid}: its event history is gone after --migrate-only")
        elif a[rid] != digest:
            msgs.append(f"run {rid}: its event history changed under --migrate-only ({digest} -> {a[rid]})")
    for rid in a.keys() - b.keys():
        msgs.append(f"run {rid}: event history appeared during --migrate-only")
    return msgs


def verify_upgrade(a, state: Path, m: dict) -> int:
    """--expect upgrade: the RC migrates the database a published release wrote, in place, and serves its runs."""
    key = m["api_key"]
    if not str(m.get("seeded_by", "")).startswith("published"):
        log(f"CONTROL RUN: this database was seeded by {m.get('seeded_by')}, not by the published {VERSION}")
    owner = a.owner_db
    d = dialect_of(owner)
    app = d.app_dsn(owner)
    tenant = m["tenant"]
    rc = Path(a.rc_worker).resolve()
    need(rc.exists() and os.access(rc, os.X_OK), f"--rc-worker {rc} is not an executable")
    findings: list[str] = []

    def finding(msg: str) -> None:
        findings.append(msg)
        print(f"FINDING: {msg}", flush=True)

    # -- 1. migrate ---------------------------------------------------------
    log(f"the RC's --migrate-only against the {VERSION} database")
    before = d.sql(owner, d.schema_versions_sql)
    p = subprocess.run([str(rc)] + d.driver_args + ["-db", owner, "-migrate-only"], capture_output=True, text=True, cwd=REPO)
    out = p.stdout + p.stderr
    applying = len(re.findall(r"\[migration\] applying", out))
    print(f"    exit={p.returncode} applying-lines={applying} versions before: {before}", flush=True)
    if p.returncode != 0:
        finding(f"the RC's --migrate-only exited {p.returncode}:\n{out[-800:]}")

    # -- 1b. what the migration must not have touched, and what it must have run ----------------
    # History first: a parked run replays its event rows when it resumes, so a migration that changed one
    # breaks a run that only fails later, at replay, and only if it is resumed.  Then the plugin runner:
    # --migrate-only runs the plugins' migrations after the core ones, and a plugin whose new step
    # did not run leaves a schema that "migrated" and is still short.
    def judged(msgs: list[str]) -> None:
        for msg in msgs:
            if "UNMEASURED-" in msg:
                raise Unmeasured(msg)
            finding(msg)

    judged(judge_history(m.get("history_digest_by_seed", ""),
                         d.sql(owner, d.history_digest_sql(m.get("history_columns_by_seed", "")), tenant)))
    plugins = PINS.get(VERSION, {}).get("plugins_with_new_migrations")
    if plugins:
        judged(judge_plugin_steps(m.get("plugin_steps_by_seed", ""), d.sql(owner, d.plugin_steps_sql), plugins))
        print(f"    plugin migrations recorded for {', '.join(plugins)}", flush=True)

    # -- 2. does the upgraded schema equal a fresh RC schema? ----------------
    fresh_name = f"cleat_upgrade_fresh_{os.getpid()}"
    fresh_owner = d.swap_database(owner, fresh_name)
    d.create_database(owner, fresh_name)
    try:
        q = subprocess.run([str(rc)] + d.driver_args + ["-db", fresh_owner, "-migrate-only"], capture_output=True, text=True, cwd=REPO)
        need(q.returncode == 0, f"the RC could not migrate a fresh database, so there is nothing to compare to:\n{(q.stdout+q.stderr)[-400:]}")
        fresh_applying = len(re.findall(r"\[migration\] applying", q.stdout + q.stderr))
        need(fresh_applying > 0, "the RC applied nothing to a FRESH database, so 'applying' lines are not countable here")
        up_fp, fr_fp = d.fingerprint(owner), d.fingerprint(fresh_owner)
    finally:
        d.drop_database(owner, fresh_name)
    compared = [("the upgraded schema", up_fp)]
    if hasattr(d, "extra_comparisons"):
        compared += d.extra_comparisons(owner, tenant, fr_fp)
    for what, got in compared:
        for facet in got:
            only_up = sorted(set(got[facet]) - set(fr_fp[facet]))
            only_fr = sorted(set(fr_fp[facet]) - set(got[facet]))
            if only_up or only_fr:
                finding(f"{what} differs from a fresh RC schema in {facet}: "
                        f"{len(only_up)} only in the upgraded database, {len(only_fr)} only in the fresh one\n"
                        f"      only upgraded: {only_up[:6]}\n      only fresh:    {only_fr[:6]}")
    print(f"    fresh database: {fresh_applying} migrations applied; the upgraded one: {applying}", flush=True)

    # -- 2b. the checksum control --------------------------------------------
    # The chain is verified by the worker when it replays a run's history; no API
    # exposes it, so "the run resumed" is the only evidence it holds -- and that
    # reads the same whether verification ran or was never reached.  So corrupt
    # one parked run's checksums first.  If the RC does not refuse THAT run, a
    # clean resume of the others proves nothing about the chain.
    # A parent, not a timer waiter (a pure sleep leaves no event rows) and not a
    # signal waiter (one event is a record, not a chain): child_workflow then
    # await_child is two events, each checksummed over the one before.
    ctl_parent = m["runs"]["parent"][-1]
    tampered = ctl_parent["id"]
    if d.tamper_checksums(owner, tenant, tampered) == 0:
        raise Unmeasured(f"the checksum control run {tampered} has no event_history rows to corrupt")

    # -- 3. serve ------------------------------------------------------------
    port = free_port()
    api = f"http://127.0.0.1:{port}"
    log(f"starting the RC worker on {api}")
    worker = Worker([str(rc)] + d.driver_args + ["-db", app, "-migrate-db", owner, "-api-addr", f":{port}",
                     "-concurrency", "4", "-poll", "200ms"], state / "rc-worker.log", api)
    try:
        worker.wait_healthy()
        all_runs = [r for kind in m["runs"].values() for r in kind]

        # -- 4. history and APIs for the runs that finished under the seeding release -----
        log("finished runs are readable under the RC")
        stopped_terminal = {rid for rid in
                            (r["id"] for kind in ("ok", "fail", "deadletter") for r in m["runs"][kind])}
        wrong = []
        recorded = m.get("status_by_run_at_stop", {})
        for kind, want in (("ok", {"done"}), ("fail", {"failed"}), ("deadletter", {"dead_lettered", "failed"})):
            for r in m["runs"][kind]:
                got = status_of(api, key, r["id"])
                if got not in want or (r["id"] in recorded and got != recorded[r["id"]]):
                    wrong.append((kind, r["id"], got, recorded.get(r["id"])))
        if wrong:
            finding(f"{len(wrong)} finished run(s) read a different status under the RC; first: {wrong[:3]}")

        # A DONE run's event history is deleted at finalize, by design (CLAUDE.md,
        # --retention-days), so only a dead-lettered run still has events to read.
        dead_lettered = [r for r in m["runs"]["deadletter"] if recorded.get(r["id"], "dead_lettered") == "dead_lettered"]
        need(bool(dead_lettered), "no dead-letter-mode run was dead-lettered at the seed, so dead-letter reads are not covered")
        for r in dead_lettered[:3]:
            code, hist = http("GET", f"{api}/api/workflows/{r['id']}/history", key)
            if code != 200 or not isinstance(hist, list) or not hist:
                finding(f"history of dead-lettered run {r['id']} is not readable under the RC (HTTP {code})")
                break
        code, dl = http("GET", f"{api}/api/dead-letters?limit=1000", key)
        listed = {w.get("id") for w in dl} if code == 200 and isinstance(dl, list) else set()
        missing = [r["id"] for r in dead_lettered if r["id"] not in listed]
        if code != 200 or missing:
            finding(f"GET /api/dead-letters answered {code} and lacks {len(missing)} of "
                    f"{len(dead_lettered)} dead-lettered run(s): {missing[:3]}")

        for path in ("/api/workflows?limit=5", "/api/definitions", "/api/schedules"):
            code, _ = http("GET", api + path, key)
            if code != 200:
                finding(f"GET {path} answered {code}")

        # -- 5. resume the in-flight runs ------------------------------------
        log("signalling the waiters; timers and reclaim run on their own")
        # The control parent and its child are outside the must-finish set: a parent
        # that fails may close its child, and neither outcome is under test.
        excluded = {tampered, ctl_parent["child"]}
        signal_targets = ([r["id"] for r in m["runs"]["signal"]]
                          + [r["child"] for r in m["runs"]["parent"] if r["child"] not in excluded])
        for rid in signal_targets:
            code, resp = http("POST", f"{api}/api/workflows/{rid}/signal", key, {"signal_name": "go", "payload": "{}"})
            if code not in (200, 201, 202, 204):
                finding(f"signal to {rid} answered {code}: {str(resp)[:120]}")
                break

        timer_left = max(0.0, m["stopped_at"] + m["timer_seconds"] - time.time())
        budget = 240 + timer_left
        expect_done = (signal_targets + [r["id"] for r in m["runs"]["timer"]]
                       + [r["id"] for r in m["runs"]["parent"] if r["id"] not in excluded]
                       + [r["id"] for r in m["runs"]["spin"]] + [r["id"] for r in m["runs"]["burst"]])
        end = time.monotonic() + budget
        pending = set(expect_done)
        final: dict[str, str] = {}
        while pending and time.monotonic() < end:
            for rid in list(pending):
                st = status_of(api, key, rid)
                if st in TERMINAL:
                    final[rid] = st
                    pending.discard(rid)
            time.sleep(2)
        # The control: the corrupted run must NOT complete.  Give it the same budget.
        ctl_end = time.monotonic() + max(30.0, end - time.monotonic())
        ctl = ""
        while time.monotonic() < ctl_end:
            ctl = status_of(api, key, tampered)
            if ctl in TERMINAL:
                break
            time.sleep(2)
        if ctl == "done":
            finding(f"the corrupted-checksum control run {tampered} completed: replay verification "
                    "did not refuse a tampered history, so a clean resume proves nothing about the chain")
        elif ctl not in TERMINAL:
            finding(f"UNMEASURED-CONTROL: the corrupted-checksum run {tampered} is still {ctl!r}; "
                    "it neither completed nor was refused")
        else:
            print(f"    checksum control: the corrupted run ended {ctl!r}, as it must", flush=True)
        notdone = {rid: s for rid, s in final.items() if s != "done"}
        if pending:
            finding(f"{len(pending)} of {len(expect_done)} in-flight run(s) never finished within {budget:.0f}s; "
                    f"first: {sorted(pending)[:3]}")
        if notdone:
            finding(f"{len(notdone)} in-flight run(s) ended other than done; first: {list(notdone.items())[:3]}")
        print(f"    resumed: {len(final) - len(notdone)} done, {len(notdone)} other, {len(pending)} still pending",
              flush=True)

        low = worker.tail(200)
        # "checksum mismatch" is NOT here: the control run above makes it appear on purpose.
        for pat in d.bad_log:
            if pat in low:
                finding(f"the RC worker log contains {pat!r}")
    finally:
        worker.stop()

    if findings:
        print(f"\n{len(findings)} finding(s).  The upgrade from {VERSION} did not hold.", flush=True)
        return 1
    print("\nall checks passed", flush=True)
    return 0


# What a refusal must say.  The refusal itself is WS-cleat's (cleat#3405, with B5).
# The marker is ONE distinctive phrase, proposed to WS-cleat on cleat#3386 to be
# included verbatim in both the --migrate-only error and the boot log.  It must
# not be a word that an unrelated failure also contains: the first draft listed
# "foreign", "chain" and "baseline", which a PostgreSQL foreign-key violation
# (the RC's 004-006 failing partway over a v0.4.0 schema), a hash-chain
# verification failure and a baseline mismatch all contain, so a half-applied
# migration would have read as a refusal.  Edit this when B5's text lands.
REFUSAL_MARKER = "foreign migration chain"


def judge_untouched(stage: str, applied: int, versions_before: str, versions_after: str,
                    fp_before: dict, fp_after: dict) -> str | None:
    """A refusal must leave the database exactly as it found it.

    Exit status and message cannot tell a refusal from a migration that applied
    some files and then failed; the database can.
    """
    if applied:
        return (f"{stage} applied {applied} migration(s) to the v0.4.0 database before it failed; "
                "a refusal changes nothing")
    if versions_before != versions_after:
        return f"{stage} changed schema_migrations: {versions_before!r} -> {versions_after!r}"
    if fp_before != fp_after:
        changed = [k for k in fp_before if fp_before[k] != fp_after.get(k)]
        return f"{stage} changed the schema ({', '.join(changed)}); a refusal changes nothing"
    return None


def judge_migrate_refusal(exit_code: int, output: str, applied: int) -> str | None:
    """None if --migrate-only refused a foreign database by name, else the finding."""
    if exit_code == 0:
        return ("MISSING (cleat#3405, WS-cleat with B5): the RC's --migrate-only ACCEPTED a v0.4.0 database "
                f"(exit 0, {applied} migration(s) applied). A 0.5.0 worker must refuse a foreign chain.")
    if REFUSAL_MARKER not in output.lower():
        return ("the RC's --migrate-only failed on the v0.4.0 database, but its message does not contain "
                f"{REFUSAL_MARKER!r}. Failing is not refusing by name. Output tail:\n{output[-500:]}")
    return None


def judge_boot_refusal(served: bool, exited: bool, exit_code: int | None, log_tail: str) -> str | None:
    """None if the boot refused a foreign database by name, else the finding."""
    if served:
        return ("MISSING (cleat#3405, WS-cleat with B5): the RC's BOOT served a v0.4.0 database "
                "(/healthz answered 200). A 0.5.0 worker must refuse to start on a foreign chain.")
    if not exited:
        return "the RC's boot neither served nor exited within 30s on the v0.4.0 database"
    if exit_code == 0:
        return "the RC's boot exited 0 on the v0.4.0 database; a refusal must be a failure status"
    if REFUSAL_MARKER not in log_tail.lower():
        return (f"the RC's boot exited {exit_code} on the v0.4.0 database but its log does not contain "
                f"{REFUSAL_MARKER!r}. Tail:\n{log_tail[-500:]}")
    return None


def verify_refusal_and_fresh(a, state: Path, m: dict) -> int:
    """--expect refuse (owner decision D18, cleat#3405): 0.5.0 requires a fresh database.

    Two halves, each reported on its own so a red job says WHICH is missing:
      REFUSAL  the RC's --migrate-only AND its boot refuse a database a v0.4.0
               worker wrote, loudly, by name.
      FRESH    the same binary migrates a fresh database and serves it.
    Each is the control for the other: a refusal means something only if the same
    binary demonstrably works on a fresh database, otherwise "exit 1" could be the
    RC being broken.  So FRESH runs first and gates the verdict on REFUSAL.
    """
    owner = a.owner_db
    need(dialect_of(owner).name == "postgres",
         "--expect refuse is the 0.5.0 gate (D18) and was only ever measured on PostgreSQL; use --expect upgrade")
    app = app_dsn(owner)
    rc = Path(a.rc_worker).resolve()
    need(rc.exists() and os.access(rc, os.X_OK), f"--rc-worker {rc} is not an executable")
    findings: list[str] = []

    def finding(half: str, msg: str) -> None:
        findings.append(f"{half}: {msg}")
        print(f"FINDING [{half}]: {msg}", flush=True)

    psql(owner, "SELECT 1")  # the database is reachable, or this is UNMEASURED and not a refusal
    seeded_versions = psql(owner, "SELECT string_agg(version, ' ' ORDER BY version) FROM schema_migrations")
    need(seeded_versions != "", "the seeded database has no schema_migrations rows; it was not seeded")

    # ---- FRESH ----------------------------------------------------------
    log("FRESH: the RC migrates and serves a fresh database")
    fresh_name = f"cleat_upgrade_fresh_{os.getpid()}"
    fresh_owner = swap_database(owner, fresh_name)
    psql(owner, f"CREATE DATABASE {fresh_name}")
    fresh_ok = False
    try:
        q = subprocess.run([str(rc), "-db", fresh_owner, "-migrate-only"], capture_output=True, text=True, cwd=REPO)
        if q.returncode != 0:
            finding("FRESH", f"the RC's --migrate-only failed on a FRESH database (exit {q.returncode}); "
                             f"until this passes the REFUSAL half below proves nothing:\n{(q.stdout+q.stderr)[-600:]}")
        else:
            psql(fresh_owner, f"ALTER ROLE cleat_app LOGIN PASSWORD '{APP_ROLE_PASSWORD}'")
            port = free_port()
            api = f"http://127.0.0.1:{port}"
            w = Worker([str(rc), "-db", app_dsn(fresh_owner), "-migrate-db", fresh_owner, "-api-addr", f":{port}",
                        "-concurrency", "2", "-poll", "200ms"], state / "rc-fresh-worker.log", api)
            try:
                try:
                    w.wait_healthy(60)
                except Unmeasured as e:
                    finding("FRESH", f"the RC did not serve a fresh database: {e}")
                else:
                    org = re.search(r"Org ID:\s+(\S+)", subprocess.run(
                        [str(rc), "-db", fresh_owner, "-create-org", "fresh-org"], capture_output=True, text=True).stdout)
                    tenant = re.search(r"Tenant ID:\s+(\S+)", subprocess.run(
                        [str(rc), "-db", fresh_owner, "-create-tenant", "fresh-tenant", "-org", org.group(1) if org else ""],
                        capture_output=True, text=True).stdout)
                    key = re.search(r"Key:\s+(\S+)", subprocess.run(
                        [str(rc), "-db", fresh_owner, "-generate-api-key", tenant.group(1) if tenant else ""],
                        capture_output=True, text=True).stdout)
                    need(bool(org and tenant and key), "could not provision a tenant and key on the fresh database")
                    wasm = state / "build" / "out" / "seed-plain" / "seed-plain.wasm"
                    need(wasm.exists(), f"{wasm} is missing; run `seed` first")
                    upload(api, key.group(1), "seed-plain", wasm)
                    rid = start_run(api, key.group(1), "seed-plain", {"n": 1, "mode": "ok"})
                    st = wait_for(lambda: status_of(api, key.group(1), rid) in TERMINAL and status_of(api, key.group(1), rid),
                                  60, "a run on the fresh database to finish", step=1)
                    if st != "done":
                        finding("FRESH", f"a run on the fresh database ended {st!r}, want 'done'")
                    else:
                        fresh_ok = True
                        print("    the RC migrated a fresh database, served it, and a run completed", flush=True)
            finally:
                w.stop()
    finally:
        psql(owner, f"DROP DATABASE IF EXISTS {fresh_name}")

    # ---- REFUSAL --------------------------------------------------------
    log(f"REFUSAL: the RC against a database a v0.4.0 worker wrote (versions: {seeded_versions})")
    fp0 = schema_fingerprint(owner)
    p = subprocess.run([str(rc), "-db", owner, "-migrate-only"], capture_output=True, text=True, cwd=REPO)
    out = p.stdout + p.stderr
    applied = len(re.findall(r"\[migration\] applying", out))
    after = psql(owner, "SELECT string_agg(version, ' ' ORDER BY version) FROM schema_migrations")
    print(f"    --migrate-only: exit={p.returncode} applying-lines={applied}; versions {seeded_versions!r} -> {after!r}", flush=True)
    why = judge_migrate_refusal(p.returncode, out, applied)
    if why:
        finding("REFUSAL", why)
    else:
        print("    --migrate-only refused, by name", flush=True)
    untouched = judge_untouched("--migrate-only", applied, seeded_versions, after, fp0, schema_fingerprint(owner))
    if untouched:
        finding("REFUSAL", untouched)

    port = free_port()
    api = f"http://127.0.0.1:{port}"
    boot = Worker([str(rc), "-db", app, "-migrate-db", owner, "-api-addr", f":{port}"],
                  state / "rc-boot-worker.log", api)
    try:
        end = time.monotonic() + 30
        served = False
        while time.monotonic() < end and boot.proc.poll() is None:
            if http("GET", f"{api}/healthz", timeout=3)[0] == 200:
                served = True
                break
            time.sleep(1)
        exited = boot.proc.poll() is not None
        why = judge_boot_refusal(served, exited, boot.proc.returncode if exited else None, boot.tail(60))
        if why:
            finding("REFUSAL", why)
        else:
            print("    boot refused, by name", flush=True)
        # The boot half runs with --migrate-db, so it can migrate; judge the
        # database again after it, against the same starting point.
        boot_applied = len(re.findall(r"\[migration\] applying", boot.tail(400)))
        untouched = judge_untouched("boot", boot_applied, seeded_versions,
                                    psql(owner, "SELECT string_agg(version, ' ' ORDER BY version) FROM schema_migrations"),
                                    fp0, schema_fingerprint(owner))
        if untouched:
            finding("REFUSAL", untouched)
    finally:
        boot.stop()

    if not fresh_ok:
        print("\nThe REFUSAL verdict above is not trustworthy while FRESH fails: the same binary has to work on "
              "a fresh database first.", flush=True)
    if findings:
        print(f"\n{len(findings)} finding(s):", flush=True)
        for f in findings:
            print(f"  - {f.splitlines()[0]}", flush=True)
        return 1
    print("\nall checks passed: a v0.4.0 database is refused by name, and a fresh database migrates and serves", flush=True)
    return 0


def cmd_verify(a) -> int:
    state = Path(a.state_dir).resolve()
    need((state / "manifest.json").exists(), f"no manifest at {state}/manifest.json; run `seed` first")
    m = json.loads((state / "manifest.json").read_text())
    use_version(m.get("version", VERSION))
    if a.expect == "upgrade":
        return verify_upgrade(a, state, m)
    return verify_refusal_and_fresh(a, state, m)


# --------------------------------------------------------------------------
# self test
# --------------------------------------------------------------------------

def self_test() -> int:
    bad = 0

    def check(name: str, cond: bool) -> None:
        nonlocal bad
        print(f"  {'ok  ' if cond else 'FAIL'}  {name}")
        bad += 0 if cond else 1

    check("swap_database keeps the query", swap_database("postgres://u:p@h:1/a?sslmode=disable", "b")
          == "postgres://u:p@h:1/b?sslmode=disable")
    check("swap_database without a query", swap_database("postgres://u:p@h:1/a", "b") == "postgres://u:p@h:1/b")
    check("app_dsn swaps the userinfo", app_dsn("postgres://postgres:x@h:5432/db?sslmode=disable")
          == f"postgres://cleat_app:{APP_ROLE_PASSWORD}@h:5432/db?sslmode=disable")
    ms = SQLServer()
    check("sqlserver swap_database replaces only the database", ms.swap_database(
        "sqlserver://sa:p@h:1433?database=a&encrypt=disable", "b") == "sqlserver://sa:p@h:1433?database=b&encrypt=disable")
    check("sqlserver app_dsn swaps the userinfo", ms.app_dsn("sqlserver://sa:x@h:1433?database=db")
          == f"sqlserver://cleat_app:{APP_ROLE_PASSWORD}@h:1433?database=db")
    check("dialect_of reads the scheme", (dialect_of("postgres://u@h/d").name, dialect_of("sqlserver://u@h?database=d").name)
          == ("postgres", "mssql"))
    try:
        dialect_of("mysql://u@h/d")
        check("dialect_of refuses a dialect the harness has not measured", False)
    except Unmeasured:
        check("dialect_of refuses a dialect the harness has not measured", True)
    check("only SQL Server passes -driver", (Postgres.driver_args, SQLServer.driver_args) == ([], ["-driver", "mssql"]))
    # Needs a SQL Server, so it runs where one is named (the SQL Server job names it) and says so where it is not.
    ms_dsn = os.environ.get("UPGRADE_SELFTEST_MSSQL")
    if ms_dsn:
        try:
            mssql_run(ms_dsn, "SELECT 1; SELECT 1/0")
            check("sqlq reports a LATER statement that fails in the batch", False)
        except Unmeasured as e:
            check("sqlq reports a LATER statement that fails in the batch", "Divide by zero" in str(e))
        check("sqlq prints the first result set of a batch", mssql_run(ms_dsn, "SELECT 1; SELECT 2") == "1")
    else:
        print("  NOT RUN  sqlq batch-error cases: UPGRADE_SELFTEST_MSSQL names no SQL Server")
    ok_msg = "refusing to start: this database holds a foreign migration chain written by v0.4.0"
    fk = 'pq: insert or update on table "workflow_leases" violates foreign key constraint "workflow_leases_fkey"'
    hashchain = "event hash chain verification failed at step 3"
    check("a migrate that exits 0 is MISSING", "MISSING" in (judge_migrate_refusal(0, "", 0) or ""))
    check("a migrate that fails for no stated cause is not a refusal by name",
          "does not contain" in (judge_migrate_refusal(1, "dial tcp: connection refused", 0) or ""))
    check("a foreign-key violation is NOT a refusal", judge_migrate_refusal(1, fk, 2) is not None)
    check("a hash-chain verification failure is NOT a refusal", judge_migrate_refusal(1, hashchain, 0) is not None)
    check("a migrate that refuses by name passes", judge_migrate_refusal(1, ok_msg, 0) is None)
    check("a boot that serves is MISSING", "MISSING" in (judge_boot_refusal(True, False, None, "") or ""))
    check("a boot that hangs is a finding", "neither served nor exited" in (judge_boot_refusal(False, False, None, "") or ""))
    check("a boot that exits 0 is a finding", "exited 0" in (judge_boot_refusal(False, True, 0, ok_msg) or ""))
    check("a boot log with only a foreign-key error is NOT a refusal", judge_boot_refusal(False, True, 1, fk) is not None)
    check("a boot log with only a hash-chain failure is NOT a refusal", judge_boot_refusal(False, True, 1, hashchain) is not None)
    check("a boot that refuses by name passes", judge_boot_refusal(False, True, 1, ok_msg) is None)
    fp = {"columns": ["a"], "indexes": ["i"], "functions": ["f"]}
    check("an untouched database passes", judge_untouched("x", 0, "1 2", "1 2", fp, dict(fp)) is None)
    check("a refusal that applied 2 migrations first is a finding", judge_untouched("x", 2, "1 2", "1 2", fp, dict(fp)) is not None)
    check("a refusal that changed schema_migrations is a finding", judge_untouched("x", 0, "1 2", "1 2 3", fp, dict(fp)) is not None)
    check("a refusal that changed the schema is a finding",
          judge_untouched("x", 0, "1 2", "1 2", fp, {**fp, "columns": ["a", "b"]}) is not None)
    steps0 = "audit-log 4\njobqueue 5\nscheduler 3"
    check("a plugin that advanced passes", judge_plugin_steps(steps0, "audit-log 4\njobqueue 7\nscheduler 3", ["jobqueue"]) == [])
    check("a plugin that did not advance is a finding",
          len(judge_plugin_steps(steps0, "audit-log 4\njobqueue 5\nscheduler 3", ["jobqueue"])) == 1)
    check("a plugin that lost its row is a finding", len(judge_plugin_steps(steps0, "audit-log 4", ["jobqueue"])) == 1)
    check("an unseeded plugin step list is UNMEASURED, not a pass",
          "UNMEASURED" in (judge_plugin_steps("", "jobqueue 7", ["jobqueue"]) or [""])[0])
    h0 = "run-a 3 abc\nrun-b 2 def"
    check("an unchanged history passes", judge_history(h0, h0) == [])
    check("a changed history is a finding", len(judge_history(h0, "run-a 3 abc\nrun-b 2 zzz")) == 1)
    check("a dropped run history is a finding", len(judge_history(h0, "run-a 3 abc")) == 1)
    check("an appeared run history is a finding", len(judge_history(h0, h0 + "\nrun-c 1 q")) == 1)
    check("an empty seeded history is UNMEASURED, not a pass", "UNMEASURED" in (judge_history("", h0) or [""])[0])
    check("use_version points the artifact names at the release",
          (use_version("v0.5.0"), bare(), VERSION)[1:] == ("0.5.0", "v0.5.0"))
    check("a pinned release names its image by digest", "@sha256:" in IMAGE)
    use_version("v0.4.0")
    check("an unpinned release names its image by tag", IMAGE.endswith(":v0.4.0"))
    try:
        use_version("0.5.0")
        check("use_version refuses a version without the v", False)
    except Unmeasured:
        check("use_version refuses a version without the v", True)
    try:
        need(False, "x")
        check("need() raises Unmeasured", False)
    except Unmeasured:
        check("need() raises Unmeasured", True)
    return 1 if bad else 0


def main(argv: list[str]) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--self-test", action="store_true")
    sub = ap.add_subparsers(dest="cmd")
    for name in ("seed", "verify"):
        p = sub.add_parser(name)
        p.add_argument("--owner-db", required=True, help="superuser DSN of an existing database (empty for seed)")
        p.add_argument("--state-dir", default=".upgrade-seed", help="where the manifest and logs go")
        if name == "seed":
            p.add_argument("--from-version", default="v0.4.0", metavar="vX.Y.Z",
                           help="the published release that seeds the database (default v0.4.0); verify reads it from the manifest")
            p.add_argument("--cache-dir", default=None,
                           help="where release assets are cached (default: a per-version directory under the temp dir)")
            p.add_argument("--worker-mode", choices=("binary", "docker"))
            p.add_argument("--worker-bin", help="POSITIVE CONTROL ONLY: seed with this cleat-worker, not the release")
            p.add_argument("--timer-seconds", type=int, default=90,
                           help="how long the timer waiters sleep; must outlast the stop, not the whole verify")
            p.add_argument("--count", action="append", metavar="KIND=N")
        else:
            p.add_argument("--rc-worker", required=True, help="the release candidate's cleat-worker binary")
            p.add_argument("--expect", choices=("refuse", "upgrade"), default="refuse",
                           help="refuse (default, owner decision D18 / cleat#3405): a v0.4.0 database is refused by "
                                "name and a fresh one serves.  upgrade: the RC migrates it in place and resumes its runs.")
    a = ap.parse_args(argv)
    if a.self_test:
        return self_test()
    if not a.cmd:
        ap.print_usage(sys.stderr)
        return 2
    try:
        if a.cmd == "seed":
            use_version(a.from_version)
            if a.cache_dir is None:
                a.cache_dir = str(Path(tempfile.gettempdir()) / f"cleat-{VERSION.replace('.', '')}-artifacts")
        return cmd_seed(a) if a.cmd == "seed" else cmd_verify(a)
    except Unmeasured as e:
        print(f"UNMEASURED: {e}\n(this is a failure of the check, not a finding about cleat)", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
