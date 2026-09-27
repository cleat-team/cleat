package engine

import "fmt"

// DefaultMaxPriorityMagnitude bounds how far a caller-supplied priority may sit
// from the default of 0, in either direction.
//
// # Why the column needs a bound at all
//
// `priority INTEGER NOT NULL DEFAULT 0` shipped with no CHECK, and the claim
// orders `priority ASC, created_at` -- lower goes first. The value arrives in
// the start request body and nothing narrowed it, so the full int32 range was
// reachable by any caller who could start a workflow.
//
// Within one tenant that lets a caller put one run permanently ahead of its own
// queue, which is a nuisance. Under --claim-across-tenants the same ordering is
// GLOBAL, so `priority: -2147483648` takes the front of every tenant's work
// indefinitely. That is not a scheduling imbalance; it is a caller deciding the
// order for a database it shares. Any per-tenant fairness scheme built on top
// of the same key inherits the hole, which is why this is bounded first and
// separately.
//
// # Why symmetric, rather than a floor of zero
//
// A NEGATIVE PRIORITY IS A FEATURE, not a malformed input, and a bound of 0..N
// would delete it. cleat#1051 exists because a hand-rolled digit scanner made
// negative priorities unexpressible: it stopped at the first non-digit, so a
// leading `-` bound 0 having consumed nothing. The fix was deliberate, and
// plugins/dag's priority_sign_test.go records the reasoning -- negative is how
// a task goes ahead of normal work without renumbering everything already at 0.
//
// So the defect here is UNBOUNDEDNESS, not sign, and the bound is symmetric
// about the default for that reason.
//
// # Why 1000
//
// Large enough that no scheme of coarse bands, numeric tiers or
// renumber-in-the-middle runs out of room, and small enough that the ordering
// stays something an operator can reason about. It is a starting point rather
// than a measurement -- there is no usage data to derive one from -- which is
// why --max-priority-magnitude can move it.
const DefaultMaxPriorityMagnitude = 1000

// ValidatePriority reports whether a caller-supplied priority is within the
// operator's bound.
//
// REFUSES rather than clamping, matching the per-run limit overrides decoded
// from the same request body a few fields away (cleat#1187): a value outside
// the bound is a caller asking for something the deployment does not offer, and
// quietly substituting a different number answers them with a run ordered
// somewhere they did not ask for. The 400 says what the bound is.
//
// A maxMagnitude of 0 or less means the operator set no bound and every value
// is accepted, which is what the flag's 0 documents and what a deployment that
// never set it gets.
func ValidatePriority(priority, maxMagnitude int) error {
	if maxMagnitude <= 0 {
		return nil
	}
	if priority < -maxMagnitude || priority > maxMagnitude {
		return fmt.Errorf("priority %d is outside the permitted range %d..%d "+
			"(lower runs sooner; 0 is the default)", priority, -maxMagnitude, maxMagnitude)
	}
	return nil
}
