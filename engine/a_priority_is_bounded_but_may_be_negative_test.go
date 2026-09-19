package engine

import (
	"strconv"
	"strings"
	"testing"
)

// A caller-supplied priority is bounded in BOTH directions, and negative values
// inside the bound are still accepted.
//
// The second half is the point of the test. The obvious way to stop
// `priority: -2147483648` from taking the front of every tenant's queue is to
// require priority >= 0, and that would silently remove a feature the project
// went to the trouble of fixing: cleat#1051 replaced a hand-rolled digit
// scanner precisely BECAUSE it made negative priorities unexpressible, and
// plugins/dag's priority_sign_test.go records why they are meaningful --
// negative is how a task goes ahead of normal work without renumbering
// everything already at 0.
//
// So a regression here would not look like a failure. It would look like a
// tightened bound, and the run that used to jump the queue would quietly stop
// jumping it. That is what this pins.
func TestAPriorityIsBoundedInBothDirections(t *testing.T) {
	const bound = 1000

	accepted := []int{
		0,     // the default
		1,     // behind normal work
		-1,    // ahead of normal work: the cleat#1051 case
		-500,  // well inside the bound, still negative
		bound, // the edge, inclusive
		-bound,
	}
	for _, p := range accepted {
		if err := ValidatePriority(p, bound); err != nil {
			t.Errorf("ValidatePriority(%d, %d) = %v; want accepted", p, bound, err)
		}
	}

	refused := []int{
		bound + 1,
		-bound - 1,
		1 << 30,
		-(1 << 31), // the value that motivated the bound
	}
	for _, p := range refused {
		err := ValidatePriority(p, bound)
		if err == nil {
			t.Errorf("ValidatePriority(%d, %d) = nil; want refused", p, bound)
			continue
		}
		// The message has to say what the bound IS. A refusal that only says
		// "out of range" leaves the caller guessing at a number the deployment
		// chose, which is the same complaint cleat#1486 makes about limits
		// nothing prints.
		if !strings.Contains(err.Error(), strconv.Itoa(bound)) {
			t.Errorf("ValidatePriority(%d, %d) error %q does not name the bound", p, bound, err)
		}
	}
}

// A bound of zero or less means the operator set none, and the full int32 range
// is restored.
//
// Separate from the case above because "0 disables it" is what the flag
// documents, and a bound applied as `priority < -0 || priority > 0` would pass
// every test written around a positive bound while refusing every non-zero
// priority on a deployment that had opted out.
func TestAPriorityBoundOfZeroAcceptsEverything(t *testing.T) {
	for _, maxMagnitude := range []int{0, -1} {
		for _, p := range []int{0, 1, -1, 1 << 30, -(1 << 31)} {
			if err := ValidatePriority(p, maxMagnitude); err != nil {
				t.Errorf("ValidatePriority(%d, %d) = %v; want accepted with no bound set",
					p, maxMagnitude, err)
			}
		}
	}
}
