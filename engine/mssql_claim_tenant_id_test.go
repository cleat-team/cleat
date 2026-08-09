package engine

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine/testutil"
)

// A claimed workflow's TenantID has to be the canonical UUID text, because the
// worker routes execution on it: cmd/cleat-worker's storeForTenant opens a
// store for exactly this string, and the SQL Server factory parses it as a
// UUID before it will build a pool.
//
// SQL Server stores UNIQUEIDENTIFIER in a mixed-endian layout, and go-mssqldb
// scans it into a Go string as the 16 raw bytes. That produced
// "\x11\x11\x11..." where every caller expects
// "11111111-1111-1111-1111-111111111111".
//
// It was harmless while TenantID only reached a log line. It stopped being
// harmless the moment execution routed on it, and it failed in the worst
// available way -- not at the claim, but one step later, as "no store for
// tenant" on every workflow the worker picked up.
func TestMSSQLClaim_ReturnsTenantIDAsCanonicalUUIDText(t *testing.T) {
	db := testutil.MSSQLTestDB(t)
	if db == nil {
		t.Skip("CLEAT_TEST_MSSQL not set")
	}
	const tid = "11111111-1111-1111-1111-111111111111"

	for _, tc := range []struct {
		name  string
		claim func(s *MSSQLStore, ctx context.Context) ([]*WorkflowInstance, error)
	}{
		{"ClaimWorkflows", func(s *MSSQLStore, ctx context.Context) ([]*WorkflowInstance, error) {
			return s.ClaimWorkflows(ctx, "tid-worker", 5)
		}},
		{"ClaimStickyWorkflows", func(s *MSSQLStore, ctx context.Context) ([]*WorkflowInstance, error) {
			return s.ClaimStickyWorkflows(ctx, "tid-worker", 5)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMSSQLStore(db)
			store.tenantID = tid
			ctx := context.Background()
			testutil.CleanupMSSQLTestData(t, db)
			setupTestData(t, store)

			wfID := "tid-" + tc.name
			if _, _, err := store.StartNewRun(ctx, wfID, "test-workflow", 1, json.RawMessage(`{}`), "", tid, 0); err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}
			if tc.name == "ClaimStickyWorkflows" {
				if err := store.UpdateStickyWorker(ctx, wfID, "tid-worker"); err != nil {
					t.Fatalf("UpdateStickyWorker: %v", err)
				}
			}

			wfs, err := tc.claim(store, ctx)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			var found *WorkflowInstance
			for _, wf := range wfs {
				if wf.ID == wfID {
					found = wf
				}
			}
			if found == nil {
				t.Fatalf("%s claimed %d workflows, none of them %s", tc.name, len(wfs), wfID)
			}

			// Parsing is the assertion that matters: it is exactly what
			// MSSQLStoreFactory.OpenStore does before it will build a pool, so
			// a value that fails here fails execution too.
			if _, err := uuid.Parse(found.TenantID); err != nil {
				t.Errorf("TenantID = %q (%d bytes) does not parse as a UUID: %v\n"+
					"the worker routes execution on this string; an unparseable one fails every workflow",
					found.TenantID, len(found.TenantID), err)
			}
			if found.TenantID != tid {
				t.Errorf("TenantID = %q, want %q", found.TenantID, tid)
			}
		})
	}
}
