package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// Skip-guard audit (fix/audit-conditional-skips, IMPROVEMENT-PLAN.md 1.10):
//
// This file holds the tenant-isolation tests that are the *only* proof
// row-level security actually works for GetWorkflowByID/ListWorkflows --
// neither carries an application-level tenant_id filter, and cleat
// historically connected as a superuser, which PostgreSQL exempts from RLS
// entirely (IMPROVEMENT-PLAN.md 1.10). A skip that fires here when it
// shouldn't means isolation goes unverified while CI stays green, so every
// guard in this file was re-derived from scratch rather than trusted:
//
//   - `if !backend.Enabled() { t.Skip(...) }` (forEachBackend and the
//     per-test loops below) is genuinely live for MySQL/MSSQL: their
//     Enabled() reads CLEAT_TEST_MYSQL/CLEAT_TEST_MSSQL, and the loop must
//     skip when nobody asked for that backend (e.g. ci.yml's test-go job,
//     which sets neither var). It is provably dead for Postgres:
//     PostgresBackend.Enabled() (store_backends_test.go) hardcodes
//     `return true`, mirroring testutil.TestDB's own unconditional
//     default-DSN fallback, so this branch can never fire on that leg --
//     Postgres has no "unconfigured" state in this suite. That is
//     intentional, not a bug: a genuinely unreachable Postgres already
//     surfaces as a t.Fatalf inside backend.Setup() -> testutil.TestDB,
//     which already applies the "configured but broken is Fatal, not Skip"
//     rule this whole file is held to. The guard is left unchanged below so
//     it stays correct for the two dialects where it does something.
//
//   - `backend.(MultiTenantStoreBackend)` type assertions, below, are
//     t.Fatalf, not t.Skipf: store_backends_test.go's init() registers
//     exactly three backends (Postgres, MySQL, MSSQL) and all three
//     implement SetupForTenant, so this assertion cannot fail today. A
//     failure would mean a backend was registered without tenant-isolation
//     support -- i.e. isolation silently went untested for it -- which must
//     stop the build, not quietly skip a subtest.
//
//   - TestUnauthenticatedQueryRejection's closing type switch: no
//     registered backend ever constructs a *ShardedStore (grepped
//     `ShardedStore{` across the repo; only sharded_store.go's own
//     NewShardedStore and sharded_store_test.go's unit tests construct one),
//     so that case is unreachable today -- left in place as the shape to
//     fill in if ShardedStore is ever registered as a backend, which would
//     also give it tenant-isolation coverage it has nowhere else in this
//     file. Its `default:` branch is t.Fatalf, not t.Skipf: the switch has
//     no `case *MySQLStore`, so whenever multi-db-ci.yml's test-mysql job
//     runs this test -- the job that exists specifically to exercise MySQL
//     -- the MySQL subtest fell into `default` and skipped itself
//     unconditionally, every time, regardless of environment. That is a
//     structural coverage hole (MySQL is never checked for
//     unauthenticated-query rejection at all), not an environment-
//     conditional skip, and this change does not fill it -- it only stops
//     the hole from being invisible.

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

		// Deploy a workflow definition. This one is visible to all tenants,
		// because forEachBackend hands out a default-tenant store and
		// workflow_defs' RLS policy admits default-tenant rows -- not because
		// definitions are global. Since IMPROVEMENT-PLAN 3.12,
		// DeployWorkflowDef writes the deploying store's tenant, so a
		// definition deployed by any other tenant is not visible to all.
		//
		// Spelled out because migrations/postgres/001_schema.sql used to cite
		// this line as evidence that definitions were "a shared/global
		// registry, not tenant-partitioned data", and justified a security
		// policy with it.
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
			"idempotency-tenant-a-1", DefaultTenantUUID, 0)
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
				// Unreachable with the current backend set (see file-level
				// comment above) -- kept as a Fatal tripwire in case a future
				// backend is registered without SetupForTenant.
				t.Fatalf("BUG: %s backend does not implement MultiTenantStoreBackend (SetupForTenant); tenant isolation is untested for this backend", backend.Name())
			}

			tenantA := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
			tenantB := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
			storeA, teardownA := mtBackend.SetupForTenant(t, tenantA)
			defer teardownA()
			storeB, teardownB := mtBackend.SetupForTenant(t, tenantB)
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
			defB := tenantBDef(def)
			if err := storeB.DeployWorkflowDef(ctx, defB); err != nil {
				t.Fatalf("DeployWorkflowDef on store B: %v", err)
			}

			// Create a workflow in store A.
			runIDA, _, err := storeA.StartNewRun(ctx, "", "test-isolation", 1,
				json.RawMessage(`{"owner":"tenant-a"}`),
				"iso-test-a-1", tenantA, 0)
			if err != nil {
				t.Fatalf("StartNewRun on store A: %v", err)
			}

			// Create a workflow in store B.
			runIDB, _, err := storeB.StartNewRun(ctx, "", defB.Name, 1,
				json.RawMessage(`{"owner":"tenant-b"}`),
				"iso-test-b-1", tenantB, 0)
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
				// Unreachable with the current backend set (see file-level
				// comment above) -- kept as a Fatal tripwire in case a future
				// backend is registered without SetupForTenant.
				t.Fatalf("BUG: %s backend does not implement MultiTenantStoreBackend (SetupForTenant); tenant isolation is untested for this backend", backend.Name())
			}

			tenantA := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
			tenantB := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
			storeA, teardownA := mtBackend.SetupForTenant(t, tenantA)
			defer teardownA()
			storeB, teardownB := mtBackend.SetupForTenant(t, tenantB)
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
			defB := tenantBDef(def)
			if err := storeB.DeployWorkflowDef(ctx, defB); err != nil {
				t.Fatalf("DeployWorkflowDef on store B: %v", err)
			}

			// Create a workflow in store A.
			runIDA, _, err := storeA.StartNewRun(ctx, "", "test-signals", 1,
				json.RawMessage(`{"owner":"tenant-a"}`),
				"signal-test-a-1", tenantA, 0)
			if err != nil {
				t.Fatalf("StartNewRun on store A: %v", err)
			}

			// Create a workflow in store B.
			runIDB, _, err := storeB.StartNewRun(ctx, "", defB.Name, 1,
				json.RawMessage(`{"owner":"tenant-b"}`),
				"signal-test-b-1", tenantB, 0)
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
				// Unreachable with the current backend set (see file-level
				// comment above) -- kept as a Fatal tripwire in case a future
				// backend is registered without SetupForTenant.
				t.Fatalf("BUG: %s backend does not implement MultiTenantStoreBackend (SetupForTenant); tenant isolation is untested for this backend", backend.Name())
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
			defB := tenantBDef(def)
			if err := storeB.DeployWorkflowDef(ctx, defB); err != nil {
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
				DefName:        defB.Name,
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
				// Unreachable with the current backend set (see file-level
				// comment above) -- kept as a Fatal tripwire in case a future
				// backend is registered without SetupForTenant.
				t.Fatalf("BUG: %s backend does not implement MultiTenantStoreBackend (SetupForTenant); tenant isolation is untested for this backend", backend.Name())
			}

			tenantA := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
			tenantB := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
			storeA, teardownA := mtBackend.SetupForTenant(t, tenantA)
			defer teardownA()
			storeB, teardownB := mtBackend.SetupForTenant(t, tenantB)
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
			defB := tenantBDef(def)
			if err := storeB.DeployWorkflowDef(ctx, defB); err != nil {
				t.Fatalf("DeployWorkflowDef on store B: %v", err)
			}

			// Create a workflow in store A.
			runIDA, _, err := storeA.StartNewRun(ctx, "", "test-event-history-iso", 1,
				json.RawMessage(`{"owner":"tenant-a"}`),
				"event-history-test-a-1", tenantA, 0)
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
				// Unreachable with the current backend set (see file-level
				// comment above) -- kept as a Fatal tripwire in case a future
				// backend is registered without SetupForTenant.
				t.Fatalf("BUG: %s backend does not implement MultiTenantStoreBackend (SetupForTenant); tenant isolation is untested for this backend", backend.Name())
			}

			tenantA := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
			tenantB := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
			storeA, teardownA := mtBackend.SetupForTenant(t, tenantA)
			defer teardownA()
			storeB, teardownB := mtBackend.SetupForTenant(t, tenantB)
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
			defB := tenantBDef(def)
			if err := storeB.DeployWorkflowDef(ctx, defB); err != nil {
				t.Fatalf("DeployWorkflowDef on store B: %v", err)
			}

			// Create a workflow in store A.
			runIDA, _, err := storeA.StartNewRun(ctx, "", "test-promises", 1,
				json.RawMessage(`{"owner":"tenant-a"}`),
				"promise-test-a-1", tenantA, 0)
			if err != nil {
				t.Fatalf("StartNewRun on store A: %v", err)
			}

			// Create a workflow in store B.
			runIDB, _, err := storeB.StartNewRun(ctx, "", defB.Name, 1,
				json.RawMessage(`{"owner":"tenant-b"}`),
				"promise-test-b-1", tenantB, 0)
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
				// Unreachable with the current backend set (see file-level
				// comment above) -- kept as a Fatal tripwire in case a future
				// backend is registered without SetupForTenant.
				t.Fatalf("BUG: %s backend does not implement MultiTenantStoreBackend (SetupForTenant); tenant isolation is untested for this backend", backend.Name())
			}

			tenantA := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
			tenantB := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
			storeA, teardownA := mtBackend.SetupForTenant(t, tenantA)
			defer teardownA()
			storeB, teardownB := mtBackend.SetupForTenant(t, tenantB)
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
			defB := tenantBDef(def)
			if err := storeB.DeployWorkflowDef(ctx, defB); err != nil {
				t.Fatalf("DeployWorkflowDef on store B: %v", err)
			}

			// Create a workflow in store A.
			runIDA, _, err := storeA.StartNewRun(ctx, "", "test-reaper", 1,
				json.RawMessage(`{}`), "reaper-a-1", tenantA, 0)
			if err != nil {
				t.Fatalf("StartNewRun on store A: %v", err)
			}

			// Create a workflow in store B.
			runIDB, _, err := storeB.StartNewRun(ctx, "", defB.Name, 1,
				json.RawMessage(`{}`), "reaper-b-1", tenantB, 0)
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
			if claimedA.Status != "running" {
				t.Fatalf("ClaimWorkflow on store A returned status %q, want %q", claimedA.Status, "running")
			}
			claimedB, err := storeB.ClaimWorkflow(ctx, "test-worker")
			if err != nil {
				t.Fatalf("ClaimWorkflow on store B: %v", err)
			}
			if claimedB == nil {
				t.Fatal("ClaimWorkflow on store B returned nil")
			}
			if claimedB.Status != "running" {
				t.Fatalf("ClaimWorkflow on store B returned status %q, want %q", claimedB.Status, "running")
			}

			// Let the heartbeat age slightly so even a 1ns timeout catches it.
			time.Sleep(10 * time.Millisecond)

			// Reap stale instances from store A.
			reaped, err := storeA.ReapStaleInstances(ctx, 1*time.Nanosecond)
			if err != nil {
				t.Fatalf("ReapStaleInstances on store A: %v", err)
			}
			if reaped < 1 {
				t.Errorf("expected storeA to reap at least 1 stale instance, got %d", reaped)
			}

			// storeA's own workflow must have been reaped.
			wfA, err := storeA.GetWorkflowByID(ctx, runIDA)
			if err != nil {
				t.Fatalf("GetWorkflowByID on store A after reap: %v", err)
			}
			if wfA == nil {
				t.Fatal("store A workflow disappeared")
			}
			if wfA.Status != "ready" {
				t.Errorf("own-tenant reaper did not reclaim workflow %s (status=%s)", runIDA, wfA.Status)
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

			// Verify storeB's own reaper also works (own-tenant reaping).
			reapedB, err := storeB.ReapStaleInstances(ctx, 1*time.Nanosecond)
			if err != nil {
				t.Fatalf("ReapStaleInstances on store B: %v", err)
			}
			if reapedB < 1 {
				t.Errorf("expected storeB to reap at least 1 stale instance, got %d", reapedB)
			}
			wfBAfter, err := storeB.GetWorkflowByID(ctx, runIDB)
			if err != nil {
				t.Fatalf("GetWorkflowByID on store B after own reap: %v", err)
			}
			if wfBAfter.Status != "ready" {
				t.Errorf("own-tenant reaper did not reclaim workflow %s (status=%s)", runIDB, wfBAfter.Status)
			}

			// Cleanup: claim and complete both workflows.
			claimedA, _ = storeA.ClaimWorkflow(ctx, "test-worker")
			if claimedA != nil {
				storeA.CompleteWorkflow(ctx, runIDA, "test-worker", 0, `{"done":true}`, nil)
			}
			claimedB, _ = storeB.ClaimWorkflow(ctx, "test-worker")
			if claimedB != nil {
				storeB.CompleteWorkflow(ctx, runIDB, "test-worker", 0, `{"done":true}`, nil)
			}
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
				// Unreachable with the current backend set (see file-level
				// comment above) -- kept as a Fatal tripwire in case a future
				// backend is registered without SetupForTenant.
				t.Fatalf("BUG: %s backend does not implement MultiTenantStoreBackend (SetupForTenant); tenant isolation is untested for this backend", backend.Name())
			}

			tenantA := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
			tenantB := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
			storeA, teardownA := mtBackend.SetupForTenant(t, tenantA)
			defer teardownA()
			storeB, teardownB := mtBackend.SetupForTenant(t, tenantB)
			defer teardownB()

			ctx := context.Background()

			// concurrency_keys.workflow_id is a real foreign key into
			// workflow_instances(id) (migrations/postgres/001_schema.sql), so
			// every workflow_id used below must be a real, already-created
			// instance -- not just an opaque string, as this test used to
			// assume back when the hand-maintained test schema in
			// engine/testutil/schema.go had no such constraint.
			def := &WorkflowDef{
				Name:       "test-concurrency-keys",
				Version:    1,
				WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1,
				MinVersion: 1,
			}
			if err := storeA.DeployWorkflowDef(ctx, def); err != nil {
				t.Fatalf("DeployWorkflowDef on store A: %v", err)
			}
			defB := tenantBDef(def)
			if err := storeB.DeployWorkflowDef(ctx, defB); err != nil {
				t.Fatalf("DeployWorkflowDef on store B: %v", err)
			}
			for _, wfID := range []string{"wf-a", "wf-a-2", "wf-a-3"} {
				if _, _, err := storeA.StartNewRun(ctx, wfID, "test-concurrency-keys", 1, json.RawMessage(`{}`), "", tenantA, 0); err != nil {
					t.Fatalf("StartNewRun(%s) on store A: %v", wfID, err)
				}
			}
			for _, wfID := range []string{"wf-b"} {
				if _, _, err := storeB.StartNewRun(ctx, wfID, defB.Name, 1, json.RawMessage(`{}`), "", tenantB, 0); err != nil {
					t.Fatalf("StartNewRun(%s) on store B: %v", wfID, err)
				}
			}

			// --- Part 1: Acquire/release cross-tenant isolation ---
			// concurrency_keys has PRIMARY KEY (key_hash) alone, so two tenants
			// cannot simultaneously hold the same key name. The test works within
			// this constraint, verifying tenant-scoped release isolation and
			// sequential reuse across tenants.

			acquired, err := storeA.AcquireConcurrencyKey(ctx, "iso-key", "wf-a", 60*time.Second)
			if err != nil {
				t.Fatalf("AcquireConcurrencyKey on store A: %v", err)
			}
			if !acquired {
				t.Fatal("expected store A to acquire iso-key")
			}

			// storeA can see its own key.
			count, err := storeA.GetConcurrencyKeyCount(ctx, "wf-a")
			if err != nil {
				t.Fatalf("GetConcurrencyKeyCount on store A: %v", err)
			}
			if count < 1 {
				t.Errorf("expected >= 1 concurrency keys for wf-a, got %d", count)
			}

			// Same-tenant conflict: second workflow in storeA cannot acquire.
			acquired, err = storeA.AcquireConcurrencyKey(ctx, "iso-key", "wf-a-2", 60*time.Second)
			if err != nil {
				t.Fatalf("AcquireConcurrencyKey (second) on store A: %v", err)
			}
			if acquired {
				t.Error("same-tenant acquire of held key should have returned false")
			}

			// storeB tries to release "iso-key" — tenant-scoped, should be a no-op
			// because the row has tenant_id = tenant A.
			if err := storeB.ReleaseConcurrencyKey(ctx, "iso-key"); err != nil {
				t.Fatalf("ReleaseConcurrencyKey on store B: %v", err)
			}

			// storeA still holds the key — storeB's release was tenant-scoped.
			count, err = storeA.GetConcurrencyKeyCount(ctx, "wf-a")
			if err != nil {
				t.Fatalf("GetConcurrencyKeyCount on store A after storeB release: %v", err)
			}
			if count < 1 {
				t.Errorf("ISOLATION BREACH: storeB's ReleaseConcurrencyKey removed storeA's key (count=%d)", count)
			}

			// storeA releases its own key.
			if err := storeA.ReleaseConcurrencyKey(ctx, "iso-key"); err != nil {
				t.Fatalf("ReleaseConcurrencyKey on store A (own key): %v", err)
			}

			// storeA confirms key is gone.
			count, err = storeA.GetConcurrencyKeyCount(ctx, "wf-a")
			if err != nil {
				t.Fatalf("GetConcurrencyKeyCount on store A after own release: %v", err)
			}
			if count != 0 {
				t.Errorf("expected 0 keys after release, got %d", count)
			}

			// After storeA released, storeB can acquire the same key name.
			acquired, err = storeB.AcquireConcurrencyKey(ctx, "iso-key", "wf-b", 60*time.Second)
			if err != nil {
				t.Fatalf("AcquireConcurrencyKey on storeB after release: %v", err)
			}
			if !acquired {
				t.Error("storeB should acquire iso-key after storeA released it")
			}

			// Now storeA cannot acquire — key is held by storeB (PK conflict).
			acquired, err = storeA.AcquireConcurrencyKey(ctx, "iso-key", "wf-a-3", 60*time.Second)
			if err != nil {
				t.Fatalf("AcquireConcurrencyKey on storeA after storeB holds: %v", err)
			}
			if acquired {
				t.Error("storeA should not acquire iso-key while storeB holds it")
			}

			// Cleanup part 1.
			if err := storeB.ReleaseConcurrencyKey(ctx, "iso-key"); err != nil {
				t.Fatalf("ReleaseConcurrencyKey on store B cleanup: %v", err)
			}

			// --- Part 2: Reap isolation — each tenant only reaps its own expired keys ---

			// storeA acquires "reap-key-a" with 1ns TTL (effectively expired).
			acquired, err = storeA.AcquireConcurrencyKey(ctx, "reap-key-a", "wf-a", 1*time.Nanosecond)
			if err != nil {
				t.Fatalf("AcquireConcurrencyKey (reap-key-a) on store A: %v", err)
			}
			if !acquired {
				t.Fatal("expected store A to acquire reap-key-a")
			}

			// storeB acquires "reap-key-b" with 1ns TTL (effectively expired).
			acquired, err = storeB.AcquireConcurrencyKey(ctx, "reap-key-b", "wf-b", 1*time.Nanosecond)
			if err != nil {
				t.Fatalf("AcquireConcurrencyKey (reap-key-b) on store B: %v", err)
			}
			if !acquired {
				t.Fatal("expected store B to acquire reap-key-b")
			}

			// Give the short TTLs a moment to expire.
			time.Sleep(10 * time.Millisecond)

			// Reap expired keys on store A. Must only reap "reap-key-a".
			reapedA, err := storeA.ReapExpiredConcurrencyKeys(ctx)
			if err != nil {
				t.Fatalf("ReapExpiredConcurrencyKeys on store A: %v", err)
			}
			if reapedA != 1 {
				t.Errorf("expected storeA to reap exactly 1 expired key, got %d", reapedA)
			}

			// Reap expired keys on store B. Must only reap "reap-key-b".
			// If storeA already cross-tenant-reaped it, this would return 0.
			reapedB, err := storeB.ReapExpiredConcurrencyKeys(ctx)
			if err != nil {
				t.Fatalf("ReapExpiredConcurrencyKeys on store B: %v", err)
			}
			if reapedB != 1 {
				t.Errorf("ISOLATION BREACH: expected storeB to reap 1 expired key, got %d (storeA may have cross-tenant-reaped it)", reapedB)
			}
		})
	}
}

