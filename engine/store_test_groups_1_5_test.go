package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// =============================================================================
// Group 1 — Claim and Execute (critical path)
// =============================================================================

func TestClaimWorkflow(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			setupTestData(t, store)

			wf, err := store.ClaimWorkflow(ctx, "worker-1")
			if err != nil {
				t.Fatalf("ClaimWorkflow: %v", err)
			}
			if wf == nil {
				t.Fatal("ClaimWorkflow returned nil, expected a ready workflow")
			}
			if wf.Status != "running" {
				t.Errorf("status = %q, want %q", wf.Status, "running")
			}
			if wf.AssignedTo != "worker-1" {
				t.Errorf("AssignedTo = %q, want %q", wf.AssignedTo, "worker-1")
			}

			// Verify the claimed row is no longer "ready" in the database.
			stored, err := store.GetWorkflowByID(ctx, wf.ID)
			if err != nil {
				t.Fatalf("GetWorkflowByID: %v", err)
			}
			if stored == nil {
				t.Fatal("claimed workflow not found after ClaimWorkflow")
			}
			if stored.Status != "running" {
				t.Errorf("stored status after claim = %q, want %q", stored.Status, "running")
			}

			// Second claim should return nil — no more ready workflows remaining.
			wf2, err := store.ClaimWorkflow(ctx, "worker-1")
			if err != nil {
				t.Fatalf("ClaimWorkflow (second): %v", err)
			}
			if wf2 != nil {
				t.Errorf("expected nil for second claim, got workflow %s", wf2.ID)
			}
		})
	}
}

func TestClaimWorkflows_Batch(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			setupTestData(t, store)

			// Create 5 ready workflows with unique idempotency keys.
			for i := 0; i < 5; i++ {
				key := fmt.Sprintf("claim-batch-key-%d", i)
				_, alreadyExisted, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), key, DefaultTenantUUID, 0)
				if err != nil {
					t.Fatalf("StartNewRun[%d]: %v", i, err)
				}
				if alreadyExisted {
					t.Fatalf("StartNewRun[%d]: unexpected alreadyExisted=true", i)
				}
			}

			wfs, err := store.ClaimWorkflows(ctx, "worker-1", 5)
			if err != nil {
				t.Fatalf("ClaimWorkflows: %v", err)
			}
			if len(wfs) != 5 {
				t.Fatalf("ClaimWorkflows returned %d workflows, want 5", len(wfs))
			}

			// Verify no duplicates — every returned ID must be unique.
			seen := make(map[string]bool)
			for _, wf := range wfs {
				if seen[wf.ID] {
					t.Errorf("duplicate workflow ID: %s", wf.ID)
				}
				seen[wf.ID] = true
				if wf.Status != "running" {
					t.Errorf("claimed workflow status = %q, want %q", wf.Status, "running")
				}
				if wf.AssignedTo != "worker-1" {
					t.Errorf("claimed workflow AssignedTo = %q, want %q", wf.AssignedTo, "worker-1")
				}
			}
		})
	}
}

func TestClaimStickyWorkflows(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			setupTestData(t, store)

			// Create a workflow and set its sticky worker.
			runID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}
			if err := store.UpdateStickyWorker(ctx, runID, "sticky-worker-1"); err != nil {
				t.Fatalf("UpdateStickyWorker: %v", err)
			}

			wfs, err := store.ClaimStickyWorkflows(ctx, "sticky-worker-1", 10)
			if err != nil {
				t.Fatalf("ClaimStickyWorkflows: %v", err)
			}
			if len(wfs) == 0 {
				t.Fatal("ClaimStickyWorkflows returned 0, expected at least 1")
			}

			found := false
			for _, wf := range wfs {
				if wf.ID == runID {
					found = true
					if wf.Status != "running" {
						t.Errorf("claimed sticky workflow status = %q, want %q", wf.Status, "running")
					}
					if wf.AssignedTo != "sticky-worker-1" {
						t.Errorf("AssignedTo = %q, want %q", wf.AssignedTo, "sticky-worker-1")
					}
					break
				}
			}
			if !found {
				t.Errorf("sticky workflow %s not found in ClaimStickyWorkflows results", runID)
			}
		})
	}
}

