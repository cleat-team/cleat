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
}

func (f *recordingFactory) OpenStore(_ context.Context, tenantID string, taskQueues ...string) (engine.WorkflowStore, io.Closer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = append(f.opened, tenantID)
	f.queuesIn = append(f.queuesIn, taskQueues)
	if f.openErr != nil {
		return nil, nil, f.openErr
	}
	return f.store, io.NopCloser(strings.NewReader("")), nil
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

// TestStoreForTenant_OpensOncePerTenant. OpenStore is cheap on PostgreSQL and
// builds a connection pool on MySQL and SQL Server, so a per-workflow open
// would create pools at claim rate.
func TestStoreForTenant_OpensOncePerTenant(t *testing.T) {
	probe := &tenantScopeProbe{}
	w := newTenantScopeWorker(t, tenantSelf, probe)
	defer w.cancel()
	f := &recordingFactory{store: w.store}
	w.storeFactory = f

	for i := 0; i < 5; i++ {
		if _, err := w.storeForTenant(tenantOther); err != nil {
			t.Fatalf("storeForTenant: %v", err)
		}
	}
	if got := f.openedTenants(); len(got) != 1 {
		t.Errorf("opened %d stores for one tenant (%v), want 1", len(got), got)
	}

	// The worker's own tenant never goes through the factory at all.
	if _, err := w.storeForTenant(tenantSelf); err != nil {
		t.Fatalf("storeForTenant(self): %v", err)
	}
	for _, tid := range f.openedTenants() {
		if tid == tenantSelf {
			t.Error("opened a second store for the worker's own tenant instead of reusing it")
		}
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
