package engine

// The vocabulary of workflow_instances.status, stated once.
//
// WHY THIS FILE EXISTS. There is no CHECK constraint on the column, so nothing
// in the database enforces which values are legal, and until now nothing in Go
// did either: every predicate that asked "is this run settled?" spelled the
// answer out by hand in SQL. A census of tracked Go on 2026-09-14 found 59
// status-list predicates in ten distinct spellings, of which four were asking
// this one question in three different ways.
//
// That is not a tidiness complaint. Two defects have come out of it:
//
//   - cleat#1227: `NOT IN ('done', 'failed')` omitted 'dead_lettered', so a
//     parent closing with TERMINATE overwrote a dead-lettered child -- losing
//     its dead-letter membership and its failure reason, and leaving error_code
//     naming a different cause than error_msg.
//   - GetChildCount, all three dialects, found by
//     engine/a_terminal_child_does_not_hold_its_parents_quota_test.go: the same
//     list omitted 'terminated', so a terminated child kept holding its
//     parent's child-workflow quota permanently.
//
// Both are the same bug, two years of drift apart, and both were a hand-written
// list that fell behind the statuses the engine actually writes.
// TWO DIFFERENT QUESTIONS, and conflating them is how the next one happens.
//
//	SETTLED            the run is over; nothing more will be written to it
//	CANNOT RUN GUEST   the run may still write, but will not execute guest code
//
// 'terminating' separates them. A terminating run is mid-shutdown, running its
// defer phase: it is NOT settled -- a terminal write is still owed -- but it
// will not accept new work, which is why cmd/cleat-worker's isTerminalStatus
// includes it and this list does not. Keep them apart deliberately; they are
// not two spellings of one idea.
const (
	// 'ready' and 'running' are part of the vocabulary named above but have no
	// constant here: nothing in this file's business needs them, and an unused
	// exported-shaped name is a maintenance claim nobody is making. The list in
	// the comment is the record; these four are the ones settledStatusList
	// builds from.
	statusDone         = "done"
	statusFailed       = "failed"
	statusDeadLettered = "dead_lettered"
	statusTerminated   = "terminated"
	// statusTerminating is declared in engine/defer_phase.go, beside the
	// two-phase transition that writes it.
)

// settledStatusList is the CANONICAL SPELLING of the settled set in SQL.
//
// IT IS DELIBERATELY NOT EMBEDDED IN ANY QUERY, and that is the opposite of
// what it looks like it is for. The obvious design is for every predicate to
// concatenate this constant. That was built, measured, and reverted, because it
// blinds a security guard:
//
// engine/mssql_tenant_predicate_test.go extracts each statement from the
// backtick literal in the AST and does no constant folding. Splitting a query
// into "... status not in (` + "`" + ` + settledStatusList + ` + "`" + `)" leaves it reading only
// the FIRST fragment. Measured 2026-09-14 on GetChildCount, which it then saw
// in full as:
//
//	select count(*) from workflow_instances where parent_workflow_id = @p1 and status not in (
//
// -- everything after the split, including any tenant predicate, invisible. One
// statement was additionally mis-attributed to package level. A refactor that
// makes a tenant-isolation guard see less of a statement is not worth the
// deduplication, whichever way the resulting verdict happens to fall.
//
// So the statements keep their literal lists, and
// engine/one_definition_of_settled_test.go enforces that every one of them
// spells this exact text. One source of truth, checked rather than shared.
//
// THE ORDER IS THE ONE THE TREE ALREADY USED, not alphabetical. Twelve of the
// fifteen sites were already correct in this order, so matching it means the
// correction touches only the three that were wrong -- and leaves the digests
// in mssql_tenant_predicate_test.go's exemption list untouched.
const settledStatusList = `'` + statusDone + `', '` + statusFailed + `', '` +
	statusDeadLettered + `', '` + statusTerminated + `'`
