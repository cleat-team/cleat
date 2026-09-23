package engine

import (
	"context"
	"encoding/json"
	"testing"
)

// cleat#2008: a reclaimed run's original execution kept running to
// completion, because BatchHeartbeat stamps rows by worker id with no
// generation check, so a worker that reclaimed and re-claimed its own run
// kept both executions' heartbeats fresh and neither was ever told anything.
// HeartbeatBatchFenced is the fix's known-loss half (decision 1): it returns
// exactly the (run, generation) pairs it did NOT stamp, so the caller can
// stop that execution before it makes another durable call.
//
// heartbeatSentinel is a fixed, deliberately ancient heartbeat_at value
// written directly to both rows before either is heartbeat-fenced. Reading
// whether a row is STILL at the sentinel afterwards is what distinguishes
// "was refreshed" from "was not" -- comparing two now() reads for ordering
// would make this a wall-clock-dependent assertion, which CLAUDE.md asks to
// avoid rather than widen.
const heartbeatSentinel = "2000-01-01T00:00:00Z"

// heartbeatSentinelFor returns heartbeatSentinel in the literal form the
// given dialect's driver accepts -- MySQL's driver rejects the RFC3339 form
// with "Incorrect datetime value", so it gets the DATETIME(6) spelling.
func heartbeatSentinelFor(dialect string) string {
	if dialect == "mysql" {
		return "2000-01-01 00:00:00"
	}
	return heartbeatSentinel
}

// TestHeartbeatBatchFencedStampsALiveGenerationAndReportsAStaleOneLost is the
// core behaviour: one run at its CURRENT generation is heartbeat-refreshed
// and not reported lost; a second, reclaimed-and-reclaimed run whose caller
// still holds the OLD generation is reported lost and its heartbeat_at is
// left untouched.
func TestHeartbeatBatchFencedStampsALiveGenerationAndReportsAStaleOneLost(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			if _, _, err := store.StartNewRun(ctx, "fenced-live", "test-workflow", 1,
				json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
				t.Fatalf("start run live: %v", err)
			}
			if _, _, err := store.StartNewRun(ctx, "fenced-stale", "test-workflow", 1,
				json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
				t.Fatalf("start run stale: %v", err)
			}

			claimed, err := store.ClaimWorkflows(ctx, "worker-1", 100)
			if err != nil {
				t.Fatalf("ClaimWorkflows: %v", err)
			}
			byID := map[string]*WorkflowInstance{}
			for _, wf := range claimed {
				byID[wf.ID] = wf
			}
			live, ok := byID["fenced-live"]
			if !ok {
				t.Fatalf("fenced-live not claimed: %v", claimed)
			}
			stale, ok := byID["fenced-stale"]
			if !ok {
				t.Fatalf("fenced-stale not claimed: %v", claimed)
			}
			staleGeneration := stale.Generation

			// Both rows got heartbeat_at = now() from the claim above. Reset
			// both to the sentinel so the assertions below can tell "was
			// refreshed by HeartbeatBatchFenced" from "was left alone" without
			// comparing two now() reads for ordering.
			setHeartbeatAtToSentinel(t, ctx, backend.Name(), store, "fenced-live")
			setHeartbeatAtToSentinel(t, ctx, backend.Name(), store, "fenced-stale")

			// Simulate the SAME worker reclaiming and re-claiming its own
			// run -- the exact scenario the issue's own probe reproduced.
			// ReapStaleInstances requires an actually-stale heartbeat, so
			// generation is bumped directly, matching what the reaper's own
			// UPDATE does to the row when it reclaims it.
			bumpGeneration(t, ctx, backend.Name(), store, "fenced-stale")

			lost, err := store.HeartbeatBatchFenced(ctx, "worker-1", []GenerationKey{
				{WorkflowID: live.ID, Generation: live.Generation},
				{WorkflowID: stale.ID, Generation: staleGeneration},
			})
			if err != nil {
				t.Fatalf("HeartbeatBatchFenced: %v", err)
			}
			if len(lost) != 1 || lost[0] != "fenced-stale" {
				t.Fatalf("lost = %v, want exactly [fenced-stale]", lost)
			}

			if heartbeatAtIsStillSentinel(t, ctx, backend.Name(), store, "fenced-live") {
				t.Fatal("fenced-live's heartbeat_at was not refreshed -- the still-live pair must be heartbeat-stamped")
			}
			if !heartbeatAtIsStillSentinel(t, ctx, backend.Name(), store, "fenced-stale") {
				t.Fatal("fenced-stale's heartbeat_at was refreshed -- a pair reported lost must not be stamped")
			}
		})
	}
}

