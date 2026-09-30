package engine

// cleat#2758: ConsumeSignal used to run its DELETE and its signal_consumed_seq
// UPDATE as two separate, untransacted statements under mssqlRetry, which
// retries on any error in isMSSQLRetryable -- including an UNKNOWN-OUTCOME
// error (isMSSQLConnectionError: a dropped connection, reported here as a
// *net.OpError), whose outcome the caller cannot tell from a genuine failure.
// The exposure: that error lands after the UPDATE already committed
// server-side but before its acknowledgement reached the caller, and
// mssqlRetry reruns the whole closure -- the now-harmless DELETE, and a
// second, real increment.
//
// This test reproduces that deterministically, with no timing dependence.
// The one constructed element is the error value itself: a real
// *net.OpError{Op: "read", ..., Err: syscall.ECONNRESET}, returned to the
// database/sql layer only after the real statement (or the real COMMIT) has
// already executed on a real SQL Server -- everything up to that point is
// genuine server execution, not a stand-in for it. faultConn wraps the
// driver connection ConsumeSignal actually uses (via a faultConnector
// stacked on the same tenantSessionConnector the store's real pools use) and
// injects the fault from inside Stmt.ExecContext / Tx.Commit, after
// delegating to the real call.
//
// Built via NewMSSQLStore(db) with s.tenantID set directly, not through
// MSSQLStoreFactory: the factory offers no hook to stack a connector under
// tenantSessionConnector, and this test needs exactly that stack (fault
// outermost, tenant session context innermost) to fire the injected error on
// the same connection ConsumeSignal's own statements ran on.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/google/uuid"
	mssql "github.com/microsoft/go-mssqldb"

	"github.com/cleat-team/cleat/engine/testutil"
)

type consumeSignalFaultMode int

const (
	faultAfterUpdate consumeSignalFaultMode = iota + 1
	faultAfterCommit
)

type consumeSignalFaultPlan struct {
	mode  consumeSignalFaultMode
	fired atomic.Int32
}

func (p *consumeSignalFaultPlan) take() bool { return p.fired.CompareAndSwap(0, 1) }

func consumeSignalConnReset() error {
	return &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
}

type consumeSignalFaultConnector struct {
	driver.Connector
	plan *consumeSignalFaultPlan
}

func (c *consumeSignalFaultConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &consumeSignalFaultConn{Conn: conn, plan: c.plan}, nil
}

type consumeSignalFaultConn struct {
	driver.Conn
	plan *consumeSignalFaultPlan
}

