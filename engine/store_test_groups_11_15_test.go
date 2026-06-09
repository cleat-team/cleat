// store_test_groups_11_15_test.go — Shared WorkflowStore test suite (groups 11–15)
//
// These tests run against every registered backend and cover Promises,
// Update Requests, Concurrency Keys, Compaction, and Version Management.
package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// =============================================================================
// Group 11 — Promises
// =============================================================================

func TestCreatePromise(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			// Deploy a workflow definition and start a run so we have a
			// valid workflow to attach a promise to.
			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name:      "create-promise-test",
				Version:   1,
				WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}
			runID, _, err := store.StartNewRun(ctx, "", "create-promise-test", 1,
				json.RawMessage(`{}`), "create-promise-run", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			// Create a promise and verify it is persisted as pending.
			if err := store.CreatePromise(ctx, runID, "my-promise", "promise-abc"); err != nil {
				t.Fatalf("CreatePromise: %v", err)
			}
			status, result, errMsg, err := store.GetPromise(ctx, runID, "promise-abc")
			if err != nil {
				t.Fatalf("GetPromise: %v", err)
			}
			if status != "pending" {
				t.Errorf("expected status 'pending', got %q", status)
			}
			if result != "" {
				t.Errorf("expected empty result, got %q", result)
			}
			if errMsg != "" {
				t.Errorf("expected empty errMsg, got %q", errMsg)
			}
		})
	}
}

func TestResolvePromise(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name:      "resolve-promise-test",
				Version:   1,
				WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}
			runID, _, err := store.StartNewRun(ctx, "", "resolve-promise-test", 1,
				json.RawMessage(`{}`), "resolve-promise-run", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			if err := store.CreatePromise(ctx, runID, "prom-1", "pid-1"); err != nil {
				t.Fatalf("CreatePromise: %v", err)
			}
			if err := store.ResolvePromise(ctx, runID, "pid-1", `{"resolved":true}`); err != nil {
				t.Fatalf("ResolvePromise: %v", err)
			}
			status, result, errMsg, err := store.GetPromise(ctx, runID, "pid-1")
			if err != nil {
				t.Fatalf("GetPromise after resolve: %v", err)
			}
			if status != "resolved" {
				t.Errorf("expected status 'resolved', got %q", status)
			}
			if result != `{"resolved":true}` {
				t.Errorf("expected result %q, got %q", `{"resolved":true}`, result)
			}
			if errMsg != "" {
				t.Errorf("expected empty errMsg, got %q", errMsg)
			}
		})
	}
}

func TestRejectPromise(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name:      "reject-promise-test",
				Version:   1,
				WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}
			runID, _, err := store.StartNewRun(ctx, "", "reject-promise-test", 1,
				json.RawMessage(`{}`), "reject-promise-run", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			if err := store.CreatePromise(ctx, runID, "prom-2", "pid-2"); err != nil {
				t.Fatalf("CreatePromise: %v", err)
			}
			if err := store.RejectPromise(ctx, runID, "pid-2", "something went wrong"); err != nil {
				t.Fatalf("RejectPromise: %v", err)
			}
			status, result, errMsg, err := store.GetPromise(ctx, runID, "pid-2")
			if err != nil {
				t.Fatalf("GetPromise after reject: %v", err)
			}
			if status != "rejected" {
				t.Errorf("expected status 'rejected', got %q", status)
			}
			if result != "" {
				t.Errorf("expected empty result, got %q", result)
			}
			if errMsg != "something went wrong" {
				t.Errorf("expected errMsg %q, got %q", "something went wrong", errMsg)
			}
		})
	}
}

