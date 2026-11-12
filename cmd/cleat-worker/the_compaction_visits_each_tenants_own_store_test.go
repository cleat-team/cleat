package main

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// cleat#3572. GetCompactionCandidates and CompactWorkflowHistory scope themselves
// to their store's tenant, and compactionLoop ran them through the worker's own
// store only, so a workflow of any other tenant never compacted however long its
// history grew. The routing half, against recording stores and the doubles from
// the_reaper_visits_each_tenants_own_store_test.go: which store is asked for
// candidates, and -- the part a candidate list alone cannot show -- which store
// compacts what it was told. The half on a real database is
// a_stale_tenants_history_is_compacted_under_rls_test.go.

// compactProbe is a store with one compaction candidate of its own, recording
// every candidate query and every workflow compacted through it.
type compactProbe struct {
	*mockStore
	mu        sync.Mutex
	queries   [][2]int // threshold, limit
	compacted []string
}

const compactTestThreshold = 5

func newCompactProbe(candidate string, candidatesErr error, compactErr error) *compactProbe {
	var failing map[string]error
	if compactErr != nil {
		failing = map[string]error{candidate: compactErr}
	}
	return newCompactProbeN([]string{candidate}, candidatesErr, failing)
}

// newCompactProbeN is a store with several candidates, of which the ones named in
// failing fail to compact. A store with ONE candidate cannot tell "one workflow's
// failure is logged and the rest go on" from "the first failure ends the tenant":
// there is no rest.
func newCompactProbeN(candidates []string, candidatesErr error, failing map[string]error) *compactProbe {
	p := &compactProbe{mockStore: &mockStore{}}
	p.mockStore.getCompactionCandidatesFn = func(_ context.Context, threshold, limit int) ([]string, error) {
		p.mu.Lock()
		p.queries = append(p.queries, [2]int{threshold, limit})
		p.mu.Unlock()
		if candidatesErr != nil {
			return nil, candidatesErr
		}
		return append([]string(nil), candidates...), nil
	}
	p.mockStore.getWorkflowByIDFn = func(_ context.Context, id string) (*engine.WorkflowInstance, error) {
		return &engine.WorkflowInstance{ID: id, DefName: "mock-wf", DefVersion: 1}, nil
	}
	p.mockStore.loadEventHistoryFn = func(context.Context, string) ([]engine.EventRecord, error) {
		events := make([]engine.EventRecord, compactTestThreshold*3)
		for i := range events {
			events[i] = engine.EventRecord{Step: i, EventType: "call"}
		}
		return events, nil
	}
	p.mockStore.compactHistoryFn = func(_ context.Context, id string, _ []byte, _, _ int) error {
		p.mu.Lock()
		p.compacted = append(p.compacted, id)
		p.mu.Unlock()
		return failing[id]
	}
	return p
}

func (p *compactProbe) snapshot() (queries [][2]int, compacted []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([][2]int(nil), p.queries...), append([]string(nil), p.compacted...)
}

type ownCompactor struct {
	*compactProbe
	tenants []string
	listErr error
}

func (s *ownCompactor) ListTenantIDs(context.Context) ([]string, error) {
	return s.tenants, s.listErr
}

func newCompactWorker(own *ownCompactor, f *perTenantFactory) *Worker {
	w := newTestWorkerFromStore(own)
	w.storeTenantID = tenantSelf
	w.claimAcrossTenants = true
	w.storeFactory = f
	w.compactionThreshold = compactTestThreshold
	return w
}

func TestCompactionCompactsEachOtherTenantsWorkflowsThroughThatTenantsOwnStore(t *testing.T) {
	own := &ownCompactor{compactProbe: newCompactProbe("wf-own", nil, nil), tenants: []string{tenantSelf, tenantOther, "tenant-c"}}
	a, c := newCompactProbe("wf-a", nil, nil), newCompactProbe("wf-c", nil, nil)
	f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: a, "tenant-c": c}}
	w := newCompactWorker(own, f)
	defer w.cancel()

	w.compactOnce()

	// THE ASSERTION. Through the worker's own store alone (the bug) the other
	// tenants' probes are never asked, and a candidate of theirs is never compacted.
	for name, tc := range map[string]struct {
		p    *compactProbe
		want string
	}{"own": {own.compactProbe, "wf-own"}, tenantOther: {a, "wf-a"}, "tenant-c": {c, "wf-c"}} {
		q, done := tc.p.snapshot()
		if len(q) != 1 || q[0] != [2]int{compactTestThreshold, compactionCandidatesPerTick} {
			t.Errorf("%s: candidate queries %v, want one with threshold %d and limit %d", name, q, compactTestThreshold, compactionCandidatesPerTick)
		}
		// The candidate found through a tenant's store is compacted through that
		// SAME store: another tenant's store would not find the workflow at all.
		if len(done) != 1 || done[0] != tc.want {
			t.Errorf("%s: compacted %v through its own store, want [%s]: its long histories are never compacted (cleat#3572)", name, done, tc.want)
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

func TestAnEarlierTenantsCompactionFailureDoesNotStopALaterTenant(t *testing.T) {
	own := &ownCompactor{compactProbe: newCompactProbe("wf-own", nil, nil), tenants: []string{tenantSelf, tenantOther, "tenant-c", "tenant-d"}}
	noCandidates := newCompactProbe("wf-b", errors.New("pool for this tenant is down"), nil)
	failsToCompact := newCompactProbe("wf-c", nil, errors.New("compact failed"))
	good := newCompactProbe("wf-d", nil, nil)
	f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: noCandidates, "tenant-c": failsToCompact, "tenant-d": good}}
	w := newCompactWorker(own, f)
	defer w.cancel()
	w.lastDBTrouble.Store(1)
	troubleBefore := w.lastDBTrouble.Load()

	w.compactOnce()

	if _, done := good.snapshot(); len(done) != 1 {
		t.Error("an earlier tenant failing stopped a later tenant being compacted")
	}
	if got := w.lastDBTrouble.Load(); got != troubleBefore {
		t.Error("one tenant's compaction failing was recorded as this worker's database trouble")
	}
}

