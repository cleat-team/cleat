package host

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// MultiTenantStoreBackend extends StoreBackend for backends that support
// creating stores scoped to specific tenants (used by tenant isolation tests).
type MultiTenantStoreBackend interface {
	StoreBackend
	// SetupForTenant creates a WorkflowStore scoped to the given tenant ID.
	SetupForTenant(t *testing.T, tenantID string) (WorkflowStore, func())
}

// forEachBackend runs a test function against every registered backend.
func forEachBackend(t *testing.T, fn func(t *testing.T, store WorkflowStore)) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			if !backend.Enabled() {
				t.Skipf("%s backend not enabled", backend.Name())
			}
			store, teardown := backend.Setup(t)
			defer teardown()
			fn(t, store)
		})
	}
}

// TestTenantSelfAccess verifies that a store can create data and then read
// back its own workflows via GetWorkflowByID and ListWorkflows. This is a
// basic smoke test that runs against every registered backend.
func TestTenantSelfAccess(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()

		// Deploy a workflow definition (visible to all tenants).
		def := &WorkflowDef{
			Name:       "test-isolation",
			Version:    1,
			WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d},
			ABIVersion: 1,
			MinVersion: 1,
		}
		if err := store.DeployWorkflowDef(ctx, def); err != nil {
			t.Fatalf("DeployWorkflowDef: %v", err)
		}

		// Start a workflow as the default tenant (from Setup).
		runID, _, err := store.StartNewRun(ctx, "", "test-isolation", 1,
			json.RawMessage(`{"owner":"tenant-a"}`),
			"idempotency-tenant-a-1", DefaultTenantUUID)
		if err != nil {
			t.Fatalf("StartNewRun: %v", err)
		}

		// Verify the store can see its own workflow.
		wf, err := store.GetWorkflowByID(ctx, runID)
		if err != nil {
			t.Fatalf("GetWorkflowByID: %v", err)
		}
		if wf == nil {
			t.Fatal("store cannot see its own workflow")
		}

		// Verify the created workflow appears in listings.
		filter := WorkflowFilter{Limit: 100}
		workflows, err := store.ListWorkflows(ctx, filter)
		if err != nil {
			t.Fatalf("ListWorkflows: %v", err)
		}

		found := false
		for _, w := range workflows {
			if w.ID == runID {
				found = true
				break
			}
		}
		if !found {
			t.Error("created workflow not found in ListWorkflows")
		}

		// Cleanup: claim and complete the workflow.
		claimed, err := store.ClaimWorkflow(ctx, "test-worker")
		if err != nil {
			t.Fatalf("ClaimWorkflow: %v", err)
		}
		if claimed != nil {
			store.CompleteWorkflow(ctx, runID, "test-worker", 0, `{"done":true}`, nil)
		}
	})
}

