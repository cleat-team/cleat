package host

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Group 16 — Query State
// ---------------------------------------------------------------------------

// TestGetQueryState verifies GetQueryState returns the query_state value
// stored via CompleteWorkflow, and returns empty string for missing keys.
func TestGetQueryState(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			t.Parallel()
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			ctx := context.Background()

			// Create a fresh workflow instance.
			runID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1,
				json.RawMessage(`{}`), "qstate-test-1", DefaultTenantUUID)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			// Claim all ready workflows so ours is assigned to a worker.
			claimed, err := store.ClaimWorkflows(ctx, "qstate-worker", 100)
			if err != nil {
				t.Fatalf("ClaimWorkflows: %v", err)
			}

			// Locate our workflow in the claimed set.
			var target *WorkflowInstance
			for _, wf := range claimed {
				if wf.ID == runID {
					target = wf
					break
				}
			}
			if target == nil {
				t.Fatal("did not claim our workflow instance")
			}

			// Complete the workflow with query state.
			err = store.CompleteWorkflow(ctx, target.ID, "qstate-worker", target.Generation,
				`{"result":"ok"}`, map[string]string{"mykey": "myval"})
			if err != nil {
				t.Fatalf("CompleteWorkflow: %v", err)
			}

			// Retrieve an existing key.
			val, err := store.GetQueryState(ctx, target.ID, "mykey")
			if err != nil {
				t.Fatalf("GetQueryState: %v", err)
			}
			if val != "myval" {
				t.Errorf("expected query state 'myval', got %q", val)
			}

			// Retrieve a non-existent key — must return empty string.
			val, err = store.GetQueryState(ctx, target.ID, "nonexistent")
			if err != nil {
				t.Fatalf("GetQueryState (nonexistent): %v", err)
			}
			if val != "" {
				t.Errorf("expected empty string for nonexistent key, got %q", val)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Group 17 — Sticky Sessions
// ---------------------------------------------------------------------------

// TestUpdateStickyWorker verifies that a sticky worker can be assigned.
func TestUpdateStickyWorker(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			t.Parallel()
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			ctx := context.Background()

			runID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1,
				json.RawMessage(`{}`), "sticky-upd-1", DefaultTenantUUID)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			err = store.UpdateStickyWorker(ctx, runID, "sticky-w1")
			if err != nil {
				t.Errorf("UpdateStickyWorker: %v", err)
			}
		})
	}
}

// TestClearStickyWorker verifies a sticky worker assignment can be removed.
func TestClearStickyWorker(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			t.Parallel()
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			ctx := context.Background()

			runID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1,
				json.RawMessage(`{}`), "sticky-clr-1", DefaultTenantUUID)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			err = store.UpdateStickyWorker(ctx, runID, "sticky-w2")
			if err != nil {
				t.Fatalf("UpdateStickyWorker: %v", err)
			}

			err = store.ClearStickyWorker(ctx, runID)
			if err != nil {
				t.Errorf("ClearStickyWorker: %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Group 18 — WASM Loading
// ---------------------------------------------------------------------------

// TestLoadWASM_RoundTrip deploys a workflow definition with known WASM bytes
// and verifies LoadWASM returns them exactly as stored.
func TestLoadWASM_RoundTrip(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			t.Parallel()
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			ctx := context.Background()

			wantWASM := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
			def := &WorkflowDef{
				Name:       "wasm-test",
				Version:    1,
				WASMBytes:  wantWASM,
				ABIVersion: 1,
				MinVersion: 1,
			}
			if err := store.DeployWorkflowDef(ctx, def); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}

			got, err := store.LoadWASM(ctx, "wasm-test", 1)
			if err != nil {
				t.Fatalf("LoadWASM: %v", err)
			}
			if len(got) != len(wantWASM) {
				t.Fatalf("WASM length mismatch: got %d, want %d", len(got), len(wantWASM))
			}
			for i := range got {
				if got[i] != wantWASM[i] {
					t.Fatalf("WASM bytes differ at offset %d: got 0x%02x, want 0x%02x",
						i, got[i], wantWASM[i])
				}
			}
		})
	}
}

// TestLoadWASM_NotFound verifies LoadWASM returns an error for a
// nonexistent workflow definition.
func TestLoadWASM_NotFound(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			t.Parallel()
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			ctx := context.Background()

			_, err := store.LoadWASM(ctx, "nonexistent", 1)
			if err == nil {
				t.Error("expected error for nonexistent WASM def")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Group 19 — Memory Stats
// ---------------------------------------------------------------------------

// TestRecordWorkflowMemorySample verifies that a memory sample can be
// recorded without error.
func TestRecordWorkflowMemorySample(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			t.Parallel()
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			ctx := context.Background()

			def := &WorkflowDef{
				Name:       "mem-test",
				Version:    1,
				WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1,
				MinVersion: 1,
			}
			if err := store.DeployWorkflowDef(ctx, def); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}

			err := store.RecordWorkflowMemorySample(ctx, "mem-test", 1024*1024)
			if err != nil {
				t.Errorf("RecordWorkflowMemorySample: %v", err)
			}
		})
	}
}

