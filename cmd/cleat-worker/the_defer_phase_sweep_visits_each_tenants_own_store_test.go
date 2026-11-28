package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// cleat#3572's defer-phase arm, the routing half, against recording stores and
// reusing the doubles from the_reaper_visits_each_tenants_own_store_test.go:
// which store is asked, and what a failure of one means. ExpireDeferPhases is
// scoped to its store's tenant, so a sweep through the worker's own store alone
// never ended another tenant's overdue phase. The half that needs a real database
// and a role the policy applies to is
// a_tenants_defer_phase_deadline_is_swept_under_rls_test.go.

// deferSweepProbe is a store that records its ExpireDeferPhases calls.
type deferSweepProbe struct {
	*mockStore
	mu    sync.Mutex
	calls int
	err   error
}

func (p *deferSweepProbe) ExpireDeferPhases(context.Context) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return 0, p.err
}

func (p *deferSweepProbe) FinalizeDeferPhase(context.Context, string, string, int64, []engine.EventRecord) error {
	return nil
}

func (p *deferSweepProbe) swept() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func newDeferSweepProbe(err error) *deferSweepProbe {
	return &deferSweepProbe{mockStore: &mockStore{}, err: err}
}

// ownDeferSweeper is the worker's own store: a probe that also enumerates tenants.
type ownDeferSweeper struct {
	*deferSweepProbe
	tenants []string
	listErr error
}

func (s *ownDeferSweeper) ListTenantIDs(context.Context) ([]string, error) {
	return s.tenants, s.listErr
}

// ownListingStore is a worker's own store that enumerates tenants but has no
// defer-phase support.
type ownListingStore struct {
	*mockStore
	tenants []string
}

func (s *ownListingStore) ListTenantIDs(context.Context) ([]string, error) { return s.tenants, nil }

func newDeferSweepWorker(own *ownDeferSweeper, f *perTenantFactory) *Worker {
	w := newTestWorkerFromStore(own)
	w.storeTenantID = tenantSelf
	w.claimAcrossTenants = true
	w.storeFactory = f
	return w
}

func TestDeferPhaseSweepRunsThroughEachOtherTenantsOwnStore(t *testing.T) {
	own := &ownDeferSweeper{deferSweepProbe: newDeferSweepProbe(nil), tenants: []string{tenantSelf, tenantOther, "tenant-c"}}
	a, c := newDeferSweepProbe(nil), newDeferSweepProbe(nil)
	f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: a, "tenant-c": c}}
	w := newDeferSweepWorker(own, f)
	defer w.cancel()

	w.expireDeferPhases()

	// THE ASSERTION. Through the worker's own store alone (the bug) the other
	// tenants' probes are never asked.
	if own.swept() != 1 {
		t.Errorf("the worker's own store swept %d times, want 1 (the own tenant is not also visited through the factory)", own.swept())
	}
	if a.swept() != 1 || c.swept() != 1 {
		t.Errorf("each other tenant's store must sweep once: got %d and %d. A defer phase of a tenant the worker is not "+
			"scoped to is never ended past its deadline (cleat#3572)", a.swept(), c.swept())
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

func TestDeferPhaseSweepsTenantFailureIsNotTheWorkersDatabaseTrouble(t *testing.T) {
	own := &ownDeferSweeper{deferSweepProbe: newDeferSweepProbe(nil), tenants: []string{tenantSelf, tenantOther, "tenant-c"}}
	bad := newDeferSweepProbe(errors.New("pool for this tenant is down"))
	good := newDeferSweepProbe(nil)
	f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: bad, "tenant-c": good}}
	w := newDeferSweepWorker(own, f)
	defer w.cancel()
	w.lastDBTrouble.Store(time.Now().Add(-time.Hour).UnixNano())
	troubleBefore := w.lastDBTrouble.Load()

	w.expireDeferPhases()

	if good.swept() != 1 {
		t.Error("one tenant's failure stopped the next tenant being swept")
	}
	// recordDBTrouble means "this worker's database is in trouble" and holds the
	// stale-run reaper in grace (#3569's review).
	if got := w.lastDBTrouble.Load(); got != troubleBefore {
		t.Error("one tenant's defer-phase sweep failing was recorded as this worker's database trouble")
	}
	if got := w.consecutiveReapSkips.Load(); got != 0 {
		t.Errorf("consecutiveReapSkips = %d after one tenant failed; that counter is for the worker's own reap", got)
	}
}

