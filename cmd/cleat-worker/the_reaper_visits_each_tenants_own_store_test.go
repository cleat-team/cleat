package main

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// cleat#3570. ReapStaleInstances has no tenant predicate; what it reaches is
// whatever row-level security admits for the store's tenant. reapOnce used to run
// through the worker's own store only, so a crashed worker's run of any other
// tenant was invisible to every survivor. These are the routing half, against
// recording stores: which store is asked to reap, and what a failure of one means.
// The half that needs a real database and a role the policy applies to is
// a_crashed_tenants_run_is_reclaimed_under_rls_test.go.

// reapProbe is a store that records its ReapStaleInstances calls.
type reapProbe struct {
	*mockStore
	mu    sync.Mutex
	calls int
}

func newReapProbe(reclaims int, err error) *reapProbe {
	p := &reapProbe{mockStore: &mockStore{}}
	p.mockStore.reapStaleInstancesFn = func(context.Context, time.Duration) (int, error) {
		p.mu.Lock()
		p.calls++
		p.mu.Unlock()
		return reclaims, err
	}
	return p
}

func (p *reapProbe) reaped() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// ownReaper is the worker's own store: a reapProbe that also enumerates tenants.
type ownReaper struct {
	*reapProbe
	tenants []string
	listErr error
}

func (s *ownReaper) ListTenantIDs(context.Context) ([]string, error) {
	return s.tenants, s.listErr
}

// perTenantFactory hands out the store registered for each tenant.
type perTenantFactory struct {
	mu      sync.Mutex
	stores  map[string]engine.WorkflowStore
	openErr map[string]error
	opened  []string
	closed  int
}

func (f *perTenantFactory) OpenStore(_ context.Context, tenantID string, _ ...string) (engine.WorkflowStore, io.Closer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = append(f.opened, tenantID)
	if err := f.openErr[tenantID]; err != nil {
		return nil, nil, err
	}
	return f.stores[tenantID], closerFunc(func() error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.closed++
		return nil
	}), nil
}
func (f *perTenantFactory) DriverName() string      { return "test" }
func (f *perTenantFactory) Dialect() engine.Dialect { return engine.DialectPostgres }

func newReapWorker(own *ownReaper, f *perTenantFactory) *Worker {
	w := newTestWorkerFromStore(own)
	w.storeTenantID = tenantSelf
	w.reclaimTimeout = 50 * time.Millisecond
	w.claimAcrossTenants = true
	w.storeFactory = f
	seedRecentlyConfirmedHealthy(w)
	return w
}