func (c *consumeSignalFaultConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	st, err := c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, q)
	if err != nil {
		return nil, err
	}
	return &consumeSignalFaultStmt{Stmt: st, q: q, plan: c.plan}, nil
}
func (c *consumeSignalFaultConn) BeginTx(ctx context.Context, o driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, o)
	if err != nil {
		return nil, err
	}
	return &consumeSignalFaultTx{Tx: tx, plan: c.plan}, nil
}
func (c *consumeSignalFaultConn) CheckNamedValue(nv *driver.NamedValue) error {
	if v, ok := c.Conn.(driver.NamedValueChecker); ok {
		return v.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}
func (c *consumeSignalFaultConn) ResetSession(ctx context.Context) error {
	if r, ok := c.Conn.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}
func (c *consumeSignalFaultConn) IsValid() bool {
	if v, ok := c.Conn.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

type consumeSignalFaultStmt struct {
	driver.Stmt
	q    string
	plan *consumeSignalFaultPlan
}

func (s *consumeSignalFaultStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	r, err := s.Stmt.(driver.StmtExecContext).ExecContext(ctx, args)
	if err == nil && s.plan.mode == faultAfterUpdate &&
		strings.Contains(s.q, "signal_consumed_seq = signal_consumed_seq + 1") && s.plan.take() {
		return nil, consumeSignalConnReset() // executed server-side; acknowledgement "lost"
	}
	return r, err
}
func (s *consumeSignalFaultStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	return s.Stmt.(driver.StmtQueryContext).QueryContext(ctx, args)
}
func (s *consumeSignalFaultStmt) CheckNamedValue(nv *driver.NamedValue) error {
	if v, ok := s.Stmt.(driver.NamedValueChecker); ok {
		return v.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}

type consumeSignalFaultTx struct {
	driver.Tx
	plan *consumeSignalFaultPlan
}

func (t *consumeSignalFaultTx) Commit() error {
	err := t.Tx.Commit()
	if err == nil && t.plan.mode == faultAfterCommit && t.plan.take() {
		return consumeSignalConnReset() // committed server-side; acknowledgement "lost"
	}
	return err
}

// runConsumeSignalUnknownOutcome is the shared body both Test* functions
// below call. check-skips.sh keys its static baseline by enclosing
// function, so this function's two t.Skip sites are what the baseline
// records against BOTH callers.
func runConsumeSignalUnknownOutcome(t *testing.T, mode consumeSignalFaultMode) {
	dsn := os.Getenv("CLEAT_TEST_MSSQL")
	if dsn == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}
	if testing.Short() {
		t.Skip("Skipping MSSQL integration test in short mode")
	}

	ctx := context.Background()
	adminDB := testutil.MSSQLTestDB(t)
	t.Cleanup(func() { adminDB.Close() })
	testutil.SetupMSSQLFullSchema(t, adminDB)
	testutil.CleanupMSSQLTestData(t, adminDB)
	t.Cleanup(func() { testutil.CleanupMSSQLTestData(t, adminDB) })
	adm := testutil.MSSQLAdminDB(t, adminDB)

	const tenantID = DefaultTenantUUID
	run := uuid.New().String()[:8]
	defName := "consume-signal-uo-def-" + run
	wfID := "consume-signal-uo-wf-" + run

	if _, err := adm.ExecContext(ctx, `
		INSERT INTO workflow_defs (name, version, wasm_bytes, abi_version, min_version, tenant_id)
		VALUES (@p1, 1, 0x0061736d, 1, 1, @p2)`, defName, tenantID); err != nil {
		t.Fatalf("seed workflow_def: %v", err)
	}
	if _, err := adm.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue, tenant_id, signal_consumed_seq)
		VALUES (@p1, @p2, 1, 'ready', DATEADD(DAY, -1, SYSUTCDATETIME()), '{}', 'default', @p3, 0)`,
		wfID, defName, tenantID); err != nil {
		t.Fatalf("seed workflow_instance: %v", err)
	}
	var signalID int64
	if err := adm.QueryRowContext(ctx, `
		INSERT INTO workflow_signals (workflow_id, signal_name, payload, tenant_id)
		OUTPUT INSERTED.id
		VALUES (@p1, 'consume-signal-uo', '{}', @p2)`, wfID, tenantID).Scan(&signalID); err != nil {
		t.Fatalf("seed signal: %v", err)
	}

	base, err := mssql.NewConnector(dsn)
	if err != nil {
		t.Fatalf("mssql.NewConnector: %v", err)
	}
	plan := &consumeSignalFaultPlan{mode: mode}
	db := sql.OpenDB(&consumeSignalFaultConnector{
		Connector: &tenantSessionConnector{Connector: base, tenantID: tenantID},
		plan:      plan,
	})
	defer db.Close()
	s := NewMSSQLStore(db)
	s.tenantID = tenantID

	callErr := s.ConsumeSignal(ctx, wfID, signalID)

	if plan.fired.Load() != 1 {
		t.Fatalf("fault never fired -- this run measured nothing, not a pass")
	}

	var remaining, seq int
	if err := adm.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workflow_signals WHERE id = @p1`, signalID).Scan(&remaining); err != nil {
		t.Fatalf("count remaining signal: %v", err)
	}
	if err := adm.QueryRowContext(ctx,
		`SELECT signal_consumed_seq FROM workflow_instances WHERE id = @p1`, wfID).Scan(&seq); err != nil {
		t.Fatalf("read signal_consumed_seq: %v", err)
	}
	t.Logf("mode=%d fault_fired=%d call_err=%v remaining=%d seq=%d", mode, plan.fired.Load(), callErr, remaining, seq)

	if callErr == nil {
		t.Errorf("ConsumeSignal returned nil despite the injected connection reset -- the fault did not propagate")
	}

	switch mode {
	case faultAfterUpdate:
		// The fault fires as the UPDATE's own driver call returns, inside the
		// transaction, before Commit. beginTxWithContext's deferred Rollback
		// undoes the DELETE too: the signal row must still be there and the
		// counter must still be zero. If this ever reads remaining=0, seq=0
		// -- the row gone, the counter not bumped -- that is a WORSE bug than
		// the one this PR fixes: a consumption silently lost rather than
		// double-counted.
		if remaining != 1 {
			t.Errorf("remaining = %d, want 1 (the transaction should have rolled back the DELETE along with the failed UPDATE)", remaining)
		}
		if seq != 0 {
			t.Errorf("signal_consumed_seq = %d, want 0 (the failed UPDATE must not have applied)", seq)
		}
	case faultAfterCommit:
		// The fault fires after Commit has already returned nil from the
		// server's point of view -- both statements are durably applied.
		// ConsumeSignal still reports an error (the caller cannot tell this
		// case apart from a genuine failure), but the data is correct: one
		// consumption, counted once.
		if remaining != 0 {
			t.Errorf("remaining = %d, want 0 (the commit succeeded server-side)", remaining)
		}
		if seq != 1 {
			t.Errorf("signal_consumed_seq = %d, want 1 (the commit succeeded server-side, exactly once)", seq)
		}
	}
}

// TestMSSQLStore_ConsumeSignalUnknownOutcomeAfterUpdate is cleat#2758's exact
// reproduction case: reverting the fix and rerunning this test gives
// seq=2, err=nil -- the DELETE's retry is a silent no-op and the UPDATE
// applies twice, because two autocommit statements share no transaction for
// a rollback to undo.
func TestMSSQLStore_ConsumeSignalUnknownOutcomeAfterUpdate(t *testing.T) {
	runConsumeSignalUnknownOutcome(t, faultAfterUpdate)
}

// TestMSSQLStore_ConsumeSignalUnknownOutcomeAfterCommit proves the OTHER
// half of the fix: with the transaction in place but mssqlRetry left in
// place of withRollbackGuaranteedRetry, this fault (after a real COMMIT)
// still gives seq=2, because mssqlRetry retries an unknown-outcome error
// unconditionally. withRollbackGuaranteedRetry does not retry this error at
// all -- it returns it, seq stays at 1, and the caller (not this store) is
// responsible for deciding whether to ask again.
func TestMSSQLStore_ConsumeSignalUnknownOutcomeAfterCommit(t *testing.T) {
	runConsumeSignalUnknownOutcome(t, faultAfterCommit)
}