// TestTenantIsolation_ActiveInstanceCounts verifies that GetActiveInstanceCountsByVersion
// returns only the calling tenant's active instances and does not leak cross-tenant data.
func TestTenantIsolation_ActiveInstanceCounts(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			if !backend.Enabled() {
				t.Skipf("%s backend not enabled", backend.Name())
			}
			mtBackend, ok := backend.(MultiTenantStoreBackend)
			if !ok {
				// Unreachable with the current backend set (see file-level
				// comment above) -- kept as a Fatal tripwire in case a future
				// backend is registered without SetupForTenant.
				t.Fatalf("BUG: %s backend does not implement MultiTenantStoreBackend (SetupForTenant); tenant isolation is untested for this backend", backend.Name())
			}

			tenantA := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
			tenantB := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
			storeA, teardownA := mtBackend.SetupForTenant(t, tenantA)
			defer teardownA()
			storeB, teardownB := mtBackend.SetupForTenant(t, tenantB)
			defer teardownB()

			ctx := context.Background()

			def := &WorkflowDef{
				Name: "test-active-counts", Version: 1,
				WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1, MinVersion: 1,
			}
			if err := storeA.DeployWorkflowDef(ctx, def); err != nil {
				t.Fatalf("DeployWorkflowDef on store A: %v", err)
			}
			defB := tenantBDef(def)
			if err := storeB.DeployWorkflowDef(ctx, defB); err != nil {
				t.Fatalf("DeployWorkflowDef on store B: %v", err)
			}

			// Create 3 instances in tenant A, 2 instances in tenant B.
			for i := 0; i < 3; i++ {
				_, _, err := storeA.StartNewRun(ctx, "", "test-active-counts", 1,
					json.RawMessage(`{}`),
					fmt.Sprintf("a-%d", i), tenantA, 0)
				if err != nil {
					t.Fatalf("StartNewRun on store A: %v", err)
				}
			}
			for i := 0; i < 2; i++ {
				_, _, err := storeB.StartNewRun(ctx, "", defB.Name, 1,
					json.RawMessage(`{}`),
					fmt.Sprintf("b-%d", i), tenantB, 0)
				if err != nil {
					t.Fatalf("StartNewRun on store B: %v", err)
				}
			}

			// Store A should see only its own counts.
			countsA, err := storeA.GetActiveInstanceCountsByVersion(ctx)
			if err != nil {
				t.Fatalf("GetActiveInstanceCountsByVersion on store A: %v", err)
			}
			if countsA["test-active-counts:1"] != 3 {
				t.Errorf("store A expected 3 active instances, got %d", countsA["test-active-counts:1"])
			}

			// Store B should see only its own counts.
			countsB, err := storeB.GetActiveInstanceCountsByVersion(ctx)
			if err != nil {
				t.Fatalf("GetActiveInstanceCountsByVersion on store B: %v", err)
			}
			keyB := defB.Name + ":1"
			if countsB[keyB] != 2 {
				t.Errorf("store B expected 2 active instances under %s, got %d", keyB, countsB[keyB])
			}
		})
	}
}

