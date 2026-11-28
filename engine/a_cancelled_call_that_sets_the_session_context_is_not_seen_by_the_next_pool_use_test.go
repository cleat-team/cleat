package engine

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3647 note 2. Two SQL Server procedures set the connection's SESSION_CONTEXT tenant_id to each
// tenant they visit and put the caller's value back at the end and in CATCH:
//
//   - dbo.library_clashing_names (migration 010), run by the library publisher's login;
//   - admin.drop_tenant (migration 003), run by the APPLICATION login (001_schema grants EXECUTE on
//     schema admin to cleat_app_role; a cleat_admin member is denied), which is the login the
//     tenant-scoped stores use.
//
// A client cancel is a TDS attention, and CATCH does not run on one, so a call cancelled inside either
// procedure leaves the last tenant it set in the SESSION. Measured 2026-10-31 (cleat#3647): the same
// session reads that tenant afterwards, for both procedures. No edit to either procedure can fix that.
//
// What stops it reaching the next statement is NOT the procedures. It is go-mssqldb v1.11.2: database/sql
// asks the driver to reset the session when a connection is reused, and the driver sends the reset, which
// clears SESSION_CONTEXT. This test pins THAT, because a driver bump, a different pooler or a changed
// connection setting can lose it silently, and nothing else in the suite would notice: every statement
// would simply run as whichever tenant the last cancelled call happened to be visiting.
//
// The first check is the one that carries the guard: a context left set by hand must be cleared by
// reusing the same physical connection. The cancelled calls add the realistic shape (a cancel arriving
// while the procedure is inside its loop, on the engine's own path: BeginTx on a pool).
//
// NOT covered, and cleat#3647 stays open for each until it is measured or consciously dropped:
//   - the worker's HTTP handler end to end (it passes r.Context() to DropTenant, so a client abort is a
//     realistic cancel; this test cancels the procedure's statement directly);
//   - how the tenant-scoped stores set their own context on a pooled connection;
//   - any driver or pooler other than go-mssqldb v1.11.2 under database/sql;
//   - READ_COMMITTED_SNAPSHOT on: the blocker below is a table lock, which a snapshot reader does not
//     wait for, so the cancel would never arrive mid-call. That is reported as UNMEASURED, not passed.
func TestACancelledCallThatSetsTheSessionContextIsNotSeenByTheNextPoolUse(t *testing.T) {
	rig := newMSSQLLibRig(t)
	c := newPrefixCase(t, rig)
	c.tdialect = testutil.DialectMSSQL
	c.registerTenants(t)
	ctx := context.Background()

	t.Run("the publisher's clash reader", func(t *testing.T) {
		c.openPublisher(t)
		c.pub.SetMaxOpenConns(1)
		lib := c.library(t, "cancelctx")
		cancelledCallLeavesNothingOnThePool(t, c, c.pub, "dbo.workflow_defs", func(ctx context.Context, q libraryPrefixQueryer) error {
			_, err := libraryPrefixClashes(ctx, q, DialectMSSQL, lib)
			return err
		})
	})

	t.Run("admin.drop_tenant on the application login", func(t *testing.T) {
		const victim = "5c2e8f10-3647-4a3b-8c1d-000000003647"
		const login, pw = "cleat_3647_app", "Cancel3647!pw1"
		var dbName string
		if err := rig.admin.QueryRowContext(ctx, `SELECT DB_NAME()`).Scan(&dbName); err != nil {
			t.Fatalf("UNMEASURED: reading the database name: %v", err)
		}
		for _, q := range []string{
			`IF SUSER_ID(N'` + login + `') IS NULL CREATE LOGIN [` + login + `] WITH PASSWORD = N'` + pw + `', CHECK_POLICY = OFF`,
			`IF DATABASE_PRINCIPAL_ID(N'` + login + `') IS NULL CREATE USER [` + login + `] FOR LOGIN [` + login + `]`,
			`ALTER ROLE [cleat_app_role] ADD MEMBER [` + login + `]`,
			`IF NOT EXISTS (SELECT 1 FROM admin.tenants WHERE tenant_id = '` + victim + `') INSERT INTO admin.tenants (tenant_id, name) VALUES ('` + victim + `', N'cancel-3647')`,
		} {
			if _, err := rig.admin.ExecContext(ctx, q); err != nil {
				t.Fatalf("UNMEASURED: setup: %v\n%s", err, q)
			}
		}
		t.Cleanup(func() {
			for _, q := range []string{
				`DELETE FROM admin.tenants WHERE tenant_id = '` + victim + `'`,
				`IF DATABASE_PRINCIPAL_ID(N'` + login + `') IS NOT NULL ALTER ROLE [cleat_app_role] DROP MEMBER [` + login + `]`,
				`IF DATABASE_PRINCIPAL_ID(N'` + login + `') IS NOT NULL DROP USER [` + login + `]`,
				`IF SUSER_ID(N'` + login + `') IS NOT NULL DROP LOGIN [` + login + `]`,
			} {
				_, _ = rig.admin.ExecContext(context.Background(), q)
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
		app, err := sql.Open("sqlserver", u.String())
		if err != nil {
			t.Fatalf("UNMEASURED: opening the application login: %v", err)
		}
		t.Cleanup(func() { app.Close() })
		app.SetMaxOpenConns(1)

		// The statement is the one auth.TenantStore.DropTenant runs (auth/tenant_drop.go).
		cancelledCallLeavesNothingOnThePool(t, c, app, "dbo.workflow_tags", func(ctx context.Context, q libraryPrefixQueryer) error {
			rows, err := q.QueryContext(ctx, `EXEC admin.drop_tenant @tenant_id = @p1`, victim)
			if rows != nil {
				rows.Close()
			}
			return err
		})

		// The cancels above must have ROLLED BACK (XACT_ABORT), or "cancelled mid-drop" was not what ran.
		var n int
		if err := rig.admin.QueryRowContext(ctx, `SELECT count(*) FROM admin.tenants WHERE tenant_id = @p1`, victim).Scan(&n); err != nil {
			t.Fatalf("UNMEASURED: counting the victim: %v", err)
		}
		if n != 1 {
			t.Fatalf("the victim's admin.tenants row count is %d after the cancelled drops, want 1: a cancelled drop did not roll back, so what was cancelled is not what this test describes", n)
		}
		if _, err := app.ExecContext(ctx, `EXEC admin.drop_tenant @tenant_id = @p1`, victim); err != nil {
			t.Fatalf("a normal drop of the same tenant afterwards failed: %v", err)
		}
	})
}

type ctxReader interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func readSessionContext(t *testing.T, q ctxReader) (int, string) {
	t.Helper()
	var spid int
	var v sql.NullString
	if err := q.QueryRowContext(context.Background(),
		`SELECT @@SPID, CAST(SESSION_CONTEXT(N'tenant_id') AS NVARCHAR(100))`).Scan(&spid, &v); err != nil {
		t.Fatalf("UNMEASURED: reading the session context: %v", err)
	}
	if !v.Valid {
		return spid, "<NULL>"
	}
	return spid, v.String
}

// cancelledCallLeavesNothingOnThePool runs `call` on a pool of ONE connection, cancels it while it is
// blocked inside the procedure, and asserts the next use of the same physical connection reads no tenant.
func cancelledCallLeavesNothingOnThePool(t *testing.T, c *prefixCase, pool *sql.DB, blockOn string, call func(context.Context, libraryPrefixQueryer) error) {
	t.Helper()
	ctx := context.Background()

	// 1. THE GUARD, and a known-positive for the read. A context left set by hand is seen on the same
	// session (so this read CAN see a leak), and reusing the connection must clear it.
	pin, err := pool.Conn(ctx)
	if err != nil {
		t.Fatalf("UNMEASURED: %v", err)
	}
	spid0, _ := readSessionContext(t, pin)
	if _, err := pin.ExecContext(ctx, `EXEC sp_set_session_context @key = N'tenant_id', @value = N'LEAK-CONTROL'`); err != nil {
		t.Fatalf("UNMEASURED: %v", err)
	}
	if _, seen := readSessionContext(t, pin); seen != "LEAK-CONTROL" {
		t.Fatalf("UNMEASURED: a context set by hand reads back as %q, so this read cannot see a leak", seen)
	}
	pin.Close()
	spid1, reused := readSessionContext(t, pool)
	if spid1 != spid0 {
		t.Fatalf("UNMEASURED: the pool handed back session %d, not %d, so reuse of one connection was not tested", spid1, spid0)
	}
	if reused != "<NULL>" {
		t.Fatalf("reusing connection %d from the pool read %q: the driver no longer resets the session on reuse, and a call cancelled inside a procedure that sets SESSION_CONTEXT will hand the next statement a tenant it never chose (cleat#3647)", spid1, reused)
	}

	// 2. A cancel that arrives while the procedure is inside its loop, on the engine's own path.
	run := func(pinned bool) (spid int, after string) {
		blocker, err := c.rig.admin.Conn(ctx)
		if err != nil {
			t.Fatalf("UNMEASURED: %v", err)
		}
		btx, err := blocker.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("UNMEASURED: %v", err)
		}
		release := func() { _ = btx.Rollback(); blocker.Close() }
		defer release()
		if _, err := btx.ExecContext(ctx, `SELECT TOP 1 1 FROM `+blockOn+` WITH (TABLOCKX, HOLDLOCK)`); err != nil {
			t.Fatalf("UNMEASURED: taking the blocking lock on %s: %v", blockOn, err)
		}

		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		done := make(chan error, 1)
		var cn *sql.Conn
		var tx *sql.Tx
		if pinned {
			if cn, err = pool.Conn(ctx); err != nil {
				t.Fatalf("UNMEASURED: %v", err)
			}
			spid, _ = readSessionContext(t, cn)
			go func() { done <- call(cctx, cn) }()
		} else {
			spid, _ = readSessionContext(t, pool)
			if tx, err = pool.BeginTx(cctx, nil); err != nil {
				t.Fatalf("UNMEASURED: %v", err)
			}
			go func() { done <- call(cctx, tx) }()
		}
		blocked := false
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline) && !blocked; {
			var n int
			if err := c.rig.admin.QueryRowContext(ctx,
				`SELECT count(*) FROM sys.dm_exec_requests WHERE session_id = @p1 AND blocking_session_id <> 0`, spid).Scan(&n); err != nil {
				t.Fatalf("UNMEASURED: reading dm_exec_requests: %v", err)
			}
			if blocked = n > 0; !blocked {
				time.Sleep(20 * time.Millisecond)
			}
		}
		if !blocked {
			t.Fatalf("UNMEASURED: session %d never blocked on %s, so no cancel arrived mid-call (READ_COMMITTED_SNAPSHOT on would do this: a snapshot reader does not wait for a table lock)", spid, blockOn)
		}
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("UNMEASURED: the call returned %v, want context.Canceled", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("UNMEASURED: the cancelled call never returned")
		}
		release()
		if pinned {
			_, after = readSessionContext(t, cn)
			cn.Close()
		} else {
			_ = tx.Rollback()
			var spid2 int
			spid2, after = readSessionContext(t, pool)
			if spid2 != spid {
				t.Fatalf("UNMEASURED: the next use was session %d, not the cancelled %d", spid2, spid)
			}
		}
		return spid, after
	}

	// The same session, before the pool can reset it: shows the cancel really arrived inside the procedure.
	// If a procedure ever restores itself on an attention this reads no tenant; that is good news and not a
	// failure, since the guard in 1 and the assertion below still hold.
	if spid, same := run(true); same == "<NULL>" {
		t.Logf("session %d held no tenant after a cancel: the procedure restored itself, or the cancel came before it set one", spid)
	} else {
		t.Logf("session %d still held tenant %s after the cancel, so the cancel arrived mid-procedure", spid, same)
	}
	spid, after := run(false)
	if after != "<NULL>" {
		t.Errorf("the next use of connection %d after a cancelled call read tenant %q: the pool handed a tenant to a statement that never chose it", spid, after)
	}
}
