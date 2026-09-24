package engine

// cleat#2239, gap #2 from review: this PR's original body claimed the
// EXISTS-check/FK-check race "not reproduced under a live race on any
// dialect (the window is a few microseconds within one statement)". That
// was wrong, and measured wrong: cleat-review ran 2000 tight-loop concurrent
// pairs against PostgreSQL and got the FK-violation path (SQLSTATE 23503)
// 384 times out of 386 not-found outcomes -- not a rare accident, close to
// deterministic under load.
//
// This test does not rely on load to hit the window; it FORCES it, on all
// three dialects:
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
// FALSIFIED PER DIALECT, and the three dialects do NOT agree on the
// mechanism -- worth recording because it means the FK-violation classifiers
// (isSignalsWorkflowFKViolationPG / isSignalsWorkflowFKViolation /
// isMSSQLSignalsWorkflowFKViolation) are not equally load-bearing for THIS
// interleave:
//
//   - PostgreSQL: forcing isSignalsWorkflowFKViolationPG to always return
//     false turns the committed-purge iterations red with the raw driver
//     error (23503) instead of ErrWorkflowNotFound. The FK trigger's row
//     check is what blocks here, exactly as deliverSignalTx's own comment
//     describes ("an AFTER ROW RI trigger's own SELECT ... FOR KEY SHARE"),
//     and the classifier is what turns the resulting error into
//     ErrWorkflowNotFound.
//   - MySQL and SQL Server: the identical falsification (forcing their own
//     classifiers false) does NOT turn this test red. Under this exact
//     interleave, INSERT ... WHERE EXISTS(...) on both blocks at the EXISTS
//     subquery itself rather than at an FK check, so by the time it
//     re-evaluates against the committed purge the predicate is simply
//     false and RowsAffected()==0 -- deliverSignalTx's OTHER not-found
//     branch, never touching the FK-violation classifier at all. Their
//     classifiers still exist for a real, narrower race (a purge landing in
//     the gap between the EXISTS check finishing and the INSERT's own FK
//     check running, rather than overlapping the whole statement the way
//     this held-open purge does), just not one this particular test can
//     force -- so a regression in either classifier would not be caught
//     here. Recorded rather than smoothed over: CLAUDE.md's own rule is
//     that a check which could not have disagreed is not evidence, and
//     these two conclusively could not.
//
// Repeated several times per dialect (not once) because a mechanism that
// merely CAN produce a result is not the same as one that reliably does --
// CLAUDE.md's "a mechanism that can produce a signature is not evidence that
// it did" -- and with a ROLLED-BACK purge as the negative control: if the
// same interleaving delivered the signal successfully every time regardless
// of commit-or-rollback, the blocking wouldn't be doing what this test
// claims it does.

