package engine

// cleat#2239, gap #2 from review: this PR's original body claimed the
// EXISTS-check/FK-check race "not reproduced under a live race on any
// dialect (the window is a few microseconds within one statement)". That
// was wrong, and measured wrong: cleat-review ran 2000 tight-loop concurrent
// pairs against PostgreSQL and got the FK-violation path (SQLSTATE 23503)
// 384 times out of 386 not-found outcomes -- not a rare accident, close to
// deterministic under load.
//
// This test does not rely on load to hit the window; it FORCES it:
//
//  1. Open a transaction that DELETEs the workflow's row and leave it
//     uncommitted (the "held-open purge").
//  2. Start DeliverSignal concurrently. Its EXISTS predicate is documented as
//     a plain, non-locking read, so in principle it could still see the row
//     and proceed to an INSERT whose FK check then blocks and fails once the
//     purge commits -- and that IS what happens on PostgreSQL, falsified
//     below. Either way -- blocked at the EXISTS or blocked at the FK check
//     -- DeliverSignal cannot return before the purge resolves, which is the
//     property this test actually needs: the outcome is decided by whether
//     the purge commits or rolls back, not by scheduling luck.
//  3. Commit the purge (or, for the negative control, roll it back) and read
//     DeliverSignal's result.
//
// FALSIFIED AT EACH DIALECT'S DEFAULT ISOLATION, and the three do NOT agree
// on mechanism there -- worth recording because it means the FK-violation
// classifiers (isSignalsWorkflowFKViolationPG / isSignalsWorkflowFKViolation
// / isMSSQLSignalsWorkflowFKViolation) are not equally load-bearing for THIS
// interleave at default isolation:
//
//   - PostgreSQL: forcing isSignalsWorkflowFKViolationPG to always return
//     false turns the committed-purge iterations red with the raw driver
//     error (23503) instead of ErrWorkflowNotFound. The FK trigger's row
//     check is what blocks here, exactly as deliverSignalTx's own comment
//     describes ("an AFTER ROW RI trigger's own SELECT ... FOR KEY SHARE"),
//     and the classifier is what turns the resulting error into
//     ErrWorkflowNotFound.
//   - MySQL (default REPEATABLE READ) and SQL Server (RCSI off, the plain
//     docker image default): the identical falsification does NOT turn the
//     "mysql"/"mssql" subtests red. Under this exact interleave,
//     INSERT ... WHERE EXISTS(...) on both blocks at the EXISTS subquery
//     itself rather than at an FK check, so by the time it re-evaluates
//     against the committed purge the predicate is simply false and
//     RowsAffected()==0 -- deliverSignalTx's OTHER not-found branch, never
//     touching the FK-violation classifier at all.
//
// THAT IS SPECIFIC TO DEFAULT ISOLATION, though, not to the dialects --
// found by cleat-review, who re-ran the same interleave at the isolation
// level docs/reference/database-backends.md actually recommends for each
// (`transaction_isolation = READ-COMMITTED` for MySQL, RCSI on for SQL
// Server) and got the FK-violation classifier firing 3/3 on both. The
// "mysql_read_committed" and "mssql_rcsi_on" subtests below force that
// configuration the same way plugins/webhookingest and plugins/notifications
// already do for their own delete-race guards (#2242, #2251) -- so a
// regression in either classifier IS caught, just not by the default-
// isolation subtests, whose job is to show the interleave is safe there too
// (via the EXISTS predicate) rather than to exercise the classifier.
//
// Repeated several times per configuration (not once) because a mechanism
// that merely CAN produce a result is not the same as one that reliably does
// -- CLAUDE.md's "a mechanism that can produce a signature is not evidence
// that it did" -- and with a ROLLED-BACK purge as the negative control: if
// the same interleaving delivered the signal successfully every time
// regardless of commit-or-rollback, the blocking wouldn't be doing what this
// test claims it does.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine/testutil"
)

// deliverSignalRaceIterations is deliberately small. The mechanism is a lock
// wait, not a timing coincidence -- if it were flaky at 5, it would still be
// flaky at 2000, just less obviously so per cleat-review's own 384/386.
const deliverSignalRaceIterations = 5

