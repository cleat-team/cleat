package engine

// Real-database reproduction of the IMPROVEMENT-PLAN.md 1.1/1.2
// "zombie writer" bug: a worker that stalls long enough to be reaped and
// have its workflow reclaimed by another worker must not be able to
// corrupt the new owner's event history or the parent's await_child event
// when it finally wakes up and calls back into the store with its now-stale
// (worker_id, generation).
//
// This exercises the real finalize_workflow_status stored procedure/
// function (installed via applyPostgresProcedures / applyMySQLProcedures /
// applyMSSQLProcedures in store_backends_procedures_test.go, reading the
// actual migrations/<dialect>/003_procedures.sql + 004_*.sql files from
// disk) rather than a hand-rolled substitute, so it needs a real database
// and is skipped when none is configured -- see registeredBackends /
// StoreBackend.Setup in store_backends_test.go:
//   - Postgres: CLEAT_TEST_POSTGRES or CLEAT_TEST_DB (falls back to
//     postgres://localhost:5432/cleat?sslmode=disable); also skipped
//     outright in `go test -short`.
//   - MySQL: CLEAT_TEST_MYSQL
//   - MSSQL: CLEAT_TEST_MSSQL

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// requireBackendReachable turns "explicitly configured but unreachable" into
// a hard test failure instead of the silent skip that testutil.TestDB's
// Postgres path currently produces.
//
// testutil.MySQLTestDB and testutil.MSSQLTestDB already t.Fatalf when their
// respective env var (CLEAT_TEST_MYSQL / CLEAT_TEST_MSSQL) is set but the
// database doesn't answer a ping -- see engine/testutil/mysql_schema.go and
// mssql_schema.go. testutil.TestDB's Postgres path does not: it calls
// t.Skipf on a ping failure unconditionally, regardless of whether the DSN
// came from an explicit CLEAT_TEST_POSTGRES/CLEAT_TEST_DB or its hardcoded
// localhost fallback (engine/testutil/schema.go). That means a Postgres
// container that stops between runs -- exactly what happened during this
// investigation -- produces a quiet "ok" instead of a failure, because every
// subtest that depends on it just skips.
//
// This preflights the same env vars backend.Setup ultimately consults and
// fails loudly, before backend.Setup gets a chance to swallow the same
// problem as a skip. It is a no-op (defers entirely to backend.Setup's own
// skip-if-unconfigured behavior) when the relevant env var isn't set at all.
func requireBackendReachable(t *testing.T, backendName string) {
	t.Helper()

	var envVars []string
	var driverName string
	switch backendName {
	case "postgres":
		envVars = []string{"CLEAT_TEST_POSTGRES", "CLEAT_TEST_DB"}
		driverName = "postgres"
	case "mysql":
		envVars = []string{"CLEAT_TEST_MYSQL"}
		driverName = "mysql"
	case "mssql":
		envVars = []string{"CLEAT_TEST_MSSQL"}
		driverName = "sqlserver"
	default:
		return
	}

	var envVar, dsn string
	for _, e := range envVars {
		if v := os.Getenv(e); v != "" {
			envVar, dsn = e, v
			break
		}
	}
	if dsn == "" {
		// Not explicitly configured -- an absent database is legitimately a
		// skip, not a failure.
		return
	}

	db, err := sql.Open(driverName, dsn)
	if err != nil {
		t.Fatalf("%s is set to %q but sql.Open failed: %v -- an explicitly configured but broken test database must FAIL, not skip", envVar, dsn, err)
	}
	defer db.Close()
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("%s is set to %q but the database is unreachable: %v -- an explicitly configured but unreachable test database must FAIL, not skip", envVar, dsn, err)
	}
}

