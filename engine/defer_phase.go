package engine

import (
	"context"
	"fmt"
	"time"
)

// The two-phase terminal transition. IMPROVEMENT-PLAN 3.75 step 2, decision D6
// in tiers.yaml, schema in migrations/{postgres/038,mysql/037,mssql/041}.
//
// A defer body needs a live instance WITH a session: 3.35 phase 2 measured that
// a defer without one panics inside any host call, and a defer that cannot call
// the host cannot release the lock it took. A live instance means replay, which
// means dispatch, which means the workflow must be claimable and NON-TERMINAL.
//
// So a host-driven terminal transition -- one that sets a terminal status by
// UPDATE rather than by a segment finalizing -- cannot both terminate the
// workflow and run its cleanup. It splits in two:
//
//	1. MARK.     TerminateWorkflow records the outcome it intends to apply in
//	             pending_terminal_status, moves the workflow to 'terminating',
//	             and leaves it schedulable. It does NOT release the workflow's
//	             resources.
//	2. FINALIZE. The dispatch loop claims it like any other workflow, replays
//	             its history as a defer segment (WithDeferPhase), runs the
//	             outstanding defers, and calls FinalizeDeferPhase -- which
//	             applies the recorded outcome and only THEN releases the
//	             resources.
//
// The ordering is the point. Before this, TerminateWorkflow called
// releaseWorkflowResources immediately after the terminal UPDATE: the host
// dropped the sticky assignment and the concurrency keys, and the defer that
// would have released them never ran. A terminated workflow's cleanup was not
// merely skipped, it was pre-empted by the host doing a DIFFERENT release, in
// the wrong order, with no record that anything was owed.
//
// Nothing new is durable per defer. What is owed is reconstructed from history
// by DeferralsFromHistory, and each defer body's own host calls are durable
// calls with their own event rows. The only new durable fact is workflow-level:
// "this workflow owes a defer phase, and here is the outcome to apply when it
// is done."

// statusTerminating is the status a workflow holds between the two phases.
//
// Its own status rather than a reuse of 'ready' -- D6 rejected the reuse
// specifically, because it would make "terminating, running its cleanup"
// indistinguishable from "runnable", which is the failure the decision exists
// to avoid. docs/reference/workflow-lifecycle.md carries the state machine.
const statusTerminating = "terminating"

// deferPhaseTimeout bounds the phase, not the worker.
//
// A phase whose worker vanished is already caught by the heartbeat sweep, which
// re-queues it -- and re-queues it again next time, forever, if the guest traps
// on every attempt. This is what stops that: past the deadline, ExpireDeferPhases
// applies the recorded outcome without the cleanup and the workflow leaves
// 'terminating'. A terminate that cannot run its defers must still terminate.
//
// Five minutes is chosen against what the phase does rather than against how
// long a workflow runs: it is a replay of existing history plus the registered
// defer bodies, on a claimed instance, and a cleanup that has not finished in
// five minutes is not going to be finished by waiting.
const deferPhaseTimeout = 5 * time.Minute

// deferPhaseOwedSQL is deferPhaseOwed as a predicate, for the set-based paths
// that close many workflows in one statement and so cannot ask Go about each
// row. IMPROVEMENT-PLAN 3.114.
//
// It has to say the same thing as the Go function, and nothing structural makes
// it: they are two carriers of one rule, which is the shape this repo has been
// bitten by before (compaction had a completeness property test and the
// database payload had none, and every payload defect was invisible until the
// behaviour it broke was noticed). TestTheSQLPredicateAgreesWithTheGoOne
// evaluates this against real rows in every status and compares the answers.
//
// No table alias. PostgreSQL and MySQL take `UPDATE workflow_instances w`,
// SQL Server takes `UPDATE w ... FROM workflow_instances w`, and one fragment
// that works in all three is worth more than a shorter one that needs three
// spellings. Referencing the target table by name inside the correlated
// subquery is legal on all three -- MySQL's "cannot select from the table you
// are updating" restriction is about the subquery's FROM, and this one reads
// event_history.
const deferPhaseOwedSQL = `(workflow_instances.status IN ('ready', 'running', 'suspended')
		   AND (workflow_instances.compaction_state IS NOT NULL
		        OR EXISTS (SELECT 1 FROM event_history e
		                   WHERE e.workflow_id = workflow_instances.id
		                     AND e.event_type = 'defer')))`

