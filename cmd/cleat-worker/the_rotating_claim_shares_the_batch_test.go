package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// queuedTenantStore is a mockStore holding a fixed backlog of claimable
// workflows for ONE tenant, so a claim that asks for n gets min(n, backlog).
type queuedTenantStore struct {
	*mockStore
	tenantID string

	mu        sync.Mutex
	remaining int
	asked     []int // limits this tenant was asked for, in call order

	// Schedule fixtures, used by the due-schedule tests in the sibling file.
	scheduleReads int
	scheduleErr   error
}

func (s *queuedTenantStore) ClaimWorkflows(_ context.Context, _ string, limit int) ([]*engine.WorkflowInstance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, limit)
	n := limit
	if n > s.remaining {
		n = s.remaining
	}
	s.remaining -= n
	out := make([]*engine.WorkflowInstance, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, &engine.WorkflowInstance{ID: s.tenantID, TenantID: s.tenantID})
	}
	return out, nil
}

// listingStore is the worker's own store: it enumerates tenants and nothing
// else, which is exactly what the rotating claim asks of it.
type listingStore struct {
	*mockStore
	tenants []string
	listErr error
	lists   int
}

func (s *listingStore) ListTenantIDs(context.Context) ([]string, error) {
	s.lists++
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.tenants, nil
}

// tenantStoreFactory hands out one queuedTenantStore per tenant.
type tenantStoreFactory struct {
	mu      sync.Mutex
	stores  map[string]*queuedTenantStore
	openErr map[string]error
}

func (f *tenantStoreFactory) OpenStore(_ context.Context, tenantID string, _ ...string) (engine.WorkflowStore, io.Closer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.openErr[tenantID]; err != nil {
		return nil, nil, err
	}
	st, ok := f.stores[tenantID]
	if !ok {
		return nil, nil, errors.New("no such tenant")
	}
	return st, io.NopCloser(strings.NewReader("")), nil
}

func (f *tenantStoreFactory) DriverName() string      { return "tenant-fixture" }
func (f *tenantStoreFactory) Dialect() engine.Dialect { return engine.DialectPostgres }

