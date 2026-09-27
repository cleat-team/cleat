package engine

// IMPROVEMENT-PLAN 3.99. GetWorkflowByID left WorkflowInstance.TenantID empty on
// PostgreSQL and SQL Server, and one caller compares it for a living.
//
// cmd/cleat-worker/api_admin.go's callerOwnsTarget is the ONLY enforcement point
// for /api/admin/instances/* -- the admin store methods take no tenant
// parameter, so nothing below that layer can tell one tenant's workflow from
// another's. It reads the workflow and compares wf.TenantID against the
// authenticated caller. With TenantID always "", that comparison was
// `"" != "<caller uuid>"` -- true for every request, including the caller's own.
//
// So with --require-auth on, every admin instance route answered 404 on
// PostgreSQL and SQL Server. Measured 2026-09-03, one run across all three:
//
//     postgres: wf.TenantID = ""                                      EMPTY
//     mysql:    wf.TenantID = "00000000-0000-0000-0000-000000000000"  populated
//     mssql:    wf.TenantID = ""                                      EMPTY
//
// WHY NOTHING CAUGHT IT, and this is the more useful half. There IS a test:
// TestAdminRoutesRejectCrossTenantTarget (cmd/cleat-worker/api_admin_test.go),
// written for 1.7, and it passes. It passes because its mock does this:
//
//     return &engine.WorkflowInstance{ID: id, TenantID: ownedBy}, nil
//
// The mock populates the field. So the test proves the COMPARISON is right,
// which it is, and says nothing about whether any real store fills in the value
// being compared -- and two of three do not. Nothing anywhere compared the mock
// against the implementations it stands in for.
//
// That is the same shape as hostabi_runtime_parity_test.go: two implementations
// of one contract, each self-consistent, with nothing checking they agree. It
// is also CLAUDE.md's "watch which layer is holding the test up", in its most
// literal form -- the layer holding that assertion up was the test's own
// fixture.
//
// It failed CLOSED, which is why no operator report surfaced it either: a gate
// that denies everything looks like a gate doing its job. The tell was that
// MySQL -- the dialect documented single-tenant-only, where this matters least
// -- was the only one that scanned the column.
//
// This test is at the store layer rather than the HTTP layer on purpose: the
// defect is that a field the API depends on is not populated, and asserting it
// here covers every future caller rather than the one that happened to notice.
// The three-dialect loop is the point, since two of three were wrong and the
// disagreement is what identified it.

import (
	"context"
	"encoding/json"
	"testing"
)

func TestGetWorkflowByIDPopulatesTenantID(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			setupTestData(t, store)

			const id = "tenant-id-populated"
			if _, _, err := store.StartNewRun(ctx, id, "test-workflow", 1,
				json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			wf, err := store.GetWorkflowByID(ctx, id)
			if err != nil {
				t.Fatalf("GetWorkflowByID: %v", err)
			}
			if wf == nil {
				t.Fatalf("GetWorkflowByID returned nothing; the fixture is broken")
			}
			if wf.TenantID != DefaultTenantUUID {
				t.Errorf("GetWorkflowByID returned TenantID %q, want %q -- "+
					"callerOwnsTarget compares this against the authenticated caller and "+
					"answers 404 when it differs, so an empty or differently-cased value "+
					"makes every /api/admin/instances/* route 404 for its rightful owner",
					wf.TenantID, DefaultTenantUUID)
			}
		})
	}
}
