package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"
)

// cleat#1965: a concurrency key or queue holder is held for as long as its
// run is non-terminal, not until its expires_at passes. These are the
// reproduction the issue's own design asks for -- age a holder row past its
// original (pre-fix) expiry while its run is still alive, and confirm a
// second run is still refused -- plus the stranded-slot case: a run that
// went terminal without its release succeeding must be freed by the reaper
// immediately, without waiting on any clock.
//
// Both arms cleat#1917 and cleat#1918 split the predicate into (the
// registered-queue semaphore and the bare-key mutex) are covered, since the
// bug and the fix are identical in shape on both.

// ageQueueHolder backdates one queue_holders row's expires_at, simulating
// the pre-cleat#1965 30-minute TTL having long since passed while its run is
// still alive.
func ageQueueHolder(t *testing.T, ctx context.Context, db *sql.DB, dialect, tenantID, queueName, workflowID string, at time.Time) {
	t.Helper()
	var q string
	switch dialect {
	case "postgres":
		q = `UPDATE queue_holders SET expires_at = $1 WHERE tenant_id = $2 AND queue_name = $3 AND workflow_id = $4`
	case "mysql":
		q = `UPDATE queue_holders SET expires_at = ? WHERE tenant_id = ? AND queue_name = ? AND workflow_id = ?`
	case "mssql":
		q = `UPDATE queue_holders SET expires_at = @p1 WHERE tenant_id = @p2 AND queue_name = @p3 AND workflow_id = @p4`
	default:
		t.Fatalf("unknown dialect %q", dialect)
	}
	if _, err := db.ExecContext(ctx, q, at, tenantID, queueName, workflowID); err != nil {
		t.Fatalf("age queue_holders row: %v", err)
	}
}

// ageConcurrencyKey is ageQueueHolder's twin for the bare-key mutex table.
// workflow_id is not concurrency_keys' primary key (key_hash is), but it is
// unique enough for one test's own row.
func ageConcurrencyKey(t *testing.T, ctx context.Context, db *sql.DB, dialect, tenantID, workflowID string, at time.Time) {
	t.Helper()
	var q string
	switch dialect {
	case "postgres":
		q = `UPDATE concurrency_keys SET expires_at = $1 WHERE tenant_id = $2 AND workflow_id = $3`
	case "mysql":
		q = `UPDATE concurrency_keys SET expires_at = ? WHERE tenant_id = ? AND workflow_id = ?`
	case "mssql":
		q = `UPDATE concurrency_keys SET expires_at = @p1 WHERE tenant_id = @p2 AND workflow_id = @p3`
	default:
		t.Fatalf("unknown dialect %q", dialect)
	}
	if _, err := db.ExecContext(ctx, q, at, tenantID, workflowID); err != nil {
		t.Fatalf("age concurrency_keys row: %v", err)
	}
}

// forceWorkflowTerminalWithoutRelease writes a terminal status directly,
// bypassing every Go-level terminal path (TerminateWorkflow, completion,
// the defer phases) and therefore ReleaseWorkflowConcurrencyKeys -- the
// stranded-slot case the issue names: a release that failed, or a worker
// that died between the terminal commit and its separate release call.
func forceWorkflowTerminalWithoutRelease(t *testing.T, ctx context.Context, db *sql.DB, dialect, tenantID, workflowID string) {
	t.Helper()
	var q string
	switch dialect {
	case "postgres":
		q = `UPDATE workflow_instances SET status = 'done', completed_at = now() WHERE tenant_id = $1 AND id = $2`
	case "mysql":
		q = `UPDATE workflow_instances SET status = 'done', completed_at = NOW(6) WHERE tenant_id = ? AND id = ?`
	case "mssql":
		q = `UPDATE workflow_instances SET status = 'done', completed_at = SYSUTCDATETIME() WHERE tenant_id = @p1 AND id = @p2`
	default:
		t.Fatalf("unknown dialect %q", dialect)
	}
	if _, err := db.ExecContext(ctx, q, tenantID, workflowID); err != nil {
		t.Fatalf("force workflow terminal: %v", err)
	}
}