// TestTenantIsolationWithSeparateStores verifies cross-tenant isolation by
// creating two stores for two different tenants and checking that data from
// one is invisible to the other.
func TestTenantIsolationWithSeparateStores(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			if !backend.Enabled() {
				t.Skipf("%s backend not enabled", backend.Name())
			}

			mtBackend, ok := backend.(MultiTenantStoreBackend)
			if !ok {
				t.Skipf("%s backend does not support multi-tenant store creation", backend.Name())
			}

			storeA, teardownA := mtBackend.SetupForTenant(t, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
			defer teardownA()
			storeB, teardownB := mtBackend.SetupForTenant(t, "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
			defer teardownB()

			ctx := context.Background()

			// Deploy a workflow definition in store A.
			def := &WorkflowDef{
				Name:       "test-isolation",
				Version:    1,
				WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1,
				MinVersion: 1,
			}
			if err := storeA.DeployWorkflowDef(ctx, def); err != nil {
				t.Fatalf("DeployWorkflowDef on store A: %v", err)
			}
			// Also deploy in store B (needed to create instances).
			if err := storeB.DeployWorkflowDef(ctx, def); err != nil {
				t.Fatalf("DeployWorkflowDef on store B: %v", err)
			}

			// Create a workflow in store A.
			runIDA, _, err := storeA.StartNewRun(ctx, "", "test-isolation", 1,
				json.RawMessage(`{"owner":"tenant-a"}`),
				"iso-test-a-1", DefaultTenantUUID)
			if err != nil {
				t.Fatalf("StartNewRun on store A: %v", err)
			}

			// Create a workflow in store B.
			runIDB, _, err := storeB.StartNewRun(ctx, "", "test-isolation", 1,
				json.RawMessage(`{"owner":"tenant-b"}`),
				"iso-test-b-1", DefaultTenantUUID)
			if err != nil {
				t.Fatalf("StartNewRun on store B: %v", err)
			}

			// Store A should see its workflow but not B's.
			wfA, err := storeA.GetWorkflowByID(ctx, runIDA)
			if err != nil || wfA == nil {
				t.Fatalf("store A cannot see its own workflow: err=%v, wf=%v", err, wfA)
			}

			wfBfromA, _ := storeA.GetWorkflowByID(ctx, runIDB)
			if wfBfromA != nil {
				t.Errorf("ISOLATION BREACH: store A can see tenant B's workflow %s", runIDB)
			}

			// Store B should see its workflow but not A's.
			wfB, err := storeB.GetWorkflowByID(ctx, runIDB)
			if err != nil || wfB == nil {
				t.Fatalf("store B cannot see its own workflow: err=%v, wf=%v", err, wfB)
			}

			wfAfromB, _ := storeB.GetWorkflowByID(ctx, runIDA)
			if wfAfromB != nil {
				t.Errorf("ISOLATION BREACH: store B can see tenant A's workflow %s", runIDA)
			}

			// Store A listing should not contain B's workflow.
			listA, err := storeA.ListWorkflows(ctx, WorkflowFilter{Limit: 100})
			if err != nil {
				t.Fatalf("ListWorkflows on store A: %v", err)
			}
			for _, w := range listA {
				if w.ID == runIDB {
					t.Errorf("ISOLATION BREACH: store A's ListWorkflows includes tenant B's workflow %s", runIDB)
				}
			}

			// Store B listing should not contain A's workflow.
			listB, err := storeB.ListWorkflows(ctx, WorkflowFilter{Limit: 100})
			if err != nil {
				t.Fatalf("ListWorkflows on store B: %v", err)
			}
			for _, w := range listB {
				if w.ID == runIDA {
					t.Errorf("ISOLATION BREACH: store B's ListWorkflows includes tenant A's workflow %s", runIDA)
				}
			}

			// Cleanup.
			claimedA, _ := storeA.ClaimWorkflow(ctx, "test-worker")
			if claimedA != nil {
				storeA.CompleteWorkflow(ctx, runIDA, "test-worker", 0, `{"done":true}`, nil)
			}
			claimedB, _ := storeB.ClaimWorkflow(ctx, "test-worker")
			if claimedB != nil {
				storeB.CompleteWorkflow(ctx, runIDB, "test-worker", 0, `{"done":true}`, nil)
			}
		})
	}
}