// zombieWriterScenario is the set of IDs and fence values produced by
// buildZombieWriterScenario, shared by TestFinalizeWorkflowSegment_ZombieWriterFence
// (which exercises the Go wrapper, store.FinalizeWorkflowSegment) and
// TestFinalizeWorkflowStatus_SQLFenceGuard (which exercises the underlying
// finalize_workflow_status stored procedure directly).
type zombieWriterScenario struct {
	parentID        string
	childID         string
	staleWorkerID   string // "worker-A"
	staleGeneration int64  // A's generation, captured before it stalled and never refreshed
	liveWorkerID    string // "worker-B"
	liveGeneration  int64  // B's generation after reclaiming the reaped child
}

// buildZombieWriterScenario reproduces the IMPROVEMENT-PLAN.md 1.1/1.2
// "zombie writer" setup: a parent workflow suspended awaiting a child, a
// worker A that claims the child and then stalls long enough to be reaped,
// and a worker B that reclaims the child and makes real (live) progress on
// it. The returned staleWorkerID/staleGeneration identify A's now-invalid
// fence; callers use it to prove that a stale writer calling back in after
// being reaped cannot corrupt B's work.
func buildZombieWriterScenario(t *testing.T, ctx context.Context, store WorkflowStore) zombieWriterScenario {
	t.Helper()

	// ---- Set up a parent workflow suspended awaiting a child ----

	parentID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "zombie-parent", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun (parent): %v", err)
	}
	parentWF, err := store.ClaimWorkflow(ctx, "worker-parent")
	if err != nil || parentWF == nil {
		t.Fatalf("ClaimWorkflow (parent): wf=%v err=%v", parentWF, err)
	}

	childID, err := store.StartChildWorkflow(ctx, parentID, "test-workflow", `{}`, 1, "abandon", 0)
	if err != nil {
		t.Fatalf("StartChildWorkflow: %v", err)
	}

	// Suspend the parent with an await_child event referencing the
	// child's run ID and an empty response -- this is exactly what
	// finalize_workflow_status looks for when injecting a child's
	// result into its parent.
	awaitEvent := EventRecord{Step: 0, EventType: "await_child", ChildName: "test-workflow", RunID: childID}
	farFuture := time.Now().Add(1 * time.Hour)
	if err := store.FinalizeWorkflowSegment(ctx, parentID, "worker-parent", parentWF.Generation,
		[]EventRecord{awaitEvent}, "ready", "", "", "", nil, farFuture); err != nil {
		t.Fatalf("FinalizeWorkflowSegment (parent suspend): %v", err)
	}

	// ---- Worker A claims the child and then stalls ----

	wfA, err := store.ClaimWorkflow(ctx, "worker-A")
	if err != nil || wfA == nil || wfA.ID != childID {
		t.Fatalf("ClaimWorkflow (A): wf=%v err=%v", wfA, err)
	}
	staleGeneration := wfA.Generation // captured before A stalls; never refreshed

	// A never calls back. The reaper reclaims the child (status ->
	// ready, assigned_to -> NULL, generation bumped per CLEAT-1.4)
	// -- a 1ms timeout is enough since A's heartbeat was just set.
	time.Sleep(2 * time.Millisecond)
	reaped, err := store.ReapStaleInstances(ctx, 1*time.Millisecond)
	if err != nil {
		t.Fatalf("ReapStaleInstances: %v", err)
	}
	if reaped < 1 {
		t.Fatalf("ReapStaleInstances reclaimed %d instances, want >= 1 (the stalled child)", reaped)
	}

	// ---- Worker B claims the reclaimed child and makes real progress ----

	wfB, err := store.ClaimWorkflow(ctx, "worker-B")
	if err != nil || wfB == nil || wfB.ID != childID {
		t.Fatalf("ClaimWorkflow (B): wf=%v err=%v", wfB, err)
	}
	if wfB.Generation == staleGeneration {
		t.Fatalf("worker B's generation (%d) equals A's stale generation (%d) -- test setup did not reproduce the scenario", wfB.Generation, staleGeneration)
	}

	liveEvent := EventRecord{Step: 0, EventType: EventTypeCall, Service: "real", Op: "b-in-progress", Response: "live-response-from-B"}
	bWake := time.Now().Add(1 * time.Hour)
	if err := store.FinalizeWorkflowSegment(ctx, childID, "worker-B", wfB.Generation,
		[]EventRecord{liveEvent}, "ready", "", "", "", nil, bWake); err != nil {
		t.Fatalf("FinalizeWorkflowSegment (B live progress): %v", err)
	}

	before, err := store.LoadEventHistory(ctx, childID)
	if err != nil {
		t.Fatalf("LoadEventHistory (before A's stale call): %v", err)
	}
	if len(before) != 1 || before[0].Response != "live-response-from-B" {
		t.Fatalf("unexpected event history before A's stale call: %+v", before)
	}

	return zombieWriterScenario{
		parentID:        parentID,
		childID:         childID,
		staleWorkerID:   "worker-A",
		staleGeneration: staleGeneration,
		liveWorkerID:    "worker-B",
		liveGeneration:  wfB.Generation,
	}
}

