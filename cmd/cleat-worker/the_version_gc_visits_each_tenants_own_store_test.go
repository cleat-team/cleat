package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// cleat#3708. versionGCLoop collected old definition versions through the worker's
// own store only, so with --version-gc-interval on every OTHER tenant the worker
// serves kept every old version. The three statements GarbageCollectVersions
// issues carry no tenant predicate; row-level security is what scopes them, so a
// pass is only ever that store's tenant's.
//
// These are the routing half, against recording stores: which store is asked and
// what it is told to purge. The half that needs a real database and a role the
// policy applies to is a_stale_tenants_versions_are_collected_under_rls_test.go.

// gcProbe is a store holding one workflow's versions, all old and eligible, and
// recording every purge. activeFor names versions an instance is still running.
type gcProbe struct {
	*mockStore
	mu       sync.Mutex
	purged   []int
	listed   int
	failList error
}

func newGCProbe(versions int, activeFor map[int]int, failPurge map[int]bool) *gcProbe {
	p := &gcProbe{mockStore: &mockStore{}}
	old := time.Now().Add(-1000 * time.Hour)
	p.mockStore.listWorkflowDefsFn = func(context.Context, string) ([]engine.WorkflowDef, error) {
		p.mu.Lock()
		p.listed++
		fail := p.failList
		p.mu.Unlock()
		if fail != nil {
			return nil, fail
		}
		var defs []engine.WorkflowDef
		for v := 1; v <= versions; v++ {
			defs = append(defs, engine.WorkflowDef{Name: "wf", Version: v, CreatedAt: old, GCEligible: true})
		}
		return defs, nil
	}
	p.mockStore.getActiveInstanceCountsByVersionFn = func(context.Context) (map[string]int, error) {
		counts := map[string]int{}
		for v, n := range activeFor {
			counts["wf:"+itoa(v)] = n
		}
		return counts, nil
	}
	p.mockStore.purgeWorkflowDefFn = func(_ context.Context, _ string, version int) error {
		if failPurge[version] {
			return errors.New("purge refused")
		}
		p.mu.Lock()
		p.purged = append(p.purged, version)
		p.mu.Unlock()
		return nil
	}
	return p
}

func itoa(v int) string {
	const digits = "0123456789"
	if v == 0 {
		return "0"
	}
	var b []byte
	for ; v > 0; v /= 10 {
		b = append([]byte{digits[v%10]}, b...)
	}
	return string(b)
}

func (p *gcProbe) purges() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.purged...)
}

type ownGC struct {
	*gcProbe
	tenants []string
	listErr error
}

func (s *ownGC) ListTenantIDs(context.Context) ([]string, error) { return s.tenants, s.listErr }

func newGCSweepWorker(own engine.WorkflowStore, f *perTenantFactory) *Worker {
	w := newTestWorkerFromStore(own)
	w.storeTenantID = tenantSelf
	w.claimAcrossTenants = true
	w.storeFactory = f
	// Keep only the newest version; everything older is eligible by age.
	w.versionGCMinVersions = 1
	w.versionGCMaxAge = time.Hour
	return w
}

func TestVersionGCCollectsEachOtherTenantsOldVersionsThroughThatTenantsOwnStore(t *testing.T) {
	own := &ownGC{gcProbe: newGCProbe(3, nil, nil), tenants: []string{tenantSelf, tenantOther, "tenant-c"}}
	a, c := newGCProbe(3, nil, nil), newGCProbe(4, nil, nil)
	f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: a, "tenant-c": c}}
	w := newGCSweepWorker(own, f)
	defer w.cancel()

	w.runVersionGCSweep()

	// THE ASSERTION. Through the worker's own store alone (the bug) the other
	// tenants' stores are never asked and keep every old version.
	if got := own.purges(); !sameInts(got, []int{1, 2}) {
		t.Errorf("the worker's own store purged %v, want [1 2]", got)
	}
	if got := a.purges(); !sameInts(got, []int{1, 2}) {
		t.Errorf("tenant %s's store purged %v, want [1 2]: its old versions were never collected (cleat#3708)", tenantOther, got)
	}
	if got := c.purges(); !sameInts(got, []int{1, 2, 3}) {
		t.Errorf("tenant-c's store purged %v, want [1 2 3]", got)
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

func sameInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[int]int{}
	for _, x := range a {
		seen[x]++
	}
	for _, x := range b {
		seen[x]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

// The flag off is the whole of what keeps this destructive loop safe by default,
// and it must stay off for other tenants too.
func TestVersionGCOffTouchesNoTenantsStore(t *testing.T) {
	own := &ownGC{gcProbe: newGCProbe(3, nil, nil), tenants: []string{tenantSelf, tenantOther}}
	other := newGCProbe(3, nil, nil)
	f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: other}}
	w := newGCSweepWorker(own, f)
	defer w.cancel()
	w.versionGCInterval = 0 // the flag default

	done := make(chan struct{})
	w.wg.Add(1)
	go func() { defer close(done); w.versionGCLoop() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("versionGCLoop did not return with --version-gc-interval unset")
	}
	f.mu.Lock()
	opened := append([]string(nil), f.opened...)
	f.mu.Unlock()
	if len(opened) != 0 || len(other.purges()) != 0 || len(own.purges()) != 0 || other.listed != 0 || own.listed != 0 {
		t.Errorf("with the flag off the loop touched stores: opened %v, other purged %v, own purged %v",
			opened, other.purges(), own.purges())
	}
}

