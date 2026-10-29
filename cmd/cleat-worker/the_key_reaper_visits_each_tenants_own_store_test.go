package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// cleat#3572. ReapExpiredConcurrencyKeys scopes itself to its store's tenant, and
// concurrencyKeyReaperLoop ran it through the worker's own store only, so a key
// freed for the worker's tenant was never freed for any other. These are the
// routing half, against recording stores, reusing the doubles from
// the_reaper_visits_each_tenants_own_store_test.go: which store is asked, and what
// a failure of one means. The half that needs a real database and a role the
// policy applies to is a_stale_concurrency_key_of_another_tenant_is_reaped_under_rls_test.go.

// keyReapProbe is a store that records its ReapExpiredConcurrencyKeys calls.
type keyReapProbe struct {
	*mockStore
	mu    sync.Mutex
	calls int
}

func newKeyReapProbe(removed int64, err error) *keyReapProbe {
	p := &keyReapProbe{mockStore: &mockStore{}}
	p.mockStore.reapExpiredConcurrencyKeysFn = func(context.Context) (int64, error) {
		p.mu.Lock()
		p.calls++
		p.mu.Unlock()
		return removed, err
	}
	return p
}

func (p *keyReapProbe) reaped() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// ownKeyReaper is the worker's own store: a keyReapProbe that also enumerates tenants.
type ownKeyReaper struct {
	*keyReapProbe
	tenants []string
	listErr error
}

func (s *ownKeyReaper) ListTenantIDs(context.Context) ([]string, error) {
	return s.tenants, s.listErr
}

func newKeyReapWorker(own *ownKeyReaper, f *perTenantFactory) *Worker {
	w := newTestWorkerFromStore(own)
	w.storeTenantID = tenantSelf
	w.claimAcrossTenants = true
	w.storeFactory = f
	return w
}

func TestKeyReaperReapsEachOtherTenantThroughThatTenantsOwnStore(t *testing.T) {
	own := &ownKeyReaper{keyReapProbe: newKeyReapProbe(1, nil), tenants: []string{tenantSelf, tenantOther, "tenant-c"}}
	a, c := newKeyReapProbe(2, nil), newKeyReapProbe(0, nil)
	f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: a, "tenant-c": c}}
	w := newKeyReapWorker(own, f)
	defer w.cancel()

	w.reapConcurrencyKeysOnce()

	// THE ASSERTION. Through the worker's own store alone (the bug) the other
	// tenants' probes are never asked.
	if own.reaped() != 1 {
		t.Errorf("the worker's own store reaped %d times, want 1 (the own tenant is not also visited through the factory)", own.reaped())
	}
	if a.reaped() != 1 || c.reaped() != 1 {
		t.Errorf("each other tenant's store must reap once: got %d and %d. A key of a tenant the worker is not scoped to "+
			"is never freed, and that tenant's new runs under it are refused (cleat#3572)", a.reaped(), c.reaped())
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

func TestKeyReapersTenantFailureIsNotTheWorkersDatabaseTrouble(t *testing.T) {
	own := &ownKeyReaper{keyReapProbe: newKeyReapProbe(0, nil), tenants: []string{tenantSelf, tenantOther, "tenant-c"}}
	bad := newKeyReapProbe(0, errors.New("pool for this tenant is down"))
	good := newKeyReapProbe(0, nil)
	f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: bad, "tenant-c": good}}
	w := newKeyReapWorker(own, f)
	defer w.cancel()
	w.lastDBTrouble.Store(time.Now().Add(-time.Hour).UnixNano())
	troubleBefore := w.lastDBTrouble.Load()

	w.reapConcurrencyKeysOnce()

	if good.reaped() != 1 {
		t.Error("one tenant's failure stopped the next tenant being reaped")
	}
	// recordDBTrouble and recordReapSkip mean "this worker's database is in
	// trouble" and keep the stale-run reaper in grace (#3569's review).
	if got := w.lastDBTrouble.Load(); got != troubleBefore {
		t.Error("one tenant's key reap failing was recorded as this worker's database trouble")
	}
	if got := w.consecutiveReapSkips.Load(); got != 0 {
		t.Errorf("consecutiveReapSkips = %d after one tenant failed; that counter is for the worker's own reap", got)
	}
}