import (
	"context"
	"database/sql"
	"errors"
	"os"
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
			ctx := context.Background()
			testutil.SetupMinimalSchema(t, be.DB, be.Dialect)

			var tenant uuid.UUID
			if be.Dialect == testutil.DialectMySQL {
				// MySQL isolates tenants by physical database (tiers.yaml
				// D1); reuse the pre-existing default tenant rather than
				// minting one nothing can create there. Same reasoning as
				// a_purged_awaiter_unregisters_across_dialects_test.go.
				tenant = uuid.MustParse("00000000-0000-0000-0000-000000000000")
			} else {
				tenant = uuid.New()
			}

			var store WorkflowStore
			// purgerDB is a pool the held-open DELETE runs on. PostgreSQL and
			// MySQL: be.DB directly -- Postgres RLS does not apply to the
			// connecting owner role in this test setup, and MySQL has no RLS
			// at all. SQL Server is different: its security policy applies to
			// EVERY principal (this file's own store comments and
			// CLAUDE.md's "SQL Server is the dialect this check exists for"
			// both say so), so an unscoped pool's DELETE would silently match
			// zero rows -- the store factory's pool is what sets
			// SESSION_CONTEXT('tenant_id') per connection, so the purge has
			// to run on THAT pool to see and delete the row at all.
			var purgerDB *sql.DB
			switch be.Dialect {
			case testutil.DialectPostgres:
				s := NewPostgresStore(be.DB)
				store = s.WithTenant(tenant.String())
				purgerDB = be.DB
			case testutil.DialectMySQL:
				store = NewMySQLStore(be.DB)
				purgerDB = be.DB
			case testutil.DialectMSSQL:
				factory := NewMSSQLStoreFactory(os.Getenv("CLEAT_TEST_MSSQL"))
				s, closer, err := factory.OpenStore(ctx, tenant.String(), "default")
				if err != nil {
					t.Fatalf("OpenStore(%s) on %s: %v", tenant, be.Name, err)
				}
				t.Cleanup(func() { _ = closer.Close() })
				store = s
				purgerDB = s.(*MSSQLStore).db
			default:
				t.Fatalf("unhandled dialect %s", be.Dialect)
			}

			deleteSQL := map[testutil.Dialect]string{
				testutil.DialectPostgres: `DELETE FROM workflow_instances WHERE id = $1`,
				testutil.DialectMySQL:    `DELETE FROM workflow_instances WHERE id = ?`,
				testutil.DialectMSSQL:    `DELETE FROM workflow_instances WHERE id = @p1`,
			}[be.Dialect]

			// race runs one held-open-purge interleave and returns
			// DeliverSignal's outcome. commitPurge=false is the negative
			// control: the delete never lands, so the row is there the whole
			// time and delivery must succeed.
			race := func(t *testing.T, commitPurge bool) error {
				t.Helper()
				defName := "cleat-2239-race-" + uuid.New().String()
				runIDs := seedReadyRuns(t, store, tenant.String(), defName, 1)
				runID := runIDs[0]

				purgeTx, err := purgerDB.BeginTx(ctx, nil)
				if err != nil {
					t.Fatalf("begin purge tx on %s: %v", be.Name, err)
				}
				res, err := purgeTx.ExecContext(ctx, deleteSQL, runID)
				if err != nil {
					_ = purgeTx.Rollback()
					t.Fatalf("purge DELETE on %s: %v", be.Name, err)
				}
				if n, _ := res.RowsAffected(); n != 1 {
					_ = purgeTx.Rollback()
					t.Fatalf("purge DELETE on %s affected %d rows, want 1 -- the held-open "+
						"purge did not find the row it just seeded, so this run proves "+
						"nothing about the race", be.Name, n)
				}

				resultCh := make(chan error, 1)
				go func() {
					resultCh <- store.DeliverSignal(ctx, runID, "approve", "{}")
				}()

				// Give DeliverSignal's INSERT time to reach the FK check and
				// block behind the purge's row lock before we resolve the
				// purge. Not a race with the assertion below: whichever way
				// this sleep lands, the purge transaction is still open when
				// it ends (nothing here can commit or roll it back early),
				// so DeliverSignal is still either blocked or about to be.
				time.Sleep(300 * time.Millisecond)

				if commitPurge {
					err = purgeTx.Commit()
				} else {
					err = purgeTx.Rollback()
				}
				if err != nil {
					t.Fatalf("resolve purge tx on %s (commit=%v): %v", be.Name, commitPurge, err)
				}

				select {
				case deliverErr := <-resultCh:
					return deliverErr
				case <-time.After(15 * time.Second):
					t.Fatalf("DeliverSignal on %s did not return within 15s of the purge "+
						"resolving -- it may not have been blocked on the row lock this test "+
						"depends on", be.Name)
					return nil
				}
			}

			for i := 0; i < deliverSignalRaceIterations; i++ {
				err := race(t, true)
				if !errors.Is(err, ErrWorkflowNotFound) {
					t.Errorf("on %s, iteration %d: DeliverSignal raced a COMMITTED purge and "+
						"returned %v, want ErrWorkflowNotFound", be.Name, i, err)
				}
			}

			// Negative control: same interleave, purge rolled back instead of
			// committed. If this failed too, the test above would not be
			// telling us anything about the commit path specifically.
			if err := race(t, false); err != nil {
				t.Errorf("on %s: purge ROLLED BACK but DeliverSignal still failed: %v -- "+
					"the positive result above may not be about the race at all", be.Name, err)
			}
		})
	}
}
