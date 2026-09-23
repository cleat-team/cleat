package engine

// Retention predicates, one definition per arm per dialect.
//
// WHY THESE EXIST AS CONSTANTS AT ALL. A retention dry-run has to report what
// the sweep would delete, and cleat#1457's decision is explicit that it must not
// do so by way of a second, separately-written query: such a query is a MODEL of
// the sweep rather than the sweep, and it diverges silently and in the direction
// that reassures. Sharing the predicate makes the two agree by construction
// instead of by discipline -- there is no second copy to drift.
//
// So each constant below is the WHERE clause of exactly one retention arm, and
// it is used twice: the sweep wraps it in a DELETE or UPDATE with a LIMIT, and
// the preview wraps it in a COUNT. Changing which rows an arm touches means
// editing one string, and both paths move together.
//
// THE COUNT IS BEST-EFFORT AND MUST NOT BE READ AS A PROMISE. It is taken at one
// instant; the sweep runs later, against a set that has moved -- workflows
// complete and age past the cutoff between the two calls, and other workers are
// writing throughout. The preview answers "roughly this much, as of now", which
// is what an operator needs to catch a mistyped window, and not "exactly these
// rows will be deleted". See retentionSweepResult.DryRun.
//
// The placeholders differ per dialect ($1 on PostgreSQL, ? elsewhere), which is
// why there is one set per dialect rather than one shared set. They are grouped
// by arm rather than by dialect so the three spellings of one predicate sit
// together and a divergence between them is visible.

// WHAT THESE DELIBERATELY DO NOT CONTAIN: the tenant clause.
//
// Each statement appends its own `AND tenant_id = ...` (or relies on row-level
// security, on PostgreSQL's events and compaction arms). Sharing the RETENTION
// predicate and not the ISOLATION one is deliberate, and the reason is that two
// static guards read these statements:
// engine/mysql_tenant_predicate_test.go and its SQL Server sibling check that
// every statement touching a tenant-scoped table compares tenant_id to a
// parameter. MySQL and SQL Server have no row-level security, so on those
// dialects that predicate IS the isolation.
//
// Both guards read the SQL as written. The MySQL one splices adjacent literals
// but DROPS the expression between them -- its own comment says so, and says
// that is enough "for a predicate question" -- and the SQL Server one relies on
// its statements being single literals, which it also says. Moving a tenant
// clause into a constant therefore makes it invisible to both: measured, the
// MySQL guard reported DeleteExpiredEvents as having no tenant predicate at all
// once the clause moved here.
//
// The guard is right to complain. A predicate it cannot see is a predicate
// nobody is checking, and "the constant is correct" is exactly the assurance a
// guard exists to replace. So the isolation clause stays in each statement,
// where it is verified per statement -- which is stronger than sharing it --
// and only the retention window, which is what cleat#1457 is about, is shared.

// ---- expired event history -------------------------------------------------
//
// THIS ARM DOES MATCH, ON EVERY DIALECT, ON A DEFAULT DEPLOYMENT. This
// comment used to say the opposite -- "on PostgreSQL this arm can never
// match, finalize_workflow_status already purges those events" -- and that
// was wrong: it described CompleteWorkflow's terminal path, which is not the
// one 'failed' workflows take. cmd/cleat-worker/setup.go's own comment on
// FinalizeWorkflowSegment's one production call site is explicit that
// finalStatus there is "only ever 'done' or 'ready' ... never 'failed'"; the
// real 'failed' path is store.FailWorkflow (engine/store_lifecycle.go), which
// deletes no event_history at all. cleat#2038, found while grounding
// cleat#1999's TLA+ model in source rather than trusting this comment.
//
// So a 'done' workflow's events are purged at finalize and this arm sees
// them already gone; a 'failed' workflow's events are NOT, and this arm is
// what removes them, --retention-days days later (default 30, on by
// default). A preview reporting a nonzero count for 'failed' rows is
// therefore measuring something real, not noise.
//
// AND history_swept_at IS NULL is cleat#2038's fix for the consequence of
// that: this arm's own DELETE has no per-row guard against a call whose
// intent (engine/callintent.go's WriteAheadIntent) was written but never
// resolved, so sweeping it left ReReplay's pending-intent guard
// (engine/admin_ops.go) unable to tell "never attempted" from "swept,
// outcome unknown" -- both read as empty history. DeleteExpiredEvents now
// sets history_swept_at on every workflow whose event_history it deletes,
// and this clause keeps an already-swept workflow from being re-selected on
// the next sweep, matching this arm's own batch-termination loop.
const (
	pgExpiredEventsWorkflows = ` FROM workflow_instances
				WHERE status IN ('done', 'failed')
				  AND completed_at IS NOT NULL
				  AND completed_at < $1
				  AND history_swept_at IS NULL`

	msExpiredEventsWorkflows = ` FROM workflow_instances
				WHERE status IN ('done', 'failed')
				  AND completed_at IS NOT NULL
				  AND completed_at < @p1
				  AND history_swept_at IS NULL`

	myExpiredEventsWorkflows = ` FROM workflow_instances
				WHERE status IN ('done', 'failed')
				  AND completed_at IS NOT NULL
				  AND completed_at < ?
				  AND history_swept_at IS NULL`
)

