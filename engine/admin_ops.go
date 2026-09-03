package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
)

// ForceComplete marks a workflow as done with the given result, bypassing
// worker ownership checks. The generation counter is checked to prevent stale
// writes. An audit event is written atomically with the status change.
func ForceComplete(ctx context.Context, store WorkflowStore, workflowID string, generation int64, operator string, result string) error {
	if workflowID == "" {
		return fmt.Errorf("force-complete: workflow ID is required")
	}
	if generation < 0 {
		return fmt.Errorf("force-complete: generation must be >= 0")
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
		return fmt.Errorf("force-complete: result must be valid JSON")
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
		return fmt.Errorf("force-fail: workflow ID is required")
	}
	if generation < 0 {
		return fmt.Errorf("force-fail: generation must be >= 0")
	}
	if operator == "" {
		operator = "unknown"
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

// ReReplay resets a workflow to 'ready' state so the dispatcher picks it up
// for re-execution from its existing event history. The generation counter is
// checked to prevent stale writes. An audit event is written atomically with
// the status change.
func ReReplay(ctx context.Context, store WorkflowStore, workflowID string, generation int64, operator string) error {
	if workflowID == "" {
		return fmt.Errorf("re-replay: workflow ID is required")
	}
	if generation < 0 {
		return fmt.Errorf("re-replay: generation must be >= 0")
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
	if history, herr := store.LoadEventHistory(ctx, workflowID); herr == nil {
		for _, rec := range history {
			if rec.isPendingIntent() {
				return fmt.Errorf("re-replay: workflow %s has an unresolved ambiguous call at step %d "+
					"(%s.%s): re-replaying would report it again. Check the external service and record "+
					"the outcome with POST /api/admin/instances/%s/steps/%d/resolve first",
					workflowID, rec.Step, rec.Service, rec.Op, workflowID, rec.Step)
			}
		}
	}
	// A failed history load is deliberately not fatal here: it would turn a
	// read this operation does not otherwise need into a reason the operation
	// cannot run. The store call below is the one that must succeed.

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
