package engine

import (
	"context"
	"database/sql"
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

// releaseTerminatedChildren runs releaseWorkflowResources for each child that a
// parent's TERMINATE close policy just failed.
//
// The close policy commits a failure -- SET status = 'failed', error_msg =
// 'parent workflow terminated' -- which is squarely inside the contract above:
// "every commit which takes a workflow out of the runnable set: completion,
// failure, termination...". It released nothing on any dialect
// (IMPROVEMENT-PLAN 3.80), so a closing parent stranded one concurrency slot
// per child until the key's TTL.
//
// Why this is a loop over ids rather than one set-based statement: the release
// path is two store methods per workflow, not SQL this package can fold into
// the UPDATE, and both are best-effort with their own logging. A parent closing
// thousands of children would make this loop worth revisiting; the shape that
// fixes it is a bulk ReleaseWorkflowConcurrencyKeys, not a different caller.
//
// The ids are collected before the UPDATE rather than returned from it because
// the three dialects do not agree on returning affected rows -- PostgreSQL has
// RETURNING, MySQL does not. A child whose status changes between the two
// statements is released anyway, which is harmless: releasing the resources of
// a workflow that finished on its own is exactly what its own terminal path
// would have done.
func releaseTerminatedChildren(log *slog.Logger, s workflowResourceReleaser, childIDs []string) {
	for _, id := range childIDs {
		releaseWorkflowResources(log, s, id)
	}
}

// maxParentCloseDepth bounds the recursion in cascadeIntoClosedChildren.
//
// The graph cannot cycle: parent_workflow_id is only ever written at INSERT, to
// a row that already exists, so it is built in creation order and a workflow
// cannot become its own ancestor. Continue-as-new INHERITS its predecessor's
// parent rather than pointing at it.
//
// That is a reading of the schema, and a terminal path is the wrong place to
// find out it was wrong -- an unbounded recursion here would be a worse defect
// than the one this cascade exists to fix. The bound costs one comparison and
// removes the possibility. 64 is far past any real nesting; a tree that deep has
// a design problem the engine should not be papering over silently, which is why
// hitting the bound is an ERROR and names what is left running.
const maxParentCloseDepth = 64

// cascadeIntoClosedChildren finishes what closing a child started: each one that
// the close policy just terminated must enforce ITS OWN close policy, exactly as
// FailWorkflow does after its commit.
//
// WHY THIS EXISTS. FailWorkflow's post-commit is two calls -- release the
// workflow's resources, then enforce its close policy on its children -- and a
// child closed by the cascade got only the first. The cascade closes children
// with a bulk UPDATE that never goes through FailWorkflow, so the recursion
// point was bypassed by the mechanism doing the closing, and a grandchild of a
// terminated root kept running (IMPROVEMENT-PLAN 3.410, cleat#1108).
//
// The defer arm never had this problem, which is what made it visible: a child
// that owes a defer phase is finalised by FinalizeDeferPhase, which DOES enforce
// the policy, so the depth of a terminate depended on whether a workflow in the
// middle happened to have deferred work (3.411). Two arms of one policy
// disagreeing, and neither the code nor any document said so.
//
// Only the plain arm's children are passed here. A defer-owing child is excluded
// from childrenClosedByTerminate by construction and reaches its own children
// later, through the phase -- so this cannot cascade into one twice.
func cascadeIntoClosedChildren(log *slog.Logger, depth int, childIDs []string, enforce func(childID string, depth int)) {
	if len(childIDs) == 0 {
		return
	}
	if depth >= maxParentCloseDepth {
		log.Error("parent close policy stopped at the depth limit; these workflows and "+
			"anything below them keep running with no parent",
			"depth", depth, "limit", maxParentCloseDepth, "abandoned", childIDs)
		return
	}
	for _, id := range childIDs {
		enforce(id, depth+1)
	}
}

// scanWorkflowIDs collects a single-column id result set. One copy so the three
// dialects' close-policy queries differ only in their placeholders.
func scanWorkflowIDs(rows *sql.Rows) ([]string, error) {
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