// newRotatingWorker wires a worker whose tenants each hold `backlog[i]`
// claimable workflows.
func newRotatingWorker(t *testing.T, backlog map[string]int) (*Worker, *tenantStoreFactory, *listingStore) {
	t.Helper()
	tenants := make([]string, 0, len(backlog))
	factory := &tenantStoreFactory{stores: map[string]*queuedTenantStore{}, openErr: map[string]error{}}
	// Sorted by construction below so the rotation order is the test's to
	// reason about rather than a map's.
	for _, id := range sortedKeys(backlog) {
		tenants = append(tenants, id)
		factory.stores[id] = &queuedTenantStore{
			mockStore: &mockStore{}, tenantID: id, remaining: backlog[id],
		}
	}
	ls := &listingStore{mockStore: &mockStore{}, tenants: tenants}
	w := newTestWorker(ls.mockStore)
	t.Cleanup(w.cancel)
	w.store = ls
	w.storeFactory = factory
	// Empty, so storeForTenant routes EVERY tenant through the factory rather
	// than short-circuiting one of them to the worker's own store.
	w.storeTenantID = ""
	w.claimAcrossTenants = true
	w.claimStrategy = claimStrategyRotate
	return w, factory, ls
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func claimedByTenant(wfs []*engine.WorkflowInstance) map[string]int {
	out := map[string]int{}
	for _, wf := range wfs {
		out[wf.TenantID]++
	}
	return out
}

// A tenant with a large backlog takes its share of the batch and no more.
//
// THIS IS THE DEFECT THE ROTATION EXISTS FOR. The widened query orders
// `priority ASC, created_at` across every tenant at once, so the tenant holding
// the oldest rows wins every slot until its backlog drains -- and a backlog of
// 10,000 drains over a great many ticks. Here tenant "a" holds 1000 rows and
// the other three hold one each; all four must be served in a single tick.
func TestTheRotatingClaimDoesNotLetABacklogTakeTheBatch(t *testing.T) {
	w, _, _ := newRotatingWorker(t, map[string]int{"a": 1000, "b": 1, "c": 1, "d": 1})

	wfs, err := w.claimRotating(8)
	if err != nil {
		t.Fatalf("claimRotating: %v", err)
	}
	got := claimedByTenant(wfs)

	// 8 across 4 tenants is a share of 2. "a" has plenty and must still stop
	// at 2; the others have one apiece and give it.
	if got["a"] != 2 {
		t.Errorf("tenant a claimed %d; want 2 (its share of a batch of 8 across 4 tenants)", got["a"])
	}
	for _, id := range []string{"b", "c", "d"} {
		if got[id] != 1 {
			t.Errorf("tenant %s claimed %d; want 1 -- a backlog in another tenant starved it", id, got[id])
		}
	}
}

// The rotation resumes where it stopped, so tenants that did not fit in one
// tick are served by the next one.
//
// Without the cursor this is not round robin: every tick would start at the
// front of the list and the tail would never be reached, which is the same
// starvation in a different shape.
func TestTheRotationResumesWhereItStopped(t *testing.T) {
	w, _, _ := newRotatingWorker(t, map[string]int{"a": 10, "b": 10, "c": 10, "d": 10})
	// Two tenants per tick, so two ticks are needed to cover four tenants.
	w.claimTenantsPerTick = 2

	first, err := w.claimRotating(4)
	if err != nil {
		t.Fatalf("first claimRotating: %v", err)
	}
	second, err := w.claimRotating(4)
	if err != nil {
		t.Fatalf("second claimRotating: %v", err)
	}

	firstTenants := claimedByTenant(first)
	secondTenants := claimedByTenant(second)
	for id := range firstTenants {
		if secondTenants[id] > 0 {
			t.Errorf("tenant %s was served on both ticks while others waited; "+
				"the cursor did not advance", id)
		}
	}
	covered := map[string]bool{}
	for id := range firstTenants {
		covered[id] = true
	}
	for id := range secondTenants {
		covered[id] = true
	}
	for _, id := range []string{"a", "b", "c", "d"} {
		if !covered[id] {
			t.Errorf("tenant %s was not served by either tick; the rotation does not cover its range", id)
		}
	}
}

// One tenant whose store will not open does not stop the tick for the others.
//
// A worker that claims nothing because a single tenant is misconfigured is a
// worse outcome than one that serves the rest and says so -- and the failure
// must not stall the cursor on the broken tenant either, or every later tick
// starts by failing on the same one.
func TestABrokenTenantDoesNotStopTheTick(t *testing.T) {
	w, factory, _ := newRotatingWorker(t, map[string]int{"a": 5, "b": 5, "c": 5})
	factory.openErr["b"] = errors.New("tenant b is unreachable")

	wfs, err := w.claimRotating(6)
	if err != nil {
		t.Fatalf("claimRotating returned an error while other tenants were servable: %v", err)
	}
	got := claimedByTenant(wfs)
	if got["a"] == 0 || got["c"] == 0 {
		t.Errorf("a broken tenant stopped the tick: claimed %v", got)
	}
	if got["b"] != 0 {
		t.Errorf("tenant b claimed %d despite its store failing to open", got["b"])
	}
}

// With every tenant failing and nothing claimed, the error is returned.
//
// The complement of the test above, and the reason it is not enough to always
// swallow the error: a total failure that returns (nil, nil) reads to the
// dispatch loop as an idle queue, so a database outage looks like quiet.
func TestARotatingClaimThatServedNobodyReportsWhy(t *testing.T) {
	w, factory, _ := newRotatingWorker(t, map[string]int{"a": 5, "b": 5})
	factory.openErr["a"] = errors.New("down")
	factory.openErr["b"] = errors.New("down")

	if _, err := w.claimRotating(4); err == nil {
		t.Fatal("claimRotating claimed nothing and reported success; the dispatch loop " +
			"cannot tell that from an empty queue")
	}
}

// A worker with no store factory, or a store that cannot enumerate tenants,
// reports that it cannot rotate rather than claiming one tenant repeatedly.
//
// storeForTenant returns the worker's OWN store when there is no factory, so a
// rotation built on it would claim the same tenant k times and call it fair.
func TestARotatingClaimRefusesWhatItCannotDo(t *testing.T) {
	t.Run("no factory", func(t *testing.T) {
		w, _, _ := newRotatingWorker(t, map[string]int{"a": 5})
		w.storeFactory = nil
		if _, err := w.claimRotating(4); !errors.Is(err, errRotatingClaimUnavailable) {
			t.Fatalf("err = %v; want errRotatingClaimUnavailable", err)
		}
	})
	t.Run("store cannot list tenants", func(t *testing.T) {
		w, _, _ := newRotatingWorker(t, map[string]int{"a": 5})
		w.store = &mockStore{} // no ListTenantIDs
		if _, err := w.claimRotating(4); !errors.Is(err, errRotatingClaimUnavailable) {
			t.Fatalf("err = %v; want errRotatingClaimUnavailable", err)
		}
	})
}

// claimGeneral prefers the rotation and falls back to the widened query only
// when it cannot rotate.
func TestClaimGeneralPrefersTheRotation(t *testing.T) {
	t.Run("rotates when it can", func(t *testing.T) {
		w, _, ls := newRotatingWorker(t, map[string]int{"a": 5, "b": 5})
		if _, err := w.claimGeneral(4); err != nil {
			t.Fatalf("claimGeneral: %v", err)
		}
		if ls.lists != 1 {
			t.Errorf("tenant list read %d times; want 1 -- claimGeneral did not take the rotating path", ls.lists)
		}
	})

	t.Run("honours claim-strategy=global", func(t *testing.T) {
		w, _, ls := newRotatingWorker(t, map[string]int{"a": 5, "b": 5})
		w.claimStrategy = claimStrategyGlobal
		// The listing store is not a CrossTenantClaimer, so claimGeneral falls
		// through to the scoped claim. What matters is that it never
		// enumerated tenants: the operator asked for the other mechanism.
		if _, err := w.claimGeneral(4); err != nil {
			t.Fatalf("claimGeneral: %v", err)
		}
		if ls.lists != 0 {
			t.Errorf("tenant list read %d times under claim-strategy=global; want 0", ls.lists)
		}
	})
}

// validateClaimStrategy accepts the two mechanisms and refuses anything else.
//
// A misspelt strategy that fell through to the default would hand an operator
// asking for `global` the very mechanism they were trying to opt out of.
func TestAnUnknownClaimStrategyIsRefused(t *testing.T) {
	for _, ok := range []string{claimStrategyRotate, claimStrategyGlobal} {
		if err := validateClaimStrategy(ok); err != nil {
			t.Errorf("validateClaimStrategy(%q) = %v; want accepted", ok, err)
		}
	}
	for _, bad := range []string{"", "globl", "Rotate", "round-robin"} {
		if err := validateClaimStrategy(bad); err == nil {
			t.Errorf("validateClaimStrategy(%q) = nil; want refused", bad)
		}
	}
}

// Cross-tenant dispatch is ON by default.
//
// The doc guard ties the documented default to the code, and would not notice
// the pair being flipped back together. This names the decision instead: a
// worker that executes only its own tenant's work, on a deployment that never
// asked for that, is a non-default tenant's workflows sitting unexecuted with
// nothing to say why.
//
// The reason it was ever off was the grant. admin.claim_workflows needs a
// BYPASSRLS owner, which only a superuser can grant and managed PostgreSQL
// cannot grant at all, so enabling it had to be deliberate. The rotating
// strategy asks nothing of the deployment, so that reason is gone.
func TestCrossTenantDispatchIsOnByDefault(t *testing.T) {
	f := flag.Lookup("claim-across-tenants")
	if f == nil {
		t.Fatal("--claim-across-tenants no longer exists")
	}
	if f.DefValue != "true" {
		t.Errorf("--claim-across-tenants defaults to %q, want \"true\". If this was "+
			"deliberate, the reason belongs here: the flag was off only because the "+
			"cross-tenant claim needed a BYPASSRLS grant, and --claim-strategy=rotate "+
			"needs none.", f.DefValue)
	}

	// And the strategy that makes it grant-free must be the default too --
	// defaulting the flag on while defaulting the strategy to `global` would
	// turn a feature that asks nothing of the deployment into one that fails
	// on every database without 023.
	s := flag.Lookup("claim-strategy")
	if s == nil {
		t.Fatal("--claim-strategy no longer exists")
	}
	if s.DefValue != claimStrategyRotate {
		t.Errorf("--claim-strategy defaults to %q, want %q", s.DefValue, claimStrategyRotate)
	}
}
