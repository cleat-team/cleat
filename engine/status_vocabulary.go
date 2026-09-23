package engine

import "database/sql"

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
	statusCancelled    = "cancelled"
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
	statusDeadLettered + `', '` + statusTerminated + `', '` + statusCancelled + `'`

// childOutcomeForSettledStatus is GetChildResult's answer to "how did my
// child's run end", derived once here and called identically from all three
// dialects instead of three independent copies of "if status == ...".
//
// cleat#1974: a parent awaiting a terminated or cancelled child never got an
// answer, because GetChildResult had its own, narrower idea of "terminal"
// than settledStatusList -- the same shape as cleat#1213 (dead_lettered
// missing from this exact function) and the GetChildCount bug
// (a_terminal_child_does_not_hold_its_parents_quota_test.go, 'terminated'
// missing there). Three drifting hand-written lists is how each of those
// happened; one function settles it.
//
// ok is false when status is not settled at all, meaning the caller should
// report ChildOutcome{} (still running) -- the only status this repo has
// that answers false today is 'ready'/'running'/'terminating', none of which
// are named in this file's constants for exactly that reason (see the file
// doc comment above).
//
// The fallback case -- settled, but not one of the five known statuses --
// exists so a SIXTH settled status added later (to statusXxx and
// settledStatusList) without a bespoke branch here still gets an answer
// rather than silently reopening this issue: Completed+Failed with the raw
// error_msg, no kind prefix. Nothing in this tree produces that case today;
// it is here so the failure mode is "generic message" rather than "parent
// waits forever."
func childOutcomeForSettledStatus(status, result string, errMsg sql.NullString) (outcome ChildOutcome, ok bool) {
	switch status {
	case statusDone:
		return ChildOutcome{Completed: true, Result: result}, true
	case statusFailed, statusDeadLettered:
		// The result column is never written on either branch; the message
		// is in error_msg, which MoveToDeadLetterQueue also writes.
		return ChildOutcome{Completed: true, Failed: true, Error: errMsg.String}, true
	case statusTerminated:
		// Kind travels as a stable message prefix, the way "[AMBIGUOUS]"
		// already does (durablecalls.go, heartbeats.go) -- no new SDK
		// surface. The guest already receives a failed child as an error
		// message; this just makes the message say which kind of failure.
		return ChildOutcome{Completed: true, Failed: true, Error: "[TERMINATED] " + errMsg.String}, true
	case statusCancelled:
		return ChildOutcome{Completed: true, Failed: true, Error: "[CANCELLED] " + errMsg.String}, true
	}
	if isSettledStatus(status) {
		return ChildOutcome{Completed: true, Failed: true, Error: errMsg.String}, true
	}
	return ChildOutcome{}, false
}

// isSettledStatus reports whether status is one of the five settledStatusList
// spells in SQL. Kept as a plain Go switch rather than parsing
// settledStatusList at runtime -- called on every GetChildResult, and also
// (cleat#1975) by preemptivelySettle and adminForceResolve, which already
// have curStatus in hand from a FOR UPDATE/UPDLOCK read and need a yes/no
// answer rather than a SQL filter. Not a third spelling of the set: the SQL
// string is deliberately not meant to be embedded or parsed outside the
// literal predicates it was built for (see settledStatusList's own comment
// on why a shared helper was reverted there), so this switch is a second,
// independent spelling of the same five statuses rather than derived from
// settledStatusList. engine/one_definition_of_settled_test.go's scan
// exempts this file by name for exactly that reason -- it is the one place
// permitted to spell the set by hand instead of matching the literal text.
func isSettledStatus(status string) bool {
	switch status {
	case statusDone, statusFailed, statusDeadLettered, statusTerminated, statusCancelled:
		return true
	}
	return false
}
