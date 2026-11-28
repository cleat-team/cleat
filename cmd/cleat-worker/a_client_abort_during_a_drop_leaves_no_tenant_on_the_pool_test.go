package main

// cleat#3647. DELETE /api/admin/tenants/{tenant} runs admin.drop_tenant on the APPLICATION login, the
// login the tenant-scoped stores use. On SQL Server that procedure sets SESSION_CONTEXT tenant_id to the
// tenant it is dropping, on a connection auth.TenantStore.beginDropTx pins. A client abort arrives as a
// cancelled request context, which is a TDS attention: CATCH does not run and the dropped tenant stays in
// the connection's SESSION.
//
// beginDropTx's release() is the drop path's own defence: it clears the context before the connection goes
// back to the pool. Measured (cleat#3647), it did not work after an abort. database/sql rolls the
// transaction back itself, the driver's connection is left holding the aborted transaction, the clear is
// refused ("server does not have an active transaction ... XACT_ABORT"), and release() discarded that error
// and pooled the connection anyway. Only the driver's session reset on reuse (go-mssqldb v1.11.2) then kept
// the next statement from running as the dropped tenant, so on this login's pool the driver was the only
// layer and not the second one.
//
// This drives the real handler chain with a real TenantStore on that login, a pool of ONE connection, and a
// client abort while the procedure is blocked inside its sweep. Then it asks which tenant the next statement
// runs as. Two cases:
//
//   - the driver as shipped, which resets the session on reuse: the guard that would notice if a driver
//     bump stopped doing that is engine.TestACancelledCallThatSetsTheSessionContextIsNotSeenByTheNextPoolUse;
//   - a driver that does NOT reset, by hiding ResetSession behind a connector wrapper: here release() is
//     the only thing standing, which is the case this test exists for. Red without the fix.
//
// Each case first proves its own control: a marker left on the session is cleared by reuse in the first and
// SURVIVES reuse in the second, so "no tenant" below cannot come from a wrapper that failed to hide the reset.

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	mssql "github.com/microsoft/go-mssqldb"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine/testutil"
)

// noResetConnector hands out connections that do NOT implement driver.SessionResetter: a driver or pooler
// that does not reset the session when a connection is reused. It forwards the optional interfaces
// go-mssqldb implements (engine.tenantSessionConn lists them) and omits ResetSession.
type noResetConnector struct{ sqldriver.Connector }

func (c noResetConnector) Connect(ctx context.Context) (sqldriver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &noResetConn{Conn: conn}, nil
}

type noResetConn struct{ sqldriver.Conn }

func (c *noResetConn) PrepareContext(ctx context.Context, q string) (sqldriver.Stmt, error) {
	if p, ok := c.Conn.(sqldriver.ConnPrepareContext); ok {
		return p.PrepareContext(ctx, q)
	}
	return c.Conn.Prepare(q)
}

func (c *noResetConn) BeginTx(ctx context.Context, o sqldriver.TxOptions) (sqldriver.Tx, error) {
	return c.Conn.(sqldriver.ConnBeginTx).BeginTx(ctx, o)
}

