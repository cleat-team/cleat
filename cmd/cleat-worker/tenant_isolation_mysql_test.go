package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

// TestTenantIsolationOverHTTP_MySQL is the MySQL half of 2.6, and it is
// deliberately not folded into a shared multi-dialect test.
//
// MySQL has no row-level security feature at all -- CREATE POLICY is a syntax
// error on 8.4 -- so unlike PostgreSQL and SQL Server there is no database
// backstop underneath the HTTP layer. Its isolation is entirely structural:
// MySQLStoreFactory gives each tenant its own *database* (`cleat_<uuid>`), so
// tenant A's store is physically unable to name tenant B's rows.
//
// A single test shared across the three dialects would pass here and read as
// though MySQL had the same protection the other two do. It does not, and the
// difference is worth keeping visible: on MySQL, a bug in the HTTP layer is the
// whole of the exposure, because nothing below it will catch the mistake.
func TestTenantIsolationOverHTTP_MySQL(t *testing.T) {
	dsn := os.Getenv("CLEAT_TEST_MYSQL")
	if dsn == "" {
		t.Skip("CLEAT_TEST_MYSQL not set, skipping MySQL tenant isolation test")
	}

	masterDB, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open master connection: %v", err)
	}
	defer masterDB.Close()
	// Not a skip: CLEAT_TEST_MYSQL was set, so a MySQL was asked for and is
	// unreachable. See the same note in the SQL Server test.
	if err := masterDB.Ping(); err != nil {
		t.Fatalf("CLEAT_TEST_MYSQL is set but MySQL is unreachable: %v", err)
	}

	factory := engine.NewMySQLStoreFactory(masterDB, mysqlBaseDSN(dsn))
	ctx := context.Background()

	seed := func(tenant, defName string) string {
		t.Helper()
		// CreateTenantDatabase creates the database but applies no schema, so
		// the tables have to be put there before a store can use it.
		tenantDB, err := factory.CreateTenantDatabase(ctx, tenant)
		if err != nil {
			t.Fatalf("create tenant database for %s: %v", tenant, err)
		}
		testutil.SetupMySQLFullSchema(t, tenantDB)

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
		return runID
	}

	runA := seed(tenantA, "wf-mysql-a")
	runB := seed(tenantB, "wf-mysql-b")
	t.Cleanup(func() {
		for _, tn := range []string{tenantA, tenantB} {
			_, _ = masterDB.Exec("DROP DATABASE IF EXISTS `cleat_" + strings.ReplaceAll(tn, "-", "_") + "`")
		}
	})

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

	for _, tc := range []struct{ tenant, want, notWant string }{
		{tenantA, runA, runB},
		{tenantB, runB, runA},
	} {
		req := asTenant(httptest.NewRequest(http.MethodGet, "/api/workflows", nil), tc.tenant)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("tenant %s: status %d, body %s", tc.tenant, w.Code, w.Body.String())
		}
		body := w.Body.String()
		if !strings.Contains(body, tc.want) {
			t.Errorf("tenant %s could not see its own run %s: %s", tc.tenant, tc.want, body)
		}
		if strings.Contains(body, tc.notWant) {
			t.Errorf("tenant %s saw the other tenant's run %s: %s", tc.tenant, tc.notWant, body)
		}
	}
}