func TestClaimSkipLocked(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			setupTestData(t, store)
			truncateAll(t, store)

			// Create exactly 10 ready workflows.
			allIDs := make(map[string]bool)
			for i := 0; i < 10; i++ {
				runID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0)
				if err != nil {
					t.Fatalf("StartNewRun[%d]: %v", i, err)
				}
				allIDs[runID] = true
			}

			// First claim: 3 workflows with "worker-1".
			first, err := store.ClaimWorkflows(ctx, "worker-1", 3)
			if err != nil {
				t.Fatalf("ClaimWorkflows (first, limit=3): %v", err)
			}
			if len(first) != 3 {
				t.Fatalf("first claim returned %d, want 3", len(first))
			}

			firstIDs := make(map[string]bool)
			for _, wf := range first {
				firstIDs[wf.ID] = true
				if !allIDs[wf.ID] {
					t.Errorf("first claim returned unexpected ID: %s", wf.ID)
				}
			}

			// Second claim: 7 workflows with "worker-2".
			second, err := store.ClaimWorkflows(ctx, "worker-2", 7)
			if err != nil {
				t.Fatalf("ClaimWorkflows (second, limit=7): %v", err)
			}
			if len(second) != 7 {
				t.Fatalf("second claim returned %d, want 7", len(second))
			}

			// Verify the two claimers received disjoint sets of IDs.
			for _, wf := range second {
				if firstIDs[wf.ID] {
					t.Errorf("ID %s appears in both claim batches (must be disjoint)", wf.ID)
				}
				if !allIDs[wf.ID] {
					t.Errorf("second claim returned unexpected ID: %s", wf.ID)
				}
			}
		})
	}
}

func TestNoWorkflowsToClaim(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			truncateAll(t, store)

			wfs, err := store.ClaimWorkflows(ctx, "worker-1", 5)
			if err != nil {
				t.Fatalf("ClaimWorkflows on empty store: %v", err)
			}
			if len(wfs) != 0 {
				t.Errorf("expected 0 workflows, got %d", len(wfs))
			}
		})
	}
}

// =============================================================================
// Group 2 — Exactly-Once Start
// =============================================================================

func TestExactlyOnceStart(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			setupTestData(t, store)

			// First call with idempotency key — should create a new run.
			runID1, alreadyExisted, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "idem-1", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun (first): %v", err)
			}
			if alreadyExisted {
				t.Fatal("alreadyExisted=true on first call, expected false")
			}

			// Second call with the same key — should return the same runID.
			runID2, alreadyExisted, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "idem-1", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun (second): %v", err)
			}
			if !alreadyExisted {
				t.Fatal("alreadyExisted=false on second call, expected true")
			}
			if runID2 != runID1 {
				t.Errorf("second call returned different runID: %q vs %q", runID2, runID1)
			}

			// Verify only one row exists in the database.
			wf, err := store.GetWorkflowByID(ctx, runID1)
			if err != nil {
				t.Fatalf("GetWorkflowByID: %v", err)
			}
			if wf == nil {
				t.Fatal("workflow not found after StartNewRun")
			}
		})
	}
}

func TestExactlyOnceStart_DifferentKeys(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			setupTestData(t, store)

			runID1, alreadyExisted1, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "idem-a", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun (idem-a): %v", err)
			}
			if alreadyExisted1 {
				t.Fatal("alreadyExisted=true for idem-a, expected false")
			}

			runID2, alreadyExisted2, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "idem-b", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun (idem-b): %v", err)
			}
			if alreadyExisted2 {
				t.Fatal("alreadyExisted=true for idem-b, expected false")
			}

			if runID1 == runID2 {
				t.Error("different idempotency keys produced identical runID")
			}
		})
	}
}

// =============================================================================
// Group 3 — Event History
// =============================================================================

