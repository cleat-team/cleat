package engine

import (
	"context"
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
