package main

// cleat#3853: a compacted run whose sealed compaction state this worker cannot
// open is offered for compaction on every tick and fails on every tick. The
// candidate query orders by event count and takes a bounded batch, so ten such
// runs fill the batch and no other run is compacted at all.
//
// Measured before the fix on a real PostgreSQL, ten poisoned runs and one fresh
// one, three ticks: candidates=10, all poisoned, fresh never selected, its event
// rows untouched. Compacting the fresh run directly succeeded, which is what makes
// this starvation by selection rather than an uncompactable run.
//
// This test drives the WORKER, not the store: the defect is that the worker keeps
// no memory between ticks, so the fix and its proof belong at that level.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

func starveKey(t *testing.T) string {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return base64.StdEncoding.EncodeToString(key)
}

func TestTenRunsTheWorkerCannotOpenDoNotStarveAFreshRun(t *testing.T) {
	db := testutil.SuiteTestDB(t, "cleat_worker_starve3853")
	ctx := context.Background()

	// Two rings. A run sealed by A cannot be opened by B: the rotated-key case,
	// with no corruption involved.
	encA, err := engine.NewPayloadEncryption(starveKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption A: %v", err)
	}
	encB, err := engine.NewPayloadEncryption(starveKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption B: %v", err)
	}
	plain := engine.NewPostgresStore(db) // the definition both rings can read
	storeA := engine.NewPostgresStore(db).WithEncryption(encA, true)
	storeB := engine.NewPostgresStore(db).WithEncryption(encB, true)

	const (
		threshold = 20 // events a run must hold to be a candidate
		poisoned  = 10 // runs B cannot compact == compactionCandidatesPerTick
		prefix    = "starve-3853-"
	)
	clear := func() {
		_, _ = db.Exec(`DELETE FROM event_history WHERE workflow_id LIKE $1`, prefix+"%")
		_, _ = db.Exec(`DELETE FROM workflow_payloads WHERE id LIKE $1`, prefix+"%")
		_, _ = db.Exec(`DELETE FROM workflow_instances WHERE id LIKE $1`, prefix+"%")
	}
	clear()
	t.Cleanup(clear)

	defName := prefix + "def"
	if err := plain.DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}
	newRun := func(t *testing.T, store *engine.PostgresStore, id string) {
		t.Helper()
		if _, _, err := store.StartNewRun(ctx, id, defName, 1, json.RawMessage(`{}`), "", engine.DefaultTenantUUID, 0); err != nil {
			t.Fatalf("StartNewRun %s: %v", id, err)
		}
	}
	appendRange := func(t *testing.T, store *engine.PostgresStore, id string, from, to int) {
		t.Helper()
		for i := from; i < to; i++ {
			rec := engine.EventRecord{
				Step: i, EventType: engine.EventTypeCall, Service: "svc", Op: fmt.Sprintf("op%d", i),
				Request: fmt.Sprintf(`{"i":%d}`, i), Response: `{"ok":true}`,
			}
			if err := store.AppendEventHistory(ctx, id, rec); err != nil {
				t.Fatalf("AppendEventHistory %s step %d: %v", id, i, err)
			}
		}
	}
	rows := func(t *testing.T, id string) int {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM event_history WHERE workflow_id = $1`, id).Scan(&n); err != nil {
			t.Fatalf("count event rows for %s: %v", id, err)
		}
		return n
	}

	// Ten runs written and compacted by A, then grown back over the threshold.
	// After the first compaction each keeps 10 of 60 rows and gains 40, so it
	// holds 50 -- more than the fresh run, which is what puts it at the front of
	// the batch.
	poisonIDs := make([]string, 0, poisoned)
	for i := 0; i < poisoned; i++ {
		id := fmt.Sprintf("%spoison-%02d", prefix, i)
		newRun(t, storeA, id)
		appendRange(t, storeA, id, 0, 60)
		if err := engine.CompactWorkflowHistory(ctx, storeA, id, threshold, nil); err != nil {
			t.Fatalf("poisoned run %d: first compaction: %v", i, err)
		}
		appendRange(t, storeA, id, 60, 100)
		poisonIDs = append(poisonIDs, id)
	}

	freshID := prefix + "fresh"
	newRun(t, storeB, freshID)
	appendRange(t, storeB, freshID, 0, 30)
	freshBefore := rows(t, freshID)
	if freshBefore != 30 {
		t.Fatalf("precondition: fresh run holds %d event rows, want 30", freshBefore)
	}

	// CONTROL: the fresh run is compactable by B when selected directly, so
	// whatever happens below is about selection and not about the run.
	w := newTestWorkerFromStore(storeB)
	defer w.cancel()
	w.compactionThreshold = threshold
	if err := engine.CompactWorkflowHistory(ctx, storeB, freshID, threshold, nil); err != nil {
		t.Fatalf("CONTROL FAILED: the fresh run is not compactable by B at all: %v", err)
	}
	if got := rows(t, freshID); got >= freshBefore {
		t.Fatalf("CONTROL FAILED: compacting the fresh run directly left %d of %d event rows", got, freshBefore)
	}

	// Reset the fresh run: one that has been compacted is no longer fresh.
	_, _ = db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, freshID)
	_, _ = db.Exec(`DELETE FROM workflow_payloads WHERE id = $1`, freshID)
	_, _ = db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, freshID)
	newRun(t, storeB, freshID)
	appendRange(t, storeB, freshID, 0, 30)
	freshBefore = rows(t, freshID)

	for tick := 1; tick <= 3; tick++ {
		w.compactOnce()
		t.Logf("tick %d: fresh run holds %d of %d event rows", tick, rows(t, freshID), freshBefore)
		if got := rows(t, freshID); got < freshBefore {
			// Compacted: the batch is no longer filled by runs this worker
			// cannot open.
			if got != threshold/2 {
				t.Fatalf("tick %d: the fresh run was compacted to %d rows, want %d (half the threshold)", tick, got, threshold/2)
			}
			for _, id := range poisonIDs {
				if n := rows(t, id); n != 50 {
					t.Errorf("a run whose state this worker cannot open lost event rows (%s: %d, want 50): "+
						"not compacting is safe and is the only correct outcome", id, n)
				}
			}
			return
		}
	}
	t.Fatalf("after 3 compaction ticks the fresh run still holds all %d event rows: %d runs whose sealed state this "+
		"worker cannot open fill every batch, so no other run is ever compacted (cleat#3853)", freshBefore, poisoned)
}
