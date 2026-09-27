package tenantctx

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// The contracts THIS package defines, asserted here because they are its own
// and no caller's test can pin them.
//
// What is deliberately absent is a `With` -> `From` round-trip standing alone.
// Both sides name the same unexported key, so such a test proves only that they
// agree with each other, and it would clear a coverage floor while touching
// neither of the two risks the package actually carries. The property that
// matters most -- that auth and engine share ONE key rather than two identical-
// looking ones -- is asserted from `auth`, through the real accessors on both
// sides, in auth/tenant_context_key_is_shared_test.go. That is the right place
// for it, because it is a cross-package property.
//
// What is left for here is the meaning of the two booleans. Both are
// load-bearing, and both have a documented caller that depends on them.

func TestFromReportsNoTenantOnlyWhenNoneIsSet(t *testing.T) {
	if tid, ok := From(context.Background()); ok {
		t.Errorf("From on a bare context = (%v, true), want the zero UUID and false.\n\n"+
			"The caller branches on this: false means \"leave the statement unscoped\".", tid)
	}
}

// The zero UUID is a SET tenant, and the boolean is the only thing that says
// so.
//
// engine's adapter branches on exactly this: !ok leaves the statement unscoped,
// whereas a tenant of uuid.Nil scopes it to a value that matches nothing --
// which reads as an empty table rather than as a missing tenant. Collapsing the
// two turns "no tenant" into "no rows", which is the failure this assertion
// exists to prevent.
func TestTheZeroUUIDIsSetRatherThanAbsent(t *testing.T) {
	tid, ok := From(With(context.Background(), uuid.Nil))
	if !ok {
		t.Fatal("From(With(ctx, uuid.Nil)) reported no tenant.\n\n" +
			"A tenant of the zero UUID must be distinguishable from an unset one, " +
			"or the adapter scopes a statement to a value that matches nothing and " +
			"reports an empty table instead of an unscoped statement.")
	}
	if tid != uuid.Nil {
		t.Errorf("got %v, want the zero UUID", tid)
	}
}

func TestWithRoundTripsAnOrdinaryTenant(t *testing.T) {
	want := uuid.New()
	got, ok := From(With(context.Background(), want))
	if !ok || got != want {
		t.Errorf("round trip = (%v, %v), want (%v, true)", got, ok, want)
	}
}

func TestCrossTenantIsNotSetByDefault(t *testing.T) {
	if reason, ok := CrossTenant(context.Background()); ok {
		t.Errorf("CrossTenant on a bare context reported a bypass (reason %q).", reason)
	}
}

// An empty reason is MARKED, not absent -- and plugindb_tenant.go depends on
// that being true.
//
// It turns `ok && TrimSpace(reason) == ""` into a hard error, precisely so that
// an author who passed "" is told so rather than being sent to look for a
// missing tenant via the fail-closed path. That behaviour rests entirely on
// this boolean being true for the empty string.
func TestAnEmptyReasonIsMarkedAndNotAnAbsentBypass(t *testing.T) {
	reason, ok := CrossTenant(WithCrossTenant(context.Background(), ""))
	if !ok {
		t.Fatal(`CrossTenant(WithCrossTenant(ctx, "")) reported no bypass.` + "\n\n" +
			"The empty reason must be distinguishable from never having called " +
			"WithCrossTenant at all, or the caller cannot tell \"I passed nothing\" " +
			"from \"I am not bypassing\", and collapses one into the other.")
	}
	if reason != "" {
		t.Errorf("reason = %q, want the empty string it was given", reason)
	}
}

// The two keys are independent, and the bypass is tested before the tenant on
// purpose (see plugin.AcrossAllTenants): marking a context which already
// carries a tenant must widen it rather than narrow it. Neither order may drop
// the other value.
func TestTheTwoKeysDoNotDisturbEachOther(t *testing.T) {
	tenant := uuid.New()

	ctx := WithCrossTenant(With(context.Background(), tenant), "fan-out migration")
	if got, ok := From(ctx); !ok || got != tenant {
		t.Errorf("the tenant was lost when the bypass was added: (%v, %v)", got, ok)
	}
	if reason, ok := CrossTenant(ctx); !ok || reason != "fan-out migration" {
		t.Errorf("the bypass was lost when the tenant was added: (%q, %v)", reason, ok)
	}

	// The reverse order, so a change that moves either accessor is caught from
	// both directions.
	ctx2 := With(WithCrossTenant(context.Background(), "sweep"), tenant)
	if got, ok := From(ctx2); !ok || got != tenant {
		t.Errorf("tenant lost (bypass set first): (%v, %v)", got, ok)
	}
	if reason, ok := CrossTenant(ctx2); !ok || reason != "sweep" {
		t.Errorf("bypass lost (tenant set first): (%q, %v)", reason, ok)
	}
}