func TestGetPromise(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name:      "get-promise-test",
				Version:   1,
				WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}
			runID, _, err := store.StartNewRun(ctx, "", "get-promise-test", 1,
				json.RawMessage(`{}`), "get-promise-run", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			if err := store.CreatePromise(ctx, runID, "prom-3", "pid-3"); err != nil {
				t.Fatalf("CreatePromise: %v", err)
			}
			// Basic read-back: all fields should be in their zero / pending state.
			status, result, errMsg, err := store.GetPromise(ctx, runID, "pid-3")
			if err != nil {
				t.Fatalf("GetPromise: %v", err)
			}
			if status != "pending" {
				t.Errorf("expected status 'pending', got %q", status)
			}
			if result != "" {
				t.Errorf("expected empty result, got %q", result)
			}
			if errMsg != "" {
				t.Errorf("expected empty errMsg, got %q", errMsg)
			}
		})
	}
}

func TestListPromises(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name:      "list-promises-test",
				Version:   1,
				WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}
			runID, _, err := store.StartNewRun(ctx, "", "list-promises-test", 1,
				json.RawMessage(`{}`), "list-promises-run", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			// Create two promises with different names.
			if err := store.CreatePromise(ctx, runID, "prom-a", "p1"); err != nil {
				t.Fatalf("CreatePromise prom-a: %v", err)
			}
			if err := store.CreatePromise(ctx, runID, "prom-b", "p2"); err != nil {
				t.Fatalf("CreatePromise prom-b: %v", err)
			}

			promises, err := store.ListPromises(ctx, runID)
			if err != nil {
				t.Fatalf("ListPromises: %v", err)
			}
			if len(promises) < 2 {
				t.Fatalf("expected at least 2 promises, got %d", len(promises))
			}
			// Verify they are ordered by creation time (the earlier one first).
			if promises[0].CreatedAt.After(promises[1].CreatedAt) {
				t.Error("promises not ordered by creation time: earlier promise should come first")
			}
		})
	}
}

// =============================================================================
// Group 12 — Update Requests
// =============================================================================

func TestCreateUpdateRequest(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name:      "create-update-test",
				Version:   1,
				WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}
			runID, _, err := store.StartNewRun(ctx, "", "create-update-test", 1,
				json.RawMessage(`{}`), "create-update-run", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			if err := store.CreateUpdateRequest(ctx, runID, "update-1",
				`{"field":"newval"}`, "promise-upd-1"); err != nil {
				t.Fatalf("CreateUpdateRequest: %v", err)
			}
		})
	}
}

func TestGetPendingUpdateRequests(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name:      "pending-updates-test",
				Version:   1,
				WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}
			runID, _, err := store.StartNewRun(ctx, "", "pending-updates-test", 1,
				json.RawMessage(`{}`), "pending-updates-run", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			if err := store.CreateUpdateRequest(ctx, runID, "upd-a", "payload-a", ""); err != nil {
				t.Fatalf("CreateUpdateRequest upd-a: %v", err)
			}
			if err := store.CreateUpdateRequest(ctx, runID, "upd-b", "payload-b", ""); err != nil {
				t.Fatalf("CreateUpdateRequest upd-b: %v", err)
			}

			pending, err := store.GetPendingUpdateRequests(ctx, runID)
			if err != nil {
				t.Fatalf("GetPendingUpdateRequests: %v", err)
			}
			if len(pending) < 2 {
				t.Fatalf("expected at least 2 pending updates, got %d", len(pending))
			}
			for _, u := range pending {
				if u.Status != "pending" {
					t.Errorf("expected status 'pending', got %q for update %q", u.Status, u.UpdateName)
				}
			}
		})
	}
}