// TestTenantIsolation_Signals verifies that signals delivered to one tenant's
// workflow are not visible to another tenant.
func TestTenantIsolation_Signals(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			if !backend.Enabled() {
				t.Skipf("%s backend not enabled", backend.Name())
			}

			mtBackend, ok := backend.(MultiTenantStoreBackend)
			if !ok {
				t.Skipf("%s backend does not support multi-tenant store creation", backend.Name())
			}

			storeA, teardownA := mtBackend.SetupForTenant(t, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
			defer teardownA()
			storeB, teardownB := mtBackend.SetupForTenant(t, "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
			defer teardownB()

			ctx := context.Background()

			// Deploy workflow def in both stores.
			def := &WorkflowDef{
				Name:       "test-signals",
				Version:    1,
				WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1,
				MinVersion: 1,
			}
			if err := storeA.DeployWorkflowDef(ctx, def); err != nil {
				t.Fatalf("DeployWorkflowDef on store A: %v", err)
			}
			if err := storeB.DeployWorkflowDef(ctx, def); err != nil {
				t.Fatalf("DeployWorkflowDef on store B: %v", err)
			}

			// Create a workflow in store A.
			runIDA, _, err := storeA.StartNewRun(ctx, "", "test-signals", 1,
				json.RawMessage(`{"owner":"tenant-a"}`),
				"signal-test-a-1", DefaultTenantUUID)
			if err != nil {
				t.Fatalf("StartNewRun on store A: %v", err)
			}

			// Create a workflow in store B.
			runIDB, _, err := storeB.StartNewRun(ctx, "", "test-signals", 1,
				json.RawMessage(`{"owner":"tenant-b"}`),
				"signal-test-b-1", DefaultTenantUUID)
			if err != nil {
				t.Fatalf("StartNewRun on store B: %v", err)
			}

			// Deliver a signal to store A's workflow.
			if err := storeA.DeliverSignal(ctx, runIDA, "test-signal", `{"from":"tenant-a"}`); err != nil {
				t.Fatalf("DeliverSignal on store A: %v", err)
			}

			// Store B should NOT see the signal delivered to store A's workflow.
			payload, found, err := storeB.PollSignal(ctx, runIDA, "test-signal")
			if err != nil {
				t.Fatalf("PollSignal on store B: %v", err)
			}
			if found {
				t.Errorf("ISOLATION BREACH: store B can poll signal from tenant A's workflow (payload: %s)", payload)
			}

			// Store A should see its own signal.
			_, found, err = storeA.PollSignal(ctx, runIDA, "test-signal")
			if err != nil {
				t.Fatalf("PollSignal on store A: %v", err)
			}
			if !found {
				t.Error("store A cannot see its own signal")
			}

			// Cleanup.
			claimedA, _ := storeA.ClaimWorkflow(ctx, "test-worker")
			if claimedA != nil {
				storeA.CompleteWorkflow(ctx, runIDA, "test-worker", 0, `{"done":true}`, nil)
			}
			claimedB, _ := storeB.ClaimWorkflow(ctx, "test-worker")
			if claimedB != nil {
				storeB.CompleteWorkflow(ctx, runIDB, "test-worker", 0, `{"done":true}`, nil)
			}
		})
	}
}

// TestTenantIsolation_Schedules verifies that schedules created by one tenant
// are not visible to another tenant.
func TestTenantIsolation_Schedules(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			if !backend.Enabled() {
				t.Skipf("%s backend not enabled", backend.Name())
			}

			mtBackend, ok := backend.(MultiTenantStoreBackend)
			if !ok {
				t.Skipf("%s backend does not support multi-tenant store creation", backend.Name())
			}

			storeA, teardownA := mtBackend.SetupForTenant(t, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
			defer teardownA()
			storeB, teardownB := mtBackend.SetupForTenant(t, "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
			defer teardownB()

			ctx := context.Background()

			// Deploy workflow def in both stores.
			def := &WorkflowDef{
				Name:       "test-schedules",
				Version:    1,
				WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1,
				MinVersion: 1,
			}
			if err := storeA.DeployWorkflowDef(ctx, def); err != nil {
				t.Fatalf("DeployWorkflowDef on store A: %v", err)
			}
			if err := storeB.DeployWorkflowDef(ctx, def); err != nil {
				t.Fatalf("DeployWorkflowDef on store B: %v", err)
			}

			now := time.Now()

			// Create a schedule in store A.
			if err := storeA.CreateSchedule(ctx, Schedule{
				Name:           "schedule-a",
				DefName:        "test-schedules",
				EntryPoint:     "main",
				CronExpression: "* * * * *",
				Input:          json.RawMessage(`{}`),
				Enabled:        true,
				NextRunAt:      now.Add(time.Hour),
			}); err != nil {
				t.Fatalf("CreateSchedule on store A: %v", err)
			}

			// Create a schedule in store B.
			if err := storeB.CreateSchedule(ctx, Schedule{
				Name:           "schedule-b",
				DefName:        "test-schedules",
				EntryPoint:     "main",
				CronExpression: "* * * * *",
				Input:          json.RawMessage(`{}`),
				Enabled:        true,
				NextRunAt:      now.Add(time.Hour),
			}); err != nil {
				t.Fatalf("CreateSchedule on store B: %v", err)
			}

			// Store A should see its own schedule but not B's.
			schedsA, err := storeA.ListSchedules(ctx)
			if err != nil {
				t.Fatalf("ListSchedules on store A: %v", err)
			}
			for _, s := range schedsA {
				if s.Name == "schedule-b" {
					t.Errorf("ISOLATION BREACH: store A's ListSchedules includes tenant B's schedule %s", s.Name)
				}
			}

			// Store B should see its own schedule but not A's.
			schedsB, err := storeB.ListSchedules(ctx)
			if err != nil {
				t.Fatalf("ListSchedules on store B: %v", err)
			}
			for _, s := range schedsB {
				if s.Name == "schedule-a" {
					t.Errorf("ISOLATION BREACH: store B's ListSchedules includes tenant A's schedule %s", s.Name)
				}
			}

			// Cleanup.
			storeA.DeleteSchedule(ctx, "schedule-a")
			storeB.DeleteSchedule(ctx, "schedule-b")
		})
	}
}

