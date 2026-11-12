package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// cleat#3572. RecordWorkflowMemory persists each sample to the store of the tenant
// the workflow belongs to, and memoryCleanupLoop trimmed the worker's own tenant
// only, so every other tenant's workflow_memory_samples was never trimmed. The
// routing half, against recording stores and the doubles from
// the_reaper_visits_each_tenants_own_store_test.go: which store is asked, with
// what cap, and what one tenant's failure means.

type memCleanProbe struct {
	*mockStore
	mu   sync.Mutex
	caps []int
}

func newMemCleanProbe(removed int64, err error) *memCleanProbe {
	p := &memCleanProbe{mockStore: &mockStore{}}
	p.mockStore.cleanupMemorySamplesFn = func(_ context.Context, max int) (int64, error) {
		p.mu.Lock()
		p.caps = append(p.caps, max)
		p.mu.Unlock()
		return removed, err
	}
	return p
}

func (p *memCleanProbe) calls() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.caps...)
}

type ownMemCleaner struct {
	*memCleanProbe
	tenants []string
	listErr error
}

func (s *ownMemCleaner) ListTenantIDs(context.Context) ([]string, error) {
	return s.tenants, s.listErr
}

func newMemCleanWorker(own *ownMemCleaner, f *perTenantFactory) *Worker {
	w := newTestWorkerFromStore(own)
	w.storeTenantID = tenantSelf
	w.claimAcrossTenants = true
	w.storeFactory = f
	return w
}

func TestMemoryCleanupTrimsEachOtherTenantThroughThatTenantsOwnStore(t *testing.T) {
	const cap = 50
	own := &ownMemCleaner{memCleanProbe: newMemCleanProbe(1, nil), tenants: []string{tenantSelf, tenantOther, "tenant-c"}}
	a, c := newMemCleanProbe(2, nil), newMemCleanProbe(0, nil)
	f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: a, "tenant-c": c}}
	w := newMemCleanWorker(own, f)
	defer w.cancel()

	w.cleanupMemorySamplesOnce(cap)

	// THE ASSERTION. Through the worker's own store alone (the bug) the other
	// tenants' probes are never asked.
	if got := own.calls(); len(got) != 1 || got[0] != cap {
		t.Errorf("the worker's own store was asked %v, want one call with the cap %d (the own tenant is not also visited through the factory)", got, cap)
	}
	for name, p := range map[string]*memCleanProbe{tenantOther: a, "tenant-c": c} {
		if got := p.calls(); len(got) != 1 || got[0] != cap {
			t.Errorf("tenant %s's store was asked %v, want one call with the cap %d: its samples are never trimmed (cleat#3572)", name, got, cap)
		}
	}
	f.mu.Lock()
	opened, closed := append([]string(nil), f.opened...), f.closed
	f.mu.Unlock()
	if !sameSet(opened, []string{tenantOther, "tenant-c"}) {
		t.Errorf("the factory opened %v, want the two other tenants and not the worker's own", opened)
	}
	if closed != 2 {
		t.Errorf("%d tenant store leases released, want 2", closed)
	}
}

func TestMemoryCleanupsTenantFailureIsNotTheWorkersDatabaseTrouble(t *testing.T) {
	own := &ownMemCleaner{memCleanProbe: newMemCleanProbe(0, nil), tenants: []string{tenantSelf, tenantOther, "tenant-c"}}
	bad := newMemCleanProbe(0, errors.New("pool for this tenant is down"))
	good := newMemCleanProbe(0, nil)
	f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: bad, "tenant-c": good}}
	w := newMemCleanWorker(own, f)
	defer w.cancel()
	w.lastDBTrouble.Store(time.Now().Add(-time.Hour).UnixNano())
	troubleBefore := w.lastDBTrouble.Load()

	w.cleanupMemorySamplesOnce(10)

	if len(good.calls()) != 1 {
		t.Error("one tenant's failure stopped the next tenant being trimmed")
	}
	if got := w.lastDBTrouble.Load(); got != troubleBefore {
		t.Error("one tenant's cleanup failing was recorded as this worker's database trouble")
	}
}

func TestMemoryCleanupWhenATenantStoreCannotBeOpenedTheOthersAreStillTrimmed(t *testing.T) {
	own := &ownMemCleaner{memCleanProbe: newMemCleanProbe(0, nil), tenants: []string{tenantSelf, tenantOther, "tenant-c"}}
	good := newMemCleanProbe(0, nil)
	f := &perTenantFactory{
		stores:  map[string]engine.WorkflowStore{"tenant-c": good},
		openErr: map[string]error{tenantOther: errors.New("no pool")},
	}
	w := newMemCleanWorker(own, f)
	defer w.cancel()

	w.cleanupMemorySamplesOnce(10)

	if len(good.calls()) != 1 {
		t.Error("a tenant whose store would not open stopped the others being trimmed")
	}
}

func TestMemoryCleanupVisitsNoOtherTenantWhenTheOwnStoreFails(t *testing.T) {
	own := &ownMemCleaner{memCleanProbe: newMemCleanProbe(0, errors.New("connection refused")), tenants: []string{tenantSelf, tenantOther}}
	other := newMemCleanProbe(0, nil)
	f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: other}}
	w := newMemCleanWorker(own, f)
	defer w.cancel()

	w.cleanupMemorySamplesOnce(10)

	if len(other.calls()) != 0 {
		t.Error("the worker's own store failed (the database is in trouble) and another tenant was trimmed anyway")
	}
}

func TestMemoryCleanupStaysOnItsOwnTenantWhenItCannotServeOthers(t *testing.T) {
	other := newMemCleanProbe(0, nil)
	f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: other}}

	t.Run("claim-across-tenants off", func(t *testing.T) {
		own := &ownMemCleaner{memCleanProbe: newMemCleanProbe(0, nil), tenants: []string{tenantSelf, tenantOther}}
		w := newMemCleanWorker(own, f)
		defer w.cancel()
		w.claimAcrossTenants = false
		w.cleanupMemorySamplesOnce(10)
		if len(other.calls()) != 0 || len(own.calls()) != 1 {
			t.Errorf("own %v, other %v; with --claim-across-tenants off only the own tenant is trimmed", own.calls(), other.calls())
		}
	})
	t.Run("no store factory", func(t *testing.T) {
		own := &ownMemCleaner{memCleanProbe: newMemCleanProbe(0, nil), tenants: []string{tenantSelf, tenantOther}}
		w := newMemCleanWorker(own, nil)
		w.storeFactory = nil
		defer w.cancel()
		w.cleanupMemorySamplesOnce(10)
		if len(own.calls()) != 1 {
			t.Errorf("own %v, want 1 call", own.calls())
		}
	})
	t.Run("a listing that fails still trims the own tenant", func(t *testing.T) {
		own := &ownMemCleaner{memCleanProbe: newMemCleanProbe(0, nil), listErr: errors.New("admin.tenants unreadable")}
		w := newMemCleanWorker(own, f)
		defer w.cancel()
		w.lastDBTrouble.Store(time.Now().Add(-time.Hour).UnixNano())
		troubleBefore := w.lastDBTrouble.Load()
		w.cleanupMemorySamplesOnce(10)
		if len(own.calls()) != 1 {
			t.Errorf("own %v, want 1 call", own.calls())
		}
		if w.lastDBTrouble.Load() != troubleBefore {
			t.Error("failing to list tenants was recorded as the worker's database trouble")
		}
	})
}
