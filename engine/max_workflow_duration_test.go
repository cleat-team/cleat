package engine

import (
	"testing"
	"time"
)

// The per-tenant workflow-duration bound reaches the deadline the backend runs
// under, and a tenant cannot use it to widen the operator's.
//
// cleat#1117. The arithmetic is pinned exhaustively and once by
// TestLimitPrecedenceOperatorTenantRun, and the columns by
// TestARunsOwnLimitsSurviveAStartAndAreReadBack. Neither of those can see the
// failure this exists for: executor.go reading e.defaultWorkflowTimeout
// directly instead of calling the resolver. The value would resolve correctly,
// round-trip correctly, and never reach a context deadline.
//
// That is not hypothetical -- it is the shape IMPROVEMENT-PLAN 3.90 hit, where
// one of two call sites was fixed and the other kept using the flag.
//
// There were two sites here too (executeWithBackend and executeCompiled), and
// reverting ONLY the second left this suite green -- measured, before the fix.
// The answer was not a second test driving a wazero module to observe a context
// deadline; it was to give both sites one statement to share. See
// Engine.withResolvedWorkflowDeadline. This test covers that statement through
// executeWithBackend, and the other path now calls the same one.
func TestATenantCanTightenTheWorkflowDurationButNotWidenIt(t *testing.T) {
	// Buckets hundreds of seconds apart, so there is no wall-clock race here.
	// #609 shipped one and failed CI on an unrelated PR.
	flag := []EngineOption{WithDefaultWorkflowTimeout(300 * time.Second)}

	noOverride := deadlineSeenBy(t, flag, TenantSettings{})
	tighter := deadlineSeenBy(t, flag, TenantSettings{MaxWorkflowDuration: 5 * time.Second})
	looser := deadlineSeenBy(t, flag, TenantSettings{MaxWorkflowDuration: 3000 * time.Second})

	if noOverride < 200*time.Second {
		t.Errorf("a tenant with no override got %v, expected the operator's 300s", noOverride)
	}
	if tighter > 60*time.Second {
		t.Errorf("a tenant that set 5s got %v.\n\n"+
			"This is the whole feature: the per-tenant value was read but did not "+
			"reach the context deadline. Check that executor.go calls "+
			"e.maxWorkflowDuration(execCtx) rather than reading "+
			"e.defaultWorkflowTimeout, at BOTH sites.", tighter)
	}
	if tighter >= noOverride {
		t.Errorf("the tenant that tightened (%v) did not end up below the tenant that "+
			"set nothing (%v).\n\n"+
			"Asserting the DIFFERENCE: a bug returning one number for every tenant "+
			"would satisfy both absolute bounds above and fail here.", tighter, noOverride)
	}
	if looser > 400*time.Second {
		t.Errorf("a tenant that asked for 3000s got %v -- MORE than the operator's "+
			"300s flag.\n\n"+
			"On a shared deployment that means any tenant can hold a worker as long "+
			"as it likes by writing its own settings row. See ClampToCeiling.", looser)
	}
}

// An operator who set no flag still lets a tenant set its own bound.
//
// This case is specific to THIS limit and is why it gets its own test.
// --max-workflow-duration defaults to 0, and the other two resolvers substitute
// a fallback when their operator value is non-positive -- wallClockCeiling uses
// the instance timeout, hostRetryBudget uses DefaultHostRetryBudget. This one
// deliberately passes 0 through, because ClampToCeiling reads a non-positive
// CEILING as "operator unbounded, take the tier below".
//
// Get that wrong by copying a sibling's fallback and the failure is silent in
// the direction that looks fine: every tenant on an unconfigured deployment
// would get some bound nobody asked for.
func TestWithNoOperatorFlagATenantsOwnBoundStillApplies(t *testing.T) {
	none := []EngineOption{} // --max-workflow-duration unset, i.e. 0

	d := deadlineSeenBy(t, none, TenantSettings{MaxWorkflowDuration: 20 * time.Second})
	if d > 60*time.Second {
		t.Fatalf("with no operator flag, a tenant asking for 20s got %v.\n\n"+
			"ClampToCeiling treats a non-positive ceiling as 'operator unbounded, "+
			"take the tenant's value'. A fallback substituted for the 0 flag would "+
			"break this -- and on a deployment that never set the flag, it is the "+
			"only tier there is.", d)
	}
	if d <= 0 {
		t.Fatalf("expected a positive remaining deadline, got %v", d)
	}
}
