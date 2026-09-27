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
	for _, tc := range []struct {
		id        string
		name      string
		suspended bool
	}{
		{live, "suspend-live-" + live[:8], false},
		{held, "suspend-held-" + held[:8], true},
	} {
		if _, err := owner.ExecContext(ctx,
			`INSERT INTO admin.tenants (tenant_id, name, suspended) VALUES ($1, $2, $3)
			 ON CONFLICT (tenant_id) DO UPDATE SET suspended = EXCLUDED.suspended`,
			tc.id, tc.name, tc.suspended); err != nil {
			t.Fatalf("seeding tenant %s: %v", tc.name, err)
		}
	}

	store := NewPostgresStore(owner)
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
