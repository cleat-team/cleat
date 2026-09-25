package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
)

// ForceComplete marks a workflow as done with the given result, bypassing
// worker ownership checks. The generation counter is checked to prevent stale
// writes. An audit event is written atomically with the status change.
func ForceComplete(ctx context.Context, store WorkflowStore, workflowID string, generation int64, operator string, result string) error {
	if workflowID == "" {
		return adminErrorf(ErrAdminBadRequest, "force-complete: workflow ID is required")
	}
	if generation < 0 {
		return adminErrorf(ErrAdminBadRequest, "force-complete: generation must be >= 0")
	}
	if operator == "" {
		operator = "unknown"
	}
	// The result column is JSON on all three dialects -- JSONB on PostgreSQL,
	// JSON on MySQL, NVARCHAR(MAX) under an ISJSON check constraint on SQL
	// Server. An operator-supplied string that is not JSON is a bad request,
	// and rejecting it here gets that answer instead of three different
	// driver-level parse errors reported as a 500.
	if result == "" {
		result = "null"
	}
	if !json.Valid([]byte(result)) {
		return adminErrorf(ErrAdminBadRequest, "force-complete: result must be valid JSON")
	}

	if err := store.AdminForceComplete(ctx, workflowID, generation, result, operator); err != nil {
		return fmt.Errorf("force-complete: %w", err)
	}

	slog.WarnContext(ctx, "admin: force-complete workflow",
		"workflow_id", workflowID,
		"operator", operator,
		"generation", generation,
	)
	return nil
}

// ForceFail marks a workflow as failed with the given error, bypassing
// worker ownership checks. The generation counter is checked to prevent stale
// writes. An audit event is written atomically with the status change.
func ForceFail(ctx context.Context, store WorkflowStore, workflowID string, generation int64, operator string, errorMsg, errorCode string) error {
	if workflowID == "" {
		return adminErrorf(ErrAdminBadRequest, "force-fail: workflow ID is required")
	}
	if generation < 0 {
		return adminErrorf(ErrAdminBadRequest, "force-fail: generation must be >= 0")
	}
	if operator == "" {
		operator = "unknown"
	}
	// cleat#1977 (D5): error_code used to be free text, so the column could
	// hold anything -- an operator typo became a permanent, unrecognized
	// classification on the row. No code at all means this failure has no
	// classification but an operator, not the engine, decided it: ErrOperator,
	// not ErrUnknown, which every other write path already means "the engine
	// could not classify this."
	if errorCode == "" {
		errorCode = ErrOperator.String()
	} else if !IsRecognizedErrorCodeString(errorCode) {
		return adminErrorf(ErrAdminBadRequest, "force-fail: error_code %q is not a recognized code", errorCode)
	}

	if err := store.AdminForceFail(ctx, workflowID, generation, errorMsg, errorCode, operator); err != nil {
		return fmt.Errorf("force-fail: %w", err)
	}

	slog.WarnContext(ctx, "admin: force-fail workflow",
		"workflow_id", workflowID,
		"operator", operator,
		"generation", generation,
		"error_code", errorCode,
	)
	return nil
}

// adminHistoryUnreadable is the refusal for an operation that must read a
// workflow's history and could not decrypt it.
//
// It is refused BEFORE any write because the write would be the harm: every one
// of these operations appends an event (admin_action, or the resolved step)
// sealed under the key THIS worker holds. On a worker with the wrong key that
// leaves a history sealed under two keys, which no single worker can read
// again, so a worker that DID hold the original key would release the run
// forever (cleat#2311). ReReplay and RetryWorkflow used to treat a failed load
// as "skip the pending-intent check and carry on", which is exactly the path
// that wrote.
//
// A 409 state_conflict, because the caller's correct response is to send the
// request to a worker that holds the key. The message says that, and carries no
// cipher text or driver text. The returned error still satisfies
// errors.Is(err, ErrPayloadDecryption) and errors.Is(err, ErrAdminStateConflict).
func adminHistoryUnreadable(op, workflowID string) error {
	return classifiedError{
		err: fmt.Errorf("%s: this worker cannot read workflow %s's history because it does not hold the payload "+
			"encryption key that sealed it. Nothing was changed. Send the request to a worker that holds that key, "+
			"or check --encryption-key-file and --encryption-key-file-previous on this one",
			op, workflowID),
		class: errors.Join(ErrAdminStateConflict, ErrPayloadDecryption),
	}
}

