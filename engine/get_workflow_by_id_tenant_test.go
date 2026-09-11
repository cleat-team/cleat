package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// GetWorkflowByID must not answer from another tenant's row, on a connection
// where row-level security is not the thing stopping it.
//
// cleat#1180. The statement was `WHERE id = $1` with no tenant predicate.
// Inside beginTxWithRLS the policy narrows it, so under ENFORCED RLS this was
// already correct and the gap was invisible. The exposure is on a BYPASSING
// connection, and one is reachable: `-rls-check` accepts `off`, and its default
// `auto` only warns unless --require-auth is set. So whether the statement was
// scoped depended on deployment flags rather than on the query.
//
// # Why the test uses a superuser connection deliberately
//
// adminDB is superuser, so the policy does not apply to it. That is the point,
// not a shortcut: it is the "policy removed" condition without dropping the
// policy, so a pass here means the Go-level predicate is doing the isolating.
// The same split is set out at length in
// TestConcurrencyKeysRLS_LayerSeparation, and this is its missing sibling --
// that test covers GetConcurrencyKeyCount, which had its `AND tenant_id = $2`
// all along.
//
// # Why the positive case is asserted too
//
// A predicate that scoped too tightly -- comparing against the wrong value, or
// a store whose tenantID is empty -- would return nothing for ANYBODY and pass
// a cross-tenant test trivially. The first subtest is what stops "returns no
// row" from being mistaken for "is correctly isolated".
func TestGetWorkflowByIDDoesNotAnswerFromAnotherTenantsRow(t *testing.T) {
	adminDB := testutil.TestDB(t, testutil.DialectPostgres)
	defer adminDB.Close()
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, adminDB)
	defer testutil.CleanupPostgresTestData(t, adminDB)

	ctx := context.Background()
	const tenantA = "d1800000-0000-4000-8000-00000000000a"
	const tenantB = "d1800000-0000-4000-8000-00000000000b"
	const defName = "get-by-id-tenant-def"

	deployDefForTenants(t, adminDB, defName, 1, tenantA, tenantB)

	wfA := fmt.Sprintf("get-by-id-a-%d", time.Now().UnixNano())
	if _, _, err := NewPostgresStore(adminDB).WithTenant(tenantA).StartNewRun(
		ctx, wfA, defName, 1, []byte(`{}`), "", tenantA, 0); err != nil {
		t.Fatalf("StartNewRun(A): %v", err)
	}

	t.Run("tenant A reads its own run", func(t *testing.T) {
		got, err := NewPostgresStore(adminDB).WithTenant(tenantA).GetWorkflowByID(ctx, wfA)
		if err != nil {
			t.Fatalf("GetWorkflowByID(A, wfA): %v", err)
		}
		if got == nil || got.ID != wfA {
			t.Fatalf("tenant A could not read its own run: got %v, want %s.\n\n"+
				"Without this, the cross-tenant assertion below passes for a store that "+
				"returns nothing to anyone.", got, wfA)
		}
	})

	t.Run("tenant B cannot read tenant A's run", func(t *testing.T) {
		got, err := NewPostgresStore(adminDB).WithTenant(tenantB).GetWorkflowByID(ctx, wfA)
		if got != nil {
			t.Errorf("tenant B's store read tenant A's run %s over a connection where the "+
				"RLS policy is bypassed.\n\n"+
				"This is cleat#1180. Both other dialects have carried "+
				"`AND tenant_id` here all along -- mysql_ops.go and "+
				"mssql_deployment.go -- because neither has row-level security to "+
				"fall back on; PostgreSQL was the only one relying on the policy "+
				"alone. Under enforced RLS this is invisible, which is what let it "+
				"survive.", wfA)
			return
		}
		// A miss may surface as (nil, nil) or as a not-found error depending on
		// how the scan reports no rows. Either is the correct outcome; what
		// must not happen is a row. An unexpected error is still worth seeing.
		if err != nil {
			t.Logf("cross-tenant read reported: %v (acceptable -- no row was returned)", err)
		}
	})
}
