package engine

import (
	"context"
	"log/slog"
)

// workflowResourceReleaser is the slice of Store that post-commit cleanup uses.
// Taking an interface rather than a concrete store is what lets one copy of this
// logic serve PostgresStore, MySQLStore and MSSQLStore.
type workflowResourceReleaser interface {
	ClearStickyWorker(ctx context.Context, workflowID string) error
	ReleaseWorkflowConcurrencyKeys(ctx context.Context, workflowID string) error
}

// releaseWorkflowResources runs the two best-effort cleanups that follow every
// commit which takes a workflow out of the runnable set: completion, failure,
// termination, continue-as-new, and the admin actions. The call sites used to
// name these as Feature 10 (sticky worker) and Feature 5 (concurrency keys);
// that reference lives here now rather than being repeated at each one.
//
// Both are best-effort by design and neither may fail the caller. The state
// change is already committed at this point, so returning an error here would
// report a terminate that did not happen.
//
// But best-effort is not the same as unobserved, and that is what this function
// exists to fix. A failed ReleaseWorkflowConcurrencyKeys leaves rows in
// concurrency_keys owned by a workflow that has finished, and each of those rows
// holds a concurrency slot that live workflows are queued behind. It is not a
// permanent leak — expires_at is NOT NULL (migrations/postgres/001_schema.sql:350)
// and cmd/cleat-worker's reaper loop deletes expired rows
// (cmd/cleat-worker/setup.go:2001) — so the stall clears itself at the key's TTL.
// It clears itself silently, which is the problem: an operator looking at
// workflows blocked on a key whose holder completed an hour ago had nothing in
// the log to explain it.
//
// Measured on develop before this helper existed: 20 call sites, every one of
// them this same ordered pair, under three treatments that did not agree with
// each other — and did not even agree between the two halves of one pair.
//
//	                                bare    `_ =`    logged
//	ClearStickyWorker                 14        6         0
//	ReleaseWorkflowConcurrencyKeys    12        6         2
//
// So the sticky-worker error was discarded at all 20 sites and the concurrency-key
// error at 18 of 20. The two that logged are both TerminateWorkflow
// (engine/db.go, engine/mssql_operations.go), where the two halves sat on
// adjacent lines under one "// Best-effort cleanup." comment with different
// treatments — which is how the inconsistency was noticed. Re-derive the site
// count with
//
//	grep -rn "releaseWorkflowResources(s.log()" --include="*.go" engine/ | grep -v _test.go
//
// context.Background() is deliberate and is not a shortcut for a ctx parameter.
// All 20 sites already passed it: the caller's context is typically the request
// context, which is cancelled as soon as the RPC that finished the workflow
// returns, and cleanup that skips itself whenever the caller is in a hurry is
// the case this is most needed for.
//
// The warning carries no dialect attribute because a worker process is
// configured with exactly one store, so the dialect is a property of the
// deployment rather than of the line.
//
// Enforced by TestBestEffortCleanupGoesThroughTheHelper in
// workflow_cleanup_guard_test.go, which fails if a new call site reintroduces
// one of the two dropped-error forms.
func releaseWorkflowResources(log *slog.Logger, s workflowResourceReleaser, workflowID string) {
	ctx := context.Background()
	if err := s.ClearStickyWorker(ctx, workflowID); err != nil {
		log.WarnContext(ctx, "clear sticky worker failed", "workflow_id", workflowID, "error", err)
	}
	if err := s.ReleaseWorkflowConcurrencyKeys(ctx, workflowID); err != nil {
		log.WarnContext(ctx, "release concurrency keys failed", "workflow_id", workflowID, "error", err)
	}
}
