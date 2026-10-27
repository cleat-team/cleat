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
// generation and commits once the call is blocked on it.
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
	// waitBlocked returns once another session is waiting on a lock holder
	// holds. It polls the server rather than sleeping, for the reason
	// waitUntilBlockedBy gives.
	waitBlocked func(t *testing.T, holder *sql.Tx)
}

// pollUntil retries probe every 5 ms for up to 20 s and fails if it never
// reports true.
func pollUntil(t *testing.T, what string, probe func() (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		ok, err := probe()
		if err != nil {
			t.Fatalf("%s: probe: %v", what, err)
		}
		if ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s: nothing became blocked on the holder within 20s: CompactHistory never waited on the row the holder locked", what)
}

// mysqlWaitersOn counts lock requests that are waiting on the session whose
// CONNECTION_ID() is connID.
//
// performance_schema.data_lock_waits has a row for every lock request that is
// waiting; BLOCKING_THREAD_ID names the thread holding what it waits for, and
// performance_schema.threads maps that to the processlist id CONNECTION_ID()
// returns. information_schema.innodb_trx is NOT used: measured on MySQL 8.4
// while writing cleat#3450, it listed only the holder while the compactor's
// FOR UPDATE sat in the processlist in state "statistics", so it never reported
// the wait.
func mysqlWaitersOn(db *sql.DB, connID int64) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM performance_schema.data_lock_waits
		WHERE BLOCKING_THREAD_ID IN (
			SELECT THREAD_ID FROM performance_schema.threads WHERE PROCESSLIST_ID = ?)`, connID).Scan(&n)
	return n, err
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
			// Scoped to the holder, as the SQL Server arm is by @@SPID and
			// PostgreSQL's would be by pg_blocking_pids: a lock wait between two
			// OTHER sessions on this instance must not release the holder early
			// (cleat#3455). See mysqlWaitersOn for the query and why it is this one.
			waitBlocked: func(t *testing.T, holder *sql.Tx) {
				var connID int64
				if err := holder.QueryRow(`SELECT CONNECTION_ID()`).Scan(&connID); err != nil {
					t.Fatalf("mysql: holder connection id: %v", err)
				}
				pollUntil(t, "mysql", func() (bool, error) {
					n, err := mysqlWaitersOn(ms.db, connID)
					return n > 0, err
				})
			},
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
		waitBlocked: func(t *testing.T, holder *sql.Tx) {
			var spid int
			if err := holder.QueryRow(`SELECT @@SPID`).Scan(&spid); err != nil {
				t.Fatalf("%s: holder session id: %v", name, err)
			}
			pollUntil(t, name, func() (bool, error) {
				var n int
				err := db.QueryRow(`SELECT COUNT(*) FROM sys.dm_exec_requests WHERE blocking_session_id = @p1`, spid).Scan(&n)
				return n > 0, err
			})
		},
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
// first, waits until the call is blocked on it, then commits a bump (reclaim) or
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
	d.waitBlocked(t, holder)
	select {
	case err := <-done:
		t.Fatalf("%s: CompactHistory returned (%v) while the instance row was held -- it no longer waits on the row a reclaim updates", d.name, err)
	default:
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
			d.waitBlocked(t, finalize)
			select {
			case err := <-compactDone:
				t.Fatalf("%s: CompactHistory returned (%v) while the instance row was held", d.name, err)
			default:
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

// The MySQL probe is scoped to the holder (cleat#3455): a lock wait between two
// OTHER sessions must not satisfy it. Without this, the scoping is a claim the
// race tests cannot check, since they only ever run with one wait in flight.
//
// Three sessions on two rows. bystander holds row Y and waiter blocks on it;
// holder holds row X and nothing waits on it. The instance-wide count the probe
// used to be sees the bystander's wait; the scoped probe reports none for the
// holder and one for the bystander.
func TestMySQLLockWaitProbeIgnoresAWaitOnAnotherSessionsLock(t *testing.T) {
	d, teardown := compactDialects[0].open(t)
	defer teardown()
	ctx := context.Background()
	db := d.store.(*MySQLStore).db
	x := d.seed(t, "probe-holder-"+d.name)
	y := d.seed(t, "probe-bystander-"+d.name)

	connID := func(tx *sql.Tx) int64 {
		t.Helper()
		var id int64
		if err := tx.QueryRow(`SELECT CONNECTION_ID()`).Scan(&id); err != nil {
			t.Fatalf("connection id: %v", err)
		}
		return id
	}
	begin := func(id string) *sql.Tx {
		t.Helper()
		tx, err := d.begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.ExecContext(ctx, d.lock, id); err != nil {
			t.Fatalf("lock %s: %v", id, err)
		}
		return tx
	}
	holder := begin(x)
	defer holder.Rollback()
	bystander := begin(y)
	defer bystander.Rollback()

	waiter, err := d.begin(ctx)
	if err != nil {
		t.Fatalf("begin waiter: %v", err)
	}
	defer waiter.Rollback()
	waiterDone := make(chan error, 1)
	go func() {
		_, err := waiter.ExecContext(ctx, d.lock, y)
		waiterDone <- err
	}()

	bystanderID, holderID := connID(bystander), connID(holder)
	pollUntil(t, "bystander", func() (bool, error) {
		n, err := mysqlWaitersOn(db, bystanderID)
		return n > 0, err
	})

	var anyWait int
	if err := db.QueryRow(`SELECT COUNT(*) FROM performance_schema.data_lock_waits`).Scan(&anyWait); err != nil {
		t.Fatalf("instance-wide count: %v", err)
	}
	if anyWait == 0 {
		t.Fatalf("the instance-wide count saw no wait although the waiter is blocked on the bystander: this test would pass against the old unscoped probe")
	}
	if n, err := mysqlWaitersOn(db, holderID); err != nil || n != 0 {
		t.Fatalf("mysqlWaitersOn(holder) = %d, %v; want 0: nothing waits on the holder's row, the wait is on the bystander's", n, err)
	}

	if err := bystander.Rollback(); err != nil {
		t.Fatalf("release bystander: %v", err)
	}
	select {
	case err := <-waiterDone:
		if err != nil {
			t.Fatalf("waiter: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("waiter still blocked 30s after the bystander released")
	}
}
