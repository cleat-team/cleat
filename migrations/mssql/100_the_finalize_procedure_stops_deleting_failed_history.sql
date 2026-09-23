-- finalize_workflow_status no longer has a 'failed' branch.
--
-- cleat#1973. No production code path ever calls this procedure with
-- @p_final_status = 'failed'. FinalizeWorkflowSegment's one production call
-- site (cmd/cleat-worker/setup.go) only ever passes 'done' or 'ready'; the
-- real failure path is store.FailWorkflow (engine/mssql_lifecycle.go), a
-- plain UPDATE that never calls this procedure at all. So the 'failed'
-- branch below -- including its event_history DELETE -- has never run in
-- production. See migrations/postgres/101_the_finalize_procedure_stops_deleting_failed_history.sql
-- for the full reasoning; this is the same change on SQL Server.
--
-- A dormant delete is the risk, not a current bug: removing it is removing a
-- branch that one call-site change would silently reactivate, deleting a
-- failed workflow's replay history at finalize instead of leaving it for
-- --retention-days to sweep (cleat#1973's corrected behaviour).
--
-- NOTE ON 056'S OWN COMMENT. 056's header says the 'failed' branch's
-- idempotency_keys write is "untouched" and protects FailWorkflow and
-- MoveToDeadLetterQueue. That was wrong the same way this whole issue was:
-- neither of those call sites ever reaches this procedure. Both already
-- write idempotency_keys.error_msg themselves, directly
-- (engine/mssql_lifecycle.go), independent of this procedure. Removing the
-- branch here loses that update from exactly nowhere it was actually coming
-- from.
--
-- WHAT CHANGES, versus 056:
--   * The @p_final_status = 'failed' branch of the status IF/ELSE chain is
--     gone. A caller that passes 'failed' now hits the final ELSE and gets
--     the same "unknown final status" THROW any other unrecognized value
--     gets.
--   * The terminal-status IF narrows from
--     (@p_final_status = 'done' OR @p_final_status = 'failed') to
--     @p_final_status = 'done' alone.
--   * The idempotency_keys error_msg UPDATE that only ran for 'failed' is
--     removed with it, per the note above.
--   * The @p_final_status parameter comment drops 'failed' from its list.
--
-- WHAT DOES NOT CHANGE: the 'done' branch, byte for byte, and its DELETE FROM
-- dbo.event_history.
--
-- Everything else here is 056 verbatim, minus the 'failed' branch. Whole
-- redefinition rather than a patch: CREATE OR ALTER is idempotent, and
-- engine/store_backends_procedures_test.go re-applies every listed procedure
-- migration by name, so this file must be complete on its own.

CREATE OR ALTER PROCEDURE dbo.finalize_workflow_status
    @p_workflow_id      NVARCHAR(255),
    @p_worker_id        NVARCHAR(255),
    @p_generation       BIGINT,
    @p_final_status     NVARCHAR(32),     -- 'done' or 'ready'
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
        -- above actually matched this caller's (worker_id, generation), and
        -- only for 'done': a real failure never reaches this procedure
        -- (cleat#1973).
        IF @rows_updated > 0 AND @p_final_status = 'done'
        BEGIN
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