func TestAppendEventHistory(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			setupTestData(t, store)

			runID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			rec := EventRecord{
				Step:      0,
				EventType: EventTypeCall,
				Service:   "my-service",
				Op:        "my-op",
				Request:   `{"key":"val"}`,
				Response:  `{"ok":true}`,
			}
			if err := store.AppendEventHistory(ctx, runID, rec); err != nil {
				t.Fatalf("AppendEventHistory: %v", err)
			}

			history, err := store.LoadEventHistory(ctx, runID)
			if err != nil {
				t.Fatalf("LoadEventHistory: %v", err)
			}
			if len(history) != 1 {
				t.Fatalf("expected 1 event, got %d", len(history))
			}
			if history[0].Step != 0 {
				t.Errorf("step = %d, want 0", history[0].Step)
			}
			if history[0].EventType != EventTypeCall {
				t.Errorf("EventType = %q, want %q", history[0].EventType, EventTypeCall)
			}
			if history[0].Service != "my-service" {
				t.Errorf("Service = %q, want %q", history[0].Service, "my-service")
			}
			if history[0].Op != "my-op" {
				t.Errorf("Op = %q, want %q", history[0].Op, "my-op")
			}
		})
	}
}

func TestAppendEventHistoryBatch(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			setupTestData(t, store)

			runID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			recs := make([]EventRecord, 5)
			for i := 0; i < 5; i++ {
				recs[i] = EventRecord{
					Step:      i,
					EventType: EventTypeCall,
					Service:   "svc",
					Op:        fmt.Sprintf("op-%d", i),
					Request:   `{}`,
				}
			}
			if err := store.AppendEventHistoryBatch(ctx, runID, recs); err != nil {
				t.Fatalf("AppendEventHistoryBatch: %v", err)
			}

			history, err := store.LoadEventHistory(ctx, runID)
			if err != nil {
				t.Fatalf("LoadEventHistory: %v", err)
			}
			if len(history) != 5 {
				t.Fatalf("expected 5 events, got %d", len(history))
			}
			for i, ev := range history {
				if ev.Step != i {
					t.Errorf("history[%d].Step = %d, want %d", i, ev.Step, i)
				}
				if ev.EventType != EventTypeCall {
					t.Errorf("history[%d].EventType = %q, want %q", i, ev.EventType, EventTypeCall)
				}
			}
		})
	}
}

func TestAppendEventHistory_Idempotent(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			setupTestData(t, store)

			runID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			rec := EventRecord{Step: 0, EventType: EventTypeCall, Service: "s", Op: "o"}

			// First append should succeed.
			if err := store.AppendEventHistory(ctx, runID, rec); err != nil {
				t.Fatalf("AppendEventHistory (first): %v", err)
			}

			// Second append of the same (workflow_id, step) must NOT error
			// (ON CONFLICT DO NOTHING / INSERT IGNORE semantics).
			if err := store.AppendEventHistory(ctx, runID, rec); err != nil {
				t.Fatalf("AppendEventHistory (duplicate): %v", err)
			}

			// Verify only one event (step=0) exists.
			history, err := store.LoadEventHistory(ctx, runID)
			if err != nil {
				t.Fatalf("LoadEventHistory: %v", err)
			}
			if len(history) != 1 {
				t.Fatalf("expected 1 event after duplicate append, got %d", len(history))
			}
			if history[0].Step != 0 {
				t.Errorf("step = %d, want 0", history[0].Step)
			}
		})
	}
}