// ReReplay resets a workflow to 'ready' state so the dispatcher picks it up
// for re-execution from its existing event history. The generation counter is
// checked to prevent stale writes. An audit event is written atomically with
// the status change.
func ReReplay(ctx context.Context, store WorkflowStore, workflowID string, generation int64, operator string) error {
	if workflowID == "" {
		return adminErrorf(ErrAdminBadRequest, "re-replay: workflow ID is required")
	}
	if generation < 0 {
		return adminErrorf(ErrAdminBadRequest, "re-replay: generation must be >= 0")
	}
	if operator == "" {
		operator = "unknown"
	}

	// Refuse to re-replay into an unresolved ambiguity. A history whose last
	// call was left mid-flight by a crash replays straight back into
	// [AMBIGUOUS] -- the workflow stops again, in the same place, for the same
	// reason, and the operator has spent a generation bump to learn nothing.
	//
	// This is a judgement rather than something the 3.20 contract states. The
	// alternative is to proceed and let replay report it, which is defensible:
	// it keeps re-replay a pure status reset. It loses the one thing the
	// operator needs, which is *which step* to reconcile -- and phase F now
	// gives them somewhere to put the answer, so pointing at it is more useful
	// than reproducing the failure.
	history, herr := store.LoadEventHistory(ctx, workflowID)
	if errors.Is(herr, ErrPayloadDecryption) {
		return adminHistoryUnreadable("re-replay", workflowID)
	}
	if herr == nil {
		for _, rec := range history {
			if rec.isPendingIntent() {
				return adminErrorf(ErrAdminStateConflict,
					"re-replay: workflow %s has an unresolved ambiguous call at step %d "+
						"(%s.%s): re-replaying would report it again. Check the external service and record "+
						"the outcome with POST /api/admin/instances/%s/steps/%d/resolve first",
					workflowID, rec.Step, rec.Service, rec.Op, workflowID, rec.Step)
			}
		}
		// cleat#2038: empty history is ambiguous between "no call was ever
		// made" and "a call's outcome was swept by --retention-days before
		// it could be recorded" -- DeleteExpiredEvents deletes a workflow's
		// event_history all at once, so a swept workflow always reads as
		// EMPTY here, indistinguishable from one that never dispatched
		// anything. history_swept_at is the only remaining signal; without
		// it this loop would have found nothing to refuse and allowed a
		// blind redispatch, which is the real S1 violation cleat#1999's
		// TLA+ model (specs/CleatDurableCallIntent.tla) traced.
		if len(history) == 0 {
			if swept, serr := store.IsHistorySwept(ctx, workflowID); serr == nil && swept {
				return adminErrorf(ErrAdminStateConflict,
					"re-replay: workflow %s's event history was already removed by the "+
						"retention sweep (--retention-days), so whether a call was left pending "+
						"when it stopped can no longer be told. Re-replaying could silently "+
						"redispatch an already-applied call. This cannot be recovered; consider "+
						"reprocessing the workflow as a new run instead",
					workflowID)
			}
			// A failed or negative IsHistorySwept lookup is deliberately not
			// fatal, same reasoning as the failed LoadEventHistory case below.
		}
	}
	// A failed history load is deliberately not fatal here: it would turn a
	// read this operation does not otherwise need into a reason the operation
	// cannot run. The store call below is the one that must succeed. The one
	// failure that IS refused is a history this worker cannot decrypt, above.

	if err := store.AdminReReplay(ctx, workflowID, generation, operator); err != nil {
		return fmt.Errorf("re-replay: %w", err)
	}

	slog.WarnContext(ctx, "admin: re-replay workflow",
		"workflow_id", workflowID,
		"operator", operator,
		"generation", generation,
	)
	return nil
}

// RetryWorkflow moves a dead_lettered workflow back to 'ready' so the
// dispatcher picks it up again. dead_lettered is one of the three
// reReplayableStatuses ReReplay also reaches, and shares the same hazard: a
// history whose last call was left mid-flight by a crash retries straight
// back into [AMBIGUOUS], redispatching a call whose outcome is still
// unknown. See ReReplay's guard above, which this mirrors.
//
// Unlike ReReplay, this does not also check IsHistorySwept.
// DeleteExpiredEvents' retention sweep only matches status IN ('done',
// 'failed') (engine/retention_predicates.go) -- dead_lettered is excluded --
// and a dead_lettered workflow's own retention sweep,
// deleteDeadLetteredWorkflowsBatch, removes the whole workflow_instances row
// rather than just its event_history. So there is no "row survives, history
// swept" state for a dead_lettered workflow to be caught in: by the time its
// history could be ambiguous this way, RetryWorkflow has no row left to act
// on either. cleat#2039.
func RetryWorkflow(ctx context.Context, store WorkflowStore, workflowID string) error {
	if workflowID == "" {
		return adminErrorf(ErrAdminBadRequest, "retry: workflow ID is required")
	}

	history, herr := store.LoadEventHistory(ctx, workflowID)
	if errors.Is(herr, ErrPayloadDecryption) {
		return adminHistoryUnreadable("retry", workflowID)
	}
	if herr == nil {
		for _, rec := range history {
			if rec.isPendingIntent() {
				return adminErrorf(ErrAdminStateConflict,
					"retry: workflow %s has an unresolved ambiguous call at step %d "+
						"(%s.%s): retrying would report it again. Check the external service and record "+
						"the outcome with POST /api/admin/instances/%s/steps/%d/resolve first",
					workflowID, rec.Step, rec.Service, rec.Op, workflowID, rec.Step)
			}
		}
	}
	// A failed history load is deliberately not fatal here, same reasoning as
	// ReReplay's identical fallback above: it would turn a read this operation
	// does not otherwise need into a reason the operation cannot run.

	if err := store.RetryWorkflow(ctx, workflowID); err != nil {
		return fmt.Errorf("retry: %w", err)
	}

	slog.WarnContext(ctx, "admin: retry dead-lettered workflow", "workflow_id", workflowID)
	return nil
}
