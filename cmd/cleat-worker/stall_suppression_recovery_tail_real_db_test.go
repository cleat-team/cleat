package main

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

// TestReapOnceRecoveryTailAgainstARealDatabase is the real-database
// reproduction of cleat-review's round-2 finding on cleat#2006 (2026-09-24,
// the "S3" case): the mock-driven tests in suspected_db_stall_test.go
// exercise stallSuppressionEpisode.evaluate directly with hand-built
// shapes, which is precise but proves nothing about whether a real
// dialect's StaleSetShape actually produces NoRecentHeartbeat=false while
// Stale stays nonzero the moment one worker's heartbeat lands -- the exact
// transition this fix depends on. Mocks already missed one real-SQL-only
// bug on this PR (the MSSQL RLS session-context gap); this is the same
// class of gap, on the read side that feeds the decision rather than the
// write side that missed session context.
//
// Three real rows, three distinct workers, all aged well past both
// reclaimAfter and missedBeatThreshold. Tick 1 suppresses (episode opens).
// Then only worker-a's heartbeat_at is refreshed -- b and c are left
// exactly as stale as before. Tick 2, run immediately after (well inside
// reclaimAfter), must still find all three rows 'running': pre-fix, this
// tick's shape reads NoRecentHeartbeat=false (a is fresh) with Stale=2 (b
// and c still are), which was the zero decision -- unsuppressed -- and
// ReapStaleInstances took b and c on a's heartbeat alone.
func TestReapOnceRecoveryTailAgainstARealDatabase(t *testing.T) {
	if os.Getenv("CLEAT_TEST_POSTGRES") == "" && os.Getenv("CLEAT_TEST_DB") == "" {
		t.Skip("CLEAT_TEST_POSTGRES not set, skipping the recovery-tail reproduction")
	}
	if testing.Short() {
		t.Skip("Skipping a real recovery-tail reproduction in short mode")
	}

	db := testutil.SuiteTestDB(t, "cleat_worker_recoverytail")
	ctx := context.Background()
	store := engine.NewPostgresStore(db)

	const defName = "recovery-tail-def"
	if err := store.DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	// reclaimAfter (3s) and missedBeatThreshold(1s) == 1s+2s(floor)+1s == 4s
	// are both comfortably under the 20s a row is seeded stale by, and
	// comfortably over the near-instant real wall-clock gap between the two
	// reapOnce calls below -- so no sleep is needed between ticks for the
	// suppression window itself, only real SQL round trips.
	const reclaimAfter = 3 * time.Second
	const heartbeat = 1 * time.Second

	ids := make([]string, 3)
	workers := []string{"recovery-tail-a", "recovery-tail-b", "recovery-tail-c"}
	for i, worker := range workers {
		ids[i] = fmt.Sprintf("recovery-tail-%d-%d", time.Now().UnixNano(), i)
		if _, err := db.ExecContext(ctx, `
			INSERT INTO workflow_instances (id, def_name, def_version, status, input,
			                                assigned_to, heartbeat_at, tenant_id)
			VALUES ($1, $2, 1, 'running', '{}', $3, now() - interval '20 seconds', $4)`,
			ids[i], defName, worker, engine.DefaultTenantUUID); err != nil {
			t.Fatalf("seeding row %d (%s): %v", i, worker, err)
		}
	}
	t.Cleanup(func() {
		for _, id := range ids {
			db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, id)
		}
	})

	w := newRealBackgroundLoopWorker(t, db, store, "recovery-tail-reaper")
	w.heartbeatInterval = heartbeat
	w.reclaimTimeout = reclaimAfter
	// Without this, reapingIsSafe's #2166 grace-period gate -- keyed on
	// lastDBTrouble being at least reclaimAfter in the past -- refuses
	// every tick in this test on its own, before reapOnce ever reaches the
	// suspected-stall logic under test. newRealBackgroundLoopWorker seeds
	// lastDBTrouble to "now" (a fresh worker), which is exactly what that
	// gate treats as recent trouble.
	seedRecentlyConfirmedHealthy(w)

	statusOf := func(id string) string {
		t.Helper()
		var status string
		if err := db.QueryRowContext(ctx, `SELECT status FROM workflow_instances WHERE id = $1`, id).Scan(&status); err != nil {
			t.Fatalf("reading status of %s: %v", id, err)
		}
		return status
	}

	// Tick 1: all three rows are 20s stale -- suspected, episode opens,
	// suppressed. Nothing should be reclaimed yet.
	w.reapOnce()
	for i, id := range ids {
		if got := statusOf(id); got != "running" {
			t.Fatalf("tick 1: %s status = %q, want running (the opening tick must suppress, not reclaim)", workers[i], got)
		}
	}

	// Only worker-a's heartbeat lands. b and c are untouched -- still 20s
	// stale, exactly as at tick 1.
	if _, err := db.ExecContext(ctx,
		`UPDATE workflow_instances SET heartbeat_at = now() WHERE id = $1`, ids[0]); err != nil {
		t.Fatalf("recovering worker-a's heartbeat: %v", err)
	}

	// Tick 2, the recovery tail: this shape's NoRecentHeartbeat now reads
	// false (a is fresh) while Stale stays 2 (b and c). All three rows must
	// still be running -- the episode from tick 1 must stay sticky rather
	// than releasing b and c on the strength of a's heartbeat alone.
	w.reapOnce()
	for i, id := range ids {
		if got := statusOf(id); got != "running" {
			t.Fatalf("tick 2 (recovery tail, %s): status = %q, want running -- "+
				"one survivor's heartbeat must not reclaim the still-stale laggards", workers[i], got)
		}
	}
}
