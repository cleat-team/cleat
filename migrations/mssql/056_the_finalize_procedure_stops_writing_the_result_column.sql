-- Step 1 of 2: finalize_workflow_status stops writing idempotency result.
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
-- ORDER MATTERS: this file must sort before 057, which drops the column. The
-- reverse leaves a window in which the live procedure references a column
-- that is already gone.
--
-- WHY THIS IS TWO MIGRATIONS AND NOT ONE. The procedure lives in its own file
-- because engine/store_backends_procedures_test.go re-applies every procedure
-- migration on each test setup, by name. A file in that list runs many times
-- against one database, so it must be idempotent -- CREATE OR REPLACE is,
-- ALTER TABLE ... DROP COLUMN is not (MySQL has no DROP COLUMN IF EXISTS at
-- all). Leaving the procedure out of that list is not an option either: the
-- harness would re-apply the previous definition on top of this one and
-- restore the write to a column that no longer exists.

CREATE OR ALTER PROCEDURE dbo.finalize_workflow_status
    @p_workflow_id      NVARCHAR(255),
    @p_worker_id        NVARCHAR(255),
    @p_generation       BIGINT,
    @p_final_status     NVARCHAR(32),     -- 'done', 'failed', or 'ready'
    @p_result           NVARCHAR(MAX),
    @p_error_code       NVARCHAR(255),
    @p_error_op         NVARCHAR(255),
    @p_query_state      NVARCHAR(MAX),
    @p_next_wake_at     DATETIMEOFFSET,
    @p_notify_channel   NVARCHAR(255)
AS
BEGIN
    SET NOCOUNT ON;

    DECLARE @rows_updated INT = 0;

    BEGIN TRY
        -- Update workflow status, fenced on (assigned_to, generation) so a
        -- caller that no longer owns the workflow cannot modify it.
        IF @p_final_status = 'done'
        BEGIN
            UPDATE dbo.workflow_instances
            SET status = 'done',
                result = @p_result,
                completed_at = SYSUTCDATETIME(),
                assigned_to = NULL,
                query_state = @p_query_state
            WHERE id = @p_workflow_id
              AND assigned_to = @p_worker_id
              AND generation = @p_generation;
            SET @rows_updated = @@ROWCOUNT;
        END
        ELSE IF @p_final_status = 'failed'
        BEGIN
            UPDATE dbo.workflow_instances
            SET status = 'failed',
                error_msg = @p_result,
                error_code = @p_error_code,
                error_op = @p_error_op,
                completed_at = SYSUTCDATETIME(),
                assigned_to = NULL,
                query_state = @p_query_state
            WHERE id = @p_workflow_id
              AND assigned_to = @p_worker_id
              AND generation = @p_generation;
            SET @rows_updated = @@ROWCOUNT;
        END
        ELSE IF @p_final_status = 'ready'
        BEGIN
            UPDATE dbo.workflow_instances
            SET status = 'ready',
                assigned_to = NULL,
                next_wake_at = CASE
                                    WHEN signal_seq <> signal_seq_at_claim
                                      OR signal_consumed_seq <> signal_consumed_at_claim
                                    THEN SYSUTCDATETIME() ELSE @p_next_wake_at END,
                query_state = @p_query_state
            WHERE id = @p_workflow_id
              AND assigned_to = @p_worker_id
              AND generation = @p_generation;
            SET @rows_updated = @@ROWCOUNT;
        END
        ELSE
        BEGIN
            THROW 50000, 'finalize_workflow_status: unknown final status', 1;
        END

        -- Terminal status side-effects -- only run if the fenced UPDATE
        -- above actually matched this caller's (worker_id, generation).
        -- If it matched zero rows, another worker now owns this workflow
        -- and none of these effects (idempotency recording, parent wake,
        -- await_child injection, event history deletion) are safe to
        -- apply on its behalf.
        IF @rows_updated > 0 AND (@p_final_status = 'done' OR @p_final_status = 'failed')
        BEGIN
            -- Record the idempotency outcome. Only a failure carries one
            -- now; #1049 dropped the result column nothing read. Still
            -- before the event delete, since the parent's await_child
            -- event references this workflow.
            IF @p_final_status = 'failed'
            BEGIN
                UPDATE dbo.idempotency_keys
                SET error_msg = @p_result
                WHERE workflow_id = @p_workflow_id
                  AND tenant_id = (SELECT tenant_id FROM dbo.workflow_instances
                                   WHERE id = @p_workflow_id);
            END

            -- Wake parent workflow atomically.
            UPDATE dbo.workflow_instances
            SET next_wake_at = SYSUTCDATETIME()
            WHERE id = (
                SELECT parent_workflow_id
                FROM dbo.workflow_instances
                WHERE id = @p_workflow_id
            )
            AND status IN ('ready', 'suspended');


            -- Delete this workflow's events -- they are no longer needed
            -- for replay once the workflow has reached a terminal state.
            -- This keeps event_history bounded to active workflows only,
            -- preventing unbounded table growth that slows per-step INSERTs.
            DELETE FROM dbo.event_history WHERE workflow_id = @p_workflow_id;
        END

        -- Dispatch hint: MSSQL has no native pg_notify equivalent.
        -- External notification (Service Broker, polling, etc.) is handled
        -- by the caller. @p_notify_channel is accepted for interface
        -- compatibility with the PostgreSQL signature.

        -- Report whether the fence held so Go callers can distinguish a
        -- lost fence (normal under reaping) from a real error.
        SELECT CASE WHEN @rows_updated > 0 THEN CAST(1 AS BIT) ELSE CAST(0 AS BIT) END AS fence_held;
    END TRY
    BEGIN CATCH
        THROW;
    END CATCH
END;