// TestTenantIsolation_EventHistory verifies that a tenant's event_history rows
// are not visible to other tenants (cross-tenant RLS enforcement).
func TestTenantIsolation_EventHistory(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			if !backend.Enabled() {
				t.Skipf("%s backend not enabled", backend.Name())
			}

			mtBackend, ok := backend.(MultiTenantStoreBackend)
			if !ok {
				t.Skipf("%s backend does not support multi-tenant store creation", backend.Name())
			}

			storeA, teardownA := mtBackend.SetupForTenant(t, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
			defer teardownA()
			storeB, teardownB := mtBackend.SetupForTenant(t, "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
			defer teardownB()

			ctx := context.Background()

			// Deploy workflow def in both stores.
			def := &WorkflowDef{
				Name:       "test-event-history-iso",
				Version:    1,
				WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1,
				MinVersion: 1,
			}
			if err := storeA.DeployWorkflowDef(ctx, def); err != nil {
				t.Fatalf("DeployWorkflowDef on store A: %v", err)
			}
			if err := storeB.DeployWorkflowDef(ctx, def); err != nil {
				t.Fatalf("DeployWorkflowDef on store B: %v", err)
			}

			// Create a workflow in store A.
			runIDA, _, err := storeA.StartNewRun(ctx, "", "test-event-history-iso", 1,
				json.RawMessage(`{"owner":"tenant-a"}`),
				"event-history-test-a-1", DefaultTenantUUID)
			if err != nil {
				t.Fatalf("StartNewRun on store A: %v", err)
			}

			// Append an event to store A's workflow event history.
			eventA := EventRecord{
				Step:      1,
				EventType: "call",
				Service:   "test-svc",
				Op:        "test-op",
			}
			if err := storeA.AppendEventHistory(ctx, runIDA, eventA); err != nil {
				t.Fatalf("AppendEventHistory on store A: %v", err)
			}

			// Store A should be able to read its own event history.
			eventsA, err := storeA.LoadEventHistory(ctx, runIDA)
			if err != nil {
				t.Fatalf("LoadEventHistory on store A: %v", err)
			}
			if len(eventsA) == 0 {
				t.Fatal("store A cannot see its own event history")
			}

			// Store B should NOT be able to read store A's event history.
			eventsB, err := storeB.LoadEventHistory(ctx, runIDA)
			if err != nil {
				t.Fatalf("LoadEventHistory on store B: %v", err)
			}
			if len(eventsB) > 0 {
				t.Errorf("ISOLATION BREACH: store B can read tenant A's event history for workflow %s (got %d events)", runIDA, len(eventsB))
			}

			// Cleanup.
			claimedA, _ := storeA.ClaimWorkflow(ctx, "test-worker")
			if claimedA != nil {
				storeA.CompleteWorkflow(ctx, runIDA, "test-worker", 0, `{"done":true}`, nil)
			}
		})
	}
}