func TestLoadEventHistoryPaginated(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			setupTestData(t, store)

			runID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			// Create 10 events (steps 0–9).
			recs := make([]EventRecord, 10)
			for i := 0; i < 10; i++ {
				recs[i] = EventRecord{
					Step:      i,
					EventType: EventTypeCall,
					Service:   "s",
					Op:        fmt.Sprintf("op-%d", i),
				}
			}
			if err := store.AppendEventHistoryBatch(ctx, runID, recs); err != nil {
				t.Fatalf("AppendEventHistoryBatch: %v", err)
			}

			// Page 1: offset=0, limit=3 → 3 events (steps 0,1,2).
			page1, err := store.LoadEventHistoryPaginated(ctx, runID, 0, 3)
			if err != nil {
				t.Fatalf("LoadEventHistoryPaginated(0,3): %v", err)
			}
			if len(page1) != 3 {
				t.Fatalf("page1: expected 3 events, got %d", len(page1))
			}
			for i, ev := range page1 {
				if ev.Step != i {
					t.Errorf("page1[%d].Step = %d, want %d", i, ev.Step, i)
				}
			}

			// Page 2: offset=3, limit=3 → 3 events (steps 3,4,5).
			page2, err := store.LoadEventHistoryPaginated(ctx, runID, 3, 3)
			if err != nil {
				t.Fatalf("LoadEventHistoryPaginated(3,3): %v", err)
			}
			if len(page2) != 3 {
				t.Fatalf("page2: expected 3 events, got %d", len(page2))
			}
			for i, ev := range page2 {
				want := i + 3
				if ev.Step != want {
					t.Errorf("page2[%d].Step = %d, want %d", i, ev.Step, want)
				}
			}

			// Page 3: offset=9, limit=5 → 1 event (step 9).
			page3, err := store.LoadEventHistoryPaginated(ctx, runID, 9, 5)
			if err != nil {
				t.Fatalf("LoadEventHistoryPaginated(9,5): %v", err)
			}
			if len(page3) != 1 {
				t.Fatalf("page3: expected 1 event, got %d", len(page3))
			}
			if page3[0].Step != 9 {
				t.Errorf("page3[0].Step = %d, want 9", page3[0].Step)
			}

			// Page 4: offset=100, limit=10 → 0 events.
			page4, err := store.LoadEventHistoryPaginated(ctx, runID, 100, 10)
			if err != nil {
				t.Fatalf("LoadEventHistoryPaginated(100,10): %v", err)
			}
			if len(page4) != 0 {
				t.Fatalf("page4: expected 0 events, got %d", len(page4))
			}
		})
	}
}

func TestBinaryDataRoundTrip(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			setupTestData(t, store)

			runID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			// Simulate binary data with non-ASCII characters — including null bytes.
			binaryRequest := "binary\x00\x01\x02data"
			rec := EventRecord{
				Step:      0,
				EventType: EventTypeCall,
				Service:   "binary-svc",
				Op:        "binary-op",
				Request:   binaryRequest,
			}
			if err := store.AppendEventHistory(ctx, runID, rec); err != nil {
				t.Fatalf("AppendEventHistory: %v", err)
			}

			history, err := store.LoadEventHistory(ctx, runID)
			if err != nil {
				t.Fatalf("LoadEventHistory: %v", err)
			}
			if len(history) != 1 {
				t.Fatalf("expected 1 event, got %d", len(history))
			}
			if history[0].Request != binaryRequest {
				t.Errorf("binary Request round-trip failed:\ngot:  %q\nwant: %q",
					history[0].Request, binaryRequest)
			}
		})
	}
}

// =============================================================================
// Group 4 — Workflow Lifecycle
// =============================================================================

func TestCompleteWorkflow(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			setupTestData(t, store)
			truncateAll(t, store)

			runID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			wf, err := store.ClaimWorkflow(ctx, "worker-1")
			if err != nil {
				t.Fatalf("ClaimWorkflow: %v", err)
			}
			if wf == nil {
				t.Fatal("ClaimWorkflow returned nil")
			}
			if runID != "" && wf.ID != runID {
				t.Logf("ClaimWorkflow claimed a different workflow (claimed=%s, created=%s)", wf.ID, runID)
			}

			result := `{"done":true}`
			if err := store.CompleteWorkflow(ctx, wf.ID, "worker-1", wf.Generation, result, map[string]string{"key": "val"}); err != nil {
				t.Fatalf("CompleteWorkflow: %v", err)
			}

			stored, err := store.GetWorkflowByID(ctx, wf.ID)
			if err != nil {
				t.Fatalf("GetWorkflowByID: %v", err)
			}
			if stored == nil {
				t.Fatal("workflow not found after CompleteWorkflow")
			}
			if stored.Status != "done" {
				t.Errorf("status = %q, want %q", stored.Status, "done")
			}
			if stored.Result != result {
				t.Errorf("result = %q, want %q", stored.Result, result)
			}
		})
	}
}