func TestDeferPhaseSweepWhenATenantStoreCannotBeOpenedTheOthersAreStillSwept(t *testing.T) {
	own := &ownDeferSweeper{deferSweepProbe: newDeferSweepProbe(nil), tenants: []string{tenantSelf, tenantOther, "tenant-c"}}
	good := newDeferSweepProbe(nil)
	f := &perTenantFactory{
		stores:  map[string]engine.WorkflowStore{"tenant-c": good},
		openErr: map[string]error{tenantOther: errors.New("no pool")},
	}
	w := newDeferSweepWorker(own, f)
	defer w.cancel()
	w.lastDBTrouble.Store(time.Now().Add(-time.Hour).UnixNano())
	troubleBefore := w.lastDBTrouble.Load()

	w.expireDeferPhases()

	if good.swept() != 1 {
		t.Error("a tenant whose store would not open stopped the others being swept")
	}
	if w.lastDBTrouble.Load() != troubleBefore {
		t.Error("a tenant store that would not open was recorded as the worker's database trouble")
	}
}

func TestDeferPhaseSweepSkipsATenantWhoseStoreCannotSweepAndKeepsGoing(t *testing.T) {
	own := &ownDeferSweeper{deferSweepProbe: newDeferSweepProbe(nil), tenants: []string{tenantSelf, tenantOther, "tenant-c"}}
	good := newDeferSweepProbe(nil)
	// tenantOther's store has no defer-phase support at all: nothing to sweep, no error.
	f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: &mockStore{}, "tenant-c": good}}
	w := newDeferSweepWorker(own, f)
	defer w.cancel()

	w.expireDeferPhases()

	if good.swept() != 1 {
		t.Error("a tenant whose store cannot sweep stopped the next tenant being swept")
	}
}

func TestDeferPhaseSweepVisitsNoOtherTenantWhenTheOwnStoreFails(t *testing.T) {
	own := &ownDeferSweeper{deferSweepProbe: newDeferSweepProbe(errors.New("connection refused")), tenants: []string{tenantSelf, tenantOther}}
	other := newDeferSweepProbe(nil)
	f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: other}}
	w := newDeferSweepWorker(own, f)
	defer w.cancel()

	w.expireDeferPhases()

	if other.swept() != 0 {
		t.Error("the worker's own store failed (the database is in trouble) and another tenant was swept anyway")
	}
}

func TestDeferPhaseSweepStaysOnItsOwnTenantWhenItCannotServeOthers(t *testing.T) {
	other := newDeferSweepProbe(nil)
	f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: other}}

	t.Run("claim-across-tenants off", func(t *testing.T) {
		own := &ownDeferSweeper{deferSweepProbe: newDeferSweepProbe(nil), tenants: []string{tenantSelf, tenantOther}}
		w := newDeferSweepWorker(own, f)
		defer w.cancel()
		w.claimAcrossTenants = false
		w.expireDeferPhases()
		if other.swept() != 0 || own.swept() != 1 {
			t.Errorf("own swept %d, other %d; with --claim-across-tenants off only the own tenant is swept", own.swept(), other.swept())
		}
	})
	t.Run("no store factory", func(t *testing.T) {
		own := &ownDeferSweeper{deferSweepProbe: newDeferSweepProbe(nil), tenants: []string{tenantSelf, tenantOther}}
		w := newDeferSweepWorker(own, nil)
		w.storeFactory = nil
		defer w.cancel()
		w.expireDeferPhases()
		if own.swept() != 1 {
			t.Errorf("own swept %d, want 1", own.swept())
		}
	})
	t.Run("a listing that fails still sweeps the own tenant", func(t *testing.T) {
		own := &ownDeferSweeper{deferSweepProbe: newDeferSweepProbe(nil), listErr: errors.New("admin.tenants unreadable")}
		w := newDeferSweepWorker(own, f)
		defer w.cancel()
		w.lastDBTrouble.Store(time.Now().Add(-time.Hour).UnixNano())
		troubleBefore := w.lastDBTrouble.Load()
		w.expireDeferPhases()
		if own.swept() != 1 {
			t.Errorf("own swept %d, want 1", own.swept())
		}
		if w.lastDBTrouble.Load() != troubleBefore {
			t.Error("failing to list tenants was recorded as the worker's database trouble")
		}
	})
}

func TestDeferPhaseSweepOpensNoTenantWhenTheOwnStoreCannotSweep(t *testing.T) {
	// A store with no defer-phase support cannot mark a phase either, so there is
	// nothing to sweep for any tenant, and opening each of them every tick would be
	// pure cost. The factory must not be asked for a single store.
	own := &ownListingStore{mockStore: &mockStore{}, tenants: []string{tenantSelf, tenantOther, "tenant-c"}}
	a := newDeferSweepProbe(nil)
	f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: a, "tenant-c": newDeferSweepProbe(nil)}}
	w := newTestWorkerFromStore(own)
	defer w.cancel()
	w.storeTenantID = tenantSelf
	w.claimAcrossTenants = true
	w.storeFactory = f

	w.expireDeferPhases()

	f.mu.Lock()
	opened := append([]string(nil), f.opened...)
	f.mu.Unlock()
	if len(opened) != 0 || a.swept() != 0 {
		t.Errorf("the factory opened %v and swept %d time(s) though the worker's own store cannot sweep", opened, a.swept())
	}
}