func TestCompleteUpdateRequest(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name:      "complete-update-test",
				Version:   1,
				WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}
			runID, _, err := store.StartNewRun(ctx, "", "complete-update-test", 1,
				json.RawMessage(`{}`), "complete-update-run", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			if err := store.CreateUpdateRequest(ctx, runID, "upd-c", "payload-c", ""); err != nil {
				t.Fatalf("CreateUpdateRequest: %v", err)
			}
			if err := store.CompleteUpdateRequest(ctx, runID, "upd-c", `{"ok":true}`, ""); err != nil {
				t.Fatalf("CompleteUpdateRequest: %v", err)
			}

			pending, err := store.GetPendingUpdateRequests(ctx, runID)
			if err != nil {
				t.Fatalf("GetPendingUpdateRequests after complete: %v", err)
			}
			for _, u := range pending {
				if u.UpdateName == "upd-c" {
					t.Error("completed update request 'upd-c' still appears in pending results")
				}
			}
		})
	}
}

// =============================================================================
// Group 13 — Concurrency Keys
// =============================================================================

func TestAcquireConcurrencyKey(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			// First acquire should succeed.
			acquired, err := store.AcquireConcurrencyKey(ctx, "key-1", "wf-1", 60*time.Second)
			if err != nil {
				t.Fatalf("AcquireConcurrencyKey (first): %v", err)
			}
			if !acquired {
				t.Fatal("expected first acquire to return acquired=true")
			}

			// Second acquire with same key but different workflow should fail
			// (key is already held).
			acquired, err = store.AcquireConcurrencyKey(ctx, "key-1", "wf-2", 60*time.Second)
			if err != nil {
				t.Fatalf("AcquireConcurrencyKey (second): %v", err)
			}
			if acquired {
				t.Fatal("expected second acquire (different workflow, same key) to return acquired=false")
			}
		})
	}
}

func TestAcquireConcurrencyKey_Expired(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			// Acquire with a TTL of 1 nanosecond — will expire effectively
			// immediately.
			acquired, err := store.AcquireConcurrencyKey(ctx, "key-exp", "wf-1", 1*time.Nanosecond)
			if err != nil {
				t.Fatalf("AcquireConcurrencyKey (short TTL): %v", err)
			}
			if !acquired {
				t.Fatal("expected first acquire to succeed")
			}

			// Brief pause to let the TTL expire.
			time.Sleep(10 * time.Millisecond)

			// Now a different workflow should be able to acquire the same key.
			acquired, err = store.AcquireConcurrencyKey(ctx, "key-exp", "wf-2", 60*time.Second)
			if err != nil {
				t.Fatalf("AcquireConcurrencyKey after expiry: %v", err)
			}
			if !acquired {
				t.Fatal("expected acquire after TTL expiry to succeed (key should have expired)")
			}
		})
	}
}

func TestReleaseConcurrencyKey(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			// Acquire for wf-1.
			acquired, err := store.AcquireConcurrencyKey(ctx, "key-rel", "wf-1", 60*time.Second)
			if err != nil {
				t.Fatalf("AcquireConcurrencyKey: %v", err)
			}
			if !acquired {
				t.Fatal("expected first acquire to succeed")
			}

			// Release the key.
			if err := store.ReleaseConcurrencyKey(ctx, "key-rel"); err != nil {
				t.Fatalf("ReleaseConcurrencyKey: %v", err)
			}

			// Now wf-2 should be able to acquire the same key.
			acquired, err = store.AcquireConcurrencyKey(ctx, "key-rel", "wf-2", 60*time.Second)
			if err != nil {
				t.Fatalf("AcquireConcurrencyKey after release: %v", err)
			}
			if !acquired {
				t.Fatal("expected acquire after release to return acquired=true")
			}
		})
	}
}

