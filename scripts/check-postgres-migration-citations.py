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
guard, and it fails on a STALE allowlist entry (one whose (file, cited name)
no longer appears as a dangling citation), so the allowlist can't rot into
looking complete.

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

SELF_PATH = "scripts/check-postgres-migration-citations.py"

CITATION_PATTERN = (
    r'migrations/postgres/[0-9]{3}_[A-Za-z_]+\.sql|migration `?[0-9]{3}_[A-Za-z_]+\.sql`?'
)

# Whole files exempted: dated journals/narrative entries describing history
# at the time they were written (the same way CLAUDE.md's "Ground rules"
# section treats a past-tense justification), plus scripts whose numbered
# filenames are synthetic test fixtures, not citations of real files. This
# script's own path is exempted too -- its ALLOWLIST reasons below quote
# dangling filenames by name, on purpose, and are not citations of them.
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
    SELF_PATH,
)

# (file, cited filename) -> reason. Keyed on the CITED NAME, not the line
# number: a line number shifts on any unrelated edit above it in the same
# file, and 25 sites across 18 files made that a real hazard (measured
# cleat#2725 review, one comment line added above engine/rls_check.go's
# allowlisted citation gave a false positive). Keying on the cited filename
# also collapses a file that legitimately cites the same dangling name twice
# (e.g. two helpers both explaining the same retired migration) into one
# entry naturally.
#
# Every entry here cites a numbered file that is genuinely absent from
# migrations/postgres/ today, on purpose: it is a historical reference (what
# a migration used to do, what fixed a defect, what a real dated measurement
# printed), not a live pointer, and its own prose says so. Add here only
# alongside the sentence explaining why -- never as a way to make this guard
# stop looking.
ALLOWLIST = {
    ("migration/runner.go", "020_event_intent.sql"):
        "reproduces a real, dated error message a pre-rebaseline tree "
        "printed; renumbering it would misrepresent the measurement.",
    ("migration/a_non_default_schema_test.go", "020_event_intent.sql"):
        "same as migration/runner.go -- a dated measurement, not a pointer.",
    ("migration/migrations_do_not_hardcode_the_schema_test.go", "005_app_role.sql"):
        "same as migration/runner.go -- a dated measurement, not a pointer.",
    ("migration/minimum_server_version.go", "077_a_plugin_policy_can_use_its_index.sql"):
        "same as migration/runner.go -- a dated measurement, not a pointer.",
    ("tiers.yaml", "077_a_plugin_policy_can_use_its_index.sql"):
        "same as migration/runner.go -- a dated measurement, not a pointer.",
    ("migrations/mssql/optional/cross_tenant_claim.sql", "012_admin_role.sql"):
        "cites 012_admin_role.sql, a stale MSSQL (not postgres) citation -- "
        "cleat#2754's scope, not this guard's.",
    ("docs/reference/worker-config.md", "024_cross_tenant_schedules.sql"):
        "held by cleat#2755's open diff at the time cleat#2725 landed; leave "
        "for that PR to fix so the two don't conflict.",
    ("cmd/cleat-worker/config.go", "023_cross_tenant_claim.sql"):
        "deferred, not historical: cleat#2755 deletes this whole sentence "
        "('--claim-strategy=global still needs the grants...') because the "
        "flag was retired in cleat#1926. A filename-only fix here would be "
        "correcting a claim #2755 removes outright -- left for #2755.",
    ("engine/cross_tenant_fixtures_test.go", "023_cross_tenant_claim.sql"):
        "deferred, not historical: cleat#2769 rewrites this exact comment, "
        "including this citation, as part of the cleat#1926 cleanup ('neither "
        "dialect implements CrossTenantClaimer' is itself false post-#1926) -- "
        "left for #2769 so the two PRs don't both land in this file.",
    ("engine/db.go", "024_cross_tenant_schedules.sql"):
        "deferred, not historical: the comment also claims to be about "
        "GetDueSchedulesAcrossTenants, which cleat#1926 removed entirely (zero "
        "func definitions in the tree) -- a filename-only fix would leave a "
        "dead-code claim standing. Tracked as cleat#2770 item 1, filed after "
        "this PR's own review found the same gap.",
    ("tiers.yaml", "023_cross_tenant_claim.sql"):
        "deferred, not historical: this whole tier-2 entry describes "
        "--claim-across-tenants/--claim-strategy, retired by cleat#1926 -- a "
        "filename-only fix leaves the surrounding SUPPORTED/tier claims "
        "standing. Same #1926-class gap as cleat#2770's other tiers.yaml "
        "items (918, 924, 959, 980, 982); this line wasn't in that list and "
        "needs its own follow-up.",
    ("migrations/mssql/003_procedures.sql", "101_the_finalize_procedure_stops_deleting_failed_history.sql"):
        "GENERATED file ('Do not hand-edit; regenerate', "
        "docs/contributor/migrations.md) -- this citation is inside "
        "dbo.finalize_workflow_status's stored definition (sys.sql_modules), "
        "so a hand-edit here is silently reverted on the next regeneration "
        "and fresh vs. existing databases would diverge. Needs a source-side "
        "fix (wherever the generator's input database's routine comment "
        "lives), not a hand-edit -- permanently deferred from this guard's "
        "reach, not merely until another PR lands.",
    ("cmd/cleatctl/droptenant_test.go", "032_drop_tenant_deletes_tenant_data.sql"):
        "apply032ForDropTenantTest's own comment: 'used to read and execute "
        "... The list is gone with the cleat#2059 rebaseline'.",
    ("docs/explanation/postgresql-schema.md", "077_a_plugin_policy_can_use_its_index.sql"):
        "'originally added by migration 077' -- historical provenance for "
        "the GRANT ... WITH INHERIT FALSE syntax, now cited by name above it.",
    ("docs/operations/upgrading.md", "008_rls_fail_closed.sql"):
        "'it originally shipped as migration 008_rls_fail_closed.sql' -- "
        "describes the pre-rebaseline upgrade path, not a live pointer.",
    ("docs/operations/upgrading.md", "002_constraints.sql"):
        "describes a pre-rebaseline migration-ordering hazard (002 vs 008) "
        "that the current single-baseline apply no longer has.",
    ("docs/operations/workflow-retention.md", "101_the_finalize_procedure_stops_deleting_failed_history.sql"):
        "'originally removed by migration 101' -- historical provenance, "
        "current location (003_procedures.sql) cited in the same sentence.",
    ("docs/operations/workflow-retention.md", "033_completed_workflow_retention_indexes.sql"):
        "'originally added by migration 033' -- historical provenance, "
        "current index names cited in the same sentence.",
    ("docs/reference/workflow-lifecycle.md", "038_defer_phase_marker.sql"):
        "'originally migrations/postgres/038_defer_phase_marker.sql, "
        "mysql/037, mssql/041' -- historical provenance for the pre-rebaseline "
        "per-dialect numbering, current index name cited in the same sentence.",
    ("engine/an_idempotency_key_belongs_to_one_tenant_test.go", "083_an_idempotency_key_belongs_to_one_tenant.sql"):
        "'originally added by migration 083' -- historical provenance, "
        "current policy name cited in the same sentence.",
    ("engine/drop_tenant_test.go", "032_drop_tenant_deletes_tenant_data.sql"):
        "'originally fixed by migration 032, now folded into the cleat#2059 "
        "baseline' -- historical provenance, current location cited too.",
    ("engine/flush_rls_test.go", "005_app_role.sql"):
        "applyAppRoleMigration's and its sibling helper's own comments: 'it "
        "used to execute migrations/postgres/005_app_role.sql, and since the "
        "cleat#2059 rebaseline ... there is no file to run'.",
    ("engine/memory_profile_rls_layer_test.go", "061_the_memory_profile_has_a_policy_behind_its_predicate.sql"):
        "'originally added by migration 061' -- historical provenance for "
        "the RLS policies, current file cited in the same sentence.",
    ("engine/rls_check.go", "005_app_role.sql"):
        "'It used to say \"apply migrations/postgres/005_app_role.sql\", and "
        "that file was deleted by the 0.3.0 postgres compaction' -- this IS "
        "the fix for the stale-runtime-message defect, not the defect.",
    ("engine/rls_check_test.go", "005_app_role.sql"):
        "asserts rls_check.go never again names a migration file in a "
        "runtime message -- the citation is the counter-example it tests for.",
    ("engine/rls_gap_concurrency_and_update_requests_test.go", "031_rls_gap_concurrency_and_update_requests.sql"):
        "'originally added by migration 031' plus apply031RLSGapMigration's "
        "own comment: 'used to read and execute ... That file is part of the "
        "consolidated baseline now' -- historical provenance, twice in one file.",
    ("specs/CleatDurableCallIntent.md", "101_the_finalize_procedure_stops_deleting_failed_history.sql"):
        "'was removed outright in migration 101' -- historical provenance, "
        "current location (003_procedures.sql) cited two sentences later.",
}


