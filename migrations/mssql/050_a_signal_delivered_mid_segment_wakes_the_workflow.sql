-- cleat#953: a signal delivered while the workflow was AWAKE was not woken for.
--
-- DeliverSignal pulls next_wake_at forward only for a workflow that is already
-- suspended -- 'running' is excluded, because a claimed workflow's row is about
-- to be overwritten here anyway. That left a window: a delivery arriving
-- mid-segment scheduled nothing, this procedure then wrote the workflow's own
-- timeout deadline over next_wake_at, and the signal sat in workflow_signals
-- until that timeout expired. AwaitSignals reported a timeout with the signal
-- it was waiting for already in the table.
--
-- signal_seq is bumped by every delivery; signal_seq_at_claim is stamped by
-- every claim. Different means a delivery landed while this segment was
-- running, so the workflow wakes now instead of at its deadline.
--
-- THE COMPARISON IS IN THIS TRANSACTION, which is the whole reason for the
-- counter over a post-commit poll. A post-commit check closes the same window
-- in the steady state -- measured, four interleavings, no gap -- but a worker
-- dying between the commit and the check loses that wake until the caller's own
-- timeout. There is no step after this one.
--
-- A COUNTER rather than "does workflow_signals have a row", which spins: a
-- workflow awaiting {a} with an unrelated {z} pending would wake, poll, find
-- nothing it wants, re-suspend, and repeat. The counter only moves on a NEW
-- delivery.
--
-- Only the 'ready' arm changes. This file otherwise reproduces
-- migrations/mssql/047_child_does_not_rewrite_parent_event.sql verbatim, because the procedure is replaced whole.

-- cleat migration 047 (mssql): a child no longer rewrites its parent's event
--
-- finalize_workflow_status injected a completing child's result into the
-- PARENT's await_child event row:
--
--   UPDATE event_history SET response = p_result
--   WHERE workflow_id = (SELECT parent_workflow_id ...)
--     AND event_type = 'await_child' AND run_id = p_workflow_id
--     AND (response IS NULL OR response = '');
--
-- and left that row's checksum untouched. computeEventChecksum hashes the whole
-- event payload, response included, and chains it with the previous event's
-- checksum -- so a row rewritten by another workflow invalidates its own
-- checksum and every one after it. The parent's next segment recomputed, saw a
-- different value, and failed permanently:
--
--   checksum verification failed: verify events: workflow <id> step 1:
--   checksum mismatch (expected a0212ee0fc7b6167, got d66082e106bc2725)
--
-- Measured 2026-09-06, 3 runs of 3: a parent awaiting a child that outlives the
-- parent's first segment never resumed. Watched directly, the await_child row
-- went from (checksum 5bbfe122a91ba25f, response '') to (checksum
-- 5bbfe122a91ba25f, response '{"tag":"cp3-4753"}').
--
-- Only single AwaitChild was affected: the predicate matches event_type
-- 'await_child', which AwaitAllChildren and AwaitAnyChild do not write. That is
-- why the fan-out tests passed throughout.
--
-- Deleting the injection loses nothing. AwaitChild's replay branch already
-- handles an empty response and says so -- "No cached result yet -- fall
-- through to fresh to re-check ... the fresh execution will record the result
-- at this same step, overwriting the empty event". The injection only let
-- replay skip that re-check, and it bought one store lookup at the cost of the
-- property the chain exists to provide: an event written once, by the workflow
-- that owns it, and never changed afterwards.
--
-- Recomputing the checksum in SQL was the alternative and is not portable: it
-- is an xxHash64 over the marshalled record, chained. See cleat#845.
--
-- Only that one statement is removed. Everything else is 046_query_state_on_suspension.sql verbatim.

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
                next_wake_at = CASE WHEN signal_seq <> signal_seq_at_claim
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
