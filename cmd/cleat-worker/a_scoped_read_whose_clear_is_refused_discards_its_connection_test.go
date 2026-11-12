package main

// cleat#3678 (owner decisions D65 and D70). auth.TenantStore.scopedRead on SQL Server pins a connection,
// sets the tenant on it and, when the read is over, has cleared the tenant before the connection goes back to the
// pool. When the clear cannot be shown to have worked the connection must be DISCARDED, not pooled: a
// connection that may still carry the tenant must never serve the next caller. That is what makes the result
// independent of the driver's session reset.
//
// THE CLEAR IS REFUSED here by a connector wrapper that fails the statements that clear the tenant, in the
// read's own batch and on its own round trip. The driver does not reset the session either
// (noResetConnector), so nothing but the discard stands between the tenant and the next statement. Asserted: the next statement runs on a DIFFERENT connection (identity is
// session id plus connection_id, because SQL Server hands a replaced connection the same session id) and
// reads no tenant. Red against a scopedRead that does not clear or discard: it leaves the tenant on the
// same connection.
//
// Why not a cancelled read: measured with this wrapper, a read cancelled inside its transaction is already
// safe on a driver that does not reset, because database/sql discards a transaction's connection on cancel
// unless the driver implements both ResetSession and IsValid. That case passes against the version that
// relied on the driver, so it cannot tell the two apart and is not what this pins. The control for this
// test is the marker case in TestAScopedReadsTenantDoesNotSurviveTheSQLServerConnectionPool.

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"errors"
	"strings"
	"testing"

	mssql "github.com/microsoft/go-mssqldb"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine/testutil"
)

// refuseClearConnector wraps a driver that does not reset the session and refuses the statement that clears
// the session tenant. Refused counts how many times it did.
type refuseClearConnector struct {
	noResetConnector
	refused *int
}

func (c refuseClearConnector) Connect(ctx context.Context) (sqldriver.Conn, error) {
	conn, err := c.noResetConnector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &refuseClearConn{noResetConn: conn.(*noResetConn), refused: c.refused}, nil
}

type refuseClearConn struct {
	*noResetConn
	refused *int
}

func refusesClear(q string) bool {
	return strings.Contains(q, "sp_set_session_context") && strings.Contains(q, "@value = NULL")
}

// PrepareContext refuses the clear when it travels in the read's own batch (D70), which database/sql sends down
// the prepare path on a driver whose connection has no QueryerContext, as the wrapper does not.
func (c *refuseClearConn) PrepareContext(ctx context.Context, q string) (sqldriver.Stmt, error) {
	if refusesClear(q) {
		*c.refused++
		return nil, errors.New("clear refused (test)")
	}
	return c.noResetConn.PrepareContext(ctx, q)
}

func (c *refuseClearConn) ExecContext(ctx context.Context, q string, args []sqldriver.NamedValue) (sqldriver.Result, error) {
	if refusesClear(q) {
		*c.refused++
		return nil, errors.New("clear refused (test)")
	}
	// go-mssqldb has no ExecerContext; ErrSkip sends database/sql down its prepare path, as without the wrapper.
	return nil, sqldriver.ErrSkip
}

func TestAScopedReadWhoseClearIsRefusedDiscardsItsConnection(t *testing.T) {
	ctx := context.Background()
	admin := testutil.TestDB(t, testutil.DialectMSSQL)
	u := mssqlDatabaseURL(t, admin)
	mssqlAppLoginURL(t, admin, u, true) // the application login; VIEW SERVER STATE only so the probe can name a connection

	cn, err := mssql.NewConnector(u.String())
	if err != nil {
		t.Fatalf("UNMEASURED: %v", err)
	}
	refused := 0
	app := sql.OpenDB(refuseClearConnector{noResetConnector{cn}, &refused})
	t.Cleanup(func() { app.Close() })
	app.SetMaxOpenConns(1)

	const identityExpr = `CONVERT(nvarchar(20), @@SPID) + N'/' + ISNULL(CONVERT(nvarchar(36),
		(SELECT c.connection_id FROM sys.dm_exec_connections c WHERE c.session_id = @@SPID)), N'-')`
	var idBefore string
	if err := app.QueryRowContext(ctx, `SELECT `+identityExpr).Scan(&idBefore); err != nil {
		t.Fatalf("UNMEASURED: %v", err)
	}

	ts, err := auth.NewTenantStoreForDialect(app, auth.DialectMSSQL)
	if err != nil {
		t.Fatalf("UNMEASURED: %v", err)
	}
	// The read fails, because its batch carries the clear and the clear is refused. What matters is what
	// becomes of the connection, so the error is not the assertion.
	_, readErr := ts.TenantForHost(ctx, "refused-clear.invalid", uuid.New())
	if readErr == nil {
		t.Fatal("UNMEASURED: the read succeeded although its clear was refused, so a refused clear that surfaces was never exercised")
	}
	if refused == 0 {
		t.Fatal("UNMEASURED: the scoped read never tried to clear the tenant, so a clear that is refused was never exercised")
	}

	var idAfter string
	var v sql.NullString
	if err := app.QueryRowContext(ctx, `SELECT `+identityExpr+`, CAST(SESSION_CONTEXT(N'tenant_id') AS NVARCHAR(100))`).Scan(&idAfter, &v); err != nil {
		t.Fatalf("UNMEASURED: %v", err)
	}
	if idAfter == idBefore {
		t.Errorf("the next statement ran on the connection the scoped read used (%s) after its clear was refused; "+
			"it must be discarded, not pooled", idBefore)
	}
	if v.String != "" {
		t.Errorf("the next statement runs as tenant %q after a scoped read whose clear was refused", v.String)
	}
}
