-- cleat migration 046 (mssql): a suspending segment stores its query state
--
-- finalize_workflow_status writes query_state on the 'done' and 'failed'
-- branches and NOT on 'ready', which is the branch a suspension takes. So a
-- workflow's queryable state reached the database only when the workflow
-- reached a terminal status -- by which point its result is available and the
-- query is the least useful it will ever be.
--
-- That inverts what the mechanism is for. docs/determinism.md:
--
--   the workflow proactively records queryable state to the database at points
--   of its choosing, and that state is durable and externally readable -- via
--   GetQueryState or GET /api/workflows/:id/query?key=X -- REGARDLESS OF
--   WHETHER ANY WORKER CURRENTLY HAS THE WORKFLOW LOADED. It answers "what is
--   this workflow's status" without needing anything to be running at query
--   time.
--
-- A suspended workflow is precisely the case that sentence names, and it was
-- the one case that returned nothing. Measured 2026-09-06 on PostgreSQL: a
-- workflow that set phase="started" and then slept read back "" for phase
-- while suspended, and "finished" only after it completed.
--
-- All three dialects had it, identically, so this is a template that omitted
-- the assignment rather than a divergence between them.
--
-- The write is unconditional, matching the terminal branches. It cannot blank
-- previously stored state: query state is not a durable event, it is rebuilt
-- by re-executing the body, so a resumed segment replays every SetQueryState
-- call made before its suspension point and arrives here with the full map.
-- The one caller that must NOT write it is the defer phase, which passes none
-- and goes through FinalizeDeferPhase rather than this routine -- see the
-- DeferPhaseStore doc comment, which says so for this exact reason.
--
-- Only the 'ready' branch changes. Everything else is 004 verbatim.

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
                next_wake_at = @p_next_wake_at,
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
