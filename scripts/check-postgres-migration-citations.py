#!/usr/bin/env python3
"""Every migrations/postgres/NNN_name.sql citation must name a file that
exists, or be an explicitly allowlisted historical reference.

cleat#2725: the postgres schema rebaseline (cleat#2059, cleat#2416) collapsed
every numbered migration into four generated files --
migrations/postgres/001_schema.sql, 002_defaults.sql, 003_procedures.sql,
004_scope_plugin_grants_to_the_install.sql. A citation of any other numbered
filename is now dangling by construction, and 43 of them accumulated across
the tree with nothing to catch the next one. This is that catch: it fails on
any NEW dangling citation, not just the ones fixed in the PR that added this
guard, and it fails on a STALE allowlist entry (one whose line no longer cites
a dangling filename), so the allowlist can't rot into looking complete.

Scoped to migrations/postgres/ only, matching cleat#2725's own scope. MySQL
and SQL Server were compacted the same week (cleat#2435, cleat#2438) and
likely carry the same defect -- cleat#2725's own "What I did NOT check"
section leaves that as a second sweep, not something this guard covers.

Usage:
  scripts/check-postgres-migration-citations.py --self-test
  scripts/check-postgres-migration-citations.py
"""
import re
import subprocess
import sys

REAL_FILES = {
    "001_schema.sql",
    "002_defaults.sql",
    "003_procedures.sql",
    "004_scope_plugin_grants_to_the_install.sql",
}

# Whole files exempted: dated journals/narrative entries describing history
# at the time they were written (the same way CLAUDE.md's "Ground rules"
# section treats a past-tense justification), plus scripts whose numbered
# filenames are synthetic test fixtures, not citations of real files.
EXEMPT_FILE_PREFIXES = (
    "IMPROVEMENT-PLAN.md",
    "IMPROVEMENT-PLAN-CLOSED.md",
    "IMPROVEMENT-PLAN.d/",
    "WORKSTREAM.md",
    "REVIEW-2026-08-09.md",
    "CLAUDE.md",
    "CHANGELOG.md",
    "docs/playbooks/",
    "scripts/check-migration-versions.sh",
    "scripts/check_migration_numbers.py",
    "scripts/check-entity-contract.py",
    "scripts/gen-postgres-baseline.py",
)