// With the flag on, the LOOP reaches the other tenants, not only the seam the
// tests above call directly.
func TestTheVersionGCLoopReachesOtherTenantsWhenOn(t *testing.T) {
	own := &ownGC{gcProbe: newGCProbe(3, nil, nil), tenants: []string{tenantSelf, tenantOther}}
	other := newGCProbe(3, nil, nil)
	f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: other}}
	w := newGCSweepWorker(own, f)
	defer w.cancel()
	w.versionGCInterval = 10 * time.Millisecond
	w.initLoopCtxUnmonitored("version_gc")

	w.wg.Add(1)
	go w.versionGCLoop()
	deadline := time.After(5 * time.Second)
	for len(other.purges()) == 0 {
		select {
		case <-deadline:
			t.Fatal("the loop never collected the other tenant's old versions")
		case <-time.After(10 * time.Millisecond):
		}
	}
	w.cancel()
	w.wg.Wait()
}

func TestVersionGCSkipsATenantWhoseActiveCountFailsAndStillSweepsTheNext(t *testing.T) {
	own := &ownGC{gcProbe: newGCProbe(2, nil, nil), tenants: []string{tenantSelf, tenantOther, "tenant-c"}}
	bad := newGCProbe(3, nil, nil)
	bad.mockStore.getActiveInstanceCountsByVersionFn = func(context.Context) (map[string]int, error) {
		return nil, errors.New("count unavailable for this tenant")
	}
	good := newGCProbe(3, nil, nil)
	f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: bad, "tenant-c": good}}
	w := newGCSweepWorker(own, f)
	defer w.cancel()
	w.lastDBTrouble.Store(time.Now().Add(-time.Hour).UnixNano())
	troubleBefore := w.lastDBTrouble.Load()

	w.runVersionGCSweep()

	// GarbageCollectVersions refuses to delete on a count it could not take: a
	// version in use must never be purged for want of knowing it is in use.
	if got := bad.purges(); len(got) != 0 {
		t.Errorf("a tenant whose active-instance count failed had %v purged anyway", got)
	}
	if got := good.purges(); !sameInts(got, []int{1, 2}) {
		t.Errorf("one tenant's failure stopped the next: tenant-c purged %v, want [1 2]", got)
	}
	if got := w.lastDBTrouble.Load(); got != troubleBefore {
		t.Error("one tenant's failure was recorded as the worker's own database trouble")
	}
}

func TestVersionGCKeepsAVersionAnotherTenantStillRuns(t *testing.T) {
	own := &ownGC{gcProbe: newGCProbe(2, nil, nil), tenants: []string{tenantSelf, tenantOther}}
	other := newGCProbe(3, map[int]int{2: 1}, nil) // version 2 has a running instance
	f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: other}}
	w := newGCSweepWorker(own, f)
	defer w.cancel()

	w.runVersionGCSweep()

	if got := other.purges(); !sameInts(got, []int{1}) {
		t.Errorf("the other tenant purged %v, want [1]: version 2 has an active instance and must be kept", got)
	}
}

func TestVersionGCPurgeFailureInOneTenantDoesNotStopTheNext(t *testing.T) {
	own := &ownGC{gcProbe: newGCProbe(2, nil, nil), tenants: []string{tenantSelf, tenantOther, "tenant-c"}}
	bad := newGCProbe(3, nil, map[int]bool{1: true})
	good := newGCProbe(3, nil, nil)
	f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: bad, "tenant-c": good}}
	w := newGCSweepWorker(own, f)
	defer w.cancel()

	w.runVersionGCSweep()

	if got := good.purges(); !sameInts(got, []int{1, 2}) {
		t.Errorf("tenant-c purged %v, want [1 2]", got)
	}
	if got := bad.purges(); !sameInts(got, []int{2}) {
		t.Errorf("the failing tenant purged %v, want [2] (version 1 was refused, 2 was not)", got)
	}
}

// Without --claim-across-tenants a worker serves its own tenant only, and a store
// that cannot list tenants (MySQL, a sharded store) cannot serve others at all.
func TestVersionGCDoesNotReachOtherTenantsWhereTheWorkerServesOnlyItsOwn(t *testing.T) {
	t.Run("--claim-across-tenants off", func(t *testing.T) {
		own := &ownGC{gcProbe: newGCProbe(3, nil, nil), tenants: []string{tenantSelf, tenantOther}}
		other := newGCProbe(3, nil, nil)
		f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: other}}
		w := newGCSweepWorker(own, f)
		defer w.cancel()
		w.claimAcrossTenants = false
		w.runVersionGCSweep()
		if len(other.purges()) != 0 || len(f.opened) != 0 {
			t.Errorf("another tenant was visited with --claim-across-tenants off: opened %v purged %v", f.opened, other.purges())
		}
		if got := own.purges(); !sameInts(got, []int{1, 2}) {
			t.Errorf("the own tenant purged %v, want [1 2]", got)
		}
	})
	t.Run("a store that cannot list tenants", func(t *testing.T) {
		own := newGCProbe(3, nil, nil) // not a TenantLister
		other := newGCProbe(3, nil, nil)
		f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: other}}
		w := newGCSweepWorker(own, f)
		defer w.cancel()
		w.runVersionGCSweep()
		if len(other.purges()) != 0 || len(f.opened) != 0 {
			t.Errorf("a store that cannot list tenants still visited some: opened %v purged %v", f.opened, other.purges())
		}
		if got := own.purges(); !sameInts(got, []int{1, 2}) {
			t.Errorf("the own tenant purged %v, want [1 2]: nothing about other tenants may stop it", got)
		}
	})
}
