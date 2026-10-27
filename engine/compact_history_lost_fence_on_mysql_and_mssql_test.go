package engine

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3409 on the other two dialects. The issue read MySQL and SQL Server from
// code and did not measure them: both read the generation, DELETE the old events,
// then run an UPDATE ... WHERE generation = ? and commit without looking at what
// the UPDATE matched. These run the same scenario as the PostgreSQL pin in
// compact_history_with_a_lost_fence_keeps_the_events_test.go: events at steps
// 0-4, compactionStep=3, keepStep=3, and a holder transaction that bumps the
// generation and commits 400 ms into the call.
//
// The generation lives on workflow_instances on these two dialects (there is no
// lease table), so the holder bumps that row.

type compactOnDialect struct {
	name  string
	store WorkflowStore
	// begin opens a transaction the way the store does (SQL Server needs its
	// tenant session context or the row-level security policy hides every row).
	begin func(ctx context.Context) (*sql.Tx, error)
	// One id argument each. The placeholder syntax differs per dialect.
	bump, lock, count, step string
}

// compactDialects lists the arms by NAME so a subtest has one before it opens
// (opening skips the test when the DSN is not set).
var compactDialects = []struct {
	name string
	// canRace: the generation read does not block behind the holder, so a reclaim
	// can land BETWEEN the read and the fence. On SQL Server with the default
	// (locking) read committed the plain SELECT itself waits for the holder and
	// then reads the NEW generation, so there is no window to hit and a
	// CompactHistory that waited simply compacts a run that was reclaimed first
	// -- the separate "does the caller still own it" question the issue records.
	canRace bool
	open    func(t *testing.T) (compactOnDialect, func())
}{
	{"mysql", true, func(t *testing.T) (compactOnDialect, func()) {
		store, teardown := (&MySQLBackend{}).Setup(t)
		ms := store.(*MySQLStore)
		return compactOnDialect{
			name: "mysql", store: store,
			begin: func(ctx context.Context) (*sql.Tx, error) { return ms.db.BeginTx(ctx, nil) },
			bump:  `UPDATE workflow_instances SET generation = generation + 1 WHERE id = ?`,
			lock:  `SELECT id FROM workflow_instances WHERE id = ? FOR UPDATE`,
			count: `SELECT COUNT(*) FROM event_history WHERE workflow_id = ?`,
			step:  `SELECT compaction_step FROM workflow_instances WHERE id = ?`,
		}, teardown
	}},
	{"mssql_rcsi_on", true, func(t *testing.T) (compactOnDialect, func()) { return openMSSQLCompactArm(t, true) }},
	{"mssql_rcsi_off", false, func(t *testing.T) (compactOnDialect, func()) { return openMSSQLCompactArm(t, false) }},
}

// openMSSQLCompactArm builds the SQL Server arm on a private database whose
// READ_COMMITTED_SNAPSHOT is set as asked, the way runReclaimRaceMSSQL does.
func openMSSQLCompactArm(t *testing.T, rcsiOn bool) (compactOnDialect, func()) {
	t.Helper()
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}
	name := "mssql_rcsi_off"
	if rcsiOn {
		name = "mssql_rcsi_on"
	}
	raw := testutil.MSSQLTestDB(t)
	db, dsn := mssqlPrivateRCSIDatabase(t, raw, "3409_"+name, rcsiOn)
	applyMSSQLProcedures(t, db)
	ws, closer, err := NewMSSQLStoreFactory(dsn).OpenStore(context.Background(), DefaultTenantUUID, "default")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	ms, ok := ws.(*MSSQLStore)
	if !ok {
		t.Fatalf("OpenStore returned %T, want *MSSQLStore", ws)
	}
	return compactOnDialect{
		name: name, store: ws,
		begin: ms.beginTxWithContext,
		bump:  `UPDATE workflow_instances SET generation = generation + 1 WHERE id = @p1`,
		lock:  `SELECT id FROM workflow_instances WITH (UPDLOCK, ROWLOCK) WHERE id = @p1`,
		count: `SELECT COUNT(*) FROM event_history WHERE workflow_id = @p1`,
		step:  `SELECT compaction_step FROM workflow_instances WHERE id = @p1`,
	}, func() { _ = closer.Close() }
}