func TestFinalizeWorkflowSegment_ZombieWriterFence(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			requireBackendReachable(t, backend.Name())
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			scenario := buildZombieWriterScenario(t, ctx, store)
			parentID, childID := scenario.parentID, scenario.childID
			staleGeneration := scenario.staleGeneration

			// ---- A wakes up and tries to finalize the child as done, using its stale fence ----

			err := store.FinalizeWorkflowSegment(ctx, childID, "worker-A", staleGeneration,
				nil, "done", `{"stale":"result-from-A"}`, "", "", nil, time.Time{})
			if !errors.Is(err, ErrFenceLost) {
				t.Fatalf("A's stale FinalizeWorkflowSegment: err = %v, want ErrFenceLost", err)
			}

			// ---- The whole point: B's event history must survive untouched ----

			after, err := store.LoadEventHistory(ctx, childID)
			if err != nil {
				t.Fatalf("LoadEventHistory (after A's stale call): %v", err)
			}
			if len(after) != 1 || after[0].Response != "live-response-from-B" {
				t.Fatalf("event history was corrupted by the stale writer: got %+v, want B's single live event untouched", after)
			}

			// The child's ownership/status must still reflect B, not have
			// been reset (or worse, finalized) by A.
			childAfter, err := store.GetWorkflowByID(ctx, childID)
			if err != nil {
				t.Fatalf("GetWorkflowByID (child): %v", err)
			}
			if childAfter.Status != "ready" {
				t.Errorf("child status = %q, want %q (untouched by A's stale call)", childAfter.Status, "ready")
			}
			if childAfter.Generation != scenario.liveGeneration {
				t.Errorf("child generation = %d, want %d (B's, untouched by A)", childAfter.Generation, scenario.liveGeneration)
			}

			// The parent's await_child event must not have been injected
			// with A's stale result.
			parentEvents, err := store.LoadEventHistory(ctx, parentID)
			if err != nil {
				t.Fatalf("LoadEventHistory (parent): %v", err)
			}
			for _, ev := range parentEvents {
				if ev.EventType == "await_child" && ev.RunID == childID && strings.Contains(ev.Response, "stale") {
					t.Fatalf("parent's await_child event was corrupted with A's stale result: %+v", ev)
				}
			}
		})
	}
}

