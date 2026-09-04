package engine

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// The parent-close TERMINATE arm, as a two-phase transition.
// IMPROVEMENT-PLAN §3.114, the second of §3.75's three transitions.
//
// A closing parent fails its TERMINATE children with a direct UPDATE and then
// releases their resources. Same defect as §3.112's and worse in one respect:
// it is a bulk operation, so one closing parent can pre-empt the cleanup of
// every child at once.

// setWorkflowShape forces a workflow's status and compaction state directly,
// so the predicate test can cover states no store API will produce on demand.
func setWorkflowShape(t *testing.T, store WorkflowStore, wfID, status string, compacted bool) {
	t.Helper()
	comp := any(nil)
	if compacted {
		comp = `{}`
	}
	switch s := store.(type) {
	case *PostgresStore:
		if _, err := s.db.Exec(
			`UPDATE workflow_instances SET status = $1, compaction_state = $2 WHERE id = $3`,
			status, comp, wfID); err != nil {
			t.Fatalf("setWorkflowShape (postgres): %v", err)
		}
	case *MySQLStore:
		if _, err := s.db.Exec(
			`UPDATE workflow_instances SET status = ?, compaction_state = ? WHERE id = ?`,
			status, comp, wfID); err != nil {
			t.Fatalf("setWorkflowShape (mysql): %v", err)
		}
	case *MSSQLStore:
		if _, err := s.db.Exec(
			`UPDATE workflow_instances SET status = @p1, compaction_state = @p2 WHERE id = @p3`,
			status, comp, wfID); err != nil {
			t.Fatalf("setWorkflowShape (mssql): %v", err)
		}
	default:
		t.Fatalf("setWorkflowShape: unknown store type %T", store)
	}
}

// sqlSaysDeferPhaseOwed evaluates deferPhaseOwedSQL against one real row.
func sqlSaysDeferPhaseOwed(t *testing.T, store WorkflowStore, wfID string) bool {
	t.Helper()
	var n int
	q := func(ph string) string {
		return `SELECT COUNT(*) FROM workflow_instances WHERE id = ` + ph + ` AND ` + deferPhaseOwedSQL
	}
	switch s := store.(type) {
	case *PostgresStore:
		if err := s.db.QueryRow(q("$1"), wfID).Scan(&n); err != nil {
			t.Fatalf("sqlSaysDeferPhaseOwed (postgres): %v", err)
		}
	case *MySQLStore:
		if err := s.db.QueryRow(q("?"), wfID).Scan(&n); err != nil {
			t.Fatalf("sqlSaysDeferPhaseOwed (mysql): %v", err)
		}
	case *MSSQLStore:
		if err := s.db.QueryRow(q("@p1"), wfID).Scan(&n); err != nil {
			t.Fatalf("sqlSaysDeferPhaseOwed (mssql): %v", err)
		}
	default:
		t.Fatalf("sqlSaysDeferPhaseOwed: unknown store type %T", store)
	}
	return n == 1
}

// deferPhaseOwed and deferPhaseOwedSQL are two carriers of one rule, and
// nothing structural keeps them saying the same thing.
//
// That is the shape this repo has been bitten by before: compaction had a
// completeness property test and the database payload carrier had none, so
// every payload defect stayed invisible until someone noticed the behaviour it
// broke (§3.98). The single-row paths ask Go; the set-based parent-close arms
// cannot, and ask SQL. This runs both against the same real rows.
//
// It also covers the two answers a reader gets wrong, on the SQL side this
// time: 'terminating' is NOT owed (a child already in its phase is not given
// another), and a COMPACTED workflow with no defer rows IS, because compaction
// pruned the rows the EXISTS looks for.
func TestTheSQLPredicateAgreesWithTheGoOne(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			cases := []struct {
				status    string
				hasDefers bool
				compacted bool
			}{
				{"running", true, false},
				{"ready", true, false},
				{"suspended", true, false},
				{"running", false, false},
				{"ready", false, false},
				{"running", false, true},
				{"ready", true, true},
				{"done", true, false},
				{"failed", true, false},
				{"terminated", true, false},
				{"dead_lettered", true, false},
				{statusTerminating, true, false},
				{"done", false, true},
			}

			agreed, owed := 0, 0
			for i, tc := range cases {
				wfID := startTerminableWorkflow(t, ctx, store,
					fmt.Sprintf("predicate-%d", i), tc.hasDefers)
				setWorkflowShape(t, store, wfID, tc.status, tc.compacted)

				want := deferPhaseOwed(tc.status, tc.hasDefers, tc.compacted)
				got := sqlSaysDeferPhaseOwed(t, store, wfID)
				if got != want {
					t.Errorf("status=%q defers=%v compacted=%v: SQL says %v, Go says %v -- "+
						"the two carriers of this rule have drifted, and the set-based "+
						"parent-close arms use the SQL one",
						tc.status, tc.hasDefers, tc.compacted, got, want)
					continue
				}
				agreed++
				if want {
					owed++
				}
			}

			// Vacuity: agreement is worthless if every answer was the same, or
			// if a broken predicate answered "no" to everything.
			if agreed != len(cases) {
				t.Fatalf("%d of %d cases agreed", agreed, len(cases))
			}
			if owed == 0 || owed == len(cases) {
				t.Fatalf("every case answered the same way (%d owed of %d), so agreement "+
					"says nothing about the predicate", owed, len(cases))
			}
		})
	}
}

