package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestAZeroClaimIsAmbiguousWhenRowsAreMerelyLocked pins cleat#923.
//
// # The behaviour, which is intended and is NOT a bug to fix here
//
// ClaimWorkflows selects candidates with FOR UPDATE SKIP LOCKED
// (engine/store_lifecycle.go), so a row locked by another transaction is
// REMOVED from the result set rather than blocking. A zero result therefore
// means either "nothing to run" or "every candidate was locked at that
// instant", and the caller cannot tell the two apart.
//
// cmd/cleat-worker/setup.go acts on the zero: it increments idleTicks and
// sleeps idleTicks*pollInterval, capped at maxIdleTicks = 6. So a worker that
// keeps seeing locked candidates backs off to 6x while runnable work sits
// there. NOTIFY and the parent wake both reset the backoff and neither closes
// this, because both fire on NEW work rather than on existing work becoming
// unlocked.
//
// This test exists to pin that the ambiguity is REACHABLE, not to assert it is
// wrong. The decision (2026-09-07) was to make it visible rather than change
// the behaviour, for the reason the measurement below gives.
//
// # Why no behavioural fix: contention does not produce this
//
// Measured 2026-09-07, two claimers racing with LIMIT 1, sweeping the number of
// runnable rows. Re-derive with the harness this file replaced -- the sweep is
// not kept as a test because 35s of runtime buys a result that cannot change
// while SKIP LOCKED means what it means:
//
//	runnable rows   2     3     4     6    11    41     6 (lock HELD)
//	claims       1565  1604  1613  1711  1884  1631          2184
//	zero+work       0     0     0     0     0     0          2184 (100%)
//	backoff        0x    0x    0x    0x    0x    0x            6x
//
// ~10,000 contended claims, not one zero-with-work. The reason is structural
// rather than statistical: SKIP LOCKED *skips*, so with more candidates than
// lockers it takes the next free row. A zero needs EVERY candidate the inner
// SELECT would take to be locked at once, and a claim transaction is far too
// short to overlap that way. No ratio will produce it.
//
// Nor does anything else in the engine hold such a lock today, which was
// checked by predicate rather than by shape:
//
//   - deleteDeadLetteredWorkflowsBatch and deleteCompletedWorkflowsBatch do
//     `DELETE FROM event_history WHERE workflow_id = ANY($1)`, taking an FK
//     KEY SHARE lock on many parent rows at once -- but on COMPLETED and
//     DEAD-LETTERED workflows, which are not `status IN ('ready','terminating')`
//     and so are not claim candidates. The lock exists and cannot collide,
//     which is a different and stronger statement than "no such lock exists".
//   - CompactHistory holds one row for one short transaction.
//
// So the risk is a future long-running transaction touching workflow_instances,
// which would present as "the queue is slow" with nothing pointing at the
// cause. That is what visibility is for.
//
// # The control is why the zeros above are evidence
//
// ~10,000 zeros are only a finding if the counter can report non-zero. The
// held-lock arm below uses the same assertion path and reports the condition
// every time. Without it, an empty sweep is indistinguishable from an
// instrument that cannot see anything.
func TestAZeroClaimIsAmbiguousWhenRowsAreMerelyLocked(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	testutil.CleanupAllTestData(t, db, testutil.DialectPostgres)
	t.Cleanup(func() { testutil.CleanupAllTestData(t, db, testutil.DialectPostgres) })

	store := NewPostgresStore(db)
	ctx := context.Background()
	setupTestData(t, store)

	const n = 3
	for i := 0; i < n; i++ {
		if _, _, err := store.StartNewRun(ctx, "", "test-workflow", 1,
			json.RawMessage(`{}`), fmt.Sprintf("skiplocked-%d-%d", i, time.Now().UnixNano()),
			DefaultTenantUUID, 0); err != nil {
			t.Fatalf("StartNewRun[%d]: %v", i, err)
		}
	}

	// Baseline: with nothing locked the claim finds the work. Asserted as a
	// floor rather than an equality because setupTestData leaves a runnable row
	// of its own -- an exact count here failed on that and would fail again the
	// next time the fixture gains a row.
	got, err := store.ClaimWorkflows(ctx, "worker-baseline", 10)
	if err != nil {
		t.Fatalf("baseline ClaimWorkflows: %v", err)
	}
	if len(got) < n {
		t.Fatalf("baseline claimed %d, want at least %d -- the fixture is not what this test assumes",
			len(got), n)
	}
	for _, wf := range got {
		if _, err := db.ExecContext(ctx,
			`UPDATE workflow_instances SET status='ready', assigned_to=NULL WHERE id=$1`, wf.ID); err != nil {
			t.Fatalf("resetting %s: %v", wf.ID, err)
		}
	}

	// Hold a lock on every candidate, as a long-running transaction would.
	holder, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("holder conn: %v", err)
	}
	defer holder.Close()
	tx, err := holder.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	defer tx.Rollback()

	// set_config, not SET LOCAL: a bind parameter is not legal in SET LOCAL,
	// and getting that wrong aborts the transaction -- after which the lock
	// below never takes and the test "measures" a zero caused by its own broken
	// fixture. Fatal rather than logged for the same reason.
	if _, err := tx.ExecContext(ctx,
		"SELECT set_config('cleat.tenant_id', $1, true)", DefaultTenantUUID); err != nil {
		t.Fatalf("setting RLS context on the holder: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		SELECT id FROM workflow_instances
		WHERE status IN ('ready','terminating') AND next_wake_at <= now()
		FOR UPDATE`); err != nil {
		t.Fatalf("holder lock: %v", err)
	}

	claimed, err := store.ClaimWorkflows(ctx, "worker-contended", 10)
	if err != nil {
		t.Fatalf("contended ClaimWorkflows: %v", err)
	}

	// Counted with the claim's OWN predicate. The issue's original suggestion
	// omitted task_queue = ANY($2) and the tenant scoping, which over-reports:
	// rows a worker is correctly declining to claim would read as ambiguous
	// zeros. This fixture does not vary task_queue, so status and wake time are
	// the whole predicate here -- but anything acting on this number in
	// production must use all of it.
	var ready int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM workflow_instances
		WHERE status IN ('ready','terminating') AND next_wake_at <= now()`).Scan(&ready); err != nil {
		t.Fatalf("ready count: %v", err)
	}

	t.Logf("claim returned %d row(s); %d row(s) were ready and runnable at the same instant",
		len(claimed), ready)

	if len(claimed) != 0 {
		t.Errorf("the holder's lock did not remove the rows from the claim's result set; claimed %d.\n\n"+
			"Either the lock did not take or SKIP LOCKED left this path, and the premise of "+
			"cleat#923 no longer holds -- which would be worth knowing, but this test can no "+
			"longer measure what it was written for.", len(claimed))
	}
	if ready == 0 {
		t.Fatalf("no rows were ready at the moment of the zero claim, so this measured nothing: " +
			"the zero was CORRECT rather than ambiguous, and the assertion above passed for the " +
			"wrong reason. This is the control -- without it a broken fixture reads as a pass.")
	}
}
