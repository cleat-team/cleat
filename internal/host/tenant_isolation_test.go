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
			"idempotency-tenant-a-1")
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
				"iso-test-a-1")
			if err != nil {
				t.Fatalf("StartNewRun on store A: %v", err)
			}

			// Create a workflow in store B.
			runIDB, _, err := storeB.StartNewRun(ctx, "", "test-isolation", 1,
				json.RawMessage(`{"owner":"tenant-b"}`),
				"iso-test-b-1")
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
				"signal-test-a-1")
			if err != nil {
				t.Fatalf("StartNewRun on store A: %v", err)
			}

			// Create a workflow in store B.
			runIDB, _, err := storeB.StartNewRun(ctx, "", "test-signals", 1,
				json.RawMessage(`{"owner":"tenant-b"}`),
				"signal-test-b-1")
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
				"promise-test-a-1")
			if err != nil {
				t.Fatalf("StartNewRun on store A: %v", err)
			}

			// Create a workflow in store B.
			runIDB, _, err := storeB.StartNewRun(ctx, "", "test-promises", 1,
				json.RawMessage(`{"owner":"tenant-b"}`),
				"promise-test-b-1")
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