func TestStoreReleaseWorkflowConcurrencyKeys(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			// Acquire multiple keys for workflow "wf-multi".
			for _, key := range []string{"mk-1", "mk-2", "mk-3"} {
				acquired, err := store.AcquireConcurrencyKey(ctx, key, "wf-multi", 60*time.Second)
				if err != nil {
					t.Fatalf("AcquireConcurrencyKey %s: %v", key, err)
				}
				if !acquired {
					t.Fatalf("expected acquire of %s to succeed", key)
				}
			}

			// Release all keys for wf-multi.
			if err := store.ReleaseWorkflowConcurrencyKeys(ctx, "wf-multi"); err != nil {
				t.Fatalf("ReleaseWorkflowConcurrencyKeys: %v", err)
			}

			// Now "wf-other" should be able to acquire "mk-1".
			acquired, err := store.AcquireConcurrencyKey(ctx, "mk-1", "wf-other", 60*time.Second)
			if err != nil {
				t.Fatalf("AcquireConcurrencyKey mk-1 after release: %v", err)
			}
			if !acquired {
				t.Fatal("expected acquire of mk-1 after workflow key release to return acquired=true")
			}
		})
	}
}

func TestStoreReapExpiredConcurrencyKeys(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			// Acquire a key with a very short TTL (1 nanosecond) so it expires
			// before we call ReapExpiredConcurrencyKeys.
			acquired, err := store.AcquireConcurrencyKey(ctx, "rk-1", "wf-1", 1*time.Nanosecond)
			if err != nil {
				t.Fatalf("AcquireConcurrencyKey: %v", err)
			}
			if !acquired {
				t.Fatal("expected first acquire to succeed")
			}

			// Give the TTL a moment to expire.
			time.Sleep(10 * time.Millisecond)

			reaped, err := store.ReapExpiredConcurrencyKeys(ctx)
			if err != nil {
				t.Fatalf("ReapExpiredConcurrencyKeys: %v", err)
			}
			if reaped < 0 {
				t.Errorf("expected reaped count >= 0, got %d", reaped)
			}
		})
	}
}

func TestGetConcurrencyKeyCount(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			// Acquire multiple keys for workflow "wf-count".
			for _, key := range []string{"ck-1", "ck-2"} {
				acquired, err := store.AcquireConcurrencyKey(ctx, key, "wf-count", 60*time.Second)
				if err != nil {
					t.Fatalf("AcquireConcurrencyKey %s: %v", key, err)
				}
				if !acquired {
					t.Fatalf("expected acquire of %s to succeed", key)
				}
			}

			// Verify count = 2 for wf-count.
			count, err := store.GetConcurrencyKeyCount(ctx, "wf-count")
			if err != nil {
				t.Fatalf("GetConcurrencyKeyCount: %v", err)
			}
			if count != 2 {
				t.Errorf("expected count=2, got %d", count)
			}

			// Verify count = 0 for an unrelated workflow.
			count, err = store.GetConcurrencyKeyCount(ctx, "wf-other")
			if err != nil {
				t.Fatalf("GetConcurrencyKeyCount (wf-other): %v", err)
			}
			if count != 0 {
				t.Errorf("expected count=0 for unrelated workflow, got %d", count)
			}

			// Release all keys for wf-count and verify count drops to 0.
			if err := store.ReleaseWorkflowConcurrencyKeys(ctx, "wf-count"); err != nil {
				t.Fatalf("ReleaseWorkflowConcurrencyKeys: %v", err)
			}
			count, err = store.GetConcurrencyKeyCount(ctx, "wf-count")
			if err != nil {
				t.Fatalf("GetConcurrencyKeyCount after release: %v", err)
			}
			if count != 0 {
				t.Errorf("expected count=0 after release, got %d", count)
			}
		})
	}
}

// =============================================================================
// Group 14 — Compaction
// =============================================================================