func (c *noResetConn) Ping(ctx context.Context) error {
	if p, ok := c.Conn.(sqldriver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

func (c *noResetConn) IsValid() bool {
	if v, ok := c.Conn.(sqldriver.Validator); ok {
		return v.IsValid()
	}
	return true
}

func (c *noResetConn) CheckNamedValue(nv *sqldriver.NamedValue) error {
	if ck, ok := c.Conn.(sqldriver.NamedValueChecker); ok {
		return ck.CheckNamedValue(nv)
	}
	return sqldriver.ErrSkip
}

// sessionTenant reads what the connection a statement lands on thinks its tenant is. "" is no tenant.
func sessionTenant(t *testing.T, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) string {
	t.Helper()
	var v sql.NullString
	if err := q.QueryRowContext(context.Background(),
		`SELECT CAST(SESSION_CONTEXT(N'tenant_id') AS NVARCHAR(100))`).Scan(&v); err != nil {
		t.Fatalf("UNMEASURED: reading the session context: %v", err)
	}
	return v.String
}

func TestAClientAbortDuringADropLeavesNoTenantOnThePool(t *testing.T) {
	for _, tc := range []struct {
		name      string
		hideReset bool
	}{
		{"the driver resets the session on reuse", false},
		{"the driver does not reset the session on reuse", true},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			admin := testutil.TestDB(t, testutil.DialectMSSQL)
			var dbName string
			if err := admin.QueryRowContext(ctx, `SELECT DB_NAME()`).Scan(&dbName); err != nil {
				t.Fatalf("UNMEASURED: reading the database name: %v", err)
			}

			// A login of its own, named per run: a fixed name collides when two test processes share one
			// server, and one's cleanup drops the login under the other.
			login := "cleat_abort_" + strings.ReplaceAll(uuid.NewString()[:13], "-", "")
			const pw = "Abort3647!pw1"
			for _, q := range []string{
				`CREATE LOGIN [` + login + `] WITH PASSWORD = N'` + pw + `', CHECK_POLICY = OFF`,
				`CREATE USER [` + login + `] FOR LOGIN [` + login + `]`,
				`ALTER ROLE [cleat_app_role] ADD MEMBER [` + login + `]`,
			} {
				if _, err := admin.ExecContext(ctx, q); err != nil {
					t.Fatalf("UNMEASURED: setup: %v\n%s", err, q)
				}
			}
			t.Cleanup(func() {
				for _, q := range []string{
					`ALTER ROLE [cleat_app_role] DROP MEMBER [` + login + `]`,
					`DROP USER [` + login + `]`,
					`DROP LOGIN [` + login + `]`,
				} {
					_, _ = admin.ExecContext(context.Background(), q)
				}
			})
			u, err := url.Parse(os.Getenv("CLEAT_TEST_MSSQL"))
			if err != nil {
				t.Fatalf("UNMEASURED: parsing the DSN: %v", err)
			}
			qv := u.Query()
			qv.Set("database", dbName)
			u.RawQuery = qv.Encode()
			u.User = url.UserPassword(login, pw)

			var app *sql.DB
			if tc.hideReset {
				cn, err := mssql.NewConnector(u.String())
				if err != nil {
					t.Fatalf("UNMEASURED: %v", err)
				}
				app = sql.OpenDB(noResetConnector{cn})
			} else if app, err = sql.Open("sqlserver", u.String()); err != nil {
				t.Fatalf("UNMEASURED: %v", err)
			}
			t.Cleanup(func() { app.Close() })
			app.SetMaxOpenConns(1)

			// THE CONTROL. A marker left on the session: the shipped driver clears it on reuse, the wrapper
			// must not. If the wrapper cleared it, the case below measured nothing.
			pin, err := app.Conn(ctx)
			if err != nil {
				t.Fatalf("UNMEASURED: %v", err)
			}
			if _, err := pin.ExecContext(ctx, `EXEC sp_set_session_context @key = N'tenant_id', @value = N'CONTROL-MARKER'`); err != nil {
				t.Fatalf("UNMEASURED: %v", err)
			}
			if got := sessionTenant(t, pin); got != "CONTROL-MARKER" {
				t.Fatalf("UNMEASURED: a marker set by hand reads back as %q, so this read cannot see a leak", got)
			}
			_ = pin.Close()
			survived := sessionTenant(t, app) == "CONTROL-MARKER"
			if survived != tc.hideReset {
				t.Fatalf("UNMEASURED: after reuse the marker survived = %v, want %v for %q, so the driver is not the one this case describes",
					survived, tc.hideReset, tc.name)
			}

			tsAdmin, err := auth.NewTenantStoreForDialect(admin, auth.DialectMSSQL)
			if err != nil {
				t.Fatalf("UNMEASURED: %v", err)
			}
			tsApp, err := auth.NewTenantStoreForDialect(app, auth.DialectMSSQL)
			if err != nil {
				t.Fatalf("UNMEASURED: %v", err)
			}
			victim := uuid.New()
			orgName := "abort-3647-" + uuid.NewString()
			if _, err := tsAdmin.ProvisionTenant(ctx, auth.ProvisionRequest{TenantID: victim, OrgName: orgName, Creator: "abort-test"}); err != nil {
				t.Fatalf("UNMEASURED: provisioning the victim: %v", err)
			}
			t.Cleanup(func() {
				_, _ = tsAdmin.DeleteTombstone(context.Background(), victim)
				_, _ = admin.ExecContext(context.Background(), `DELETE FROM admin.tenants WHERE tenant_id = @p1`, victim.String())
				_, _ = admin.ExecContext(context.Background(), `DELETE FROM admin.orgs WHERE name = @p1`, orgName)
			})
			if err := tsAdmin.SetTenantSuspended(ctx, victim, true); err != nil {
				t.Fatalf("UNMEASURED: suspending the victim: %v", err)
			}

			f := buildChain(t, false)
			f.api.provisioner = tsApp

			// Hold the lock the procedure's sweep needs, from another session, so the abort arrives while the
			// procedure is inside it and has already set the victim's context.
			blocker, err := admin.Conn(ctx)
			if err != nil {
				t.Fatalf("UNMEASURED: %v", err)
			}
			btx, err := blocker.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("UNMEASURED: %v", err)
			}
			released := false
			release := func() {
				if !released {
					released = true
					_ = btx.Rollback()
					_ = blocker.Close()
				}
			}
			defer release()
			if _, err := btx.ExecContext(ctx, `SELECT TOP 1 1 FROM dbo.workflow_tags WITH (TABLOCKX, HOLDLOCK)`); err != nil {
				t.Fatalf("UNMEASURED: taking the blocking lock: %v", err)
			}

			cctx, cancel := context.WithCancel(ctx)
			defer cancel()
			req := httptest.NewRequest(http.MethodDelete, "/api/admin/tenants/"+victim.String(), nil).WithContext(cctx)
			req.Header.Set("Authorization", "Bearer "+f.adminKey)
			req.Header.Set("X-Confirm", victim.String())
			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { f.handler.ServeHTTP(w, req); close(done) }()

			blocked := false
			for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline) && !blocked; {
				var n int
				if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM sys.dm_exec_requests r
					JOIN sys.dm_exec_sessions s ON s.session_id = r.session_id
					WHERE s.login_name = @p1 AND r.blocking_session_id <> 0`, login).Scan(&n); err != nil {
					t.Fatalf("UNMEASURED: reading dm_exec_requests: %v", err)
				}
				if blocked = n > 0; !blocked {
					time.Sleep(20 * time.Millisecond)
				}
			}
			if !blocked {
				t.Fatalf("UNMEASURED: the drop never blocked inside the procedure (so no abort arrived mid-call; "+
					"READ_COMMITTED_SNAPSHOT on would do this). Status so far %d", w.Code)
			}
			cancel() // the client aborts
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("UNMEASURED: the handler never returned after the abort")
			}
			release()

			if w.Code != http.StatusInternalServerError {
				t.Errorf("the aborted drop answered %d %s, want 500", w.Code, strings.TrimSpace(w.Body.String()))
			}
			// The abort must have ROLLED BACK, or what was aborted is not a drop in flight.
			var n int
			if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM admin.tenants WHERE tenant_id = @p1`, victim.String()).Scan(&n); err != nil {
				t.Fatalf("UNMEASURED: counting the victim: %v", err)
			}
			if n != 1 {
				t.Fatalf("the victim's admin.tenants row count is %d after the abort, want 1: the abort did not roll the drop back", n)
			}

			// THE QUESTION. The next statement on the pool: which tenant does it run as?
			if got := sessionTenant(t, app); got != "" {
				t.Errorf("the next statement on the application login's pool ran as tenant %s after a client abort during the drop of %s",
					got, victim)
			}

			// And the pool still works, and the same tenant can then be dropped for real.
			req2 := httptest.NewRequest(http.MethodDelete, "/api/admin/tenants/"+victim.String(), nil)
			req2.Header.Set("Authorization", "Bearer "+f.adminKey)
			req2.Header.Set("X-Confirm", victim.String())
			w2 := httptest.NewRecorder()
			f.handler.ServeHTTP(w2, req2)
			if w2.Code != http.StatusOK {
				t.Errorf("dropping the same tenant afterwards answered %d %s, want 200", w2.Code, strings.TrimSpace(w2.Body.String()))
			}
		})
	}
}
