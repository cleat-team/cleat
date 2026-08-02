-- cleat migration 004 (mssql): fix finalize_workflow_status generation fence
-- Idempotent: uses CREATE OR ALTER PROCEDURE.
--
-- Bug: the generation-fenced status UPDATE inside finalize_workflow_status
-- correctly matches zero rows when a stale caller's (worker_id, generation)
-- no longer owns the workflow (e.g. the worker stalled, was reaped, and the
-- workflow was reclaimed by another worker). But the terminal side-effect
-- block that follows -- idempotency-key overwrite, parent wake, injecting
-- this caller's result into the parent's await_child event, and
-- unconditionally DELETE FROM event_history for the workflow -- ran
-- regardless of whether the fenced UPDATE actually matched. A stalled
-- worker calling this procedure after being reaped and reclaimed could
-- therefore delete the new owner's live event history and inject its own
-- stale result into the parent workflow.
--
-- Fix: capture the fenced UPDATE's affected-row count via @@ROWCOUNT
-- (immediately after each UPDATE, since any subsequent statement resets
-- it) and skip the entire terminal side-effect block when it is zero. The
-- procedure now emits a trailing single-row, single-column result set
-- indicating whether the fence held, so Go callers can detect a lost
-- fence and treat it as the normal "reassigned to another worker" case
-- rather than an error.
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
                next_wake_at = @p_next_wake_at
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
            -- Record idempotency outcome (before deleting events, since
            -- the parent's await_child event references this workflow).
            IF @p_final_status = 'done'
            BEGIN
                UPDATE dbo.idempotency_keys
                SET result = @p_result
                WHERE workflow_id = @p_workflow_id;
            END
            ELSE
            BEGIN
                UPDATE dbo.idempotency_keys
                SET error_msg = @p_result
                WHERE workflow_id = @p_workflow_id;
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

            -- Populate parent's await_child event result (reads from the
            -- parent's events, not the child's, so this is safe to run
            -- before deleting the child's events).
            UPDATE dbo.event_history
            SET response = @p_result
            WHERE workflow_id = (
                SELECT parent_workflow_id
                FROM dbo.workflow_instances
                WHERE id = @p_workflow_id
            )
            AND event_type = 'await_child'
            AND run_id = @p_workflow_id
            AND (response IS NULL OR response = '');

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
