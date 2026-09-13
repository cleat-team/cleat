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
// NOTE FOR ANYONE READING A ZERO HERE: on PostgreSQL this arm can never match.
// finalize_workflow_status already purges those events (cleat#1016), which
// DeleteExpiredEvents' own comment records. A preview reporting 0 for events is
// therefore correct rather than broken, and a test that asserts "preview equals
// sweep" on this arm passes without measuring anything.
const (
	pgExpiredEventsWorkflows = ` FROM workflow_instances
				WHERE status IN ('done', 'failed')
				  AND completed_at IS NOT NULL
				  AND completed_at < $1`

	msExpiredEventsWorkflows = ` FROM workflow_instances
				WHERE status IN ('done', 'failed')
				  AND completed_at IS NOT NULL
				  AND completed_at < @p1`

	myExpiredEventsWorkflows = ` FROM workflow_instances
				WHERE status IN ('done', 'failed')
				  AND completed_at IS NOT NULL
				  AND completed_at < ?`
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
const (
	msCompletedWorkflows = ` FROM workflow_instances
		WHERE status IN ('done', 'failed', 'terminated')
		  AND completed_at IS NOT NULL
		  AND completed_at < @p1`

	myCompletedWorkflows = ` FROM workflow_instances
				WHERE status IN ('done', 'failed', 'terminated')
				  AND completed_at IS NOT NULL
				  AND completed_at < ?`

	pgCompletedWorkflows = ` FROM workflow_instances
		WHERE status IN ('done', 'failed', 'terminated')
		  AND completed_at IS NOT NULL
		  AND completed_at < $1`
)
