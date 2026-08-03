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
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// requireBackendReachable used to live here: a local "explicitly configured
// but unreachable must Fatal, not skip" preflight for backend.Setup, added
// in f9bce35 because testutil.TestDB's Postgres path unconditionally
// t.Skipf'd on a ping failure even when CLEAT_TEST_POSTGRES/CLEAT_TEST_DB
// was set. That gap was closed centrally in c26c332: TestDB (and
// MySQLTestDB/MSSQLTestDB, which already did this) now Fatal on a
// configured-but-unreachable database for every dialect. All three call
// sites below (registeredBackends' Setup, PostgresBackend.Setup directly,
// MySQLBackend.Setup directly) route through that shared path, so the local
// copy was pure duplication and was removed. See
// engine/testutil/schema.go's TestDB doc comment for the current behavior,
// and schema_bootstrap_test.go's bootstrapScratchDB for the one caller in
// this package that cannot use TestDB and needs the same discrimination
// inlined instead.
//
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

// TestFinalizeWorkflowStatus_SQLFenceGuard_MySQL is the MySQL counterpart of
// TestFinalizeWorkflowStatus_SQLFenceGuard above -- see that test's doc
// comment for why it exists. Before this test was added, MySQL had no
// coverage that could actually detect a missing/broken SQL-level fence
// guard in migrations/mysql/004_fix_finalize_workflow_status_fence.sql:
// TestFinalizeWorkflowSegment_ZombieWriterFence/mysql calls
// finalize_workflow_status through MySQLStore.FinalizeWorkflowSegment
// (engine/mysql_lifecycle.go), which wraps the CALL in its own
// tx.BeginTx/tx.Rollback and returns ErrFenceLost *before* tx.Commit() on a
// lost fence -- the deferred Rollback then discards everything the
// procedure did, including an unconditional DELETE FROM event_history,
// regardless of whether the SQL guard
// (`IF v_rows_updated > 0 AND (p_final_status = 'done' OR ...)`) is present.
// That means the existing zombie-writer test cannot tell whether the SQL
// guard itself does anything on MySQL, same as documented for Postgres.
//
// This test calls the procedure directly against a plain *sql.DB (no
// enclosing transaction, ordinary MySQL autocommit) so the SQL guard is what
// is actually under test, with no Go-level rollback to fall back on.
func TestFinalizeWorkflowStatus_SQLFenceGuard_MySQL(t *testing.T) {
	backend := &MySQLBackend{}
	if !backend.Enabled() {
		t.Skip("CLEAT_TEST_MYSQL not set, skipping MySQL tests")
	}
	store, teardown := backend.Setup(t)
	defer teardown()
	setupTestData(t, store)
	truncateAll(t, store)
	ctx := context.Background()

	mysqlStore, ok := store.(*MySQLStore)
	if !ok {
		t.Fatalf("expected *MySQLStore, got %T", store)
	}

	scenario := buildZombieWriterScenario(t, ctx, store)

	// ---- Call finalize_workflow_status directly: worker A's stale fence,
	// plain *sql.DB, ordinary autocommit, no enclosing transaction and so no
	// rollback available to cover for a missing SQL guard. ----

	var fenceHeld bool
	err := mysqlStore.db.QueryRowContext(ctx, `
		CALL finalize_workflow_status(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		scenario.childID,
		scenario.staleWorkerID,
		scenario.staleGeneration,
		"done",
		`{"stale":"result-from-A-direct-sql"}`,
		"",   // p_error_code
		"",   // p_error_op
		"{}", // p_query_state
		nil,  // p_next_wake_at -- NULL; irrelevant to a "done" call and MySQL
		// rejects the Go zero time.Time{} sentinel under strict sql_mode
		"", // p_notify_channel
	).Scan(&fenceHeld)
	if err != nil {
		t.Fatalf("direct finalize_workflow_status call: %v", err)
	}
	if fenceHeld {
		t.Fatalf("finalize_workflow_status($4=%q stale worker/generation) returned fence held = true, want false -- the SQL-level generation fence did not hold", scenario.staleWorkerID)
	}

	// ---- event_history must be completely untouched: no DELETE ran,
	// because the terminal side-effect block must not have executed for a
	// caller whose fenced UPDATE matched zero rows. ----

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
