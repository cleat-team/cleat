package engine

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine/testutil"
)

// A suspended tenant is not enumerated, so neither loop does work for it.
//
// Enforcing suspension in the tenant enumeration is what makes it cheap: the
// dispatch claim and the due-schedule read both reach per-tenant work through
// this one function, so one predicate stops new work and cron together. This
// test is therefore standing in for both, which is worth saying out loud --
// if the two ever stop sharing an enumeration, this stops covering one of them.
func TestASuspendedTenantIsNotEnumerated(t *testing.T) {
	owner := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, owner, testutil.DialectPostgres)
	applyPostgresProcedures(t, owner)
	testutil.CleanupPostgresTestData(t, owner)
	defer owner.Close()

	ctx := context.Background()
	// OWN TENANT IDS AND OWN NAMES, not the shared xtcTenant fixtures.
	// admin.tenants.name carries a UNIQUE constraint, so a fixed fixture name
	// collides with whatever a previously failed run left behind -- which is
	// how this test first failed after being given its own ids but not its own
	// names. These tests write a
	// column other tests READ -- TestATenantListNeedsNoRLSExemption asserts
	// that tenant B is enumerated -- and leaving one suspended failed that
	// test from across the package. A suspended tenant is invisible to the
	// enumeration by design, which is exactly what makes the leak confusing.
	live, held := uuid.NewString(), uuid.NewString()
	t.Cleanup(func() {
		_, _ = owner.Exec(`DELETE FROM admin.tenants WHERE tenant_id = ANY($1)`,
			"{"+live+","+held+"}")
	})
	// A previous failed run can leave these rows behind, and the ON CONFLICT below
	// would then UPDATE rather than insert them -- so clear them first, because
	// the delta assertions further down would otherwise measure nothing.
	store := NewPostgresStore(owner)
	if _, err := owner.ExecContext(ctx,
		`DELETE FROM admin.tenants WHERE tenant_id = ANY($1)`, "{"+live+","+held+"}"); err != nil {
		t.Fatalf("clearing a previous run's rows: %v", err)
	}
	seed := func(id, name string, suspended bool) {
		t.Helper()
		if _, err := owner.ExecContext(ctx,
			`INSERT INTO admin.tenants (tenant_id, name, suspended) VALUES ($1, $2, $3)
			 ON CONFLICT (tenant_id) DO UPDATE SET suspended = EXCLUDED.suspended`,
			id, name, suspended); err != nil {
			t.Fatalf("seeding tenant %s: %v", name, err)
		}
	}

	seed(live, "suspend-live-"+live[:8], false)
	countWithLive, err := CountTenants(ctx, owner)
	if err != nil {
		t.Fatalf("CountTenants: %v", err)
	}
	enumeratedWithLive, err := store.ListTenantIDs(ctx)
	if err != nil {
		t.Fatalf("ListTenantIDs: %v", err)
	}

	seed(held, "suspend-held-"+held[:8], true)

	// cleat#3773: the runtime guard's predicate is CountTenants, because
	// suspension stops starts and not reads, and a suspended tenant's rows are
	// still readable through a bypassing connection. So the two functions must
	// disagree about WHICH rows they count: every row, versus every live one.
	// Asserted as a DELTA around the suspended row rather than an absolute
	// count, because admin.tenants is shared with other tests in this package.
	// Without this, adding `WHERE NOT suspended` to CountTenants would break the
	// guard's whole premise and nothing would go red.
	countAfter, err := CountTenants(ctx, owner)
	if err != nil {
		t.Fatalf("CountTenants after seeding a suspended tenant: %v", err)
	}
	if got := countAfter - countWithLive; got != 1 {
		t.Errorf("CountTenants counted %d of the SUSPENDED tenant just seeded, want 1: it is "+
			"the runtime guard's predicate and must count suspended tenants (cleat#3773)", got)
	}
	enumeratedAfter, err := store.ListTenantIDs(ctx)
	if err != nil {
		t.Fatalf("ListTenantIDs after seeding a suspended tenant: %v", err)
	}
	if got := len(enumeratedAfter) - len(enumeratedWithLive); got != 0 {
		t.Errorf("ListTenantIDs enumerated %d more rows after a SUSPENDED tenant was seeded, "+
			"want 0", got)
	}

	ids, err := store.ListTenantIDs(ctx)
	if err != nil {
		t.Fatalf("ListTenantIDs: %v", err)
	}
	seen := map[string]bool{}
	for _, id := range ids {
		seen[id] = true
	}

	if seen[held] {
		t.Errorf("a suspended tenant was enumerated, so its work would still be claimed "+
			"and its cron would still fire: %v", ids)
	}
	// The control. Without it this test passes just as well against an
	// enumeration that returns nothing at all.
	if !seen[live] {
		t.Errorf("the live tenant was not enumerated either, so the assertion above "+
			"proves nothing: %v", ids)
	}

	// Resuming puts it back, which is the whole point of suspension being the
	// reversible instrument.
	if _, err := owner.ExecContext(ctx,
		`UPDATE admin.tenants SET suspended = false WHERE tenant_id = $1`, held); err != nil {
		t.Fatalf("resuming: %v", err)
	}
	ids, err = store.ListTenantIDs(ctx)
	if err != nil {
		t.Fatalf("ListTenantIDs after resume: %v", err)
	}
	resumed := false
	for _, id := range ids {
		if id == held {
			resumed = true
		}
	}
	if !resumed {
		t.Errorf("a resumed tenant was still not enumerated: %v", ids)
	}
}

// IsTenantSuspended answers for one tenant, and a tenant with no row is NOT
// suspended.
//
// The second half is a decision rather than an oversight. admin.tenants is a
// registry a deployment can run without populating -- 002_defaults.sql seeds
// only the default tenant -- so treating "absent" as suspended would refuse
// every start on a configuration that works today.
func TestAnAbsentTenantIsNotSuspended(t *testing.T) {
	owner := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, owner, testutil.DialectPostgres)
	applyPostgresProcedures(t, owner)
	testutil.CleanupPostgresTestData(t, owner)
	defer owner.Close()

	ctx := context.Background()
	held := uuid.NewString()
	t.Cleanup(func() {
		_, _ = owner.Exec(`DELETE FROM admin.tenants WHERE tenant_id = $1`, held)
	})
	if _, err := owner.ExecContext(ctx,
		`INSERT INTO admin.tenants (tenant_id, name, suspended) VALUES ($1, $2, true)`,
		held, "susp-absent-"+held[:8]); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	store := NewPostgresStore(owner)
	for _, tc := range []struct {
		name   string
		tenant string
		want   bool
	}{
		{"suspended", held, true},
		{"no row at all", "11111111-1111-4111-8111-111111111111", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := store.IsTenantSuspended(ctx, tc.tenant)
			if err != nil {
				t.Fatalf("IsTenantSuspended: %v", err)
			}
			if got != tc.want {
				t.Errorf("IsTenantSuspended(%s) = %v, want %v", tc.tenant, got, tc.want)
			}
		})
	}
}
