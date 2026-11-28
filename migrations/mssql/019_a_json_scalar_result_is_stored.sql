-- cleat#3816: a promise result, a workflow result and an update result may be a JSON scalar, as they already
-- may be on PostgreSQL and MySQL.
--
-- 001 gave these three columns `CHECK ([result] IS NULL OR isjson([result])=(1))`. ISJSON with one argument
-- is 0 for a bare scalar on every version, SQL Server 2022 included (measured 2026-11-14 on 16.0.4295.3:
-- isjson('"yes"') = 0, isjson('"yes"', VALUE) = 1), so ResolvePromise and CompleteWorkflow failed with
-- "conflicted with the CHECK constraint" for a string, a number, a boolean and `null`: only an object or an
-- array could be stored, and a Promise[string] could not be resolved at all. CompleteUpdateRequest refuses the
-- same four values through the third constraint.
--
-- The remedy is the one 001 already applies to the two PAYLOAD columns, where it reads
-- `isjson([payload],VALUE)=(1)`. The two-argument form needs SQL Server 2022, which is the supported minimum
-- (tiers.yaml dialect_versions.mssql).
--
-- WHAT STAYS REFUSED: VALUE accepts any JSON value and nothing else. An empty string, `yes` and `{` still fail,
-- as they did.
--
-- WHY WITH NOCHECK. Adding a constraint WITH CHECK scans the table, and workflow_instances.result is
-- NVARCHAR(MAX) on a table that can hold millions of runs. The scan proves nothing here: each new predicate
-- is strictly weaker than the one it replaces (every value isjson(x) accepts, isjson(x, VALUE) accepts), so no
-- existing row can fail it. 001 uses the same form for the sibling constraints it adds WITH NOCHECK. The
-- constraint is enforced on every row written from now on; what WITH NOCHECK gives up is only the optimiser's
-- "trusted" flag, which nothing here depends on.
--
-- THE FILE RUNS IN ONE TRANSACTION (migration/runner.go applyMigration), so a failure between a drop and its
-- re-add leaves the old constraint in place.
--
-- THE NUMBER IS 019, WHICH IS NOT THE NEXT FREE ONE. 015 to 018 are named by comments in the tree, or by
-- scripts/migration-citation-baseline.txt, as migrations that once existed on another dialect (cleat#2438
-- compacted the chain), and a citation that resolves to the wrong file is what scripts/check-postgres-migration-
-- citations.py exists to refuse: with this file as 015 it reported six such citations resolving to it. 019 is the
-- first number above 014 that nothing names. scripts/check-migration-versions.sh rejects duplicates, not holes.
IF EXISTS (SELECT 1 FROM sys.check_constraints WHERE name = N'ck_workflow_instances_result' AND parent_object_id = OBJECT_ID(N'dbo.workflow_instances'))
    ALTER TABLE dbo.workflow_instances DROP CONSTRAINT ck_workflow_instances_result;
GO
ALTER TABLE dbo.workflow_instances WITH NOCHECK ADD CONSTRAINT ck_workflow_instances_result CHECK ([result] IS NULL OR isjson([result],VALUE)=(1));
GO
IF EXISTS (SELECT 1 FROM sys.check_constraints WHERE name = N'ck_workflow_promises_result' AND parent_object_id = OBJECT_ID(N'dbo.workflow_promises'))
    ALTER TABLE dbo.workflow_promises DROP CONSTRAINT ck_workflow_promises_result;
GO
ALTER TABLE dbo.workflow_promises WITH NOCHECK ADD CONSTRAINT ck_workflow_promises_result CHECK ([result] IS NULL OR isjson([result],VALUE)=(1));
GO
IF EXISTS (SELECT 1 FROM sys.check_constraints WHERE name = N'ck_workflow_update_requests_result' AND parent_object_id = OBJECT_ID(N'dbo.workflow_update_requests'))
    ALTER TABLE dbo.workflow_update_requests DROP CONSTRAINT ck_workflow_update_requests_result;
GO
ALTER TABLE dbo.workflow_update_requests WITH NOCHECK ADD CONSTRAINT ck_workflow_update_requests_result CHECK ([result] IS NULL OR isjson([result],VALUE)=(1));
GO
