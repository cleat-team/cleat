package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// Execution has to write through a store scoped to the WORKFLOW's tenant, not
// the worker's. Until that is true, widening the claim to run other tenants
// would write their history under the dispatch loop's tenant -- with RLS
// satisfied at every step, because that store genuinely is that tenant.

type recordingFactory struct {
	mu       sync.Mutex
	opened   []string // tenant IDs, in call order
	store    engine.WorkflowStore
	openErr  error
	dialect  engine.Dialect
	queuesIn [][]string
	closed   int // leases released
}

func (f *recordingFactory) OpenStore(_ context.Context, tenantID string, taskQueues ...string) (engine.WorkflowStore, io.Closer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = append(f.opened, tenantID)
	f.queuesIn = append(f.queuesIn, taskQueues)
	if f.openErr != nil {
		return nil, nil, f.openErr
	}
	return f.store, closerFunc(func() error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.closed++
		return nil
	}), nil
}

// closerFunc lets the recording factory hand back a lease it can count.
type closerFunc func() error

func (c closerFunc) Close() error { return c() }

func (f *recordingFactory) leasesReleased() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}
func (f *recordingFactory) DriverName() string      { return "recording" }
func (f *recordingFactory) Dialect() engine.Dialect { return f.dialect }

func (f *recordingFactory) openedTenants() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.opened...)
}

var errOpenStore = errors.New("no such tenant")

const (
	tenantSelf  = "00000000-0000-0000-0000-000000000000"
	tenantOther = "11111111-1111-1111-1111-111111111111"
)

// TestExecuteWorkflow_RoutesToTheWorkflowsOwnTenantStore is the property the
// cross-tenant claim depends on. Without it, widening the claim is a data
// corruption bug rather than a feature.
func TestExecuteWorkflow_RoutesToTheWorkflowsOwnTenantStore(t *testing.T) {
	// Two DISTINCT stores, each with its own probe. Handing the factory the
	// worker's own store would make this test pass whether execution routed or
	// not -- the failure path also resolves a store, so the tenant would be
	// recorded either way. Only a separate store can tell which one execution
	// actually wrote through.
	workerProbe := &tenantScopeProbe{}
	w := newTenantScopeWorker(t, tenantSelf, workerProbe)
	defer w.cancel()

	tenantProbe := &tenantScopeProbe{}
	tenantStore := probedMockStore(tenantProbe)
	w.storeFactory = &recordingFactory{store: tenantStore}
	w.taskQueues = []string{"default"}

	runExecuteWorkflow(w, &engine.WorkflowInstance{
		ID: "wf-routed", DefName: "test-workflow", TenantID: tenantOther,
	})

	opened := w.storeFactory.(*recordingFactory).openedTenants()
	if len(opened) == 0 || opened[0] != tenantOther {
		t.Fatalf("opened stores for %v, want the workflow's own tenant %s first", opened, tenantOther)
	}
	if q := w.storeFactory.(*recordingFactory).queuesIn[0]; len(q) != 1 || q[0] != "default" {
		t.Errorf("tenant store opened with task queues %v, want the worker's own set; a different set "+
			"would give that tenant a different slice of the work", q)
	}

	tenantProbe.mu.Lock()
	tenantTraced := tenantProbe.traced
	tenantProbe.mu.Unlock()
	workerProbe.mu.Lock()
	workerTraced, failed, failMsg := workerProbe.traced, workerProbe.failed, workerProbe.failMsg
	workerProbe.mu.Unlock()

	if !tenantTraced {
		t.Error("the first store write did not go to the workflow's own tenant store")
	}
	if workerTraced {
		t.Error("the first store write went to the WORKER's store; another tenant's history " +
			"would be written under the dispatch loop's tenant")
	}
	// It must NOT be refused any more: routing is what makes it safe to run.
	if failed && strings.Contains(failMsg, "refusing") {
		t.Errorf("a routable workflow was still refused: %s", failMsg)
	}
}