// TestARegisteredQueueHolderPastItsOldExpiryStillBlocksWhileItsRunIsLive is
// the issue's own reproduction, registered-queue arm: age a holder's
// expires_at 45 minutes into the past -- past the OLD 30-minute TTL, well
// inside the NEW week-long backstop, so only the run-state check is what
// can still be refusing -- while its run stays non-terminal, and confirm a
// second run against the same (limit=1) queue is still refused. On the
// pre-fix tree the candidate predicate's `expires_at > now()` test alone
// decided validity, so an aged row reads as free and this fails.
func TestARegisteredQueueHolderPastItsOldExpiryStillBlocksWhileItsRunIsLive(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			name := queueTestName("expiry-live")
			db := queueClaimTestDB(t, store)
			if err := NewQueueStore(db, backend.Name()).CreateQueue(ctx, DefaultTenantUUID, name, 1, nil, nil, nil); err != nil {
				t.Fatalf("CreateQueue: %v", err)
			}

			starter, ok := store.(claimQueueConcurrencyKeyStarter)
			if !ok {
				t.Fatalf("%T cannot record a concurrency key", store)
			}
			if _, _, err := starter.StartNewRunWithConcurrencyKey(ctx,
				"expiry-live-a", "test-workflow", 1, json.RawMessage(`{}`),
				"", DefaultTenantUUID, 0, name); err != nil {
				t.Fatalf("start run A: %v", err)
			}
			claimedA, err := store.ClaimWorkflows(ctx, "worker-1", 100)
			if err != nil {
				t.Fatalf("ClaimWorkflows(A): %v", err)
			}
			if len(claimedA) != 1 || claimedA[0].ID != "expiry-live-a" {
				t.Fatalf("claimed %v, want exactly [expiry-live-a]", claimedA)
			}

			ageQueueHolder(t, ctx, db, backend.Name(), DefaultTenantUUID, name, "expiry-live-a",
				time.Now().Add(-45*time.Minute))

			if _, _, err := starter.StartNewRunWithConcurrencyKey(ctx,
				"expiry-live-b", "test-workflow", 1, json.RawMessage(`{}`),
				"", DefaultTenantUUID, 0, name); err != nil {
				t.Fatalf("start run B: %v", err)
			}
			claimedB, err := store.ClaimWorkflows(ctx, "worker-2", 100)
			if err != nil {
				t.Fatalf("ClaimWorkflows(B): %v", err)
			}
			if len(claimedB) != 0 {
				t.Fatalf("run B was admitted (%v) while run A's aged-but-still-live holder should have "+
					"blocked it -- a holder must be freed by its run going terminal, not by its "+
					"expires_at passing", claimedB)
			}
		})
	}
}