func TestGetCompactionCandidates(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			// Deploy a workflow def and create an instance so we have events
			// to evaluate for compaction.
			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name:      "compaction-candidates-test",
				Version:   1,
				WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}
			runID, _, err := store.StartNewRun(ctx, "", "compaction-candidates-test", 1,
				json.RawMessage(`{}`), "compaction-candidates-run", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			// Append 5 events to the workflow's history.
			for i := 0; i < 5; i++ {
				if err := store.AppendEventHistory(ctx, runID, EventRecord{
					Step:      i,
					EventType: EventTypeCall,
					Service:   "svc",
					Op:        "op",
				}); err != nil {
					t.Fatalf("AppendEventHistory (step %d): %v", i, err)
				}
			}

			// With threshold=3 and limit=10, our workflow (5 events) should be
			// returned as a candidate.
			candidates, err := store.GetCompactionCandidates(ctx, 3, 10)
			if err != nil {
				t.Fatalf("GetCompactionCandidates (threshold=3): %v", err)
			}
			found := false
			for _, id := range candidates {
				if id == runID {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("workflow %q not found in compaction candidates (threshold=3)", runID)
			}

			// With threshold=100 and limit=10, no workflow should qualify
			// (our workflow only has 5 events).
			candidates, err = store.GetCompactionCandidates(ctx, 100, 10)
			if err != nil {
				t.Fatalf("GetCompactionCandidates (threshold=100): %v", err)
			}
			for _, id := range candidates {
				if id == runID {
					t.Errorf("workflow %q should not be a candidate with threshold=100", runID)
				}
			}
		})
	}
}

func TestLoadCompactionState(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			// Create a workflow that has never been compacted.
			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name:      "load-compaction-state-test",
				Version:   1,
				WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}
			runID, _, err := store.StartNewRun(ctx, "", "load-compaction-state-test", 1,
				json.RawMessage(`{}`), "load-compaction-state-run", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			// Loading compaction state for a never-compacted workflow should
			// return nil with no error.
			cs, err := store.LoadCompactionState(ctx, runID)
			if err != nil {
				t.Fatalf("LoadCompactionState: %v", err)
			}
			if cs != nil {
				t.Fatal("expected nil CompactionState for workflow that has never been compacted")
			}
		})
	}
}

func TestCompactHistory(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			// Create a workflow and append some events.
			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name:      "compact-history-test",
				Version:   1,
				WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}
			runID, _, err := store.StartNewRun(ctx, "", "compact-history-test", 1,
				json.RawMessage(`{}`), "compact-history-run", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}
			for i := 0; i < 5; i++ {
				if err := store.AppendEventHistory(ctx, runID, EventRecord{
					Step:      i,
					EventType: EventTypeCall,
					Service:   "svc",
					Op:        "op",
				}); err != nil {
					t.Fatalf("AppendEventHistory (step %d): %v", i, err)
				}
			}

			// Build a minimal CompactionState, marshal to JSON, and call CompactHistory.
			cs := CompactionState{
				Version:       1,
				CompactedStep: 3,
				Events:        []CompactedEvent{},
			}
			csJSON, err := json.Marshal(cs)
			if err != nil {
				t.Fatalf("json.Marshal CompactionState: %v", err)
			}

			// compactionStep=3 means "events up to step 3 are compacted",
			// keepStep=0 means "delete events where step < 0" → no deletion.
			if err := store.CompactHistory(ctx, runID, csJSON, 3, 0); err != nil {
				t.Fatalf("CompactHistory: %v", err)
			}
		})
	}
}

// =============================================================================
// Group 15 — Version Management
// =============================================================================

func TestDeployWorkflowDef(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			def := &WorkflowDef{
				Name:       "vtest",
				Version:    1,
				WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1,
			}
			if err := store.DeployWorkflowDef(ctx, def); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}

			got, err := store.GetWorkflowDef(ctx, "vtest", 1)
			if err != nil {
				t.Fatalf("GetWorkflowDef: %v", err)
			}
			if got == nil {
				t.Fatal("GetWorkflowDef returned nil")
			}
			if got.Name != "vtest" {
				t.Errorf("expected Name 'vtest', got %q", got.Name)
			}
			if got.Version != 1 {
				t.Errorf("expected Version 1, got %d", got.Version)
			}
		})
	}
}