// TestStoreForTenant_ReleasesEveryStoreItOpens.
//
// # This test used to assert the opposite, and the change is deliberate
//
// It was TestStoreForTenant_OpensOncePerTenant, guarding a sync.Map of
// tenant -> store on the reasoning that "OpenStore is cheap on PostgreSQL and
// builds a connection pool on MySQL and SQL Server, so a per-workflow open
// would create pools at claim rate".
//
// The premise was wrong in the half that mattered. The POOL is cached inside
// the factory, not the store, so a warm tenant's OpenStore is a struct
// allocation on every dialect -- it does not build a pool, it looks one up.
// What the worker's cache actually held was a store, and therefore a pool, for
// the life of the process: it was the reason those pools could never be
// reaped, which is cleat#1928. (The one genuinely expensive case, PostgreSQL
// with a non-public schema re-issuing CREATE SCHEMA per call, is now latched
// after the first success in the factory.)
//
// So the invariant worth guarding is no longer "open once" but "release what
// you open". A resolve that does not release pins its tenant's pool forever,
// which looks exactly like the leak the reaper was added to fix.
func TestStoreForTenant_ReleasesEveryStoreItOpens(t *testing.T) {
	probe := &tenantScopeProbe{}
	w := newTenantScopeWorker(t, tenantSelf, probe)
	defer w.cancel()
	f := &recordingFactory{store: w.store}
	w.storeFactory = f

	const resolves = 5
	for i := 0; i < resolves; i++ {
		st, release, err := w.storeForTenant(tenantOther)
		if err != nil {
			t.Fatalf("storeForTenant: %v", err)
		}
		if st == nil {
			t.Fatal("storeForTenant returned no store and no error")
		}
		release()
	}
	if got := len(f.openedTenants()); got != resolves {
		t.Errorf("opened %d stores across %d resolves, want one per resolve -- "+
			"a cache here is what made the pools unreapable", got, resolves)
	}
	if got := f.leasesReleased(); got != resolves {
		t.Errorf("released %d leases across %d resolves, want one per resolve. "+
			"An unreleased lease pins that tenant's pool for the life of the "+
			"process, which is the leak the reaper exists to prevent.", got, resolves)
	}

	// The worker's own tenant never goes through the factory at all: its store
	// is the process-wide one, whose lease is held in main for the life of the
	// process.
	_, selfRelease, err := w.storeForTenant(tenantSelf)
	if err != nil {
		t.Fatalf("storeForTenant(self): %v", err)
	}
	selfRelease()
	for _, tid := range f.openedTenants() {
		if tid == tenantSelf {
			t.Error("opened a store for the worker's own tenant instead of using the process store")
		}
	}
	// The control. Without it the release count above passes just as well
	// against a storeForTenant that opens nothing and returns w.store for
	// every tenant.
	if len(f.openedTenants()) == 0 {
		t.Fatal("the factory was never called, so nothing above proves anything")
	}
}

// TestExecuteWorkflow_FailsWhenTheTenantStoreCannotBeOpened: falling back to
// the worker's store here would write the workflow under the wrong tenant,
// which is the whole thing this routing exists to prevent.
func TestExecuteWorkflow_FailsWhenTheTenantStoreCannotBeOpened(t *testing.T) {
	probe := &tenantScopeProbe{}
	w := newTenantScopeWorker(t, tenantSelf, probe)
	defer w.cancel()
	w.storeFactory = &recordingFactory{store: w.store, openErr: errOpenStore}

	runExecuteWorkflow(w, &engine.WorkflowInstance{
		ID: "wf-no-store", DefName: "test-workflow", TenantID: tenantOther,
	})

	probe.mu.Lock()
	defer probe.mu.Unlock()
	if probe.traced {
		t.Error("execution wrote to a store after the tenant store failed to open")
	}
	if !probe.failed {
		t.Fatal("workflow was neither run nor failed")
	}
	if probe.failOp != "tenant_store" {
		t.Errorf("errorOp = %q, want tenant_store", probe.failOp)
	}
}

// The tenant's lease is held for the WHOLE execution, and dropped after it.
//
// # Why this is the one call site the lease exists for
//
// Every other holder of a tenant store is bounded: a claim tick, a schedule
// fire, an HTTP request. A workflow is not -- it can sit inside a single
// activity for an hour without touching the database. So "nothing has opened a
// store for this tenant recently" says nothing about whether one is in use,
// and a reaper that evicted on that alone would close the pool under a live
// run. Its next query then fails with "sql: database is closed": a memory
// tidy-up turned into a workflow failure.
//
// # And why the release matters as much
//
// A lease that is never released pins the pool for the life of the process,
// which is the leak cleat#1928 is about, rebuilt with more code and a reaper
// that looks like it is working.
func TestExecuteWorkflow_HoldsTheTenantsLeaseForTheWholeRun(t *testing.T) {
	workerProbe := &tenantScopeProbe{}
	w := newTenantScopeWorker(t, tenantSelf, workerProbe)
	defer w.cancel()

	var f *recordingFactory
	outstandingAtFirstWrite := -1

	tenantStore := &mockStore{}
	tenantStore.traceWorkflowFn = func(context.Context, string, string) error {
		// The first store write in executeWorkflow, so this is inside the
		// window the lease has to cover.
		outstandingAtFirstWrite = len(f.openedTenants()) - f.leasesReleased()
		return nil
	}
	f = &recordingFactory{store: tenantStore}
	w.storeFactory = f
	w.taskQueues = []string{"default"}

	runExecuteWorkflow(w, &engine.WorkflowInstance{
		ID: "wf-leased", DefName: "test-workflow", TenantID: tenantOther,
	})

	if outstandingAtFirstWrite < 0 {
		t.Fatal("the tenant store was never written through, so this test observed nothing")
	}
	if outstandingAtFirstWrite < 1 {
		t.Errorf("no lease was outstanding at the first store write (opened-released = %d). "+
			"The reaper is free to close this tenant's pool mid-run.", outstandingAtFirstWrite)
	}
	if opened, released := len(f.openedTenants()), f.leasesReleased(); opened != released {
		t.Errorf("execution opened %d tenant stores and released %d leases. An unreleased "+
			"lease pins that tenant's pool for the life of the process.", opened, released)
	}
}
