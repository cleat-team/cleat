package main

// cleat#1040's second half, and the half a store-level test cannot see.
//
// The engine test TestTheMemoryProfileIsScopedToTenant drives two
// tenant-scoped stores directly, so it proves the STATEMENTS carry a tenant
// predicate. It says nothing about which store the worker hands the sample
// to -- and the worker holds exactly one store of its own, opened as
// storeTenantID, while executing workflows for any tenant it can claim.
//
// So before this fix there were two defects stacked, and fixing only the
// first would have left every tenant's samples attributed to the worker's
// own tenant: no longer a cross-tenant READ, but the feature silently dead
// for every other tenant, with the engine test green throughout. That is the
// "watch which layer is holding the test up" case from CLAUDE.md.

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestAMemorySampleIsPersistedThroughTheWorkflowsOwnTenantStore asserts the
// routing decision, not the SQL: the sample must reach the store it was
// handed, and must not reach the controller's own.
func TestAMemorySampleIsPersistedThroughTheWorkflowsOwnTenantStore(t *testing.T) {
	var mu sync.Mutex
	var controllerStoreGot, tenantStoreGot []string

	controllerStore := &mockStore{
		recordWorkflowMemorySampleFn: func(_ context.Context, defName string, _ int64) error {
			mu.Lock()
			defer mu.Unlock()
			controllerStoreGot = append(controllerStoreGot, defName)
			return nil
		},
	}
	tenantStore := &mockStore{
		recordWorkflowMemorySampleFn: func(_ context.Context, defName string, _ int64) error {
			mu.Lock()
			defer mu.Unlock()
			tenantStoreGot = append(tenantStoreGot, defName)
			return nil
		},
	}

	mc := newTestController(newTestMonitor(), 10, 0.80, 0.95)
	mc.store = controllerStore

	mc.RecordWorkflowMemory(context.Background(), tenantStore, "wf-of-another-tenant", 4*1024*1024)

	// The persist is deliberately asynchronous so a slow write cannot delay a
	// workflow, so this waits on the observable effect rather than sleeping a
	// fixed interval -- an assertion on a wall clock is what CLAUDE.md asks
	// not to write.
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		done := len(tenantStoreGot)+len(controllerStoreGot) > 0
		mu.Unlock()
		if done || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(tenantStoreGot) != 1 || tenantStoreGot[0] != "wf-of-another-tenant" {
		t.Errorf("the workflow's own tenant store received %v, want one sample for wf-of-another-tenant",
			tenantStoreGot)
	}
	if len(controllerStoreGot) != 0 {
		t.Errorf("the controller's own store received %v; a sample routed there is attributed to "+
			"the worker's tenant rather than the workflow's", controllerStoreGot)
	}
}

// TestAMemorySampleFallsBackToTheControllerStore pins the nil case, so the
// fallback is a decision with a test rather than an accident of the nil
// check. It is what a single-tenant deployment takes, and what the execute
// path takes when the tenant's store cannot be opened.
func TestAMemorySampleFallsBackToTheControllerStore(t *testing.T) {
	var mu sync.Mutex
	var got []string

	mc := newTestController(newTestMonitor(), 10, 0.80, 0.95)
	mc.store = &mockStore{
		recordWorkflowMemorySampleFn: func(_ context.Context, defName string, _ int64) error {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, defName)
			return nil
		},
	}

	mc.RecordWorkflowMemory(context.Background(), nil, "wf-single-tenant", 1024)

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		done := len(got) > 0
		mu.Unlock()
		if done || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != "wf-single-tenant" {
		t.Errorf("controller store received %v, want one sample for wf-single-tenant", got)
	}
}