func (d compactOnDialect) query(t *testing.T, q, id string, dest any) {
	t.Helper()
	tx, err := d.begin(context.Background())
	if err != nil {
		t.Fatalf("%s: begin read: %v", d.name, err)
	}
	defer tx.Rollback()
	if err := tx.QueryRow(q, id).Scan(dest); err != nil {
		t.Fatalf("%s: %s: %v", d.name, q, err)
	}
}

func (d compactOnDialect) seed(t *testing.T, name string) string {
	t.Helper()
	ctx := context.Background()
	if err := d.store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: name, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("%s: DeployWorkflowDef: %v", d.name, err)
	}
	id, _, err := d.store.StartNewRun(ctx, "", name, 1, []byte(`{}`), name+"-key", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("%s: StartNewRun: %v", d.name, err)
	}
	for step := 0; step < 5; step++ {
		if err := d.store.AppendEventHistory(ctx, id, EventRecord{Step: step, EventType: EventTypeCall, Service: "svc", Op: "op"}); err != nil {
			t.Fatalf("%s: AppendEventHistory step %d: %v", d.name, step, err)
		}
	}
	return id
}

// compact runs CompactHistory(3, 3) with a holder that takes the generation row
// first, lets the call block on it for 400 ms, then commits a bump (reclaim) or
// rolls back (a bystander).
func (d compactOnDialect) compact(t *testing.T, id string, reclaim bool) error {
	t.Helper()
	ctx := context.Background()
	holder, err := d.begin(ctx)
	if err != nil {
		t.Fatalf("%s: begin holder: %v", d.name, err)
	}
	defer holder.Rollback()
	stmt := d.lock
	if reclaim {
		stmt = d.bump
	}
	if _, err := holder.ExecContext(ctx, stmt, id); err != nil {
		t.Fatalf("%s: holder statement: %v", d.name, err)
	}
	done := make(chan error, 1)
	go func() {
		done <- d.store.CompactHistory(ctx, id, []byte(`{"version":1,"compacted_step":3,"events":[]}`), 3, 3)
	}()
	select {
	case err := <-done:
		t.Fatalf("%s: CompactHistory returned (%v) while the instance row was held -- it no longer waits on the row a reclaim updates", d.name, err)
	case <-time.After(400 * time.Millisecond):
	}
	release := holder.Rollback
	if reclaim {
		release = holder.Commit
	}
	if err := release(); err != nil {
		t.Fatalf("%s: release holder: %v", d.name, err)
	}
	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		t.Fatalf("%s: CompactHistory still blocked 30s after the holder released", d.name)
		return nil
	}
}

func TestCompactHistoryCompactsWhenNothingMovesTheGenerationOnMySQLAndMSSQL(t *testing.T) {
	for _, arm := range compactDialects {
		arm := arm
		t.Run(arm.name, func(t *testing.T) {
			d, teardown := arm.open(t)
			defer teardown()
			id := d.seed(t, "compact-control-"+d.name)
			if err := d.compact(t, id, false); err != nil {
				t.Fatalf("%s: CompactHistory with an unchanged generation: %v", d.name, err)
			}
			var n int
			d.query(t, d.count, id, &n)
			if n != 2 {
				t.Errorf("%s: events left = %d, want 2 (steps 3 and 4 kept)", d.name, n)
			}
			var step sql.NullInt64
			d.query(t, d.step, id, &step)
			if !step.Valid || step.Int64 != 3 {
				t.Errorf("%s: compaction_step = %+v, want 3: the control must really compact, or the lost-fence case proves nothing", d.name, step)
			}
		})
	}
}

