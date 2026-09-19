package engine

import (
	"context"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// The whole rotating claim rests on one asymmetry: a connection that RLS
// applies to can see WHICH tenants exist, but not their workflows.
//
// If that were not true the design would need the `cleat_dispatcher` role after
// all, and with it the `BYPASSRLS` attribute that managed PostgreSQL cannot
// grant -- measured on PostgreSQL 16 against a role of exactly the shape AWS
// documents for the RDS master user (`NOSUPERUSER ... CREATEDB CREATEROLE`):
//
//	ERROR:  permission denied to create role
//	DETAIL:  Only roles with the BYPASSRLS attribute may create roles with
//	         the BYPASSRLS attribute.
//
// So this is not a test that ListTenantIDs returns rows. It is a test of the
// premise that makes a grant-free multi-tenant worker possible, and it asserts
// BOTH halves on the SAME connection: a test that only checked the tenant list
// would pass just as happily against a connection that could see everything.
//
// It runs as `cleat_app` -- the role that actually ships -- rather than as the
// synthetic RLS test role, because the grant under test is one 005_app_role.sql
// makes. A test role granted SELECT on admin.tenants by its own fixture would
// prove the query works and say nothing about whether a deployed worker may run
// it.
func TestATenantListNeedsNoRLSExemption(t *testing.T) {
	owner := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, owner, testutil.DialectPostgres)
	applyPostgresProcedures(t, owner)
	testutil.CleanupPostgresTestData(t, owner)
	defer owner.Close()

	applyAppRoleMigration(t, owner)
	appDB := appRoleDB(t, owner)

	ctx := context.Background()
	deployCrossTenantDef(t, owner)
	for _, tc := range []struct{ id, name string }{
		{xtcTenantA, "rotate-list-a"},
		{xtcTenantB, "rotate-list-b"},
	} {
		if _, err := owner.ExecContext(ctx,
			`INSERT INTO admin.tenants (tenant_id, name) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			tc.id, tc.name); err != nil {
			t.Fatalf("seeding tenant %s: %v", tc.name, err)
		}
	}

	// A workflow belonging to tenant B. The store below is scoped to A, so RLS
	// must hide this row -- and must not hide B itself.
	seedWorkflow(t, owner, seedWorkflowOpts{tenantID: xtcTenantB})

	store := NewPostgresStore(appDB).WithTenant(xtcTenantA)

	// Half one: the tenant list is visible.
	ids, err := store.ListTenantIDs(ctx)
	if err != nil {
		t.Fatalf("ListTenantIDs as cleat_app: %v", err)
	}
	seen := map[string]bool{}
	for _, id := range ids {
		seen[id] = true
	}
	for _, want := range []string{xtcTenantA, xtcTenantB} {
		if !seen[want] {
			t.Errorf("ListTenantIDs did not return tenant %s; got %v", want, ids)
		}
	}

	// Half two, the control: the same connection cannot read tenant B's
	// workflows. Without this the half above proves only that a row came back,
	// not that the connection was ever constrained.
	claimed, err := store.ClaimWorkflows(ctx, "rotate-list-worker", 10)
	if err != nil {
		t.Fatalf("ClaimWorkflows as cleat_app: %v", err)
	}
	for _, wf := range claimed {
		if wf.TenantID == xtcTenantB {
			t.Fatalf("a store scoped to %s claimed a workflow belonging to %s; RLS is not "+
				"constraining this connection, so the first half of this test proves nothing",
				xtcTenantA, xtcTenantB)
		}
	}

	// POSITIVE CONTROL for half two. "Claimed nothing" is the expected result
	// above and is also what a claim that is broken for some unrelated reason
	// returns, so the same connection is pointed at tenant B and must now find
	// the row it could not see a moment ago. Without this, half two passes on
	// a store that claims nothing for anyone.
	asB := NewPostgresStore(appDB).WithTenant(xtcTenantB)
	got, err := asB.ClaimWorkflows(ctx, "rotate-list-worker", 10)
	if err != nil {
		t.Fatalf("ClaimWorkflows as cleat_app scoped to B: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("a store scoped to tenant B claimed nothing; the row is unclaimable for some " +
			"reason other than RLS, so the cross-tenant assertion above is vacuous")
	}
}