// TestABareKeyPastItsOldExpiryStillBlocksWhileItsRunIsLive is the same
// reproduction, mutex arm: an unregistered concurrency_key, no queue.
func TestABareKeyPastItsOldExpiryStillBlocksWhileItsRunIsLive(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			key := queueTestName("bare-expiry-live")
			db := queueClaimTestDB(t, store)

			starter, ok := store.(claimQueueConcurrencyKeyStarter)
			if !ok {
				t.Fatalf("%T cannot record a concurrency key", store)
			}
			if _, _, err := starter.StartNewRunWithConcurrencyKey(ctx,
				"bare-expiry-live-a", "test-workflow", 1, json.RawMessage(`{}`),
				"", DefaultTenantUUID, 0, key); err != nil {
				t.Fatalf("start run A: %v", err)
			}
			claimedA, err := store.ClaimWorkflows(ctx, "worker-1", 100)
			if err != nil {
				t.Fatalf("ClaimWorkflows(A): %v", err)
			}
			if len(claimedA) != 1 || claimedA[0].ID != "bare-expiry-live-a" {
				t.Fatalf("claimed %v, want exactly [bare-expiry-live-a]", claimedA)
			}

			ageConcurrencyKey(t, ctx, db, backend.Name(), DefaultTenantUUID, "bare-expiry-live-a",
				time.Now().Add(-45*time.Minute))

			if _, _, err := starter.StartNewRunWithConcurrencyKey(ctx,
				"bare-expiry-live-b", "test-workflow", 1, json.RawMessage(`{}`),
				"", DefaultTenantUUID, 0, key); err != nil {
				t.Fatalf("start run B: %v", err)
			}
			claimedB, err := store.ClaimWorkflows(ctx, "worker-2", 100)
			if err != nil {
				t.Fatalf("ClaimWorkflows(B): %v", err)
			}
			if len(claimedB) != 0 {
				t.Fatalf("run B was admitted (%v) while run A's aged-but-still-live bare key should have "+
					"blocked it -- mutual exclusion must not lapse while the holder is still alive",
					claimedB)
			}
		})
	}
}

// TestTheReaperFreesATerminalRegisteredQueueHolderWithoutWaitingForExpiry is
// the stranded-slot case: run A goes terminal without its release
// succeeding (the best-effort call the issue describes as separate from the
// terminal commit), so its queue_holders row survives with a fresh,
// far-future expires_at. The reaper must free it on run state alone.
func TestTheReaperFreesATerminalRegisteredQueueHolderWithoutWaitingForExpiry(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			name := queueTestName("stranded")
			db := queueClaimTestDB(t, store)
			if err := NewQueueStore(db, backend.Name()).CreateQueue(ctx, DefaultTenantUUID, name, 1, nil, nil, nil); err != nil {
				t.Fatalf("CreateQueue: %v", err)
			}

			starter, ok := store.(claimQueueConcurrencyKeyStarter)
			if !ok {
				t.Fatalf("%T cannot record a concurrency key", store)
			}
			if _, _, err := starter.StartNewRunWithConcurrencyKey(ctx,
				"stranded-a", "test-workflow", 1, json.RawMessage(`{}`),
				"", DefaultTenantUUID, 0, name); err != nil {
				t.Fatalf("start run A: %v", err)
			}
			claimedA, err := store.ClaimWorkflows(ctx, "worker-1", 100)
			if err != nil {
				t.Fatalf("ClaimWorkflows(A): %v", err)
			}
			if len(claimedA) != 1 {
				t.Fatalf("claimed %v, want exactly one", claimedA)
			}

			forceWorkflowTerminalWithoutRelease(t, ctx, db, backend.Name(), DefaultTenantUUID, "stranded-a")

			// Confirm the stranding: the holder row is still there, unexpired.
			if got := mustCountQueueHolders(t, ctx, db, backend.Name(), name); got != 1 {
				t.Fatalf("queue_holders = %d immediately after forcing run A terminal without release, "+
					"want 1 -- the fixture must strand the row, not release it", got)
			}

			if _, err := store.ReapExpiredConcurrencyKeys(ctx); err != nil {
				t.Fatalf("ReapExpiredConcurrencyKeys: %v", err)
			}

			if got := mustCountQueueHolders(t, ctx, db, backend.Name(), name); got != 0 {
				t.Fatalf("queue_holders = %d after reaping a terminal-but-stranded holder, want 0 -- "+
					"the reaper must free a terminal run's slot without waiting for expires_at", got)
			}

			// The freed slot is usable: a second run now claims cleanly.
			if _, _, err := starter.StartNewRunWithConcurrencyKey(ctx,
				"stranded-b", "test-workflow", 1, json.RawMessage(`{}`),
				"", DefaultTenantUUID, 0, name); err != nil {
				t.Fatalf("start run B: %v", err)
			}
			claimedB, err := store.ClaimWorkflows(ctx, "worker-2", 100)
			if err != nil {
				t.Fatalf("ClaimWorkflows(B): %v", err)
			}
			if len(claimedB) != 1 || claimedB[0].ID != "stranded-b" {
				t.Fatalf("claimed %v after reaping, want exactly [stranded-b]", claimedB)
			}
		})
	}
}