// TestHeartbeatBatchFencedReportsEveryPairLostWhenTheWorkerDoesNotOwnAnyOfThem
// is the boundary the fenced predicate exists to enforce: a caller passing
// pairs for runs it never held (wrong worker id) gets every one back as
// lost, and stamps nothing.
func TestHeartbeatBatchFencedReportsEveryPairLostWhenTheWorkerDoesNotOwnAnyOfThem(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			if _, _, err := store.StartNewRun(ctx, "fenced-owned-by-other", "test-workflow", 1,
				json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
				t.Fatalf("start run: %v", err)
			}
			claimed, err := store.ClaimWorkflows(ctx, "worker-real", 100)
			if err != nil {
				t.Fatalf("ClaimWorkflows: %v", err)
			}
			if len(claimed) != 1 {
				t.Fatalf("claimed %v, want exactly one", claimed)
			}

			lost, err := store.HeartbeatBatchFenced(ctx, "worker-impostor", []GenerationKey{
				{WorkflowID: claimed[0].ID, Generation: claimed[0].Generation},
			})
			if err != nil {
				t.Fatalf("HeartbeatBatchFenced: %v", err)
			}
			if len(lost) != 1 || lost[0] != claimed[0].ID {
				t.Fatalf("lost = %v, want exactly [%s]", lost, claimed[0].ID)
			}
		})
	}
}

// bumpGeneration reclaims one workflow by directly advancing its generation,
// the same net effect ReapStaleInstances' UPDATE has on the row -- without
// needing a genuinely stale heartbeat_at to trigger it.
func bumpGeneration(t *testing.T, ctx context.Context, dialect string, store WorkflowStore, workflowID string) {
	t.Helper()
	sqlDB := queueClaimTestDB(t, store)
	var q string
	switch dialect {
	case "postgres":
		q = `UPDATE workflow_instances SET generation = generation + 1 WHERE tenant_id = $1 AND id = $2`
	case "mysql":
		q = `UPDATE workflow_instances SET generation = generation + 1 WHERE tenant_id = ? AND id = ?`
	case "mssql":
		q = `UPDATE workflow_instances SET generation = generation + 1 WHERE tenant_id = @p1 AND id = @p2`
	default:
		t.Fatalf("unknown dialect %q", dialect)
	}
	if _, err := sqlDB.ExecContext(ctx, q, DefaultTenantUUID, workflowID); err != nil {
		t.Fatalf("bump generation: %v", err)
	}
}

// setHeartbeatAtToSentinel writes heartbeatSentinel directly into one row's
// heartbeat_at, bypassing every Go-level heartbeat path.
func setHeartbeatAtToSentinel(t *testing.T, ctx context.Context, dialect string, store WorkflowStore, workflowID string) {
	t.Helper()
	sqlDB := queueClaimTestDB(t, store)
	var q string
	switch dialect {
	case "postgres":
		q = `UPDATE workflow_instances SET heartbeat_at = $1 WHERE tenant_id = $2 AND id = $3`
	case "mysql":
		q = `UPDATE workflow_instances SET heartbeat_at = ? WHERE tenant_id = ? AND id = ?`
	case "mssql":
		q = `UPDATE workflow_instances SET heartbeat_at = @p1 WHERE tenant_id = @p2 AND id = @p3`
	default:
		t.Fatalf("unknown dialect %q", dialect)
	}
	if _, err := sqlDB.ExecContext(ctx, q, heartbeatSentinelFor(dialect), DefaultTenantUUID, workflowID); err != nil {
		t.Fatalf("set heartbeat_at sentinel: %v", err)
	}
}

// heartbeatAtIsStillSentinel reports whether one row's heartbeat_at is still
// at or before heartbeatSentinel -- true means nothing has stamped it since
// setHeartbeatAtToSentinel ran.
func heartbeatAtIsStillSentinel(t *testing.T, ctx context.Context, dialect string, store WorkflowStore, workflowID string) bool {
	t.Helper()
	sqlDB := queueClaimTestDB(t, store)
	var q string
	switch dialect {
	case "postgres":
		q = `SELECT heartbeat_at <= $1 FROM workflow_instances WHERE tenant_id = $2 AND id = $3`
	case "mysql":
		q = `SELECT heartbeat_at <= ? FROM workflow_instances WHERE tenant_id = ? AND id = ?`
	case "mssql":
		q = `SELECT CASE WHEN heartbeat_at <= @p1 THEN 1 ELSE 0 END FROM workflow_instances WHERE tenant_id = @p2 AND id = @p3`
	default:
		t.Fatalf("unknown dialect %q", dialect)
	}
	row := sqlDB.QueryRowContext(ctx, q, heartbeatSentinelFor(dialect), DefaultTenantUUID, workflowID)
	var stillSentinel bool
	if err := row.Scan(&stillSentinel); err != nil {
		t.Fatalf("read heartbeat_at: %v", err)
	}
	return stillSentinel
}