// The deadline expression, per dialect, with the timeout embedded as a literal
// rather than bound as a parameter.
//
// Embedded because these go into statements whose parameter lists already
// differ between dialects -- SQL Server's parent-close steps carry a tenant
// parameter the other two do not -- and threading one more argument through
// three different runners to pass a compile-time constant would be three
// changes to buy nothing. It is not user input: it is int(deferPhaseTimeout).
var (
	deferPhaseDeadlinePostgres = fmt.Sprintf("now() + interval '%d seconds'", int(deferPhaseTimeout.Seconds()))
	deferPhaseDeadlineMySQL    = fmt.Sprintf("NOW(6) + INTERVAL %d SECOND", int(deferPhaseTimeout.Seconds()))
	deferPhaseDeadlineMSSQL    = fmt.Sprintf("DATEADD(SECOND, %d, SYSUTCDATETIME())", int(deferPhaseTimeout.Seconds()))
)

// DeferPhaseStore is implemented by stores that can complete a two-phase
// terminal transition.
//
// A capability interface rather than two more methods on Store, for the same
// reason ConcurrencyKeyStore and SignalStore are: Store is implemented by every
// mock in the tree, and a store that cannot finalize a defer phase is also a
// store whose TerminateWorkflow never marks one, so the pair is consistent by
// construction rather than by everyone implementing both.
type DeferPhaseStore interface {
	// FinalizeDeferPhase appends the defer segment's events and applies the
	// outcome recorded at mark time, fenced on the caller's claim. It
	// returns ErrFenceLost if the claim has moved on -- including to
	// ExpireDeferPhases, which bumps the generation for exactly that reason.
	//
	// It takes no query state. A defer segment replays a body that has
	// already decided what it exposes to queries and then runs cleanup, so
	// the snapshot it would carry is the one the last real segment already
	// stored -- and writing back an empty one would blank it.
	FinalizeDeferPhase(ctx context.Context, runID, workerID string, generation int64, newEvents []EventRecord) error

	// ExpireDeferPhases applies the recorded outcome to every workflow whose
	// defer phase has outrun its deadline, and reports how many.
	ExpireDeferPhases(ctx context.Context) (int, error)
}

// deferPhaseOwed reports whether a workflow being terminated should run a defer
// phase before its outcome is applied, given what a single row and one EXISTS
// can tell us.
//
// hasDeferEvents is exact for an uncompacted workflow: defer registrations are
// EventTypeDefer rows in event_history.
//
// compacted is the conservative half, and it is deliberately coarse. Compaction
// PRUNES the rows it folded (engine/db.go's `DELETE FROM event_history WHERE
// workflow_id = $1 AND step < $2`) and carries the registrations forward in
// CompactionState.PendingDefers instead -- so on a compacted workflow the
// EXISTS above answers "no" about rows that no longer exist rather than about
// defers that were never registered. Reading PendingDefers out of the JSON
// would be exact, and would be three different JSON dialects' worth of SQL to
// answer a question whose wrong answer costs one segment that runs nothing.
// The cost of being coarse is a defer phase for a compacted workflow that owes
// nothing; the cost of being exact-looking would be skipped cleanup on the
// long-running workflows most likely to hold a lock.
func deferPhaseOwed(status string, hasDeferEvents, compacted bool) bool {
	if !claimableStatus(status) {
		// Already terminal, already terminating, or dead-lettered: there is
		// no instance to replay into and no outcome left to defer. An
		// already-terminated workflow still matches TerminateWorkflow's
		// UPDATE, because terminate is idempotent -- it just does not
		// acquire a defer phase on the way through.
		//
		// 'terminating' is the interesting member of that set: terminating a
		// workflow that is already running its defer phase terminates it NOW,
		// and the one-phase UPDATE clears the marker on its way past. So a
		// second terminate cuts the cleanup short rather than restarting its
		// clock, and there is no state in which a terminal workflow is left
		// carrying a marker for a phase that will never run.
		return false
	}
	return hasDeferEvents || compacted
}

// claimableStatus reports whether a workflow in this status can still be
// dispatched, which is the same question as "can a defer phase replay it".
func claimableStatus(status string) bool {
	switch status {
	case "ready", "running", "suspended":
		return true
	}
	return false
}

// Every store that can mark a defer phase must also be able to finish one.
// TerminateWorkflow and this pair are one mechanism split across two calls, and
// a store with only the first half would leave workflows in 'terminating' that
// nothing could ever finalize.
var (
	_ DeferPhaseStore = (*PostgresStore)(nil)
	_ DeferPhaseStore = (*MySQLStore)(nil)
	_ DeferPhaseStore = (*MSSQLStore)(nil)
	_ DeferPhaseStore = (*ShardedStore)(nil)
)