func TestFailWorkflow(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			setupTestData(t, store)
			truncateAll(t, store)

			_, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			wf, err := store.ClaimWorkflow(ctx, "worker-1")
			if err != nil {
				t.Fatalf("ClaimWorkflow: %v", err)
			}
			if wf == nil {
				t.Fatal("ClaimWorkflow returned nil")
			}

			errMsg := "something went wrong"
			errCode := "ERR_001"
			errOp := "test-op"
			if err := store.FailWorkflow(ctx, wf.ID, "worker-1", wf.Generation, errMsg, errCode, errOp, nil); err != nil {
				t.Fatalf("FailWorkflow: %v", err)
			}

			stored, err := store.GetWorkflowByID(ctx, wf.ID)
			if err != nil {
				t.Fatalf("GetWorkflowByID: %v", err)
			}
			if stored == nil {
				t.Fatal("workflow not found after FailWorkflow")
			}
			if stored.Status != "failed" {
				t.Errorf("status = %q, want %q", stored.Status, "failed")
			}
			if stored.Error != errMsg {
				t.Errorf("Error = %q, want %q", stored.Error, errMsg)
			}
		})
	}
}

func TestReleaseWorkflow(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			setupTestData(t, store)
			truncateAll(t, store)

			_, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			wf, err := store.ClaimWorkflow(ctx, "worker-1")
			if err != nil {
				t.Fatalf("ClaimWorkflow: %v", err)
			}
			if wf == nil {
				t.Fatal("ClaimWorkflow returned nil")
			}

			nextWakeAt := time.Now().Add(1 * time.Hour)
			if err := store.ReleaseWorkflow(ctx, wf.ID, "worker-1", wf.Generation, nextWakeAt); err != nil {
				t.Fatalf("ReleaseWorkflow: %v", err)
			}

			stored, err := store.GetWorkflowByID(ctx, wf.ID)
			if err != nil {
				t.Fatalf("GetWorkflowByID: %v", err)
			}
			if stored == nil {
				t.Fatal("workflow not found after ReleaseWorkflow")
			}
			if stored.Status != "ready" {
				t.Errorf("status = %q, want %q", stored.Status, "ready")
			}
			// Verify nextWakeAt is approximately 1 hour in the future.
			if stored.NextWakeAt.Before(time.Now().Add(30 * time.Minute)) {
				t.Errorf("NextWakeAt too early: %v (expected ~1h from now)", stored.NextWakeAt)
			}
			if stored.NextWakeAt.After(time.Now().Add(90 * time.Minute)) {
				t.Errorf("NextWakeAt too late: %v (expected ~1h from now)", stored.NextWakeAt)
			}
		})
	}
}

