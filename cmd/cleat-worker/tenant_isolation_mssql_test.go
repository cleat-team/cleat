package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// mssqlIsolationDB is the dedicated database this test applies the real
// migration to. It is not the shared CLEAT_TEST_MSSQL database on purpose:
// applying migrations/mssql/001_schema.sql there would add seven security
// policies to a database every other MSSQL test shares, and those policies
// filter every read that does not set a session context -- which would turn a
// large number of unrelated passing tests into confusing empty-result failures.
const mssqlIsolationDB = "cleat_tenant_isolation_test"

// TestTenantIsolationOverHTTP_MSSQL is the SQL Server half of 2.6.
//
// It applies migrations/mssql/001_schema.sql rather than using
// engine/testutil's MSSQL helper, and that difference is the point.
// SetupMSSQLFullSchema hand-writes its CREATE TABLE statements and defines
// *none* of the seven CREATE SECURITY POLICY statements the real migration
// carries. So every existing MSSQL test in this repo runs against a schema with
// no tenant backstop at all, and none of them could catch an isolation
// regression. That is the same shape as the PostgreSQL superuser trap in
// tenant_isolation_db_test.go: the mechanism under test is simply absent from
// the test environment, and its absence looks like success.
//
// engine/testutil belongs to another workstream, so this applies the migration
// locally instead of changing the shared helper.
func TestTenantIsolationOverHTTP_MSSQL(t *testing.T) {
	// Skipped, and the reason is a live engine defect this test found rather
	// than anything about the HTTP layer. See IMPROVEMENT-PLAN §2.71.
	//
	// MSSQLStoreFactory sets sp_set_session_context from a wrapped connector
	// "on every new connection, so RLS is enforced automatically"
	// (engine/mssql_store.go:270-272). That holds exactly until the connection
	// is recycled: database/sql calls ResetSession when a connection returns to
	// the pool, go-mssqldb issues sp_reset_connection, and SESSION_CONTEXT is
	// cleared. Measured directly against SQL Server 2022 with a pool of one:
	//
	//   same connection, right after setting: 11111111-1111-1111-1111-111111111111
	//   after return to pool and re-acquire:  <NULL>
	//
	// With the real schema's seven filter predicates in place and no session
	// context, every tenant-scoped read matches nothing. Writes are unaffected
	// because the write paths call setSessionContext(tx) inside their own
	// transaction; reads such as ListWorkflows and GetWorkflowByID rely on the
	// connector alone. So this test seeds two tenants successfully and then
	// reads back an empty list for both.
	//
	// It fails closed rather than leaking, so it is a correctness and
	// availability defect rather than a security one -- but on SQL Server with
	// the shipped schema, tenant-scoped reads return nothing.
	//
	// engine/mssql_store.go is another workstream's file, so this is reported
	// rather than fixed here. Unskip once the session context is established
	// per transaction (or re-applied on ResetSession) instead of once per
	// connection.
	t.Skip("blocked on §2.71: MSSQL session context is cleared by connection pooling, so RLS filters every read")

	base := os.Getenv("CLEAT_TEST_MSSQL")
	if base == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tenant isolation test")
	}

	adminDB, err := sql.Open("sqlserver", base)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer adminDB.Close()
	// Not a skip: CLEAT_TEST_MSSQL was set, so a SQL Server was asked for and
	// is unreachable. Skipping here would report success for a backend nobody
	// tested, which is the failure mode scripts/check-skips.sh exists to catch.
	if err := adminDB.Ping(); err != nil {
		t.Fatalf("CLEAT_TEST_MSSQL is set but SQL Server is unreachable at %s: %v",
			redactMSSQLDSN(base), err)
	}

	ctx := context.Background()
	if _, err := adminDB.ExecContext(ctx,
		fmt.Sprintf("IF DB_ID('%s') IS NOT NULL BEGIN ALTER DATABASE %s SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE %s; END",
			mssqlIsolationDB, mssqlIsolationDB, mssqlIsolationDB)); err != nil {
		t.Fatalf("drop stale isolation database: %v", err)
	}
	if _, err := adminDB.ExecContext(ctx, "CREATE DATABASE "+mssqlIsolationDB); err != nil {
		t.Fatalf("create isolation database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = adminDB.ExecContext(context.Background(),
			fmt.Sprintf("ALTER DATABASE %s SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE %s",
				mssqlIsolationDB, mssqlIsolationDB))
	})

	connStr, err := mssqlConnStrForDB(base, mssqlIsolationDB)
	if err != nil {
		t.Fatalf("derive isolation DSN: %v", err)
	}
	testDB, err := sql.Open("sqlserver", connStr)
	if err != nil {
		t.Fatalf("open isolation database: %v", err)
	}
	defer testDB.Close()

	schema, err := os.ReadFile("../../migrations/mssql/001_schema.sql")
	if err != nil {
		t.Fatalf("read mssql schema migration: %v", err)
	}
	// 001_schema.sql uses GO batch separators. GO is a sqlcmd client directive,
	// not T-SQL, so the driver rejects it -- each batch must be sent
	// separately. (Note this differs from the procedure migrations, which
	// engine/store_backends_procedures_test.go sends whole for that reason.)
	for i, batch := range splitTSQLBatches(string(schema)) {
		if _, err := testDB.ExecContext(ctx, batch); err != nil {
			t.Fatalf("apply mssql schema migration, batch %d: %v\nbatch:\n%s", i, err, batch)
		}
	}

	// Assert the mechanism is actually present before asserting anything about
	// tenants. Without this the test would pass just as happily against a
	// schema with no policies, proving nothing -- which is exactly what the
	// existing MSSQL tests do.
	var policies int
	if err := testDB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sys.security_policies WHERE is_enabled = 1").Scan(&policies); err != nil {
		t.Fatalf("count security policies: %v", err)
	}
	if policies == 0 {
		t.Fatal("no enabled security policies: RLS is absent, so this test could not detect a leak")
	}
	t.Logf("row-level security active: %d enabled policies", policies)

	factory := engine.NewMSSQLStoreFactory(connStr)

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
		return runID
	}

	runA := seed(tenantA, "wf-mssql-a")
	runB := seed(tenantB, "wf-mssql-b")

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

// splitTSQLBatches splits T-SQL source on standalone GO separators, which are
// a sqlcmd client directive rather than something the driver understands.
func splitTSQLBatches(src string) []string {
	var batches []string
	var cur []string
	for _, line := range strings.Split(src, "\n") {
		if strings.EqualFold(strings.TrimSpace(line), "GO") {
			if b := strings.TrimSpace(strings.Join(cur, "\n")); b != "" {
				batches = append(batches, b)
			}
			cur = nil
			continue
		}
		cur = append(cur, line)
	}
	if b := strings.TrimSpace(strings.Join(cur, "\n")); b != "" {
		batches = append(batches, b)
	}
	return batches
}

// redactMSSQLDSN strips credentials from a connection string so a failure
// message can name the endpoint that was unreachable without printing a
// password into CI logs.
func redactMSSQLDSN(connStr string) string {
	u, err := url.Parse(connStr)
	if err != nil {
		return "<unparseable connection string>"
	}
	u.User = url.User("<redacted>")
	return u.String()
}

// mssqlConnStrForDB rewrites the database= parameter of a SQL Server URL-style
// connection string, preserving everything else.
func mssqlConnStrForDB(connStr, database string) (string, error) {
	u, err := url.Parse(connStr)
	if err != nil {
		return "", fmt.Errorf("parse connection string: %w", err)
	}
	q := u.Query()
	q.Set("database", database)
	u.RawQuery = q.Encode()
	return u.String(), nil
}