func TestKeyReaperWhenATenantStoreCannotBeOpenedTheOthersAreStillReaped(t *testing.T) {
	own := &ownKeyReaper{keyReapProbe: newKeyReapProbe(0, nil), tenants: []string{tenantSelf, tenantOther, "tenant-c"}}
	good := newKeyReapProbe(0, nil)
	f := &perTenantFactory{
		stores:  map[string]engine.WorkflowStore{"tenant-c": good},
		openErr: map[string]error{tenantOther: errors.New("no pool")},
	}
	w := newKeyReapWorker(own, f)
	defer w.cancel()
	w.lastDBTrouble.Store(time.Now().Add(-time.Hour).UnixNano())
	troubleBefore := w.lastDBTrouble.Load()

	w.reapConcurrencyKeysOnce()

	if good.reaped() != 1 {
		t.Error("a tenant whose store would not open stopped the others being reaped")
	}
	if w.lastDBTrouble.Load() != troubleBefore {
		t.Error("a tenant store that would not open was recorded as the worker's database trouble")
	}
}

func TestKeyReaperVisitsNoOtherTenantWhenTheOwnStoreFails(t *testing.T) {
	own := &ownKeyReaper{keyReapProbe: newKeyReapProbe(0, errors.New("connection refused")), tenants: []string{tenantSelf, tenantOther}}
	other := newKeyReapProbe(0, nil)
	f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: other}}
	w := newKeyReapWorker(own, f)
	defer w.cancel()

	w.reapConcurrencyKeysOnce()

	if other.reaped() != 0 {
		t.Error("the worker's own store failed (the database is in trouble) and another tenant's keys were reaped anyway")
	}
}

func TestKeyReaperStaysOnItsOwnTenantWhenItCannotServeOthers(t *testing.T) {
	other := newKeyReapProbe(0, nil)
	f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: other}}

	t.Run("claim-across-tenants off", func(t *testing.T) {
		own := &ownKeyReaper{keyReapProbe: newKeyReapProbe(0, nil), tenants: []string{tenantSelf, tenantOther}}
		w := newKeyReapWorker(own, f)
		defer w.cancel()
		w.claimAcrossTenants = false
		w.reapConcurrencyKeysOnce()
		if other.reaped() != 0 || own.reaped() != 1 {
			t.Errorf("own reaped %d, other %d; with --claim-across-tenants off only the own tenant is reaped", own.reaped(), other.reaped())
		}
	})
	t.Run("no store factory", func(t *testing.T) {
		own := &ownKeyReaper{keyReapProbe: newKeyReapProbe(0, nil), tenants: []string{tenantSelf, tenantOther}}
		w := newKeyReapWorker(own, nil)
		w.storeFactory = nil
		defer w.cancel()
		w.reapConcurrencyKeysOnce()
		if own.reaped() != 1 {
			t.Errorf("own reaped %d, want 1", own.reaped())
		}
	})
	t.Run("a listing that fails still reaps the own tenant", func(t *testing.T) {
		own := &ownKeyReaper{keyReapProbe: newKeyReapProbe(0, nil), listErr: errors.New("admin.tenants unreadable")}
		w := newKeyReapWorker(own, f)
		defer w.cancel()
		w.lastDBTrouble.Store(time.Now().Add(-time.Hour).UnixNano())
		troubleBefore := w.lastDBTrouble.Load()
		w.reapConcurrencyKeysOnce()
		if own.reaped() != 1 {
			t.Errorf("own reaped %d, want 1", own.reaped())
		}
		if w.lastDBTrouble.Load() != troubleBefore {
			t.Error("failing to list tenants was recorded as the worker's database trouble")
		}
	})
}