// TestTenantIsolation_Promises verifies that promises created by one tenant
// are not visible to another tenant.
func TestTenantIsolation_Promises(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			if !backend.Enabled() {
				t.Skipf("%s backend not enabled", backend.Name())
			}

			mtBackend, ok := backend.(MultiTenantStoreBackend)
			if !ok {
				t.Skipf("%s backend does not support multi-tenant store creation", backend.Name())
			}

			storeA, teardownA := mtBackend.SetupForTenant(t, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
			defer teardownA()
			storeB, teardownB := mtBackend.SetupForTenant(t, "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
			defer teardownB()

			ctx := context.Background()

			// Deploy workflow def in both stores.
			def := &WorkflowDef{
				Name:       "test-promises",
				Version:    1,
				WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1,
				MinVersion: 1,
			}
			if err := storeA.DeployWorkflowDef(ctx, def); err != nil {
				t.Fatalf("DeployWorkflowDef on store A: %v", err)
			}
			if err := storeB.DeployWorkflowDef(ctx, def); err != nil {
				t.Fatalf("DeployWorkflowDef on store B: %v", err)
			}

			// Create a workflow in store A.
			runIDA, _, err := storeA.StartNewRun(ctx, "", "test-promises", 1,
				json.RawMessage(`{"owner":"tenant-a"}`),
				"promise-test-a-1", DefaultTenantUUID)
			if err != nil {
				t.Fatalf("StartNewRun on store A: %v", err)
			}

			// Create a workflow in store B.
			runIDB, _, err := storeB.StartNewRun(ctx, "", "test-promises", 1,
				json.RawMessage(`{"owner":"tenant-b"}`),
				"promise-test-b-1", DefaultTenantUUID)
			if err != nil {
				t.Fatalf("StartNewRun on store B: %v", err)
			}

			// Create a promise in store A's workflow.
			if err := storeA.CreatePromise(ctx, runIDA, "test-promise", "promise-tenant-a"); err != nil {
				t.Fatalf("CreatePromise on store A: %v", err)
			}

			// Store B should NOT be able to see store A's promise.
			promisesB, err := storeB.ListPromises(ctx, runIDA)
			if err != nil {
				t.Fatalf("ListPromises on store B: %v", err)
			}
			for _, p := range promisesB {
				if p.PromiseID == "promise-tenant-a" {
					t.Errorf("ISOLATION BREACH: store B's ListPromises includes tenant A's promise %s", p.PromiseID)
				}
			}

			// Store A should see its own promise.
			promisesA, err := storeA.ListPromises(ctx, runIDA)
			if err != nil {
				t.Fatalf("ListPromises on store A: %v", err)
			}
			found := false
			for _, p := range promisesA {
				if p.PromiseID == "promise-tenant-a" {
					found = true
					break
				}
			}
			if !found {
				t.Error("store A cannot see its own promise")
			}

			// Cleanup.
			claimedA, _ := storeA.ClaimWorkflow(ctx, "test-worker")
			if claimedA != nil {
				storeA.CompleteWorkflow(ctx, runIDA, "test-worker", 0, `{"done":true}`, nil)
			}
			claimedB, _ := storeB.ClaimWorkflow(ctx, "test-worker")
			if claimedB != nil {
				storeB.CompleteWorkflow(ctx, runIDB, "test-worker", 0, `{"done":true}`, nil)
			}
		})
	}
}

