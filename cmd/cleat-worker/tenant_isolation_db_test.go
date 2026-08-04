package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

// TestTenantIsolationOverHTTP_Postgres is the database-backed half of the 1.7
// regression tests, and the one that proves the mechanism rather than the
// wiring.
//
// The unit tests in tenant_isolation_test.go use a fake factory, so they show
// that the HTTP layer asks for the right tenant's store. They cannot show that
// asking for it produces isolation. That depends on machinery below the handler:
// PostgresStore.ListWorkflows carries no tenant predicate at all -- it calls
// beginTxWithRLS, which sets cleat.tenant_id from the store's tenantID, and the
// row-level security policies in migrations/postgres/001_schema.sql are the only
// thing filtering the result. If the HTTP layer hands that machinery the default
// tenant, RLS enforces the wrong scope perfectly and every caller reads the same
// rows.
//
// So this test runs the real factory against a real PostgreSQL with the real
// schema, and asserts through the HTTP handler that one tenant cannot see
// another's workflow.
func TestTenantIsolationOverHTTP_Postgres(t *testing.T) {
	if os.Getenv("CLEAT_TEST_POSTGRES") == "" && os.Getenv("CLEAT_TEST_DB") == "" {
		t.Skip("CLEAT_TEST_POSTGRES not set, skipping database-backed tenant isolation test")
	}

	db := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)

	// The factory must run on a NON-superuser connection or this test proves
	// nothing. PostgreSQL bypasses RLS unconditionally for superusers, and
	// CLEAT_TEST_POSTGRES conventionally points at one (the postgres image's
	// POSTGRES_USER bootstrap role is a superuser). Run against that role and
	// both tenants see every row no matter how correct the policies are --
	// which is exactly what this test did on the first attempt, and what
	// migrations/postgres/005_app_role.sql exists to prevent in production.
	rlsDB := testutil.OpenPostgresRLSTestDB(t, db)
	factory := engine.NewPostgresStoreFactory(rlsDB, "public")

	ctx := context.Background()
	seed := func(tenant, defName string) string {
		t.Helper()
		st, _, err := factory.OpenStore(ctx, tenant)
		if err != nil {
			t.Fatalf("open store for %s: %v", tenant, err)
		}
		if err := st.DeployWorkflowDef(ctx, &engine.WorkflowDef{
			Name: defName, Version: 1, WASMBytes: []byte{0x00},
		}); err != nil {
			t.Fatalf("deploy def for %s: %v", tenant, err)
		}
		runID, _, err := st.StartNewRun(ctx, "", defName, 1, json.RawMessage(`{}`), "", tenant, 0)
		if err != nil {
			t.Fatalf("start run for %s: %v", tenant, err)
		}
		t.Cleanup(func() { testutil.CleanupTestData(t, db, testutil.DialectPostgres, runID) })
		return runID
	}

	runA := seed(tenantA, "wf-tenant-a")
	runB := seed(tenantB, "wf-tenant-b")
	if runA == runB {
		t.Fatalf("seeded runs collided: %s", runA)
	}

	defaultStore, _, err := factory.OpenStore(ctx, engine.DefaultTenantUUID)
	if err != nil {
		t.Fatalf("open default store: %v", err)
	}
	api := &apiServer{
		store:       defaultStore,
		worker:      newTestWorker(&mockStore{}),
		maxBodySize: 1 << 20,
		factory:     factory,
		requireAuth: true,
	}
	mux := http.NewServeMux()
	registerRoutes(mux, api)

	get := func(tenant, path string) (int, string) {
		t.Helper()
		req := asTenant(httptest.NewRequest(http.MethodGet, path, nil), tenant)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}

	t.Run("list shows only the caller's workflows", func(t *testing.T) {
		code, body := get(tenantA, "/api/workflows")
		if code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", code, body)
		}
		if !strings.Contains(body, runA) {
			t.Errorf("tenant A's list did not contain its own run %s: %s", runA, body)
		}
		if strings.Contains(body, runB) {
			t.Errorf("tenant A's list contained tenant B's run %s: %s", runB, body)
		}
	})

	t.Run("cannot read another tenant's workflow by id", func(t *testing.T) {
		// A direct GET by ID is the sharper test: listing could plausibly be
		// filtered somewhere incidental, but fetching a known ID has nothing
		// between the caller and the row except the tenant scope.
		code, body := get(tenantA, "/api/workflows/"+runB)
		if code == http.StatusOK && strings.Contains(body, runB) {
			t.Errorf("tenant A read tenant B's workflow by ID: status %d, body %s", code, body)
		}
	})

	t.Run("each tenant sees its own workflow", func(t *testing.T) {
		for _, tc := range []struct{ tenant, want, notWant string }{
			{tenantA, runA, runB},
			{tenantB, runB, runA},
		} {
			code, body := get(tc.tenant, "/api/workflows")
			if code != http.StatusOK {
				t.Fatalf("tenant %s: status %d, body %s", tc.tenant, code, body)
			}
			if !strings.Contains(body, tc.want) {
				t.Errorf("tenant %s could not see its own run %s: %s", tc.tenant, tc.want, body)
			}
			if strings.Contains(body, tc.notWant) {
				t.Errorf("tenant %s saw the other tenant's run %s: %s", tc.tenant, tc.notWant, body)
			}
		}
	})

	t.Run("unauthenticated request is refused, not defaulted", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/workflows", nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401; body = %s", w.Code, w.Body.String())
		}
	})
}