# (file, line) -> reason. Every entry here cites a numbered file that is
# genuinely absent from migrations/postgres/ today, on purpose: it is a
# historical reference (what a migration used to do, what fixed a defect,
# what a real dated measurement printed), not a live pointer, and its own
# prose says so. Add here only alongside the sentence explaining why --
# never as a way to make this guard stop looking.
ALLOWLIST = {
    ("migration/runner.go", 238):
        "reproduces a real, dated error message a pre-rebaseline tree "
        "printed; renumbering it would misrepresent the measurement.",
    ("migration/a_non_default_schema_test.go", 18):
        "same as migration/runner.go:238 -- a dated measurement, not a pointer.",
    ("migration/migrations_do_not_hardcode_the_schema_test.go", 46):
        "same as migration/runner.go:238 -- a dated measurement, not a pointer.",
    ("migration/minimum_server_version.go", 37):
        "same as migration/runner.go:238 -- a dated measurement, not a pointer.",
    ("tiers.yaml", 392):
        "same as migration/runner.go:238 -- a dated measurement, not a pointer.",
    ("migrations/mssql/optional/cross_tenant_claim.sql", 59):
        "cites 012_admin_role.sql, a stale MSSQL (not postgres) citation -- "
        "cleat#2754's scope, not this guard's.",
    ("docs/reference/worker-config.md", 1282):
        "held by cleat#2755's open diff at the time cleat#2725 landed; leave "
        "for that PR to fix so the two don't conflict.",
    ("cmd/cleatctl/droptenant_test.go", 27):
        "apply032ForDropTenantTest's own comment: 'used to read and execute "
        "... The list is gone with the cleat#2059 rebaseline'.",
    ("docs/explanation/postgresql-schema.md", 18):
        "'originally added by migration 077' -- historical provenance for "
        "the GRANT ... WITH INHERIT FALSE syntax, now cited by name above it.",
    ("docs/operations/upgrading.md", 677):
        "'it originally shipped as migration 008_rls_fail_closed.sql' -- "
        "describes the pre-rebaseline upgrade path, not a live pointer.",
    ("docs/operations/upgrading.md", 697):
        "describes a pre-rebaseline migration-ordering hazard (002 vs 008) "
        "that the current single-baseline apply no longer has.",
    ("docs/operations/workflow-retention.md", 46):
        "'originally removed by migration 101' -- historical provenance, "
        "current location (003_procedures.sql) cited in the same sentence.",
    ("docs/operations/workflow-retention.md", 312):
        "'originally added by migration 033' -- historical provenance, "
        "current index names cited in the same sentence.",
    ("docs/reference/workflow-lifecycle.md", 639):
        "'originally migrations/postgres/038_defer_phase_marker.sql, "
        "mysql/037, mssql/041' -- historical provenance for the pre-rebaseline "
        "per-dialect numbering, current index name cited in the same sentence.",
    ("engine/an_idempotency_key_belongs_to_one_tenant_test.go", 5):
        "'originally added by migration 083' -- historical provenance, "
        "current policy name cited in the same sentence.",
    ("engine/drop_tenant_test.go", 5):
        "'originally fixed by migration 032, now folded into the cleat#2059 "
        "baseline' -- historical provenance, current location cited too.",
    ("engine/flush_rls_test.go", 196):
        "applyAppRoleMigration's own comment: 'it used to execute "
        "migrations/postgres/005_app_role.sql, and since the cleat#2059 "
        "rebaseline ... there is no file to run'.",
    ("engine/flush_rls_test.go", 228):
        "same helper as line 196: 'it used to read and execute "
        "migrations/postgres/005_app_role.sql. Since the cleat#2059 "
        "rebaseline ... the helper asserts rather than installs'.",
    ("engine/memory_profile_rls_layer_test.go", 6):
        "'originally added by migration 061' -- historical provenance for "
        "the RLS policies, current file cited in the same sentence.",
    ("engine/rls_check.go", 161):
        "'It used to say \"apply migrations/postgres/005_app_role.sql\", and "
        "that file was deleted by the 0.3.0 postgres compaction' -- this IS "
        "the fix for the stale-runtime-message defect, not the defect.",
    ("engine/rls_check_test.go", 151):
        "asserts rls_check.go never again names a migration file in a "
        "runtime message -- the citation is the counter-example it tests for.",
    ("engine/rls_gap_concurrency_and_update_requests_test.go", 6):
        "'originally added by migration 031' -- historical provenance, "
        "current file cited in the same sentence.",
    ("engine/rls_gap_concurrency_and_update_requests_test.go", 80):
        "apply031RLSGapMigration's own comment: 'used to read and execute "
        "migrations/postgres/031_....sql. That file is part of the "
        "consolidated baseline now'.",
    ("specs/CleatDurableCallIntent.md", 81):
        "'was removed outright in migration 101' -- historical provenance, "
        "current location (003_procedures.sql) cited two sentences later.",
}


def git_tracked_matches():
    # git grep enumerates candidate lines fast, over tracked files only (no
    # .gitignore surprises, no rglob descending into a scratch worktree --
    # see CLAUDE.md's "prefer git ls-files over rglob"). Re-match in Python
    # rather than trust git grep -E's own group boundaries across engines.
    raw = subprocess.run(
        ["git", "grep", "-noE",
         r'migrations/postgres/[0-9]{3}_[A-Za-z_]+\.sql|migration `?[0-9]{3}_[A-Za-z_]+\.sql`?'],
        capture_output=True, text=True, cwd=".",
    )
    if raw.returncode not in (0, 1):
        print("UNMEASURED: git grep failed: " + raw.stderr, file=sys.stderr)
        sys.exit(2)
    results = []
    for line in raw.stdout.splitlines():
        m = re.match(r"^(.*?):(\d+):(.*)$", line)
        if not m:
            continue
        path, lineno, cite = m.group(1), int(m.group(2)), m.group(3)
        fm = re.search(r"([0-9]{3}_[A-Za-z_]+\.sql)", cite)
        fname = fm.group(1) if fm else None
        results.append((path, lineno, fname))
    return results