// TestTheReaperFreesATerminalBareKeyWithoutWaitingForExpiry is the same
// stranded-slot case, mutex arm.
func TestTheReaperFreesATerminalBareKeyWithoutWaitingForExpiry(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			key := queueTestName("bare-stranded")
			db := queueClaimTestDB(t, store)

			starter, ok := store.(claimQueueConcurrencyKeyStarter)
			if !ok {
				t.Fatalf("%T cannot record a concurrency key", store)
			}
			if _, _, err := starter.StartNewRunWithConcurrencyKey(ctx,
				"bare-stranded-a", "test-workflow", 1, json.RawMessage(`{}`),
				"", DefaultTenantUUID, 0, key); err != nil {
				t.Fatalf("start run A: %v", err)
			}
			claimedA, err := store.ClaimWorkflows(ctx, "worker-1", 100)
			if err != nil {
				t.Fatalf("ClaimWorkflows(A): %v", err)
			}
			if len(claimedA) != 1 {
				t.Fatalf("claimed %v, want exactly one", claimedA)
			}

			forceWorkflowTerminalWithoutRelease(t, ctx, db, backend.Name(), DefaultTenantUUID, "bare-stranded-a")

			if got := mustCountConcurrencyKeys(t, ctx, db, backend.Name(), "bare-stranded-a"); got != 1 {
				t.Fatalf("concurrency_keys = %d immediately after forcing run A terminal without release, "+
					"want 1 -- the fixture must strand the row, not release it", got)
			}

			if _, err := store.ReapExpiredConcurrencyKeys(ctx); err != nil {
				t.Fatalf("ReapExpiredConcurrencyKeys: %v", err)
			}

			if got := mustCountConcurrencyKeys(t, ctx, db, backend.Name(), "bare-stranded-a"); got != 0 {
				t.Fatalf("concurrency_keys = %d after reaping a terminal-but-stranded key, want 0 -- "+
					"the reaper must free a terminal run's key without waiting for expires_at", got)
			}

			if _, _, err := starter.StartNewRunWithConcurrencyKey(ctx,
				"bare-stranded-b", "test-workflow", 1, json.RawMessage(`{}`),
				"", DefaultTenantUUID, 0, key); err != nil {
				t.Fatalf("start run B: %v", err)
			}
			claimedB, err := store.ClaimWorkflows(ctx, "worker-2", 100)
			if err != nil {
				t.Fatalf("ClaimWorkflows(B): %v", err)
			}
			if len(claimedB) != 1 || claimedB[0].ID != "bare-stranded-b" {
				t.Fatalf("claimed %v after reaping, want exactly [bare-stranded-b]", claimedB)
			}
		})
	}
}

// mustCountConcurrencyKeys counts live concurrency_keys rows for one
// workflow, mirroring mustCountQueueHolders for the mutex table.
func mustCountConcurrencyKeys(t *testing.T, ctx context.Context, db *sql.DB, dialect, workflowID string) int {
	t.Helper()
	var q string
	switch dialect {
	case "postgres":
		q = `SELECT count(*) FROM concurrency_keys WHERE workflow_id = $1`
	case "mysql":
		q = `SELECT count(*) FROM concurrency_keys WHERE workflow_id = ?`
	case "mssql":
		q = `SELECT count(*) FROM concurrency_keys WHERE workflow_id = @p1`
	default:
		t.Fatalf("unknown dialect %q", dialect)
	}
	var n int
	if err := db.QueryRowContext(ctx, q, workflowID).Scan(&n); err != nil {
		t.Fatalf("count concurrency_keys: %v", err)
	}
	return n
}