func TestDeliverSignalFKRaceIsDeterministicAcrossDialects(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			testutil.SetupMinimalSchema(t, be.DB, be.Dialect)
			store, purgerDB, tenant := buildDeliverSignalRaceStore(t, be.Dialect, be.DB, os.Getenv("CLEAT_TEST_MSSQL"))
			runDeliverSignalRaceSuite(t, be.Name, be.Dialect, store, purgerDB, tenant)
		})
	}

	t.Run("mysql_read_committed", func(t *testing.T) {
		if os.Getenv("CLEAT_TEST_MYSQL") == "" {
			t.Skip("CLEAT_TEST_MYSQL not set, skipping MySQL tests")
		}
		db := mysqlReadCommittedTestDB(t)
		testutil.SetupMinimalSchema(t, db, testutil.DialectMySQL)
		store, purgerDB, tenant := buildDeliverSignalRaceStore(t, testutil.DialectMySQL, db, "")
		runDeliverSignalRaceSuite(t, "mysql_read_committed", testutil.DialectMySQL, store, purgerDB, tenant)
	})

	t.Run("mssql_rcsi_on", func(t *testing.T) {
		if os.Getenv("CLEAT_TEST_MSSQL") == "" {
			t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
		}
		connStr := mssqlPrivateRCSIConnStr(t)
		store, purgerDB, tenant := buildDeliverSignalRaceStore(t, testutil.DialectMSSQL, nil, connStr)
		runDeliverSignalRaceSuite(t, "mssql_rcsi_on", testutil.DialectMSSQL, store, purgerDB, tenant)
	})
}

// buildDeliverSignalRaceStore returns a tenant-scoped store plus the pool the
// held-open purge should run on. For PostgreSQL and MySQL that is db itself
// -- PostgreSQL RLS does not apply to the connecting owner role in this test
// setup, and MySQL has no RLS at all. For SQL Server, db is ignored (pass
// nil) and mssqlConnStr is opened through NewMSSQLStoreFactory instead: its
// security policy applies to EVERY principal (this file's own store
// comments, and CLAUDE.md's "SQL Server is the dialect this check exists
// for"), so an unscoped pool's DELETE would silently match zero rows -- the
// factory's pool is what sets SESSION_CONTEXT('tenant_id') per connection,
// so the purge has to run on THAT pool to see and delete the row at all.
func buildDeliverSignalRaceStore(t *testing.T, dialect testutil.Dialect, db *sql.DB, mssqlConnStr string) (store WorkflowStore, purgerDB *sql.DB, tenant uuid.UUID) {
	t.Helper()
	if dialect == testutil.DialectMySQL {
		// MySQL isolates tenants by physical database (tiers.yaml D1); reuse
		// the pre-existing default tenant rather than minting one nothing
		// can create there. Same reasoning as
		// a_purged_awaiter_unregisters_across_dialects_test.go.
		tenant = uuid.MustParse("00000000-0000-0000-0000-000000000000")
	} else {
		tenant = uuid.New()
	}

	switch dialect {
	case testutil.DialectPostgres:
		s := NewPostgresStore(db)
		return s.WithTenant(tenant.String()), db, tenant
	case testutil.DialectMySQL:
		return NewMySQLStore(db), db, tenant
	case testutil.DialectMSSQL:
		factory := NewMSSQLStoreFactory(mssqlConnStr)
		s, closer, err := factory.OpenStore(context.Background(), tenant.String(), "default")
		if err != nil {
			t.Fatalf("OpenStore(%s): %v", tenant, err)
		}
		t.Cleanup(func() { _ = closer.Close() })
		return s, s.(*MSSQLStore).db, tenant
	default:
		t.Fatalf("unhandled dialect %s", dialect)
		return nil, nil, uuid.UUID{}
	}
}