def check(matches, real_files=REAL_FILES, allowlist=ALLOWLIST,
          exempt_prefixes=EXEMPT_FILE_PREFIXES):
    dangling = set()
    for path, lineno, fname in matches:
        if fname in real_files:
            continue
        if path.startswith(exempt_prefixes):
            continue
        dangling.add((path, lineno))

    unallowed = sorted(dangling - set(allowlist.keys()))
    stale = sorted(set(allowlist.keys()) - dangling)
    return unallowed, stale


def self_test():
    ok = True

    # 1. A citation of a real baseline file must not be flagged.
    matches = [("docs/x.md", 1, "001_schema.sql")]
    unallowed, stale = check(matches, allowlist={})
    if unallowed:
        print("self-test FAIL: a real-file citation was flagged", file=sys.stderr)
        ok = False

    # 2. A citation of a dangling file, not allowlisted, must be flagged.
    matches = [("docs/x.md", 1, "099_made_up.sql")]
    unallowed, stale = check(matches, allowlist={})
    if unallowed != [("docs/x.md", 1)]:
        print("self-test FAIL: a dangling citation was not flagged", file=sys.stderr)
        ok = False

    # 3. A citation of a dangling file that IS allowlisted must not be flagged.
    matches = [("docs/x.md", 1, "099_made_up.sql")]
    unallowed, stale = check(matches, allowlist={("docs/x.md", 1): "test"})
    if unallowed:
        print("self-test FAIL: an allowlisted citation was flagged", file=sys.stderr)
        ok = False
    if stale:
        print("self-test FAIL: a matching allowlist entry read as stale", file=sys.stderr)
        ok = False

    # 4. An allowlist entry whose line no longer cites a dangling file must
    #    be reported stale (the citation was fixed or the line changed).
    matches = [("docs/x.md", 1, "001_schema.sql")]
    unallowed, stale = check(matches, allowlist={("docs/x.md", 1): "test"})
    if stale != [("docs/x.md", 1)]:
        print("self-test FAIL: a stale allowlist entry was not caught", file=sys.stderr)
        ok = False

    # 5. A file-prefix exemption must suppress the match entirely (so it is
    #    not counted as dangling AND does not need an allowlist entry).
    matches = [("CHANGELOG.md", 1, "099_made_up.sql")]
    unallowed, stale = check(matches, allowlist={})
    if unallowed:
        print("self-test FAIL: an exempt-prefix file was flagged", file=sys.stderr)
        ok = False

    # 6. Every real ALLOWLIST entry must carry a non-empty reason.
    for key, reason in ALLOWLIST.items():
        if not reason or not reason.strip():
            print(f"self-test FAIL: empty reason for {key}", file=sys.stderr)
            ok = False

    if ok:
        print("self-test: OK (6/6)")
        return 0
    return 1


def main():
    if "--self-test" in sys.argv:
        sys.exit(self_test())

    matches = git_tracked_matches()
    unallowed, stale = check(matches)

    if not unallowed and not stale:
        print(f"OK: every migrations/postgres/NNN citation names a file that exists "
              f"or is allowlisted as historical ({len(ALLOWLIST)} entries).")
        sys.exit(0)

    if unallowed:
        print("ERROR: dangling migrations/postgres/NNN citation(s), not in the allowlist:")
        for path, lineno in unallowed:
            print(f"  {path}:{lineno}")
        print()
        print("Either the citation is wrong (fix it to name the current object and")
        print("migrations/postgres/{001_schema,002_defaults,003_procedures,")
        print("004_scope_plugin_grants_to_the_install}.sql), or it is a deliberate")
        print("historical reference -- add it to ALLOWLIST in this script with a reason.")

    if stale:
        print("ERROR: ALLOWLIST entries whose line no longer cites a dangling file "
              "(the citation was fixed, or the line moved/changed -- update the "
              "entry or remove it):")
        for path, lineno in stale:
            print(f"  {path}:{lineno}")

    sys.exit(1)


if __name__ == "__main__":
    main()