// TestTenantIsolation_Reaper verifies that ReapStaleInstances on tenant A
// does not reclaim tenant B's running workflows.
func TestTenantIsolation_Reaper(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			if !backend.Enabled() {
				t.Skipf("%s backend not enabled", backend.Name())
			}

			mtBackend, ok := backend.(MultiTenantStoreBackend)
			if !ok {
				t.Skipf("%s backend does not support multi-tenant store creation", backend.Name())
			}

			storeA, teardownA := mtBackend.SetupForTenant(t, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
			defer teardownA()
			storeB, teardownB := mtBackend.SetupForTenant(t, "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
			defer teardownB()

			ctx := context.Background()

			def := &WorkflowDef{
				Name:       "test-reaper",
				Version:    1,
				WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1,
				MinVersion: 1,
			}
			if err := storeA.DeployWorkflowDef(ctx, def); err != nil {
				t.Fatalf("DeployWorkflowDef on store A: %v", err)
			}
			if err := storeB.DeployWorkflowDef(ctx, def); err != nil {
				t.Fatalf("DeployWorkflowDef on store B: %v", err)
			}

			// Create a workflow in store A.
			runIDA, _, err := storeA.StartNewRun(ctx, "", "test-reaper", 1,
				json.RawMessage(`{}`), "reaper-a-1", DefaultTenantUUID)
			if err != nil {
				t.Fatalf("StartNewRun on store A: %v", err)
			}

			// Create a workflow in store B.
			runIDB, _, err := storeB.StartNewRun(ctx, "", "test-reaper", 1,
				json.RawMessage(`{}`), "reaper-b-1", DefaultTenantUUID)
			if err != nil {
				t.Fatalf("StartNewRun on store B: %v", err)
			}

			// Claim both — moves them to "running" with a fresh heartbeat.
			claimedA, err := storeA.ClaimWorkflow(ctx, "test-worker")
			if err != nil {
				t.Fatalf("ClaimWorkflow on store A: %v", err)
			}
			if claimedA == nil {
				t.Fatal("ClaimWorkflow on store A returned nil")
			}
			claimedB, err := storeB.ClaimWorkflow(ctx, "test-worker")
			if err != nil {
				t.Fatalf("ClaimWorkflow on store B: %v", err)
			}
			if claimedB == nil {
				t.Fatal("ClaimWorkflow on store B returned nil")
			}

			// Let the heartbeat age slightly so even a 1ns timeout catches it.
			time.Sleep(10 * time.Millisecond)

			// Reap stale instances from store A.
			reaped, err := storeA.ReapStaleInstances(ctx, 1*time.Nanosecond)
			if err != nil {
				t.Fatalf("ReapStaleInstances on store A: %v", err)
			}
			if reaped < 0 {
				t.Errorf("ReapStaleInstances returned negative count: %d", reaped)
			}

			// Store B's workflow must still be running — reaping on store A
			// must not cross tenant boundaries.
			wfB, err := storeB.GetWorkflowByID(ctx, runIDB)
			if err != nil {
				t.Fatalf("GetWorkflowByID on store B: %v", err)
			}
			if wfB == nil {
				t.Fatal("store B workflow disappeared")
			}
			if wfB.Status != "running" {
				t.Errorf("ISOLATION BREACH: tenant A's reaper reclaimed tenant B's workflow %s (status=%s)", runIDB, wfB.Status)
			}

			// Cleanup: complete both workflows.
			storeA.CompleteWorkflow(ctx, runIDA, "test-worker", 0, `{"done":true}`, nil)
			storeB.CompleteWorkflow(ctx, runIDB, "test-worker", 0, `{"done":true}`, nil)
		})
	}
}