def real_postgres_files():
    """The set of migrations/postgres/*.sql basenames that exist right now.

    Derived, not hardcoded: a hardcoded set gives a false positive on the
    first new numbered migration docs/contributor/migrations.md itself
    prescribes adding, since a correct citation of it would still read as
    'not one of the four baseline files'.
    """
    out = subprocess.run(
        ["git", "ls-files", "migrations/postgres/"],
        capture_output=True, text=True, cwd=".",
    )
    if out.returncode != 0:
        print("UNMEASURED: git ls-files failed: " + out.stderr, file=sys.stderr)
        sys.exit(2)
    files = set()
    for path in out.stdout.splitlines():
        if path.endswith(".sql"):
            files.add(path.rsplit("/", 1)[-1])
    return files


def git_tracked_matches():
    # git grep enumerates candidate lines fast, over tracked files only (no
    # .gitignore surprises, no rglob descending into a scratch worktree --
    # see CLAUDE.md's "prefer git ls-files over rglob"). Re-match in Python
    # rather than trust git grep -E's own group boundaries across engines.
    # Excludes this script's own path: see EXEMPT_FILE_PREFIXES.
    raw = subprocess.run(
        ["git", "grep", "-noE", CITATION_PATTERN, "--", ".", f":!{SELF_PATH}"],
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


def check(matches, real_files, allowlist=ALLOWLIST, exempt_prefixes=EXEMPT_FILE_PREFIXES):
    dangling = set()
    for path, _lineno, fname in matches:
        if fname in real_files:
            continue
        if path.startswith(exempt_prefixes):
            continue
        dangling.add((path, fname))

    unallowed = sorted(dangling - set(allowlist.keys()))
    stale = sorted(set(allowlist.keys()) - dangling)
    return unallowed, stale


def self_test():
    ok = True
    real_files = {"001_schema.sql", "002_defaults.sql", "003_procedures.sql",
                  "004_scope_plugin_grants_to_the_install.sql"}

    # 1. A citation of a real baseline file must not be flagged.
    matches = [("docs/x.md", 1, "001_schema.sql")]
    unallowed, stale = check(matches, real_files, allowlist={})
    if unallowed:
        print("self-test FAIL: a real-file citation was flagged", file=sys.stderr)
        ok = False

    # 2. A citation of a dangling file, not allowlisted, must be flagged.
    matches = [("docs/x.md", 1, "099_made_up.sql")]
    unallowed, stale = check(matches, real_files, allowlist={})
    if unallowed != [("docs/x.md", "099_made_up.sql")]:
        print("self-test FAIL: a dangling citation was not flagged", file=sys.stderr)
        ok = False

    # 3. A citation of a dangling file that IS allowlisted must not be flagged.
    matches = [("docs/x.md", 1, "099_made_up.sql")]
    unallowed, stale = check(matches, real_files,
                              allowlist={("docs/x.md", "099_made_up.sql"): "test"})
    if unallowed:
        print("self-test FAIL: an allowlisted citation was flagged", file=sys.stderr)
        ok = False
    if stale:
        print("self-test FAIL: a matching allowlist entry read as stale", file=sys.stderr)
        ok = False

    # 4. An allowlist entry whose (file, name) no longer appears as a
    #    dangling citation must be reported stale.
    matches = [("docs/x.md", 1, "001_schema.sql")]
    unallowed, stale = check(matches, real_files,
                              allowlist={("docs/x.md", "099_made_up.sql"): "test"})
    if stale != [("docs/x.md", "099_made_up.sql")]:
        print("self-test FAIL: a stale allowlist entry was not caught", file=sys.stderr)
        ok = False

    # 5. A file-prefix exemption must suppress the match entirely (so it is
    #    not counted as dangling AND does not need an allowlist entry).
    matches = [("CHANGELOG.md", 1, "099_made_up.sql")]
    unallowed, stale = check(matches, real_files, allowlist={})
    if unallowed:
        print("self-test FAIL: an exempt-prefix file was flagged", file=sys.stderr)
        ok = False

    # 6. A line number shifting above an allowlisted citation (an unrelated
    #    edit elsewhere in the same file) must NOT produce a false positive --
    #    the regression this guard's own review found. Same (path, fname),
    #    different line.
    matches = [("docs/x.md", 99, "099_made_up.sql")]
    unallowed, stale = check(matches, real_files,
                              allowlist={("docs/x.md", "099_made_up.sql"): "test"})
    if unallowed or stale:
        print("self-test FAIL: a shifted line number broke an allowlist match",
              file=sys.stderr)
        ok = False

    # 7. Every real ALLOWLIST entry must carry a non-empty reason.
    for key, reason in ALLOWLIST.items():
        if not reason or not reason.strip():
            print(f"self-test FAIL: empty reason for {key}", file=sys.stderr)
            ok = False

    # 8. The real scan, against the real tree: this script's own file must
    #    not appear in the matches at all (EXEMPT_FILE_PREFIXES), which is
    #    what stops this guard flagging itself over its own ALLOWLIST
    #    reasons -- measured broken in review before this exemption existed.
    real_matches = git_tracked_matches()
    if any(path == SELF_PATH for path, _lineno, _fname in real_matches):
        print("self-test FAIL: git_tracked_matches() did not exclude "
              "this script's own path", file=sys.stderr)
        ok = False

    # 9. The real scan, against the real tree, must be clean: no unallowed
    #    dangling citations, no stale allowlist entries. This is the
    #    guard's own main-mode check, run here too so --self-test catches a
    #    tree that would fail the second invocation the CI step also runs.
    real_files_now = real_postgres_files()
    unallowed, stale = check(real_matches, real_files_now)
    if unallowed:
        print(f"self-test FAIL: {len(unallowed)} unallowed dangling citation(s) "
              "in the real tree", file=sys.stderr)
        ok = False
    if stale:
        print(f"self-test FAIL: {len(stale)} stale ALLOWLIST entries against "
              "the real tree", file=sys.stderr)
        ok = False

    if ok:
        print("self-test: OK (9/9)")
        return 0
    return 1


def main():
    if "--self-test" in sys.argv:
        sys.exit(self_test())

    real_files = real_postgres_files()
    if not real_files:
        print("UNMEASURED: git ls-files migrations/postgres/ returned nothing -- "
              "either the directory is gone or this is not a git checkout.",
              file=sys.stderr)
        sys.exit(2)

    matches = git_tracked_matches()
    unallowed, stale = check(matches, real_files)

    if not unallowed and not stale:
        print(f"OK: every migrations/postgres/NNN citation names a file that exists "
              f"({sorted(real_files)}) or is allowlisted as historical "
              f"({len(ALLOWLIST)} entries).")
        sys.exit(0)

    if unallowed:
        print("ERROR: dangling migrations/postgres/NNN citation(s), not in the allowlist:")
        for path, fname in unallowed:
            print(f"  {path}: cites {fname}")
        print()
        print("Either the citation is wrong (fix it to name the current object and")
        print(f"migrations/postgres/{{{', '.join(sorted(real_files))}}}), or it is a")
        print("deliberate historical reference -- add it to ALLOWLIST in this script")
        print("with a reason.")

    if stale:
        print("ERROR: ALLOWLIST entries whose (file, cited name) no longer appears as "
              "a dangling citation (the citation was fixed, or the line changed -- "
              "update the entry or remove it):")
        for path, fname in stale:
            print(f"  {path}: {fname}")

    sys.exit(1)


if __name__ == "__main__":
    main()