// TestUnauthenticatedQueryRejection verifies that RLS-scoped operations fail
// when no tenant context is set, rather than silently seeing default-tenant data.
func TestUnauthenticatedQueryRejection(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			if !backend.Enabled() {
				t.Skipf("%s backend not enabled", backend.Name())
			}
			store, teardown := backend.Setup(t)
			defer teardown()

			// Only test backends where we can construct a zero-tenant store.
			switch s := store.(type) {
			case *PostgresStore:
				zeroStore := *s
				zeroStore.tenantID = ""
				_, err := zeroStore.GetActiveInstanceCountsByVersion(context.Background())
				if err == nil {
					t.Error("expected error for unauthenticated query, got nil")
				}
			case *MSSQLStore:
				zeroStore := *s
				zeroStore.tenantID = ""
				_, err := zeroStore.GetActiveInstanceCountsByVersion(context.Background())
				if err == nil {
					t.Error("expected error for unauthenticated query, got nil")
				}
			case *MySQLStore:
				// This case did not exist until the conditional-skip audit.
				// MySQL fell through to `default:` and skipped, so the dialect
				// was never once checked here -- including in multi-db-ci.yml's
				// test-mysql job, which exists to test it.
				//
				// It matters more on MySQL than on PostgreSQL. Postgres has
				// seven RLS policies as a database-level backstop; MySQL has
				// none (IMPROVEMENT-PLAN.md 1.7), so a query that runs with no
				// tenant is bounded by nothing but the Go code that built it.
				zeroStore := *s
				zeroStore.tenantID = ""
				_, err := zeroStore.GetActiveInstanceCountsByVersion(context.Background())
				if err == nil {
					t.Error("expected error for unauthenticated query, got nil")
				}
			case *ShardedStore:
				// No registered StoreBackend ever constructs a *ShardedStore
				// (see file-level comment above), so this case is
				// unreachable today. Left in place as the shape to fill in
				// if ShardedStore is ever registered as a backend.
				t.Skip("ShardedStore unauthenticated test requires base store access")
			default:
				// Was t.Skipf. This switch has no `case *MySQLStore`, so the
				// MySQL subtest -- run for real by multi-db-ci.yml's
				// test-mysql job -- fell in here and skipped itself every
				// time, unconditionally, never once verifying
				// unauthenticated-query rejection for MySQL (see file-level
				// comment above). Fatal turns any unhandled store type,
				// including that one, into a build failure instead of an
				// invisible pass.
				t.Fatalf("unauthenticated rejection test not implemented for %T", store)
			}
		})
	}
}

// tenantBDef returns tenant B's own copy of a definition, under a name of its
// own.
//
// These tests used to deploy one *WorkflowDef to both stores, which worked
// only because a deploy silently overwrote whatever definition already held
// that (name, version) -- the defect IMPROVEMENT-PLAN 3.12 closes. The primary
// key on workflow_defs still carries no tenant, so two tenants genuinely
// cannot hold the same name; giving B its own is what a multi-tenant
// deployment has to do until the key changes.
//
// Nothing here asserts on the definition itself, so the name is fixture
// detail. What the tests assert -- that neither tenant sees the other's
// instances, events, signals, schedules, promises or counts -- is unchanged.
func tenantBDef(def *WorkflowDef) *WorkflowDef {
	b := *def
	b.Name = def.Name + "-tenant-b"
	return &b
}
