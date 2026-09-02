package engine

import (
	"context"
	"fmt"
	"time"
)

// Admin resolution of a pending call intent -- IMPROVEMENT-PLAN 1.4 phase F.
//
// A durable call that was in flight when the process died leaves a pending row
// (intent_at IS NOT NULL AND checksum IS NULL). Replay reports it as
// [AMBIGUOUS] and tells the operator to "Check the external service before
// retrying" -- and until now there was nowhere to put the answer. An
// AmbiguityResolver can supply one, but resolveAmbiguity returns immediately
// when none is configured, which is most deployments, so the workflow reports
// the same ambiguity on every replay forever.
//
// Everything the resolution needs already existed and was reachable from one
// caller. ResolveCallIntent is implemented on all three dialects, is
// tenant-scoped, requires the row to still be pending, and computes the
// checksum from the preceding row inside its own transaction. This is the
// admin path to it, and it is deliberately dialect-agnostic rather than three
// new SQL bodies: the per-dialect work was already done.

// adminActionResolveStep is the audit action name. It is the operation column
// on the audit event, and handleAdminOpError maps errors by substring, so the
// wording travels.
const adminActionResolveStep = "resolve-step"

// ResolveStep records an outcome for a call left ambiguous by a crash, on the
// authority of an operator who checked the external service.
//
// The response is written to the pending event as though the call had returned
// it, because that is what replay has to see -- a workflow cannot be made to
// branch on "a human said so". What keeps that honest is EventRecord.ResolvedBy,
// stored in the same row, in the same statement: the outcome is usable by
// replay and permanently marked as asserted rather than observed.
//
// Two writes, and they are not atomic. The resolution and the audit event are
// separate statements, and making them one would mean per-dialect SQL for a
// path that otherwise needs none. The trade is deliberate: the fact that must
// not be lost travels in the resolved row itself (see ResolvedBy), so a failed
// audit append costs a timeline entry rather than the provenance. It is
// reported rather than swallowed.
func ResolveStep(ctx context.Context, store WorkflowStore, workflowID string, step int, response, operator string) error {
	if workflowID == "" {
		return fmt.Errorf("admin %s: workflow ID is required", adminActionResolveStep)
	}
	if step < 0 {
		return fmt.Errorf("admin %s: step must be >= 0", adminActionResolveStep)
	}
	if operator == "" {
		operator = "unknown"
	}

	resolver, ok := store.(callIntentResolver)
	if !ok {
		return fmt.Errorf("admin %s: store %T cannot resolve call intents", adminActionResolveStep, store)
	}

	history, err := store.LoadEventHistory(ctx, workflowID)
	if err != nil {
		return fmt.Errorf("admin %s: load history for workflow %s: %w", adminActionResolveStep, workflowID, err)
	}

	// A workflow with no history at all is absent rather than unresolvable:
	// the tenant-scoped load returns nothing for another tenant's workflow
	// too, which is the "no oracle for valid IDs" stance the rest of the
	// admin surface takes (see adminResolveMiss).
	if len(history) == 0 {
		return adminNotFound(adminActionResolveStep, workflowID)
	}

	var target *EventRecord
	maxStep := -1
	for i := range history {
		if history[i].Step > maxStep {
			maxStep = history[i].Step
		}
		if history[i].Step == step {
			target = &history[i]
		}
	}
	if target == nil {
		// "not found" is load-bearing: handleAdminOpError maps it to 404.
		return fmt.Errorf("admin %s: step %d not found for workflow %s", adminActionResolveStep, step, workflowID)
	}
	// Not pending is a conflict, not a miss, and the two need different words
	// because the HTTP layer separates them: an operator who resolved the same
	// step twice, or whose worker got there first on replay, has asked for
	// something that is no longer true rather than for something absent.
	if !target.Pending {
		return fmt.Errorf("admin %s: generation mismatch for workflow %s: step %d is not pending",
			adminActionResolveStep, workflowID, step)
	}

	completed := *target
	completed.Response = response
	completed.Err = ""
	completed.ErrCode = ""
	completed.ErrNonRetryable = false
	completed.Pending = false
	completed.ResolvedBy = operator
	if completed.TimestampMs == 0 {
		completed.TimestampMs = time.Now().UnixMilli()
	}

	payload, err := eventRecordToPayload(completed)
	if err != nil {
		return fmt.Errorf("admin %s: encode step %d: %w", adminActionResolveStep, step, err)
	}

	// workerID "" skips the fence, per callIntentStore's doc. An operator is
	// not a claimant: they are asserting what the external service did, which
	// is true regardless of who holds the workflow. The row's own
	// pending predicate is the guard that matters, and it makes a race with a
	// worker resolving the same step safe -- whoever writes first wins and the
	// loser gets errIntentNotPending rather than overwriting a resolution.
	if err := resolver.ResolveCallIntent(ctx, workflowID, completed, payload, "", 0); err != nil {
		return fmt.Errorf("admin %s: workflow %s step %d: %w", adminActionResolveStep, workflowID, step, err)
	}

	audit := EventRecordFromEvent(AdminActionEvent{
		step:     maxStep + 1,
		Action:   adminActionResolveStep,
		Operator: operator,
		Reason:   fmt.Sprintf("resolved pending call at step %d", step),
	})
	audit.TimestampMs = time.Now().UnixMilli()
	if err := store.AppendEventHistory(ctx, workflowID, audit); err != nil {
		// Loud, and it names what did happen: the resolution is committed and
		// the operator is recorded on the resolved row, so this is a missing
		// timeline entry rather than an untraceable change. Reporting success
		// here would be the lie; reporting plain failure would invite a retry
		// that now finds the step no longer pending.
		return fmt.Errorf("admin %s: workflow %s step %d was resolved and recorded as resolved by %q, "+
			"but the audit event could not be appended (do not retry; the step is no longer pending): %w",
			adminActionResolveStep, workflowID, step, operator, err)
	}
	return nil
}