func TestCompactHistoryRollsBackItsDeleteWhenTheFenceIsLostOnMySQLAndMSSQL(t *testing.T) {
	for _, arm := range compactDialects {
		arm := arm
		if !arm.canRace {
			continue
		}
		t.Run(arm.name, func(t *testing.T) {
			d, teardown := arm.open(t)
			defer teardown()
			id := d.seed(t, "compact-lost-fence-"+d.name)

			err := d.compact(t, id, true)

			if err == nil {
				t.Errorf("%s: CompactHistory returned nil though a reclaim committed while it waited", d.name)
			} else if !errors.Is(err, ErrFenceLost) {
				t.Errorf("%s: CompactHistory = %v, want an error wrapping ErrFenceLost", d.name, err)
			}
			var n int
			d.query(t, d.count, id, &n)
			if n != 5 {
				t.Errorf("%s: events left = %d, want all 5: the delete committed although the fence was lost", d.name, n)
			}
			var step sql.NullInt64
			d.query(t, d.step, id, &step)
			if step.Valid {
				t.Errorf("%s: compaction_step = %+v, want NULL: a snapshot was written for a run whose generation had moved", d.name, step)
			}
		})
	}
}

// The lock-order question from the issue, on the other two dialects. finalize
// (migrations/mysql/003_procedures.sql and migrations/mssql/003_procedures.sql,
// the last migrations that define it) UPDATEs the instance row and THEN deletes
// the run's events. CompactHistory used to delete events and then update the
// instance, the reverse. A transaction stands in for finalize: it takes the
// instance row, CompactHistory starts while it holds it, and then it deletes the
// events -- the same two statements the procedure runs, with a gap between them
// the procedure does not leave. Emulated, not the real procedure, for that reason.
func TestCompactHistoryAndFinalizeTakeTheirLocksInTheSameOrderOnMySQLAndMSSQL(t *testing.T) {
	for _, arm := range compactDialects {
		arm := arm
		t.Run(arm.name, func(t *testing.T) {
			d, teardown := arm.open(t)
			defer teardown()
			ctx := context.Background()
			id := d.seed(t, "compact-lock-order-"+d.name)

			finalize, err := d.begin(ctx)
			if err != nil {
				t.Fatalf("%s: begin finalize stand-in: %v", d.name, err)
			}
			defer finalize.Rollback()
			// finalize's first statement: a write lock on the instance row, without
			// moving the generation (CompactHistory's own fence must still match).
			if _, err := finalize.ExecContext(ctx, d.lock, id); err != nil {
				t.Fatalf("%s: finalize stand-in: lock instance row: %v", d.name, err)
			}

			compactDone := make(chan error, 1)
			go func() {
				compactDone <- d.store.CompactHistory(ctx, id, []byte(`{"version":1,"compacted_step":3,"events":[]}`), 3, 3)
			}()
			select {
			case err := <-compactDone:
				t.Fatalf("%s: CompactHistory returned (%v) while the instance row was held", d.name, err)
			case <-time.After(400 * time.Millisecond):
			}

			deleteEvents := `DELETE FROM event_history WHERE workflow_id = ?`
			if arm.name != "mysql" {
				deleteEvents = `DELETE FROM event_history WHERE workflow_id = @p1`
			}
			finalizeDone := make(chan error, 1)
			go func() {
				if _, err := finalize.ExecContext(ctx, deleteEvents, id); err != nil {
					finalizeDone <- err
					return
				}
				finalizeDone <- finalize.Commit()
			}()

			var finalizeErr, compactErr error
			for i := 0; i < 2; i++ {
				select {
				case finalizeErr = <-finalizeDone:
				case compactErr = <-compactDone:
				case <-time.After(60 * time.Second):
					t.Fatalf("%s: CompactHistory and the finalize stand-in are still blocked after 60s", d.name)
				}
			}
			if isCompactDeadlock(finalizeErr) {
				t.Errorf("%s: the finalize stand-in was chosen as the deadlock victim: %v", d.name, finalizeErr)
			}
			if isCompactDeadlock(compactErr) {
				t.Errorf("%s: CompactHistory was chosen as the deadlock victim: %v", d.name, compactErr)
			}
			if finalizeErr != nil && !isCompactDeadlock(finalizeErr) {
				t.Errorf("%s: finalize stand-in: %v", d.name, finalizeErr)
			}
		})
	}
}

// isCompactDeadlock is a deadlock-victim error from either dialect: MySQL 1213
// or SQL Server 1205.
func isCompactDeadlock(err error) bool {
	return err != nil && (isDeadlockError(err) || isMSSQLDeadlock(err))
}