func runDeliverSignalRaceSuite(t *testing.T, name string, dialect testutil.Dialect, store WorkflowStore, purgerDB *sql.DB, tenant uuid.UUID) {
	t.Helper()
	ctx := context.Background()

	deleteSQL := map[testutil.Dialect]string{
		testutil.DialectPostgres: `DELETE FROM workflow_instances WHERE id = $1`,
		testutil.DialectMySQL:    `DELETE FROM workflow_instances WHERE id = ?`,
		testutil.DialectMSSQL:    `DELETE FROM workflow_instances WHERE id = @p1`,
	}[dialect]

	// race runs one held-open-purge interleave and returns DeliverSignal's
	// outcome. commitPurge=false is the negative control: the delete never
	// lands, so the row is there the whole time and delivery must succeed.
	race := func(t *testing.T, commitPurge bool) error {
		t.Helper()
		defName := "cleat-2239-race-" + uuid.New().String()
		runIDs := seedReadyRuns(t, store, tenant.String(), defName, 1)
		runID := runIDs[0]

		purgeTx, err := purgerDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin purge tx on %s: %v", name, err)
		}
		res, err := purgeTx.ExecContext(ctx, deleteSQL, runID)
		if err != nil {
			_ = purgeTx.Rollback()
			t.Fatalf("purge DELETE on %s: %v", name, err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			_ = purgeTx.Rollback()
			t.Fatalf("purge DELETE on %s affected %d rows, want 1 -- the held-open "+
				"purge did not find the row it just seeded, so this run proves "+
				"nothing about the race", name, n)
		}

		resultCh := make(chan error, 1)
		go func() {
			resultCh <- store.DeliverSignal(ctx, runID, "approve", "{}")
		}()

		// Give DeliverSignal's INSERT time to reach the FK check (or the
		// EXISTS subquery, at default isolation) and block behind the
		// purge's row lock before we resolve the purge. Not a race with the
		// assertion below: whichever way this sleep lands, the purge
		// transaction is still open when it ends (nothing here can commit
		// or roll it back early), so DeliverSignal is still either blocked
		// or about to be.
		time.Sleep(300 * time.Millisecond)

		if commitPurge {
			err = purgeTx.Commit()
		} else {
			err = purgeTx.Rollback()
		}
		if err != nil {
			t.Fatalf("resolve purge tx on %s (commit=%v): %v", name, commitPurge, err)
		}

		select {
		case deliverErr := <-resultCh:
			return deliverErr
		case <-time.After(15 * time.Second):
			t.Fatalf("DeliverSignal on %s did not return within 15s of the purge "+
				"resolving -- it may not have been blocked on the row lock this test "+
				"depends on", name)
			return nil
		}
	}

	for i := 0; i < deliverSignalRaceIterations; i++ {
		err := race(t, true)
		if !errors.Is(err, ErrWorkflowNotFound) {
			t.Errorf("on %s, iteration %d: DeliverSignal raced a COMMITTED purge and "+
				"returned %v, want ErrWorkflowNotFound", name, i, err)
		}
	}

	// Negative control: same interleave, purge rolled back instead of
	// committed. If this failed too, the test above would not be telling us
	// anything about the commit path specifically.
	if err := race(t, false); err != nil {
		t.Errorf("on %s: purge ROLLED BACK but DeliverSignal still failed: %v -- "+
			"the positive result above may not be about the race at all", name, err)
	}
}

// mysqlReadCommittedTestDB opens CLEAT_TEST_MYSQL with transaction_isolation
// forced to READ-COMMITTED, the isolation level
// docs/reference/database-backends.md actually recommends for this dialect.
// Same helper, same reasoning, as plugins/webhookingest's and
// plugins/notifications's own copies (#2242, #2251) -- duplicated rather
// than shared because it is package-local test infrastructure in each.
//
// The DSN param, not a SET SESSION after connect: the pool may hand this
// test's two concurrent statements (the purge and the racing signal)
// different pooled connections, and a SET SESSION issued on only one would
// leave the other at the default isolation with no visible error --
// silently testing the wrong thing. The DSN param applies to every
// connection the pool opens.
func mysqlReadCommittedTestDB(t *testing.T) *sql.DB {
	t.Helper()
	base := os.Getenv("CLEAT_TEST_MYSQL")
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	dsn := base + sep + "transaction_isolation=%27READ-COMMITTED%27"
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open MySQL at READ COMMITTED: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping MySQL at READ COMMITTED: %v", err)
	}

	var iso string
	if err := db.QueryRow("SELECT @@transaction_isolation").Scan(&iso); err != nil {
		t.Fatalf("read @@transaction_isolation: %v", err)
	}
	if iso != "READ-COMMITTED" {
		t.Fatalf("precondition: @@transaction_isolation reads %q, want READ-COMMITTED -- "+
			"mysqlReadCommittedTestDB's own setup is wrong, not the tree", iso)
	}
	return db
}

