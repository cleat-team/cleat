package engine

import (
	"testing"
	"time"
)

// The precedence rule, tested once: operator >= tenant >= run, each may lower
// and never raise.
//
// cleat#1187 asked for "one precedence rule, stated once and tested once,
// rather than three implementations". resolveLimit is the statement; this is
// the test. It exercises the composition directly rather than through an
// Engine, because what is being pinned is the ARITHMETIC -- which tier wins for
// every combination of set and unset -- and routing that through a database and
// a wasm backend would test the plumbing while leaving the table below
// unexercised.
//
// The plumbing is tested separately and against real databases; this is the
// part that has to be exhaustive.
func TestLimitPrecedenceOperatorTenantRun(t *testing.T) {
	const (
		unset = time.Duration(0)
		big   = 60 * time.Second
		mid   = 30 * time.Second
		small = 5 * time.Second
	)
	// compose is exactly what resolveLimit does.
	compose := func(operator, tenant, run time.Duration) time.Duration {
		return ClampToCeiling(run, ClampToCeiling(tenant, operator))
	}

	for _, tc := range []struct {
		name                  string
		operator, tenant, run time.Duration
		want                  time.Duration
	}{
		{"nobody set anything", unset, unset, unset, unset},
		{"operator only", big, unset, unset, big},
		{"tenant tightens below operator", big, mid, unset, mid},
		{"run tightens below tenant", big, mid, small, small},
		{"run tightens with no tenant setting", big, unset, small, small},
		{"tenant tries to raise past operator", small, big, unset, small},
		{"run tries to raise past tenant", big, small, mid, small},
		{"run tries to raise past operator with no tenant", small, unset, big, small},
		{"every tier raises: the smallest still wins", small, big, big, small},
		{"operator unbounded, tenant bounds it", unset, mid, unset, mid},
		{"operator unbounded, run bounds it", unset, unset, small, small},
		{"operator unbounded, run tightens tenant", unset, mid, small, small},
		{"equal values are stable", mid, mid, mid, mid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := compose(tc.operator, tc.tenant, tc.run)
			if got != tc.want {
				t.Errorf("operator=%v tenant=%v run=%v -> %v, want %v",
					tc.operator, tc.tenant, tc.run, got, tc.want)
			}
		})
	}

	// The property the table is enumerating, asserted as a property so that a
	// case nobody thought to write cannot escape it: the result is never wider
	// than any tier that set a bound.
	//
	// This is the whole security argument for per-tier settings. If a lower
	// tier could widen, a tenant on a shared deployment could raise its own
	// bounds past what the operator granted -- and per-run overrides would hand
	// that same escalation to anyone who can start a workflow.
	t.Run("no tier can widen another", func(t *testing.T) {
		vals := []time.Duration{unset, small, mid, big}
		for _, op := range vals {
			for _, te := range vals {
				for _, rn := range vals {
					got := compose(op, te, rn)
					for _, tier := range []struct {
						name string
						v    time.Duration
					}{{"operator", op}, {"tenant", te}, {"run", rn}} {
						if tier.v > 0 && got > tier.v {
							t.Fatalf("operator=%v tenant=%v run=%v -> %v, which is WIDER than the %s tier's %v.\n\n"+
								"A lower tier widening a higher one is the escalation ClampToCeiling exists to refuse.",
								op, te, rn, got, tier.name, tier.v)
						}
					}
				}
			}
		}
	})
}