// ---- compaction bookkeeping on terminal workflows ---------------------------
const (
	pgExpiredCompactionState = ` FROM workflow_instances
				WHERE status IN ('done', 'failed')
				  AND completed_at IS NOT NULL
				  AND completed_at < $1
				  AND compaction_state IS NOT NULL`

	msExpiredCompactionState = ` FROM workflow_instances
				WHERE status IN ('done', 'failed')
				  AND completed_at IS NOT NULL
				  AND completed_at < @p1
				  AND compaction_state IS NOT NULL`

	myExpiredCompactionState = ` FROM workflow_instances
				WHERE status IN ('done', 'failed')
				  AND completed_at IS NOT NULL
				  AND completed_at < ?
				  AND compaction_state IS NOT NULL`
)

// ---- dead-lettered workflow records ----------------------------------------
const (
	msDeadLetteredWorkflows = ` FROM workflow_instances
		WHERE status = 'dead_lettered'
		  AND completed_at IS NOT NULL
		  AND completed_at < @p1`

	myDeadLetteredWorkflows = ` FROM workflow_instances
				WHERE status = 'dead_lettered'
				  AND completed_at IS NOT NULL
				  AND completed_at < ?`

	pgDeadLetteredWorkflows = ` FROM workflow_instances
		WHERE status = 'dead_lettered'
		  AND completed_at IS NOT NULL
		  AND completed_at < $1`
)

// ---- completed workflow records --------------------------------------------
//
// 'cancelled' JOINED THIS ARM WITH THE STATUS ITSELF (cleat#1153), and leaving
// it out would have shipped a fresh instance of cleat#1023. That issue measured
// the retention picture and found it aimed away from the rows that survive: the
// statuses whose event history is NOT purged at finalize are exactly the ones
// the on-by-default sweep excludes. A new terminal status collected by no arm
// at all is the same defect one step worse -- 'dead_lettered' at least has its
// own arm above; 'cancelled' would have had none, and would accumulate forever
// on every deployment.
//
// This is the flag-gated arm (--completed-workflow-retention-days, off by
// default), so an operator who has opted into collecting terminated runs now
// also collects cancelled ones. That is what they would expect: the two
// statuses are the same kind of event, both imposed by an operator on a run
// that did not finish on its own.
const (
	msCompletedWorkflows = ` FROM workflow_instances
		WHERE status IN ('done', 'failed', 'terminated', 'cancelled')
		  AND completed_at IS NOT NULL
		  AND completed_at < @p1`

	myCompletedWorkflows = ` FROM workflow_instances
				WHERE status IN ('done', 'failed', 'terminated', 'cancelled')
				  AND completed_at IS NOT NULL
				  AND completed_at < ?`

	pgCompletedWorkflows = ` FROM workflow_instances
		WHERE status IN ('done', 'failed', 'terminated', 'cancelled')
		  AND completed_at IS NOT NULL
		  AND completed_at < $1`
)