// TestLoadMemoryEstimates verifies that after recording a sample,
// LoadMemoryEstimates contains the expected def name with a positive
// float64 value.
func TestLoadMemoryEstimates(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			t.Parallel()
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			ctx := context.Background()

			def := &WorkflowDef{
				Name:       "mem-test",
				Version:    1,
				WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1,
				MinVersion: 1,
			}
			if err := store.DeployWorkflowDef(ctx, def); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}

			err := store.RecordWorkflowMemorySample(ctx, "mem-test", 2048)
			if err != nil {
				t.Fatalf("RecordWorkflowMemorySample: %v", err)
			}

			estimates, err := store.LoadMemoryEstimates(ctx)
			if err != nil {
				t.Fatalf("LoadMemoryEstimates: %v", err)
			}

			val, ok := estimates["mem-test"]
			if !ok {
				t.Error("LoadMemoryEstimates missing 'mem-test' key")
			} else if val <= 0 {
				t.Errorf("expected positive memory estimate for 'mem-test', got %f", val)
			}
		})
	}
}

// TestCleanupMemorySamples verifies that cleanup removes excess samples
// beyond maxSamplesPerDef without error.
func TestCleanupMemorySamples(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			t.Parallel()
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			ctx := context.Background()

			def := &WorkflowDef{
				Name:       "mem-test",
				Version:    1,
				WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1,
				MinVersion: 1,
			}
			if err := store.DeployWorkflowDef(ctx, def); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}

			// Record three samples for the same def.
			for i := 0; i < 3; i++ {
				err := store.RecordWorkflowMemorySample(ctx, "mem-test", 1024)
				if err != nil {
					t.Fatalf("RecordWorkflowMemorySample #%d: %v", i, err)
				}
			}

			// Cleanup retaining only 1 sample per def.
			deleted, err := store.CleanupMemorySamples(ctx, 1)
			if err != nil {
				t.Errorf("CleanupMemorySamples: %v", err)
			}
			if deleted < 0 {
				t.Errorf("expected non-negative cleanup count, got %d", deleted)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Group 20 — Maintenance
// ---------------------------------------------------------------------------

// TestQueueDepth verifies QueueDepth reports at least the number of
// ready workflow instances.
func TestQueueDepth(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			t.Parallel()
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			ctx := context.Background()

			// Create several ready workflows on top of the one from setupTestData.
			for i := 0; i < 3; i++ {
				key := "qd-test-" + string(rune('0'+i))
				_, _, err := store.StartNewRun(ctx, "", "test-workflow", 1,
					json.RawMessage(`{}`), key, DefaultTenantUUID)
				if err != nil {
					t.Fatalf("StartNewRun #%d: %v", i, err)
				}
			}

			depth, err := store.QueueDepth(ctx)
			if err != nil {
				t.Fatalf("QueueDepth: %v", err)
			}
			if depth < 3 {
				t.Errorf("expected queue depth >= 3, got %d", depth)
			}
		})
	}
}

// TestDeleteExpiredEvents verifies that expired events can be cleaned
// up without error after completing a workflow.
func TestDeleteExpiredEvents(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			t.Parallel()
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			ctx := context.Background()

			// Create a workflow and complete it so it becomes a terminal
			// (expired) candidate.
			runID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1,
				json.RawMessage(`{}`), "expired-test-1", DefaultTenantUUID)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			claimed, err := store.ClaimWorkflows(ctx, "expired-worker", 100)
			if err != nil {
				t.Fatalf("ClaimWorkflows: %v", err)
			}

			var target *WorkflowInstance
			for _, wf := range claimed {
				if wf.ID == runID {
					target = wf
					break
				}
			}
			if target == nil {
				t.Fatal("did not claim our workflow instance")
			}

			err = store.CompleteWorkflow(ctx, target.ID, "expired-worker", target.Generation,
				`{"result":"done"}`, nil)
			if err != nil {
				t.Fatalf("CompleteWorkflow: %v", err)
			}

			// All terminal workflows with completed_at before a time in the
			// future are considered expired.
			count, err := store.DeleteExpiredEvents(ctx, time.Now().Add(1*time.Hour))
			if err != nil {
				t.Errorf("DeleteExpiredEvents: %v", err)
			}
			if count < 0 {
				t.Errorf("expected non-negative delete count, got %d", count)
			}
		})
	}
}