// A closing parent gives a child that owes cleanup its defer phase, and does
// NOT release that child's resources on the way past.
//
// The two children are the point: one with defers and one without, closed by
// the same statement pair, ending in different states. A change that sent
// every child down one arm would fail on the other child.
func TestParentCloseGivesAChildWithDefersItsPhase(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			deployConcurrencyTestWorkflows(t, store, "wf-parent-dp", "wf-heir-dp")

			withDefers, err := store.StartChildWorkflow(ctx, "wf-parent-dp",
				"concurrency-test-workflow", `{}`, 1, "TERMINATE", 0)
			if err != nil {
				t.Fatalf("StartChildWorkflow (with defers): %v", err)
			}
			plain, err := store.StartChildWorkflow(ctx, "wf-parent-dp",
				"concurrency-test-workflow", `{}`, 1, "TERMINATE", 0)
			if err != nil {
				t.Fatalf("StartChildWorkflow (plain): %v", err)
			}
			if err := store.AppendEventHistoryBatch(ctx, withDefers, []EventRecord{
				{Step: 0, EventType: EventTypeDefer, DeferID: "defer-0", DeferDescription: "release the lock"},
			}); err != nil {
				t.Fatalf("AppendEventHistoryBatch: %v", err)
			}

			// The child with defers takes the only slot for its key.
			if acquired, err := store.AcquireConcurrencyKey(ctx, "key-child-dp", withDefers, time.Hour); err != nil {
				t.Fatalf("AcquireConcurrencyKey: %v", err)
			} else if !acquired {
				t.Fatal("the first acquire must succeed, or the rest of this test is vacuous")
			}
			if taken, err := store.AcquireConcurrencyKey(ctx, "key-child-dp", "wf-heir-dp", time.Hour); err != nil {
				t.Fatalf("AcquireConcurrencyKey (contended): %v", err)
			} else if taken {
				t.Fatal("a second workflow acquired a held key, so this test cannot tell a " +
					"released slot from one that was never held")
			}

			// Close the parent through a path that enforces the close policy.
			// TerminateWorkflow does not (§3.79 is about the other direction);
			// AdminForceComplete does.
			if err := store.AdminForceComplete(ctx, "wf-parent-dp", 0, `{}`, "test"); err != nil {
				t.Fatalf("AdminForceComplete(parent): %v", err)
			}

			// The two arms partitioned the children.
			if wf := mustGetWorkflow(t, ctx, store, plain); wf.Status != "failed" {
				t.Fatalf("the child with no defers is %q, want \"failed\": the plain arm "+
					"must be unchanged for a child that owes no cleanup", wf.Status)
			}
			if wf := mustGetWorkflow(t, ctx, store, withDefers); wf.Status != statusTerminating {
				t.Fatalf("the child with defers is %q, want %q: a child that owes cleanup "+
					"has to stay claimable long enough to run it", wf.Status, statusTerminating)
			}

			// The ordering property: still held.
			if taken, err := store.AcquireConcurrencyKey(ctx, "key-child-dp", "wf-heir-dp", time.Hour); err != nil {
				t.Fatalf("AcquireConcurrencyKey (during defer phase): %v", err)
			} else if taken {
				t.Fatal("the closing parent released its child's concurrency slot before the " +
					"child's defers had run, which is the pre-emption §3.75 describes")
			}

			// The phase completes like any other.
			claimed := claimByID(t, ctx, store, "worker-parent-close", withDefers)
			if claimed.PendingTerminalStatus != "failed" {
				t.Fatalf("claim carried PendingTerminalStatus %q, want %q -- the outcome the "+
					"close policy recorded, not the one terminate records",
					claimed.PendingTerminalStatus, "failed")
			}
			dps, ok := store.(DeferPhaseStore)
			if !ok {
				t.Fatalf("%T cannot finalize a defer phase, but it marked one", store)
			}
			if err := dps.FinalizeDeferPhase(ctx, withDefers, "worker-parent-close", claimed.Generation, nil); err != nil {
				t.Fatalf("FinalizeDeferPhase: %v", err)
			}
			if wf := mustGetWorkflow(t, ctx, store, withDefers); wf.Status != "failed" {
				t.Fatalf("status = %q after the defer phase, want \"failed\": the close "+
					"policy's outcome is the one that has to be applied", wf.Status)
			}
			if taken, err := store.AcquireConcurrencyKey(ctx, "key-child-dp", "wf-heir-dp", time.Hour); err != nil {
				t.Fatalf("AcquireConcurrencyKey (after finalize): %v", err)
			} else if !taken {
				t.Fatal("the key was never released: a closed child is holding a concurrency " +
					"slot that live workflows queue behind")
			}
		})
	}
}

