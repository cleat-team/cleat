package engine

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestPerStepFlushWorksOnEveryDialect is the regression test for a defect with
// the same shape as the RLS one in flush_rls_test.go, on a different axis: the
// per-step flush is written in PostgreSQL's dialect and runs against whatever
// database the worker was pointed at.
//
// engine/flush.go's insertEventSQL uses $1..$31 positional parameters. MySQL
// parses `$1` as an identifier and answers
//
//	Error 1054 (42S22): Unknown column '$1' in 'field list'
//
// SQL Server wants @p1 and fails likewise. Nothing in the worker guards this:
// cmd/cleat-worker/main.go opens the MySQL or SQL Server handle, assigns it to
// Worker.db, and cmd/cleat-worker/setup.go:1580 passes it to engine.WithDB
// unconditionally -- the comment there reads "Always provide DB so per-step
// flush and adaptive flusher work." flushEvent's only guard is `e.db == nil`.
//
// So on those two dialects every per-step flush fails, and
// engine/lifecycle.go:180 logs the failure and carries on. The events are not
// lost outright -- the worker's FinalizeWorkflowSegment appends the whole
// segment through the dialect-correct store path when the segment ends -- but
// per-step durability is the thing that makes a crash *mid-segment* survivable.
// A MySQL or SQL Server deployment therefore gets exactly the behaviour
// docs/durable-calls.md attributes to --no-per-step-flush ("higher throughput,
// weaker crash safety") without having asked for it, and with no way to tell
// from the outside: the flag reads as off, the log line is one line among many,
// and the workflow completes normally.
//
// The Postgres subtest is the control. If it fails too, the fixture is wrong
// and the MySQL result says nothing about dialects.
func TestPerStepFlushWorksOnEveryDialect(t *testing.T) {
	t.Run("postgres", func(t *testing.T) {
		db := testutil.TestDB(t, testutil.DialectPostgres)
		testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
		defer db.Close()

		wfID := "flush-dialect-pg"
		seedWorkflowInstance(t, db, testutil.DialectPostgres, wfID)
		requireFlushPersists(t, db, testutil.DialectPostgres, wfID)
	})

	t.Run("mssql", func(t *testing.T) {
		db := testutil.TestDB(t, testutil.DialectMSSQL)
		testutil.SetupMSSQLFullSchema(t, db)
		defer db.Close()

		wfID := "flush-dialect-mssql"
		seedWorkflowInstance(t, db, testutil.DialectMSSQL, wfID)
		requireFlushPersists(t, db, testutil.DialectMSSQL, wfID)
	})

	t.Run("mysql", func(t *testing.T) {
		db := testutil.TestDB(t, testutil.DialectMySQL)
		testutil.SetupMySQLFullSchema(t, db)
		defer db.Close()

		wfID := "flush-dialect-mysql"
		seedWorkflowInstance(t, db, testutil.DialectMySQL, wfID)
		requireFlushPersists(t, db, testutil.DialectMySQL, wfID)
	})
}