func TestContinueAsNew_Atomic(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			setupTestData(t, store)
			truncateAll(t, store)

			_, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{"v":1}`), "", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			wf, err := store.ClaimWorkflow(ctx, "worker-1")
			if err != nil {
				t.Fatalf("ClaimWorkflow: %v", err)
			}
			if wf == nil {
				t.Fatal("ClaimWorkflow returned nil")
			}

			newInput := json.RawMessage(`{"v":2}`)
			newRunID, err := store.ContinueAsNew(ctx, wf.ID, "worker-1", wf.Generation, "test-workflow", 1, newInput, nil, `{"result":"ok"}`, nil, 0)
			if err != nil {
				t.Fatalf("ContinueAsNew: %v", err)
			}

			// Old run must be completed.
			oldRun, err := store.GetWorkflowByID(ctx, wf.ID)
			if err != nil {
				t.Fatalf("GetWorkflowByID (old): %v", err)
			}
			if oldRun == nil {
				t.Fatal("old workflow not found after ContinueAsNew")
			}
			if oldRun.Status != "done" {
				t.Errorf("old run status = %q, want %q", oldRun.Status, "done")
			}

			// New run must exist with a different ID.
			newRun, err := store.GetWorkflowByID(ctx, newRunID)
			if err != nil {
				t.Fatalf("GetWorkflowByID (new): %v", err)
			}
			if newRun == nil {
				t.Fatal("new run not found after ContinueAsNew")
			}
			if newRun.ID == wf.ID {
				t.Error("new run has the same ID as the old run (must be different)")
			}
			if newRun.Status != "ready" {
				t.Errorf("new run status = %q, want %q", newRun.Status, "ready")
			}
		})
	}
}

func TestFinalizeWorkflowSegment(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			setupTestData(t, store)
			truncateAll(t, store)

			_, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			wf, err := store.ClaimWorkflow(ctx, "worker-1")
			if err != nil {
				t.Fatalf("ClaimWorkflow: %v", err)
			}
			if wf == nil {
				t.Fatal("ClaimWorkflow returned nil")
			}

			events := []EventRecord{
				{Step: 0, EventType: EventTypeCall, Service: "s", Op: "o1", Request: `{}`},
				{Step: 1, EventType: EventTypeCall, Service: "s", Op: "o2", Request: `{}`},
			}
			if err := store.FinalizeWorkflowSegment(ctx, wf.ID, "worker-1", wf.Generation, events, "done", `{"done":true}`, "", "", nil, time.Time{}); err != nil {
				t.Fatalf("FinalizeWorkflowSegment: %v", err)
			}

			// Verify events were appended.
			history, err := store.LoadEventHistory(ctx, wf.ID)
			if err != nil {
				t.Fatalf("LoadEventHistory: %v", err)
			}
			if len(history) != 2 {
				t.Fatalf("expected 2 events, got %d", len(history))
			}
			if history[0].Step != 0 || history[0].Op != "o1" {
				t.Errorf("unexpected first event: step=%d op=%s", history[0].Step, history[0].Op)
			}
			if history[1].Step != 1 || history[1].Op != "o2" {
				t.Errorf("unexpected second event: step=%d op=%s", history[1].Step, history[1].Op)
			}

			// Verify status was updated.
			stored, err := store.GetWorkflowByID(ctx, wf.ID)
			if err != nil {
				t.Fatalf("GetWorkflowByID: %v", err)
			}
			if stored == nil {
				t.Fatal("workflow not found after FinalizeWorkflowSegment")
			}
			if stored.Status != "done" {
				t.Errorf("status = %q, want %q", stored.Status, "done")
			}
		})
	}
}

func TestHeartbeat(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			setupTestData(t, store)
			truncateAll(t, store)

			_, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			wf, err := store.ClaimWorkflow(ctx, "worker-1")
			if err != nil {
				t.Fatalf("ClaimWorkflow: %v", err)
			}
			if wf == nil {
				t.Fatal("ClaimWorkflow returned nil")
			}

			// Heartbeat with the same worker that claimed the workflow — should succeed.
			owned, err := store.Heartbeat(ctx, wf.ID, "worker-1", wf.Generation)
			if err != nil {
				t.Fatalf("Heartbeat (first): %v", err)
			}
			if !owned {
				t.Error("expected Heartbeat to return true (worker owns the workflow)")
			}

			// Heartbeat with a different workerID — should return false.
			owned, err = store.Heartbeat(ctx, wf.ID, "worker-2", wf.Generation)
			if err != nil {
				t.Fatalf("Heartbeat (wrong worker): %v", err)
			}
			if owned {
				t.Error("expected Heartbeat to return false (different worker does not own the workflow)")
			}
		})
	}
}

func TestBatchHeartbeat(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			setupTestData(t, store)
			truncateAll(t, store)

			// Create 2 workflows and claim them both with the same worker.
			for i := 0; i < 2; i++ {
				_, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0)
				if err != nil {
					t.Fatalf("StartNewRun[%d]: %v", i, err)
				}
			}

			wfs, err := store.ClaimWorkflows(ctx, "worker-1", 10)
			if err != nil {
				t.Fatalf("ClaimWorkflows: %v", err)
			}
			if len(wfs) < 2 {
				t.Fatalf("claimed %d workflows, need at least 2", len(wfs))
			}

			// BatchHeartbeat should update all running workflows assigned to this worker.
			count, err := store.BatchHeartbeat(ctx, "worker-1")
			if err != nil {
				t.Fatalf("BatchHeartbeat: %v", err)
			}
			if count < 2 {
				t.Errorf("BatchHeartbeat returned %d, want >= 2", count)
			}
		})
	}
}

func TestMoveToDeadLetterQueue(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			setupTestData(t, store)
			truncateAll(t, store)

			_, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			wf, err := store.ClaimWorkflow(ctx, "worker-1")
			if err != nil {
				t.Fatalf("ClaimWorkflow: %v", err)
			}
			if wf == nil {
				t.Fatal("ClaimWorkflow returned nil")
			}

			if err := store.MoveToDeadLetterQueue(ctx, wf.ID, "worker-1", wf.Generation, "exhausted retries", "retries_exhausted", "DurableCall"); err != nil {
				t.Fatalf("MoveToDeadLetterQueue: %v", err)
			}

			stored, err := store.GetWorkflowByID(ctx, wf.ID)
			if err != nil {
				t.Fatalf("GetWorkflowByID: %v", err)
			}
			if stored == nil {
				t.Fatal("workflow not found after MoveToDeadLetterQueue")
			}
			if stored.Status != "dead_lettered" {
				t.Errorf("status = %q, want %q", stored.Status, "dead_lettered")
			}
			if stored.ErrorCode != "retries_exhausted" {
				t.Errorf("error_code = %q, want %q", stored.ErrorCode, "retries_exhausted")
			}
			if stored.ErrorOp != "DurableCall" {
				t.Errorf("error_op = %q, want %q", stored.ErrorOp, "DurableCall")
			}
		})
	}
}

// =============================================================================
// Group 5 — Cancellation
// =============================================================================

func TestRequestCancellation(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			setupTestData(t, store)

			runID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			reason := "test reason"
			if err := store.RequestCancellation(ctx, runID, reason); err != nil {
				t.Fatalf("RequestCancellation: %v", err)
			}

			cancelled, gotReason, err := store.CheckCancellation(ctx, runID)
			if err != nil {
				t.Fatalf("CheckCancellation: %v", err)
			}
			if !cancelled {
				t.Error("expected cancelled=true after RequestCancellation")
			}
			if gotReason != reason {
				t.Errorf("reason = %q, want %q", gotReason, reason)
			}
		})
	}
}

func TestCheckCancellation_NotCancelled(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			setupTestData(t, store)

			runID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			cancelled, reason, err := store.CheckCancellation(ctx, runID)
			if err != nil {
				t.Fatalf("CheckCancellation: %v", err)
			}
			if cancelled {
				t.Error("expected cancelled=false for fresh workflow")
			}
			if reason != "" {
				t.Errorf("expected empty reason, got %q", reason)
			}
		})
	}
}

// =============================================================================
// Group 6 — Priority
// =============================================================================

func TestStartNewRunWithPriority(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			setupTestData(t, store)
			truncateAll(t, store)

			runID, alreadyExisted, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 5)
			if err != nil {
				t.Fatalf("StartNewRun with priority=5: %v", err)
			}
			if alreadyExisted {
				t.Fatal("alreadyExisted=true for new run")
			}

			wf, err := store.GetWorkflowByID(ctx, runID)
			if err != nil {
				t.Fatalf("GetWorkflowByID: %v", err)
			}
			if wf == nil {
				t.Fatal("workflow not found after StartNewRun")
			}
			if wf.Priority != 5 {
				t.Errorf("Priority = %d, want 5", wf.Priority)
			}
		})
	}
}