// A direct terminal UPDATE that lands on a workflow in its defer phase has to
// take the marker with it.
//
// This is a hazard §3.112 opened and did not close, found while building
// §3.114 rather than by looking for it. A marker outlives the row's new status:
// ExpireDeferPhases sweeps on `pending_terminal_status IS NOT NULL AND
// defer_phase_deadline < now()` and applies whatever it finds recorded there.
// So an operator force-completes a workflow that is running its cleanup, sees
// `done`, and five minutes later the deadline sweep applies the OLD outcome
// over it.
//
// The assertion is on the sweep rather than on the column, because the column
// is the implementation and the overwritten outcome is the defect.
func TestForceResolvingADeferPhaseTakesItsMarkerWithIt(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			dps, ok := store.(DeferPhaseStore)
			if !ok {
				t.Fatalf("%T is not a DeferPhaseStore", store)
			}

			wfID := startTerminableWorkflow(t, ctx, store, "force-over-phase", true)
			if err := store.TerminateWorkflow(ctx, wfID, "terminated by test"); err != nil {
				t.Fatalf("TerminateWorkflow: %v", err)
			}
			wf := mustGetWorkflow(t, ctx, store, wfID)
			if wf.Status != statusTerminating {
				t.Fatalf("status = %q, want %q: without a live defer phase this test is vacuous",
					wf.Status, statusTerminating)
			}

			// The operator overrides it.
			if err := store.AdminForceComplete(ctx, wfID, wf.Generation, `{"forced":true}`, "test"); err != nil {
				t.Fatalf("AdminForceComplete: %v", err)
			}
			if got := mustGetWorkflow(t, ctx, store, wfID); got.Status != "done" {
				t.Fatalf("status = %q after a force-complete, want \"done\"", got.Status)
			}

			// Past the deadline the sweep must find nothing to apply.
			setDeferPhaseDeadline(t, store, wfID, 60)
			if n, err := dps.ExpireDeferPhases(ctx); err != nil {
				t.Fatalf("ExpireDeferPhases: %v", err)
			} else if n != 0 {
				t.Fatalf("the deadline sweep found %d phase(s) on a force-resolved workflow", n)
			}
			if got := mustGetWorkflow(t, ctx, store, wfID); got.Status != "done" {
				t.Fatalf("status = %q, want \"done\": the deadline sweep applied the outcome "+
					"recorded before the operator overrode it, so a force-resolve silently "+
					"un-does itself five minutes later", got.Status)
			}
		})
	}
}
