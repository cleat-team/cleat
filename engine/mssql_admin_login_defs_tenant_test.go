package engine

// IMPROVEMENT-PLAN 3.86, third table -- and the first one where the failure is
// not a leak but WRONG CODE EXECUTING.
//
// D7 (3.77) made this reachable, which is worth stating plainly: before
// migration 035 two tenants could not both hold a definition called
// "order-processor", so `WHERE name = @p1 AND version = @p2` identified exactly
// one row even on a connection with no tenant filtering. After 035 it does not.
// On a dbo.cleat_admin login -- which a multi-tenant SQL Server deployment must
// use, because GetDueSchedulesAcrossTenants and the cross-tenant claim require
// it -- dbo.fn_tenant_filter is off, so that predicate now matches BOTH
// tenants' rows and QueryRow takes whichever the engine hands back first.
//
// Measured before the fix, with tenant A holding order-processor v1 as 0xAA and
// tenant B holding its own v1 as 0xBB:
//
//     LoadWASM(A) -> 0xAA   correct
//     LoadWASM(B) -> 0xAA   TENANT B WAS HANDED TENANT A'S COMPILED CODE
//     ListVersions(B) -> [1 1]        each tenant deployed exactly one version
//     ListWorkflowDefs(B) -> 2 rows   each tenant deployed exactly one
//
// So the consequence is not that B learns something about A. It is that B's
// workflow runs A's WASM. Everything downstream -- the ABI check, the replay
// history, the result -- is then about a program the tenant never deployed.
//
// THE ASSERTIONS HERE ARE DELIBERATELY STRONGER THAN 3.77 STEP 1'S. That file
// (def_lookup_tenant_property_test.go) asserts "tenant B sees NOTHING", which
// was right when only one tenant could hold a name and is wrong now: B is
// entitled to its own order-processor. So every case below asserts B sees ITS
// OWN answer, and the fixture is lopsided -- A holds v1 and v2, B holds only v1
// -- so "its own" and "the other's" are different values rather than the same
// one. A symmetric fixture would pass against no fix at all.
//
// And step 1's file could not have caught this even with the right assertion,
// because it runs on tenant-scoped stores where the security policy does the
// filtering. On that connection these statements are correct. Which connection
// the test runs on decides the answer -- the same lesson the tags file records.

import (
	"context"
	"fmt"
	"testing"
)

const (
	adminDefName = "order-processor"
	aMarker      = 0xAA
	bMarker      = 0xBB
)

// seedLopsidedDefs gives tenant A v1 and v2 of one name and tenant B only v1,
// with WASM bytes that identify the owner.
func seedLopsidedDefs(t *testing.T, storeA, storeB *MSSQLStore) {
	t.Helper()
	ctx := context.Background()
	for _, v := range []int{1, 2} {
		if err := storeA.DeployWorkflowDef(ctx, &WorkflowDef{
			Name: adminDefName, Version: v,
			WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d, aMarker},
			ABIVersion: 1, MinVersion: 1,
		}); err != nil {
			t.Fatalf("tenant A deploy v%d: %v", v, err)
		}
	}
	if err := storeB.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: adminDefName, Version: 1,
		WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d, bMarker},
		ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("tenant B deploy v1: %v", err)
	}
}

// TestAdminLoginLoadWASMReturnsTheCallersOwnCode is the one that matters.
func TestAdminLoginLoadWASMReturnsTheCallersOwnCode(t *testing.T) {
	storeA, storeB := adminLoginStores(t)
	seedLopsidedDefs(t, storeA, storeB)
	ctx := context.Background()

	for _, tc := range []struct {
		who   string
		store *MSSQLStore
		want  byte
	}{{"A", storeA, aMarker}, {"B", storeB, bMarker}} {
		got, err := tc.store.LoadWASM(ctx, adminDefName, 1)
		if err != nil {
			t.Fatalf("LoadWASM(%s): %v", tc.who, err)
		}
		if len(got) == 0 {
			t.Fatalf("LoadWASM(%s) returned nothing; the fixture is broken", tc.who)
		}
		if got[len(got)-1] != tc.want {
			t.Errorf("tenant %s loaded WASM ending 0x%02X, want its own 0x%02X -- "+
				"this tenant's workflow would execute the other tenant's compiled code",
				tc.who, got[len(got)-1], tc.want)
		}
	}
}