// TestFinalizeWorkflowStatus_SQLFenceGuard exercises the finalize_workflow_status
// stored PROCEDURE ITSELF -- not store.FinalizeWorkflowSegment -- with a plain
// *sql.DB call that commits normally, with no enclosing transaction to roll
// back.
//
// Why this test exists, separate from TestFinalizeWorkflowSegment_ZombieWriterFence
// above: PostgresStore.FinalizeWorkflowSegment (engine/store_lifecycle.go)
// calls finalize_workflow_status inside its own transaction, and on a lost
// fence returns ErrFenceLost *before* tx.Commit(). The deferred tx.Rollback()
// then discards everything the procedure did in that transaction --
// including its unconditional DELETE FROM event_history -- regardless of
// whether the SQL-level guard
// (`IF v_rows_updated > 0 AND (p_final_status = 'done' OR ...)` in
// migrations/postgres/004_fix_finalize_workflow_status_fence.sql) is present
// or not. That's good defence in depth in production, but it means the
// existing zombie-writer test above cannot actually tell whether the SQL
// guard is doing anything: delete the `v_rows_updated > 0` condition from
// all three migrations and that test still passes, because the Go-level
// rollback silently covers for the missing guard.
//
// This test calls the function directly against a plain *sql.DB (no
// transaction, no rollback) so that the SQL guard itself -- not the Go
// wrapper's transaction discipline -- is what's under test. A lost fence
// here can only be caught by the stored procedure's own IF, because there is
// no Go-level safety net to fall back on.
func TestFinalizeWorkflowStatus_SQLFenceGuard(t *testing.T) {
	requireBackendReachable(t, "postgres")

	backend := &PostgresBackend{}
	store, teardown := backend.Setup(t)
	defer teardown()
	setupTestData(t, store)
	truncateAll(t, store)
	ctx := context.Background()

	pgStore, ok := store.(*PostgresStore)
	if !ok {
		t.Fatalf("expected *PostgresStore, got %T", store)
	}

	scenario := buildZombieWriterScenario(t, ctx, store)

	// ---- Call finalize_workflow_status directly: worker A's stale fence,
	// plain *sql.DB, no enclosing transaction, no rollback available to
	// cover for a missing SQL guard. ----

	var fenceHeld bool
	err := pgStore.db.QueryRowContext(ctx, `
		SELECT finalize_workflow_status($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`,
		scenario.childID,
		scenario.staleWorkerID,
		scenario.staleGeneration,
		"done",
		`{"stale":"result-from-A-direct-sql"}`,
		"",   // p_error_code
		"",   // p_error_op
		"{}", // p_query_state
		time.Time{},
		"", // p_notify_channel
	).Scan(&fenceHeld)
	if err != nil {
		t.Fatalf("direct finalize_workflow_status call: %v", err)
	}
	if fenceHeld {
		t.Fatalf("finalize_workflow_status($4=%q stale worker/generation) returned fence held = true, want false -- the SQL-level generation fence did not hold", scenario.staleWorkerID)
	}

	// ---- event_history must be completely untouched: no DELETE ran, because
	// the terminal side-effect block must not have executed for a caller
	// whose fenced UPDATE matched zero rows. ----

	after, err := store.LoadEventHistory(ctx, scenario.childID)
	if err != nil {
		t.Fatalf("LoadEventHistory (after A's direct stale call): %v", err)
	}
	if len(after) != 1 || after[0].Response != "live-response-from-B" {
		t.Fatalf("event_history was corrupted by the stale writer's direct SQL call: got %+v, want B's single live event untouched", after)
	}

	// ---- The child's ownership/status must still reflect B. ----

	childAfter, err := store.GetWorkflowByID(ctx, scenario.childID)
	if err != nil {
		t.Fatalf("GetWorkflowByID (child): %v", err)
	}
	if childAfter.Status != "ready" {
		t.Errorf("child status = %q, want %q (untouched by A's direct stale call)", childAfter.Status, "ready")
	}
	if childAfter.Generation != scenario.liveGeneration {
		t.Errorf("child generation = %d, want %d (B's, untouched by A)", childAfter.Generation, scenario.liveGeneration)
	}

	// ---- The parent's await_child event must not have been injected with
	// A's stale result. ----

	parentEvents, err := store.LoadEventHistory(ctx, scenario.parentID)
	if err != nil {
		t.Fatalf("LoadEventHistory (parent): %v", err)
	}
	for _, ev := range parentEvents {
		if ev.EventType == "await_child" && ev.RunID == scenario.childID && strings.Contains(ev.Response, "stale") {
			t.Fatalf("parent's await_child event was corrupted by the stale writer's direct SQL call: %+v", ev)
		}
	}
}