// TestTenantIsolation_ConcurrencyKeys verifies that concurrency key operations
// (acquire, reap) respect tenant boundaries.
func TestTenantIsolation_ConcurrencyKeys(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			if !backend.Enabled() {
				t.Skipf("%s backend not enabled", backend.Name())
			}

			mtBackend, ok := backend.(MultiTenantStoreBackend)
			if !ok {
				t.Skipf("%s backend does not support multi-tenant store creation", backend.Name())
			}

			storeA, teardownA := mtBackend.SetupForTenant(t, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
			defer teardownA()
			storeB, teardownB := mtBackend.SetupForTenant(t, "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
			defer teardownB()

			ctx := context.Background()

			// --- Part 1: same key, different tenants, both should acquire ---

			acquiredA, err := storeA.AcquireConcurrencyKey(ctx, "iso-key", "wf-a", 60*time.Second)
			if err != nil {
				t.Fatalf("AcquireConcurrencyKey on store A: %v", err)
			}
			if !acquiredA {
				t.Fatal("expected store A to acquire iso-key")
			}

			// Same key, different tenant — should succeed because tenants are isolated.
			acquiredB, err := storeB.AcquireConcurrencyKey(ctx, "iso-key", "wf-b", 60*time.Second)
			if err != nil {
				t.Fatalf("AcquireConcurrencyKey on store B: %v", err)
			}
			if !acquiredB {
				t.Error("ISOLATION BREACH: store B cannot acquire iso-key — tenant A's key leaked across tenants")
			}

			// Within the same tenant, a second workflow should NOT acquire the same key.
			acquiredA2, err := storeA.AcquireConcurrencyKey(ctx, "iso-key", "wf-a-2", 60*time.Second)
			if err != nil {
				t.Fatalf("AcquireConcurrencyKey (second) on store A: %v", err)
			}
			if acquiredA2 {
				t.Error("same-tenant acquire of held key should have returned false")
			}

			// Clean up part 1.
			if err := storeA.ReleaseConcurrencyKey(ctx, "iso-key"); err != nil {
				t.Fatalf("ReleaseConcurrencyKey on store A: %v", err)
			}
			if err := storeB.ReleaseConcurrencyKey(ctx, "iso-key"); err != nil {
				t.Fatalf("ReleaseConcurrencyKey on store B: %v", err)
			}

			// --- Part 2: ReapExpiredConcurrencyKeys respects tenant boundaries ---

			// Store B acquires a key with a long TTL.
			acquiredLong, err := storeB.AcquireConcurrencyKey(ctx, "reap-iso-key", "wf-b-long", 300*time.Second)
			if err != nil {
				t.Fatalf("AcquireConcurrencyKey (long TTL) on store B: %v", err)
			}
			if !acquiredLong {
				t.Fatal("expected store B to acquire reap-iso-key with long TTL")
			}

			// Store A acquires the same key name with a 1ns TTL (effectively expired).
			acquiredShort, err := storeA.AcquireConcurrencyKey(ctx, "reap-iso-key", "wf-a-short", 1*time.Nanosecond)
			if err != nil {
				t.Fatalf("AcquireConcurrencyKey (short TTL) on store A: %v", err)
			}
			if !acquiredShort {
				t.Fatal("expected store A to acquire reap-iso-key with short TTL")
			}

			// Give the short TTL a moment to expire.
			time.Sleep(10 * time.Millisecond)

			// Reap expired keys on store A. This must not touch store B's key.
			reaped, err := storeA.ReapExpiredConcurrencyKeys(ctx)
			if err != nil {
				t.Fatalf("ReapExpiredConcurrencyKeys on store A: %v", err)
			}
			if reaped < 0 {
				t.Errorf("ReapExpiredConcurrencyKeys returned negative count: %d", reaped)
			}

			// Store B's long-lived key must still be held — a second workflow
			// should fail to acquire it within tenant B.
			stillHeld, err := storeB.AcquireConcurrencyKey(ctx, "reap-iso-key", "wf-b-2", 60*time.Second)
			if err != nil {
				t.Fatalf("AcquireConcurrencyKey on store B after reap: %v", err)
			}
			if stillHeld {
				t.Errorf("ISOLATION BREACH: tenant A's ReapExpiredConcurrencyKeys removed tenant B's non-expired key")
			}

			// Cleanup part 2.
			if err := storeB.ReleaseConcurrencyKey(ctx, "reap-iso-key"); err != nil {
				t.Fatalf("ReleaseConcurrencyKey on store B: %v", err)
			}
		})
	}
}
