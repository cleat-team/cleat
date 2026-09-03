#!/usr/bin/env python3
"""Report SQL Server statements that touch a tenant-scoped table without a tenant predicate.

Why this exists
---------------
dbo.fn_tenant_filter admits any connection whose login is a member of dbo.cleat_admin,
regardless of SESSION_CONTEXT (migrations/mssql/012_admin_role.sql) -- and a multi-tenant
deployment MUST grant that role, because GetDueSchedulesAcrossTenants and the cross-tenant
claim require it and without them a non-default tenant's workflows never fire. So on such a
deployment every tenant-scoped store runs unfiltered and the Go-level `AND tenant_id` is the
whole of the isolation. IMPROVEMENT-PLAN 3.86.

Three tables were audited by hand, by reading one file at a time while scoping something else,
and each read turned up five, five and thirteen leaking statements. That is not a method. This
reports the whole surface at once so the remaining count is a number rather than "unknown".

What it is NOT
--------------
This is a REPORT, not a gate. It is deliberately not wired into CI yet, because the statements
it still flags are a different class -- they key on a generated id (workflow_instances(id),
event_history(workflow_id)) where a UUID cannot be guessed -- and turning it into a failing
check means first deciding whether that argument is good enough, which is a product call rather
than a cleanup. Making it a gate with an allowlist is the next step 3.86 describes; when that
happens this file grows a baseline and moves under a _test.go, following
engine/mssql_uuid_projection_test.go, which is the same shape and already runs in every job.

The table list is DERIVED from the security-policy bindings in the migrations rather than
hardcoded, so a table brought under the policy later is covered without anyone remembering.
"""

import glob
import io
import os
import re
import sys


def tenant_scoped_tables(repo_root):
    """Tables bound to dbo.fn_tenant_filter, read from the shipped migrations."""
    tables = set()
    pattern = re.compile(
        r"ADD FILTER PREDICATE dbo\.fn_tenant_filter\(tenant_id\)\s+ON\s+dbo\.([a-z_]+)",
        re.IGNORECASE,
    )
    for path in glob.glob(os.path.join(repo_root, "migrations", "mssql", "*.sql")):
        with io.open(path, encoding="utf-8") as fh:
            tables.update(m.group(1).lower() for m in pattern.finditer(fh.read()))
    return tables


def statements(repo_root):
    """Every backtick SQL literal in the non-test MSSQL store files."""
    for path in sorted(glob.glob(os.path.join(repo_root, "engine", "mssql*.go"))):
        if path.endswith("_test.go"):
            continue
        with io.open(path, encoding="utf-8") as fh:
            src = fh.read()
        for m in re.finditer(r"`([^`]*)`", src):
            sql = m.group(1)
            if not re.search(r"\b(select|insert|update|delete|merge)\b", sql, re.IGNORECASE):
                continue
            yield os.path.relpath(path, repo_root), src[: m.start()].count("\n") + 1, sql


def main():
    repo_root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    tables = tenant_scoped_tables(repo_root)
    if not tables:
        # A parse that finds nothing would report a clean tree no matter what the
        # store did -- the same vacuous-pass failure this repo keeps paying for.
        sys.exit("no tenant-scoped tables found in migrations/mssql -- the parse is broken")

    with_predicate = 0
    without = []
    for path, line, sql in statements(repo_root):
        low = sql.lower()
        if not any(re.search(r"\b" + t + r"\b", low) for t in tables):
            continue
        if "tenant_id" in low:
            with_predicate += 1
        else:
            without.append((path, line, " ".join(sql.split())))

    print("tenant-scoped tables (derived): %d" % len(tables))
    print("statements WITH a tenant predicate:    %d" % with_predicate)
    print("statements WITHOUT a tenant predicate: %d" % len(without))
    if without:
        print()
        for path, line, sql in without:
            print("  %s:%d\n      %s" % (path, line, sql[:110]))
    return 0


if __name__ == "__main__":
    sys.exit(main())