func TestReaperReapsEachOtherTenantsRunsThroughThatTenantsOwnStore(t *testing.T) {
	own := &ownReaper{reapProbe: newReapProbe(1, nil), tenants: []string{tenantSelf, tenantOther, "tenant-c"}}
	a, c := newReapProbe(2, nil), newReapProbe(0, nil)
	f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: a, "tenant-c": c}}
	w := newReapWorker(own, f)
	defer w.cancel()

	w.reapOnce()

	// THE ASSERTION. Through the worker's own store alone (the bug) the other
	// tenants' probes are never asked.
	if own.reaped() != 1 {
		t.Errorf("the worker's own store reaped %d times, want 1 (the own tenant is not also visited through the factory)", own.reaped())
	}
	if a.reaped() != 1 || c.reaped() != 1 {
		t.Errorf("each other tenant's store must reap once: got %d and %d. A run of a tenant the surviving worker is not scoped to "+
			"is invisible to it, so it is never reclaimed (cleat#3570)", a.reaped(), c.reaped())
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

func TestReapersTenantFailureIsNotTheWorkersDatabaseTrouble(t *testing.T) {
	own := &ownReaper{reapProbe: newReapProbe(0, nil), tenants: []string{tenantSelf, tenantOther, "tenant-c"}}
	bad := newReapProbe(0, errors.New("pool for this tenant is down"))
	good := newReapProbe(0, nil)
	f := &perTenantFactory{
		stores:  map[string]engine.WorkflowStore{tenantOther: bad, "tenant-c": good},
		openErr: map[string]error{},
	}
	w := newReapWorker(own, f)
	defer w.cancel()
	w.lastDBTrouble.Store(time.Now().Add(-time.Hour).UnixNano())
	troubleBefore := w.lastDBTrouble.Load()

	w.reapOnce()

	if good.reaped() != 1 {
		t.Error("one tenant's failure stopped the next tenant being reaped")
	}
	if got := w.lastDBTrouble.Load(); got != troubleBefore {
		t.Error("one tenant's reap failing was recorded as this worker's database trouble, which keeps its own reaper in grace")
	}
	if got := w.consecutiveReapSkips.Load(); got != 0 {
		t.Errorf("consecutiveReapSkips = %d after one tenant failed; that counter is for the worker's own reap", got)
	}
}

func TestReaperWhenATenantStoreCannotBeOpenedTheOthersAreStillReaped(t *testing.T) {
	own := &ownReaper{reapProbe: newReapProbe(0, nil), tenants: []string{tenantSelf, tenantOther, "tenant-c"}}
	good := newReapProbe(0, nil)
	f := &perTenantFactory{
		stores:  map[string]engine.WorkflowStore{"tenant-c": good},
		openErr: map[string]error{tenantOther: errors.New("no pool")},
	}
	w := newReapWorker(own, f)
	defer w.cancel()
	w.lastDBTrouble.Store(time.Now().Add(-time.Hour).UnixNano())
	troubleBefore := w.lastDBTrouble.Load()

	w.reapOnce()

	if good.reaped() != 1 {
		t.Error("a tenant whose store would not open stopped the others being reaped")
	}
	if w.lastDBTrouble.Load() != troubleBefore {
		t.Error("a tenant store that would not open was recorded as the worker's database trouble")
	}
}

func TestReaperVisitsNoOtherTenantWhenTheOwnStoreFails(t *testing.T) {
	own := &ownReaper{reapProbe: newReapProbe(0, errors.New("connection refused")), tenants: []string{tenantSelf, tenantOther}}
	other := newReapProbe(0, nil)
	f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: other}}
	w := newReapWorker(own, f)
	defer w.cancel()
	before := time.Now()

	w.reapOnce()

	if other.reaped() != 0 {
		t.Error("the worker's own store failed (the database is in trouble) and another tenant's stale runs were reclaimed anyway")
	}
	if time.Unix(0, w.lastDBTrouble.Load()).Before(before) {
		t.Error("the worker's own reap failed and no database trouble was recorded")
	}
}

func TestReaperStaysOnItsOwnTenantWhenItCannotServeOthers(t *testing.T) {
	other := newReapProbe(0, nil)
	f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: other}}

	t.Run("claim-across-tenants off", func(t *testing.T) {
		own := &ownReaper{reapProbe: newReapProbe(0, nil), tenants: []string{tenantSelf, tenantOther}}
		w := newReapWorker(own, f)
		defer w.cancel()
		w.claimAcrossTenants = false
		w.reapOnce()
		if other.reaped() != 0 || own.reaped() != 1 {
			t.Errorf("own reaped %d, other %d; with --claim-across-tenants off only the own tenant is reaped", own.reaped(), other.reaped())
		}
	})
	t.Run("no store factory", func(t *testing.T) {
		own := &ownReaper{reapProbe: newReapProbe(0, nil), tenants: []string{tenantSelf, tenantOther}}
		w := newReapWorker(own, nil)
		w.storeFactory = nil
		defer w.cancel()
		w.reapOnce()
		if own.reaped() != 1 {
			t.Errorf("own reaped %d, want 1", own.reaped())
		}
	})
	t.Run("a listing that fails is not database trouble", func(t *testing.T) {
		own := &ownReaper{reapProbe: newReapProbe(0, nil), listErr: errors.New("admin.tenants unreadable")}
		w := newReapWorker(own, f)
		defer w.cancel()
		w.lastDBTrouble.Store(time.Now().Add(-time.Hour).UnixNano())
		troubleBefore := w.lastDBTrouble.Load()
		w.reapOnce()
		if w.lastDBTrouble.Load() != troubleBefore {
			t.Error("failing to list tenants was recorded as the worker's database trouble")
		}
	})
}