// TestAdminLoginDefinitionReadsAnswerAboutTheCallersTenant covers the rest of
// the by-name read surface as one table.
//
// Each case reports what the CALLER would conclude, not merely that a number
// differed: a version list with a duplicate in it, or an existence check that
// says yes about somebody else's version, are both answers a caller acts on.
func TestAdminLoginDefinitionReadsAnswerAboutTheCallersTenant(t *testing.T) {
	storeA, storeB := adminLoginStores(t)
	seedLopsidedDefs(t, storeA, storeB)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		bad  func() string
	}{
		{"ListVersions — B holds only v1", func() string {
			v, err := storeB.ListVersions(ctx, adminDefName)
			if err != nil {
				return "errored: " + err.Error()
			}
			if len(v) != 1 || v[0] != 1 {
				return fmt.Sprintf("sees versions %v, want exactly [1]", v)
			}
			return ""
		}},
		{"ListWorkflowDefs — B deployed one definition", func() string {
			d, err := storeB.ListWorkflowDefs(ctx, adminDefName)
			if err != nil {
				return "errored: " + err.Error()
			}
			if len(d) != 1 {
				return fmt.Sprintf("lists %d definitions, want exactly 1", len(d))
			}
			return ""
		}},
		{"GetWorkflowDef — B's own bytes", func() string {
			d, err := storeB.GetWorkflowDef(ctx, adminDefName, 1)
			if err != nil {
				return "errored: " + err.Error()
			}
			if d == nil || len(d.WASMBytes) == 0 {
				return "returned nothing for B's own definition"
			}
			if d.WASMBytes[len(d.WASMBytes)-1] != bMarker {
				return fmt.Sprintf("returned WASM ending 0x%02X, want B's own 0xBB", d.WASMBytes[len(d.WASMBytes)-1])
			}
			return ""
		}},
		{"ValidateVersion — v2 is A's, B has no v2", func() string {
			ok, err := storeB.ValidateVersion(ctx, adminDefName, 2)
			if err != nil {
				return ""
			}
			if ok {
				return "confirms v2 exists; only tenant A deployed v2"
			}
			return ""
		}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if bad := tc.bad(); bad != "" {
				t.Errorf("tenant B %s", bad)
			}
		})
	}
}

// TestAdminLoginDefinitionWritesTouchOnlyTheCallersTenant — the destructive
// half. PurgeWorkflowDef deletes compiled code; MarkVersionDeprecated stops a
// version being selected for new runs.
func TestAdminLoginDefinitionWritesTouchOnlyTheCallersTenant(t *testing.T) {
	t.Run("MarkVersionDeprecated", func(t *testing.T) {
		storeA, storeB := adminLoginStores(t)
		seedLopsidedDefs(t, storeA, storeB)
		ctx := context.Background()

		if err := storeB.MarkVersionDeprecated(ctx, adminDefName, 1, true); err != nil {
			t.Fatalf("tenant B MarkVersionDeprecated: %v", err)
		}
		ok, err := storeA.ValidateVersion(ctx, adminDefName, 1)
		if err != nil {
			t.Fatalf("tenant A ValidateVersion: %v", err)
		}
		if !ok {
			t.Error("tenant B deprecating its own v1 deprecated tenant A's v1 as well")
		}
	})

	t.Run("PurgeWorkflowDef", func(t *testing.T) {
		storeA, storeB := adminLoginStores(t)
		seedLopsidedDefs(t, storeA, storeB)
		ctx := context.Background()

		if err := storeB.PurgeWorkflowDef(ctx, adminDefName, 1); err != nil {
			t.Fatalf("tenant B PurgeWorkflowDef: %v", err)
		}
		got, err := storeA.LoadWASM(ctx, adminDefName, 1)
		if err != nil || len(got) == 0 {
			t.Errorf("tenant B purging its own v1 deleted tenant A's compiled code too "+
				"(A's LoadWASM: %d bytes, err %v)", len(got), err)
		}
	})
}