// mssqlPrivateRCSIConnStr creates a private database with
// READ_COMMITTED_SNAPSHOT ON and returns a connection string pointing at it,
// for NewMSSQLStoreFactory to open its own pool against -- the store needs a
// connection STRING, not an already-open *sql.DB, since the factory manages
// SESSION_CONTEXT per connection itself. Same setup as
// plugins/webhookingest's webhookingestPrivateRCSIDatabase (#2242), adapted
// to return the DSN rather than an open pool, and to its own database name
// so the two cannot collide if both happen to run against the same server.
func mssqlPrivateRCSIConnStr(t *testing.T) string {
	t.Helper()
	ctx := context.Background()

	base := os.Getenv("CLEAT_TEST_MSSQL")
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse CLEAT_TEST_MSSQL: %v", err)
	}
	adminDB, err := sql.Open("sqlserver", base)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer adminDB.Close()

	const dbName = "cleat_test_2239_rcsi"
	drop := fmt.Sprintf(
		"IF DB_ID('%s') IS NOT NULL BEGIN ALTER DATABASE [%s] SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE [%s]; END",
		dbName, dbName, dbName)
	if _, err := adminDB.ExecContext(ctx, drop); err != nil {
		t.Fatalf("drop any %s left by a prior interrupted run: %v", dbName, err)
	}
	if _, err := adminDB.ExecContext(ctx, "CREATE DATABASE ["+dbName+"]"); err != nil {
		t.Fatalf("create %s: %v", dbName, err)
	}
	t.Cleanup(func() {
		cleanupDB, err := sql.Open("sqlserver", base)
		if err != nil {
			t.Logf("cleanup: reopen admin connection: %v", err)
			return
		}
		defer cleanupDB.Close()
		if _, err := cleanupDB.ExecContext(context.Background(), drop); err != nil {
			t.Logf("cleanup: drop %s: %v", dbName, err)
		}
	})

	// Set before anything else connects to dbName, so there is nothing
	// else's session to roll back.
	if _, err := adminDB.ExecContext(ctx,
		fmt.Sprintf("ALTER DATABASE [%s] SET READ_COMMITTED_SNAPSHOT ON WITH ROLLBACK IMMEDIATE", dbName)); err != nil {
		t.Fatalf("set READ_COMMITTED_SNAPSHOT on %s: %v", dbName, err)
	}

	q := u.Query()
	q.Set("database", dbName)
	u.RawQuery = q.Encode()
	dsn := u.String()

	verifyDB, err := sql.Open("sqlserver", dsn)
	if err != nil {
		t.Fatalf("open %s: %v", dbName, err)
	}
	defer verifyDB.Close()
	if err := verifyDB.Ping(); err != nil {
		t.Fatalf("ping %s: %v", dbName, err)
	}

	var rcsi bool
	if err := verifyDB.QueryRowContext(ctx,
		`SELECT is_read_committed_snapshot_on FROM sys.databases WHERE name = DB_NAME()`,
	).Scan(&rcsi); err != nil {
		t.Fatalf("read is_read_committed_snapshot_on: %v", err)
	}
	if !rcsi {
		t.Fatalf("precondition: %s reads READ_COMMITTED_SNAPSHOT=false, want true -- "+
			"mssqlPrivateRCSIConnStr's own setup is wrong, not the tree", dbName)
	}

	testutil.SetupMSSQLFullSchema(t, verifyDB)
	return dsn
}
