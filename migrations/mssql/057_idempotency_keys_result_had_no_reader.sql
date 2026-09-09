-- Step 2 of 2: drop idempotency_keys.result. 056 stopped the procedure writing it.
--
-- cleat-team/cleat#1049. Nine statements wrote this column -- three Go
-- lifecycle methods (one per dialect's CompleteWorkflow) and the
-- finalize_workflow_status procedure on each of the three backends -- and no
-- production code path ever selected it. The only SELECTs of idempotency_keys
-- outside tests read `workflow_id, def_name`, which is what the idempotency
-- lookup actually needs.
--
-- WHAT REPLACES IT: nothing, and that is the point. The `done` branch of the
-- outcome record now writes no idempotency row at all. The `failed` branch is
-- untouched and still writes error_msg through the same tenant predicate, so
-- FailWorkflow and MoveToDeadLetterQueue -- two of the three sites the Finding
-- S1 regression test names -- keep the protection that #1017/#1019 gave them.
--
-- WHY THIS IS NOT A LOSS OF COVERAGE. The tenant-scope regression test
-- (engine/idempotency_update_tenant_scope_test.go) moves from `result` to
-- `error_msg`: the same UPDATE shape, the same WHERE clause, the same
-- three-dialect table, the other branch of the same IF. What does go away is
-- TestIdempotencyResultSurvivesANonJSONResult, which existed only because
-- this column had to be valid JSON on the way in -- on this backend enforced
-- by the CHECK constraint ck_idempotency_keys_result rather than by a cast.
-- error_msg is plain text with no cast and no constraint, so the failure mode that test
-- guarded stops being expressible rather than stopping being tested.
--
-- The CHECK goes first: SQL Server refuses to drop a column a constraint
-- names, and reports it as "The object 'ck_idempotency_keys_result' is
-- dependent on column 'result'" rather than as anything about the drop.
IF EXISTS (SELECT 1 FROM sys.check_constraints
           WHERE name = N'ck_idempotency_keys_result'
             AND parent_object_id = OBJECT_ID(N'dbo.idempotency_keys'))
    ALTER TABLE dbo.idempotency_keys DROP CONSTRAINT ck_idempotency_keys_result;

IF EXISTS (SELECT 1 FROM sys.columns
           WHERE name = N'result'
             AND object_id = OBJECT_ID(N'dbo.idempotency_keys'))
    ALTER TABLE dbo.idempotency_keys DROP COLUMN result;
