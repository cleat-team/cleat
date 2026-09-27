package engine_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// admin.in_flight_workflow_ids() must answer for a caller that row-level
// security applies to. cleat#1528, migration 073.
//
// WHY THIS IS A BEHAVIOURAL TEST AND NOT A CATALOGUE ONE. The property that
// matters is "a background sweep with no tenant can ask which workflows are in
// flight", and three separate catalogue facts have to hold for it: the function
// is SECURITY DEFINER, it is OWNED BY cleat_dispatcher, and cleat_dispatcher
// has BYPASSRLS. Asserting the three separately is a test that goes green while
// the thing it is about is broken, because it is the CONJUNCTION that works --
// and the conjunction is easy to break by accident. A later migration that
// re-creates the function without the ALTER ... OWNER TO leaves it owned by the
// migration runner, which under FORCE ROW LEVEL SECURITY is subject to the
// policy like anyone else, and the sweep goes back to raising P0001.
//
// The catalogue assertions are still here, below the behavioural one, because
// when this fails they say which of the three is missing -- and because they
// are not redundant. Measured while writing this: re-owning the function to
// `postgres` left the BEHAVIOURAL arm green, since postgres is a superuser and
// bypasses RLS for its own reasons. Only the owner assertion reported it. On a
// deployment whose migration runner is not a superuser the behavioural arm
// would fire too, which is exactly why neither alone is enough: the two fail on
// overlapping but different sets of databases.
//
// AND IT CONNECTS AS PostgresRLSTestRole, WHICH IS THE WHOLE TEST. Run as the
// superuser the engine suite normally uses, every arm passes whether or not
// migration 073 was ever applied -- a superuser bypasses RLS unconditionally,
// so the query it is protecting was never refused in the first place.
func TestTheInFlightQueryAnswersARoleThatRLSAppliesTo(t *testing.T) {
	su := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, su, testutil.DialectPostgres)
	ctx := context.Background()

	rlsDB := testutil.OpenPostgresRLSTestDB(t, su)
	defer func() { _ = rlsDB.Close() }()

	var isSuper, canBypass bool
	if err := rlsDB.QueryRowContext(ctx,
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`,
	).Scan(&isSuper, &canBypass); err != nil {
		t.Fatalf("reading the connecting role's RLS exemptions: %v", err)
	}
	if isSuper || canBypass {
		t.Fatalf("UNMEASURED: connected as a role with rolsuper=%v rolbypassrls=%v, "+
			"which PostgreSQL exempts from row-level security. Every assertion below "+
			"would pass against a database where 073 was never applied.", isSuper, canBypass)
	}

	// THE CONTROL, AND IT IS THE REASON THE REST MEANS ANYTHING. The same read,
	// written out directly, is what blobstore's sweep used to do and what it
	// was refused for. If this ever stops failing, the policy on
	// workflow_instances has changed and the function may no longer be needed
	// -- but the test must not go quietly green on that, so it is asserted.
	var n int
	err := rlsDB.QueryRowContext(ctx,
		`SELECT count(*) FROM workflow_instances WHERE status IN ('ready','running')`).Scan(&n)
	if err == nil {
		t.Fatal("reading workflow_instances directly with no tenant in context SUCCEEDED.\n\n" +
			"That is the state migration 073 exists to work around. Either the policy on " +
			"workflow_instances changed, or this connection is exempt from it. If the " +
			"former, admin.in_flight_workflow_ids() may be removable -- but check before " +
			"deleting it, because the sweeps depend on it.")
	}
	if !strings.Contains(err.Error(), "cleat.tenant_id is not set") {
		t.Fatalf("the direct read failed, but not because the policy refused it: %v", err)
	}

	// THE FUNCTION, ON THE SAME CONNECTION, WITH NO TENANT SET. An empty result
	// is a pass: what is asserted is that the call is ANSWERED rather than
	// refused. Seeding a running workflow would need a tenant, which is the one
	// thing this caller has not got.
	if err := rlsDB.QueryRowContext(ctx,
		`SELECT count(*) FROM admin.in_flight_workflow_ids()`).Scan(&n); err != nil {
		t.Fatalf("admin.in_flight_workflow_ids() was refused for a role RLS applies to: %v\n\n"+
			"This is the defect cleat#1528 describes, reaching the sweep through the "+
			"function that was supposed to remove it. Check the three catalogue "+
			"properties below.", err)
	}

	// Now say WHICH of the three is missing, for whoever is reading the failure
	// above. None of these three is sufficient on its own.
	var secdef bool
	var owner, volatility string
	if err := su.QueryRowContext(ctx, `
		SELECT p.prosecdef, pg_get_userbyid(p.proowner), p.provolatile
		FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = 'admin' AND p.proname = 'in_flight_workflow_ids'`,
	).Scan(&secdef, &owner, &volatility); err != nil {
		t.Fatalf("reading the function's catalogue entry: %v", err)
	}
	if !secdef {
		t.Error("admin.in_flight_workflow_ids() is not SECURITY DEFINER, so it runs as its caller")
	}
	if owner != "cleat_dispatcher" {
		t.Errorf("admin.in_flight_workflow_ids() is owned by %q, want cleat_dispatcher. "+
			"SECURITY DEFINER lends the OWNER's exemption, and under FORCE ROW LEVEL "+
			"SECURITY the table owner has none to lend -- only BYPASSRLS does, which is "+
			"what cleat_dispatcher holds and nothing else does.", owner)
	}
	// 's' is STABLE. A VOLATILE function may not appear in an index condition
	// and cannot be hoisted out of a scan; cleat#1488 is what that cost when
	// every RLS policy in the schema rested on one.
	if volatility != "s" {
		t.Errorf("admin.in_flight_workflow_ids() has provolatile=%q, want \"s\" (STABLE)", volatility)
	}

	var bypass bool
	if err := su.QueryRowContext(ctx,
		`SELECT rolbypassrls FROM pg_roles WHERE rolname = 'cleat_dispatcher'`).Scan(&bypass); err != nil {
		t.Fatalf("reading cleat_dispatcher: %v", err)
	}
	if !bypass {
		t.Error("cleat_dispatcher does not hold BYPASSRLS, so owning the function lends nothing")
	}
}
