-- cleat mssql stored procedures (consolidated)
-- Compiled from migration 010_finalize_workflow_segment_proc.
-- Idempotent: uses CREATE OR ALTER PROCEDURE.

-- ===========================================================================
-- Procedure: dbo.finalize_workflow_status
--
-- Bundles the terminal UPDATE portion of FinalizeWorkflowSegment into one
-- server-side call: status update, idempotency recording, parent wake,
-- await_child population, and event cleanup.
--
-- Called within an existing transaction after events have been appended and
-- event_count has been incremented.  RLS is enforced by the session context
-- (sp_set_session_context) set by the caller, so no explicit tenant_id filter
-- is needed inside the body.
-- ===========================================================================
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

    BEGIN TRY
        -- Update workflow status
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
        END
        ELSE
        BEGIN
            THROW 50000, 'finalize_workflow_status: unknown final status', 1;
        END

        -- Terminal status side-effects
        IF @p_final_status = 'done' OR @p_final_status = 'failed'
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
        -- by the caller.  @p_notify_channel is accepted for interface
        -- compatibility with the PostgreSQL signature.
    END TRY
    BEGIN CATCH
        THROW;
    END CATCH
END;