// One workflow's failure to compact is logged and the tenant's NEXT candidate is still compacted: a
// store with a single candidate cannot show that, so this tenant has two, the first failing, and the
// tenant after it has one.
func TestAWorkflowThatFailsToCompactDoesNotStopThatTenantsNextCandidate(t *testing.T) {
	own := &ownCompactor{compactProbe: newCompactProbe("wf-own", nil, nil), tenants: []string{tenantSelf, tenantOther, "tenant-c"}}
	two := newCompactProbeN([]string{"wf-b1", "wf-b2"}, nil, map[string]error{"wf-b1": errors.New("compact failed")})
	next := newCompactProbe("wf-c", nil, nil)
	f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: two, "tenant-c": next}}
	w := newCompactWorker(own, f)
	defer w.cancel()

	w.compactOnce()

	_, done := two.snapshot()
	if len(done) != 2 || done[0] != "wf-b1" || done[1] != "wf-b2" {
		t.Errorf("the failing tenant's candidates were compacted as %v, want [wf-b1 wf-b2]: the first failure ended that tenant's pass, "+
			"and its second workflow is never compacted until the next tick finds it again", done)
	}
	if _, done := next.snapshot(); len(done) != 1 {
		t.Errorf("the tenant after the failing one compacted %v, want [wf-c]", done)
	}
}

func TestCompactionWhenATenantStoreCannotBeOpenedTheOthersAreStillCompacted(t *testing.T) {
	own := &ownCompactor{compactProbe: newCompactProbe("wf-own", nil, nil), tenants: []string{tenantSelf, tenantOther, "tenant-c"}}
	good := newCompactProbe("wf-c", nil, nil)
	f := &perTenantFactory{
		stores:  map[string]engine.WorkflowStore{"tenant-c": good},
		openErr: map[string]error{tenantOther: errors.New("no pool")},
	}
	w := newCompactWorker(own, f)
	defer w.cancel()

	w.compactOnce()

	if _, done := good.snapshot(); len(done) != 1 {
		t.Error("a tenant whose store would not open stopped the others being compacted")
	}
}

func TestCompactionVisitsNoOtherTenantWhenTheOwnStoreFailsToFindCandidates(t *testing.T) {
	own := &ownCompactor{compactProbe: newCompactProbe("wf-own", errors.New("connection refused"), nil), tenants: []string{tenantSelf, tenantOther}}
	other := newCompactProbe("wf-a", nil, nil)
	f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: other}}
	w := newCompactWorker(own, f)
	defer w.cancel()

	w.compactOnce()

	if q, _ := other.snapshot(); len(q) != 0 {
		t.Error("the worker's own store failed (the database is in trouble) and another tenant was visited anyway")
	}
}

func TestCompactionStaysOnItsOwnTenantWhenItCannotServeOthers(t *testing.T) {
	other := newCompactProbe("wf-a", nil, nil)
	f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: other}}

	t.Run("claim-across-tenants off", func(t *testing.T) {
		own := &ownCompactor{compactProbe: newCompactProbe("wf-own", nil, nil), tenants: []string{tenantSelf, tenantOther}}
		w := newCompactWorker(own, f)
		defer w.cancel()
		w.claimAcrossTenants = false
		w.compactOnce()
		oq, _ := other.snapshot()
		q, _ := own.snapshot()
		if len(oq) != 0 || len(q) != 1 {
			t.Errorf("own %d queries, other %d; with --claim-across-tenants off only the own tenant is compacted", len(q), len(oq))
		}
	})
	t.Run("no store factory", func(t *testing.T) {
		own := &ownCompactor{compactProbe: newCompactProbe("wf-own", nil, nil), tenants: []string{tenantSelf, tenantOther}}
		w := newCompactWorker(own, nil)
		w.storeFactory = nil
		defer w.cancel()
		w.compactOnce()
		if q, _ := own.snapshot(); len(q) != 1 {
			t.Errorf("own %d queries, want 1", len(q))
		}
	})
	t.Run("a listing that fails still compacts the own tenant", func(t *testing.T) {
		own := &ownCompactor{compactProbe: newCompactProbe("wf-own", nil, nil), listErr: errors.New("admin.tenants unreadable")}
		w := newCompactWorker(own, f)
		defer w.cancel()
		w.lastDBTrouble.Store(1)
		troubleBefore := w.lastDBTrouble.Load()
		w.compactOnce()
		if q, _ := own.snapshot(); len(q) != 1 {
			t.Errorf("own %d queries, want 1", len(q))
		}
		if w.lastDBTrouble.Load() != troubleBefore {
			t.Error("failing to list tenants was recorded as the worker's database trouble")
		}
	})
}