// requireFlushPersists runs one per-step flush through the engine exactly as
// recordEvent does, then reads the row back with a query in the dialect's own
// placeholder syntax -- so a failure here is about the flush, not the check.
func requireFlushPersists(t *testing.T, db *sql.DB, dialect testutil.Dialect, wfID string) {
	t.Helper()

	// Configured the way cmd/cleat-worker/setup.go configures it. All three
	// options matter here:
	//
	//   WithTenantID   -- the tenant of the claimed workflow. On PostgreSQL it
	//                     selects the explicit-transaction path that sets the
	//                     RLS context, so omitting it would exercise a branch
	//                     the worker never takes.
	//   WithWorkflowStore -- how the engine learns which dialect it is on.
	//   WithDB         -- the handle the per-step flush writes through.
	eng := NewEngine(nil, nil,
		WithWorkflowID(wfID),
		WithTenantID(DefaultTenantUUID),
		WithWorkflowStore(storeFor(t, dialect, db)),
		WithDB(db))

	rec := EventRecord{
		Step:      0,
		EventType: EventTypeCall,
		Service:   "payments",
		Op:        "Charge",
		Request:   `{"amount":100}`,
		Response:  `{"charge_id":"chg-1"}`,
	}

	if err := eng.flushEvent(context.Background(), wfID, rec, ""); err != nil {
		t.Fatalf("per-step flush failed on %s: %v\n\n"+
			"engine/flush.go's insertEventSQL is PostgreSQL-dialect SQL and the "+
			"worker hands it whatever *sql.DB the --db DSN produced. "+
			"engine/lifecycle.go:180 logs this error and continues, so the "+
			"workflow finishes normally and only FinalizeWorkflowSegment ever "+
			"writes history -- which means a crash mid-segment loses the whole "+
			"segment on this dialect.", dialect, err)
	}

	// Read back on the privileged handle, not on db. db is what the engine
	// writes through, and on SQL Server it is subject to the security policies
	// -- so this count came back 0 for a flush that had in fact succeeded. See
	// testutil.AdminDB for why the PostgreSQL arm never noticed.
	var n int
	if err := testutil.AdminDB(t, db, dialect).QueryRow(countEventsSQL(dialect), wfID).Scan(&n); err != nil {
		t.Fatalf("counting events on %s: %v", dialect, err)
	}
	if n != 1 {
		t.Errorf("event_history has %d rows on %s after one flush, want 1", n, dialect)
	}
}

func countEventsSQL(dialect testutil.Dialect) string {
	switch dialect {
	case testutil.DialectMySQL:
		return `SELECT count(*) FROM event_history WHERE workflow_id = ?`
	case testutil.DialectMSSQL:
		return `SELECT count(*) FROM event_history WHERE workflow_id = @p1`
	default:
		return `SELECT count(*) FROM event_history WHERE workflow_id = $1`
	}
}

// seedWorkflowInstance inserts the parent row event_history's foreign key
// requires. Written per dialect rather than through a store so that the test
// exercises the flush path and nothing else.
func seedWorkflowInstance(t *testing.T, db *sql.DB, dialect testutil.Dialect, wfID string) {
	t.Helper()

	// workflow_instances has a foreign key to workflow_defs on SQL Server, so
	// the definition row has to exist first. Harmless where it does not.
	var defStmt string
	switch dialect {
	case testutil.DialectMySQL:
		defStmt = `INSERT INTO workflow_defs (name, version, wasm_bytes, tenant_id)
		           VALUES ('flush-dialect', 1, '', '` + DefaultTenantUUID + `')`
	case testutil.DialectMSSQL:
		defStmt = `INSERT INTO workflow_defs (name, version, wasm_bytes, tenant_id)
		           VALUES ('flush-dialect', 1, 0x, '` + DefaultTenantUUID + `')`
	default:
		defStmt = `INSERT INTO workflow_defs (name, version, wasm_bytes, tenant_id)
		           VALUES ('flush-dialect', 1, '', '` + DefaultTenantUUID + `')`
	}
	if _, err := db.Exec(defStmt); err != nil && !isDuplicateKey(err) {
		t.Fatalf("seeding workflow_defs on %s: %v", dialect, err)
	}

	var stmt string
	switch dialect {
	case testutil.DialectMySQL:
		stmt = `INSERT INTO workflow_instances (id, def_name, def_version, status, input, tenant_id)
		        VALUES (?, 'flush-dialect', 1, 'running', '{}', '` + DefaultTenantUUID + `')`
	case testutil.DialectMSSQL:
		stmt = `INSERT INTO workflow_instances (id, def_name, def_version, status, input, tenant_id)
		        VALUES (@p1, 'flush-dialect', 1, 'running', '{}', '` + DefaultTenantUUID + `')`
	default:
		stmt = `INSERT INTO workflow_instances (id, def_name, def_version, status, input, tenant_id)
		        VALUES ($1, 'flush-dialect', 1, 'running', '{}', '` + DefaultTenantUUID + `')`
	}

	// Re-runs against a persistent local database are ordinary; a row left by
	// the previous run is not a failure, and deleting the whole table would
	// take other suites' rows with it (see tests/crash, which needed its own
	// database for exactly this reason).
	if _, err := db.Exec(stmt, wfID); err != nil && !isDuplicateKey(err) {
		t.Fatalf("seeding workflow_instances on %s: %v", dialect, err)
	}

	// Start from a known state. These databases are long-lived locally, and the
	// row survives between runs: leaving event_count as the previous run left it
	// makes TestPerStepFlushDoesNotDoubleCountEvents pass or fail depending on
	// history rather than on the code. Old event rows go for the same reason.
	if _, err := db.Exec(deleteEventsSQL(dialect), wfID); err != nil {
		t.Fatalf("clearing prior events on %s: %v", dialect, err)
	}
	if _, err := db.Exec(resetEventCountSQL(dialect), wfID); err != nil {
		t.Fatalf("resetting event_count on %s: %v", dialect, err)
	}

	t.Cleanup(func() {
		_, _ = db.Exec(deleteEventsSQL(dialect), wfID)
	})
}