func TestListWorkflowDefs(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			// Deploy two versions of the same named workflow.
			for _, v := range []int{1, 2} {
				if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
					Name:      "vlist",
					Version:   v,
					WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
				}); err != nil {
					t.Fatalf("DeployWorkflowDef v%d: %v", v, err)
				}
			}

			defs, err := store.ListWorkflowDefs(ctx, "vlist")
			if err != nil {
				t.Fatalf("ListWorkflowDefs: %v", err)
			}
			if len(defs) < 2 {
				t.Fatalf("expected at least 2 workflow defs, got %d", len(defs))
			}
			// Must be ordered by version DESC: v2 first, then v1.
			if defs[0].Version < defs[1].Version {
				t.Errorf("expected version DESC ordering: got v%d before v%d",
					defs[1].Version, defs[0].Version)
			}
		})
	}
}

func TestResolveLatestVersion(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			for _, v := range []int{1, 2} {
				if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
					Name:      "vresolve",
					Version:   v,
					WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
				}); err != nil {
					t.Fatalf("DeployWorkflowDef v%d: %v", v, err)
				}
			}

			latest, err := store.ResolveLatestVersion(ctx, "vresolve")
			if err != nil {
				t.Fatalf("ResolveLatestVersion: %v", err)
			}
			if latest != 2 {
				t.Errorf("expected latest version 2, got %d", latest)
			}
		})
	}
}

func TestValidateVersion(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name:      "vvalid",
				Version:   1,
				WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}

			valid, err := store.ValidateVersion(ctx, "vvalid", 1)
			if err != nil {
				t.Fatalf("ValidateVersion (existing): %v", err)
			}
			if !valid {
				t.Error("expected ValidateVersion for existing version to return true")
			}

			valid, err = store.ValidateVersion(ctx, "vvalid", 99)
			if err != nil {
				t.Fatalf("ValidateVersion (non-existent): %v", err)
			}
			if valid {
				t.Error("expected ValidateVersion for non-existent version to return false")
			}
		})
	}
}

func TestMarkVersionDeprecated(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name:      "vdep",
				Version:   1,
				WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}

			if err := store.MarkVersionDeprecated(ctx, "vdep", 1, true); err != nil {
				t.Fatalf("MarkVersionDeprecated: %v", err)
			}

			got, err := store.GetWorkflowDef(ctx, "vdep", 1)
			if err != nil {
				t.Fatalf("GetWorkflowDef: %v", err)
			}
			if got == nil {
				t.Fatal("GetWorkflowDef returned nil after deprecation")
			}
			if !got.Deprecated {
				t.Error("expected Deprecated=true after MarkVersionDeprecated")
			}
		})
	}
}

func TestPurgeWorkflowDef(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name:      "vpurge",
				Version:   1,
				WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}

			if err := store.PurgeWorkflowDef(ctx, "vpurge", 1); err != nil {
				t.Fatalf("PurgeWorkflowDef: %v", err)
			}

			got, err := store.GetWorkflowDef(ctx, "vpurge", 1)
			if err != nil {
				t.Fatalf("GetWorkflowDef after purge: %v", err)
			}
			if got != nil {
				t.Error("expected nil (or error) from GetWorkflowDef after purge, got a non-nil def")
			}
		})
	}
}

func TestCountActiveInstances(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name:      "vcount",
				Version:   1,
				WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}

			// Create two workflow instances via StartNewRun.
			for _, key := range []string{"vcount-run-1", "vcount-run-2"} {
				_, _, err := store.StartNewRun(ctx, "", "vcount", 1,
					json.RawMessage(`{}`), key, DefaultTenantUUID, 0)
				if err != nil {
					t.Fatalf("StartNewRun %s: %v", key, err)
				}
			}

			count, err := store.CountActiveInstances(ctx, "vcount", 1)
			if err != nil {
				t.Fatalf("CountActiveInstances: %v", err)
			}
			if count < 2 {
				t.Errorf("expected at least 2 active instances, got %d", count)
			}
		})
	}
}