func deleteEventsSQL(dialect testutil.Dialect) string {
	switch dialect {
	case testutil.DialectMySQL:
		return `DELETE FROM event_history WHERE workflow_id = ?`
	case testutil.DialectMSSQL:
		return `DELETE FROM event_history WHERE workflow_id = @p1`
	default:
		return `DELETE FROM event_history WHERE workflow_id = $1`
	}
}

func isDuplicateKey(err error) bool {
	s := err.Error()
	return strings.Contains(s, "Duplicate entry") || // MySQL 1062
		strings.Contains(s, "duplicate key value") || // PostgreSQL 23505
		strings.Contains(s, "Violation of PRIMARY KEY") // SQL Server 2627
}

// storeFor builds the store the worker would have built for this dialect. The
// tenant matches the engine's, as it does in the worker: both come from the
// claimed workflow's tenant_id.
//
// SQL Server goes through the factory, and that is not a stylistic difference.
// NewMSSQLStore(db) sets a tenantID field and nothing else, so every read it
// does outside a transaction -- PollSignal, GetWorkflowByID,
// GetAllowedSignalCallers -- runs on a connection with no
// sp_set_session_context, and on a database built from the shipped migrations
// the security policies return nothing to it. The store cannot see rows it
// just wrote. Nothing outside tests constructs it that way: cmd/cleat-worker,
// cmd/cleat-bench and cmd/deploy-workflow all call
// MSSQLStoreFactory.OpenStore, which wraps the connector so every connection
// in the pool carries the tenant.
//
// So the doc comment above was true of two dialects out of three, and
// TestSignalPayloadRoundTripsOnEveryDialect failed all six of its SQL Server
// cases with "signal not found" on signals that had been delivered
// successfully.
func storeFor(t *testing.T, dialect testutil.Dialect, db *sql.DB) WorkflowStore {
	t.Helper()
	switch dialect {
	case testutil.DialectMySQL:
		s := NewMySQLStore(db)
		s.tenantID = DefaultTenantUUID
		return s
	case testutil.DialectMSSQL:
		ws, closer, err := NewMSSQLStoreFactory(os.Getenv("CLEAT_TEST_MSSQL")).
			OpenStore(context.Background(), DefaultTenantUUID, "default")
		if err != nil {
			t.Fatalf("open a tenant-scoped SQL Server store: %v", err)
		}
		t.Cleanup(func() { _ = closer.Close() })
		return ws
	default:
		s := NewPostgresStore(db)
		s.tenantID = DefaultTenantUUID
		return s
	}
}

// TestPerStepFlushDoesNotDoubleCountEvents pins the one design decision in the
// dialect fix that could be silently wrong.
//
// On MySQL and SQL Server the per-step flush goes through the store's
// appendEventsInTx, which maintains workflow_instances.event_count. But
// FinalizeWorkflowSegment appends the *whole* segment again when it ends -- its
// insert is idempotent (INSERT IGNORE / WHERE NOT EXISTS) so no duplicate rows
// appear, but its event_count increment is unconditional. If the per-step flush
// counted as well, every event would be counted twice: GetEventCount would
// report double, and the --max-events-per-workflow quota would trip at half the
// configured limit. Nothing about that failure looks like a flush bug.
//
// So flushEventForStep passes incrementCount=false, and this is what says so.
// PostgreSQL's raw per-step insert has never touched event_count either, which
// is what makes leaving it alone the consistent choice rather than a special
// case.
func TestPerStepFlushDoesNotDoubleCountEvents(t *testing.T) {
	run := func(t *testing.T, dialect testutil.Dialect, db *sql.DB, wfID string) {
		t.Helper()
		seedWorkflowInstance(t, db, dialect, wfID)

		store := storeFor(t, dialect, db)
		flusher, ok := store.(perStepEventFlusher)
		if !ok {
			t.Fatalf("%s store does not implement perStepEventFlusher, so the "+
				"per-step flush on this dialect is still the PostgreSQL SQL that "+
				"cannot run here", dialect)
		}

		ctx := context.Background()
		rec := EventRecord{
			Step: 0, EventType: EventTypeCall,
			Service: "payments", Op: "Charge",
			Request: `{"amount":100}`, Response: `{"charge_id":"chg-1"}`,
		}

		if err := flusher.flushEventForStep(ctx, wfID, rec); err != nil {
			t.Fatalf("per-step flush on %s: %v", dialect, err)
		}
		if got := eventCountOf(t, db, dialect, wfID); got != 0 {
			t.Errorf("event_count is %d after a per-step flush on %s, want 0: the "+
				"finalize append counts the segment, so counting here too doubles "+
				"every event and halves the effective quota", got, dialect)
		}

		// Now the segment ends, exactly as FinalizeWorkflowSegment does it.
		if err := store.AppendEventHistoryBatch(ctx, wfID, []EventRecord{rec}); err != nil {
			t.Fatalf("finalize append on %s: %v", dialect, err)
		}
		if got := eventCountOf(t, db, dialect, wfID); got != 1 {
			t.Errorf("event_count is %d after the finalize append on %s, want 1", got, dialect)
		}

		// And the row is still there exactly once. Privileged handle, for the
		// reason spelled out in testutil.AdminDB.
		var n int
		if err := testutil.AdminDB(t, db, dialect).QueryRow(countEventsSQL(dialect), wfID).Scan(&n); err != nil {
			t.Fatalf("counting events on %s: %v", dialect, err)
		}
		if n != 1 {
			t.Errorf("event_history has %d rows on %s, want 1", n, dialect)
		}
	}

	t.Run("mysql", func(t *testing.T) {
		db := testutil.TestDB(t, testutil.DialectMySQL)
		testutil.SetupMySQLFullSchema(t, db)
		defer db.Close()
		run(t, testutil.DialectMySQL, db, "flush-count-mysql")
	})

	t.Run("mssql", func(t *testing.T) {
		db := testutil.TestDB(t, testutil.DialectMSSQL)
		testutil.SetupMSSQLFullSchema(t, db)
		defer db.Close()
		run(t, testutil.DialectMSSQL, db, "flush-count-mssql")
	})
}

func eventCountOf(t *testing.T, db *sql.DB, dialect testutil.Dialect, wfID string) int64 {
	t.Helper()
	q := `SELECT event_count FROM workflow_instances WHERE id = ?`
	if dialect == testutil.DialectMSSQL {
		q = `SELECT event_count FROM workflow_instances WHERE id = @p1`
	}
	var n int64
	if err := testutil.AdminDB(t, db, dialect).QueryRow(q, wfID).Scan(&n); err != nil {
		t.Fatalf("reading event_count on %s: %v", dialect, err)
	}
	return n
}

func resetEventCountSQL(dialect testutil.Dialect) string {
	if dialect == testutil.DialectMSSQL {
		return `UPDATE workflow_instances SET event_count = 0 WHERE id = @p1`
	}
	if dialect == testutil.DialectMySQL {
		return `UPDATE workflow_instances SET event_count = 0 WHERE id = ?`
	}
	return `UPDATE workflow_instances SET event_count = 0 WHERE id = $1`
}
