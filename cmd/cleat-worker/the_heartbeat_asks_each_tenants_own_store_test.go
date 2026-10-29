package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// cleat#3566. heartbeatAndFenceInFlight sent every in-flight run, of every
// tenant, through the worker's own store. HeartbeatBatchFenced reads
// workflow_leases under row-level security, and the tenant it sees belongs to
// the store instance, so a run of any other tenant was reported lost at its
// first beat.
//
// These are the routing half, against recording stores: which store is asked
// about which run. The half that needs a real database and a role that does not
// bypass the policy is a_tenants_run_outlives_its_heartbeats_under_rls_test.go;
// a recording store cannot show that a store "sees" a row, only that it was
// asked, which is exactly why a suite built from them never noticed.

// heartbeatProbe is a store that records every HeartbeatBatchFenced call made
// through it and answers with a fixed verdict.
type heartbeatProbe struct {
	*mockStore
	mu    sync.Mutex
	calls [][]string // the run ids of each call, in order
	lost  []string
	err   error
}

func newHeartbeatProbe(lost []string, err error) *heartbeatProbe {
	p := &heartbeatProbe{mockStore: &mockStore{}, lost: lost, err: err}
	p.mockStore.heartbeatBatchFencedFn = func(_ context.Context, _ string, runs []engine.GenerationKey) ([]string, error) {
		ids := make([]string, 0, len(runs))
		for _, r := range runs {
			ids = append(ids, r.WorkflowID)
		}
		p.mu.Lock()
		p.calls = append(p.calls, ids)
		p.mu.Unlock()
		return p.lost, p.err
	}
	return p
}

func (p *heartbeatProbe) asked() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var all []string
	for _, c := range p.calls {
		all = append(all, c...)
	}
	return all
}

// registerTenantExecution is registerInflightExecution for a run that belongs to
// a tenant.
func registerTenantExecution(w *Worker, id, tenant string, generation int64) context.Context {
	ctx := registerInflightExecution(w, id, "test-def", generation)
	wf, _ := w.inflight.Load(id)
	wf.(*engine.WorkflowInstance).TenantID = tenant
	return ctx
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
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

func TestHeartbeatAsksEachTenantsRunsThroughThatTenantsOwnStore(t *testing.T) {
	own := newHeartbeatProbe(nil, nil)
	other := newHeartbeatProbe([]string{"other-lost"}, nil)
	w := newTestWorker(own.mockStore)
	defer w.cancel()
	w.storeTenantID = tenantSelf
	factory := &recordingFactory{store: other.mockStore}
	w.storeFactory = factory

	registerTenantExecution(w, "own-run", tenantSelf, 1)
	registerTenantExecution(w, "other-live", tenantOther, 1)
	registerTenantExecution(w, "other-lost", tenantOther, 2)

	if landed := w.heartbeatAndFenceInFlight(); !landed {
		t.Fatal("every group was answered, so the call must report that it landed")
	}

	// THE ASSERTION. The worker's own store is asked about the worker's own
	// tenant's runs and nothing else; the other tenant's store about its own runs.
	// Routed everything through heartbeatBatchStore() (the bug) the own store is
	// asked about all three and the tenant's store about none.
	if got := own.asked(); !sameSet(got, []string{"own-run"}) {
		t.Errorf("the worker's own store was asked about %v; want only its own tenant's [own-run]. "+
			"A tenant's lease rows are invisible through a store scoped to someone else, so asking it "+
			"reports the run lost whether or not it is (cleat#3566)", got)
	}
	if got := other.asked(); !sameSet(got, []string{"other-live", "other-lost"}) {
		t.Errorf("the other tenant's store was asked about %v; want [other-live other-lost]", got)
	}
	if len(other.calls) != 1 {
		t.Errorf("the other tenant's runs took %d heartbeat calls, want one batch", len(other.calls))
	}

	// The verdict that store gave is applied, to the run it named and to no other.
	if !w.runIsFenced("other-lost", 2) {
		t.Error("the other tenant's store reported other-lost lost, and it was not fenced")
	}
	if w.runIsFenced("other-live", 1) || w.runIsFenced("own-run", 1) {
		t.Error("a run that was not reported lost was fenced")
	}

	// The lease on the tenant's store is released, as every storeForTenant caller must.
	factory.mu.Lock()
	opened, closed := append([]string(nil), factory.opened...), factory.closed
	factory.mu.Unlock()
	if len(opened) != 1 || opened[0] != tenantOther {
		t.Errorf("the factory opened %v, want exactly [%s]", opened, tenantOther)
	}
	if closed != 1 {
		t.Errorf("the tenant store's lease was released %d times, want 1", closed)
	}
}

// The worker's own tenant keeps the isolated heartbeat pool (cleat#2009).
func TestHeartbeatKeepsTheWorkersOwnRunsOnTheIsolatedHeartbeatPool(t *testing.T) {
	main := newHeartbeatProbe(nil, nil)
	isolated := newHeartbeatProbe(nil, nil)
	w := newTestWorker(main.mockStore)
	defer w.cancel()
	w.storeTenantID = tenantSelf
	w.heartbeatStore = isolated.mockStore
	w.storeFactory = &recordingFactory{store: newHeartbeatProbe(nil, nil).mockStore}

	registerTenantExecution(w, "own-run", tenantSelf, 1)
	w.heartbeatAndFenceInFlight()

	if got := isolated.asked(); !sameSet(got, []string{"own-run"}) {
		t.Errorf("the isolated heartbeat store was asked about %v, want [own-run]", got)
	}
	if got := main.asked(); len(got) != 0 {
		t.Errorf("the main pool was asked about %v; the worker's own heartbeat belongs on the isolated one", got)
	}
}

// A group that cannot be asked says nothing about its runs, and does not stop
// the other groups' verdicts being applied.
func TestHeartbeatOneTenantsFailureDoesNotHideAnotherTenantsFence(t *testing.T) {
	own := newHeartbeatProbe([]string{"own-lost"}, nil)
	other := newHeartbeatProbe(nil, errors.New("connection reset by peer"))
	w := newTestWorker(own.mockStore)
	defer w.cancel()
	w.storeTenantID = tenantSelf
	w.storeFactory = &recordingFactory{store: other.mockStore}

	registerTenantExecution(w, "own-lost", tenantSelf, 1)
	registerTenantExecution(w, "other-run", tenantOther, 1)

	if landed := w.heartbeatAndFenceInFlight(); landed {
		t.Error("one group failed, so the call did not land and the loop must retry sooner")
	}
	if !w.runIsFenced("own-lost", 1) {
		t.Error("the worker's own store answered that own-lost is lost, and another tenant's failure stopped it being fenced")
	}
	if w.runIsFenced("other-run", 1) {
		t.Error("a tenant whose heartbeat could not be asked had its run fenced: a failed call says nothing about which runs are lost")
	}
}

func TestHeartbeatWhenATenantStoreCannotBeOpenedFencesNothingOfThatTenant(t *testing.T) {
	own := newHeartbeatProbe(nil, nil)
	w := newTestWorker(own.mockStore)
	defer w.cancel()
	w.storeTenantID = tenantSelf
	w.storeFactory = &recordingFactory{openErr: errors.New("no pool for this tenant")}

	registerTenantExecution(w, "own-run", tenantSelf, 1)
	registerTenantExecution(w, "other-run", tenantOther, 1)

	if landed := w.heartbeatAndFenceInFlight(); landed {
		t.Error("a tenant's store could not be opened, so the call did not land")
	}
	if w.runIsFenced("other-run", 1) {
		t.Error("a run was fenced because its store could not be opened; nothing asked whether it is lost")
	}
	if got := own.asked(); !sameSet(got, []string{"own-run"}) {
		t.Errorf("the own group was not still served: asked %v", got)
	}
}

// With no factory there is nothing to route with. This is storeForTenant's own
// first condition, and the heartbeat must agree with it: the group is only right
// if the store it picks is the store execution picks for the same run.
func TestHeartbeatWithNoFactoryServesEveryRunFromTheWorkersOwnStore(t *testing.T) {
	own := newHeartbeatProbe(nil, nil)
	w := newTestWorker(own.mockStore)
	defer w.cancel()
	w.storeTenantID = tenantSelf

	registerTenantExecution(w, "a", tenantSelf, 1)
	registerTenantExecution(w, "b", tenantOther, 1)
	w.heartbeatAndFenceInFlight()

	if got := own.asked(); !sameSet(got, []string{"a", "b"}) {
		t.Errorf("with no store factory the worker's own store was asked about %v, want [a b]", got)
	}
	for _, tenant := range []string{"", tenantSelf, tenantOther} {
		_, release, err := w.storeForTenant(tenant)
		if err != nil {
			t.Fatalf("storeForTenant(%q): %v", tenant, err)
		}
		release()
		if g := w.heartbeatGroupFor(tenant); g != "" {
			t.Errorf("heartbeatGroupFor(%q) = %q with no factory; storeForTenant returns the worker's own store for it", tenant, g)
		}
	}
}

// What a beat is evidence of (cleat#3566 review). The worker-wide readings --
// lastHeartbeatOK, which heartbeatPresumedLost() turns into "refuse every new
// durable call", and lastDBTrouble, which holds this worker's reaper back -- are
// about whether THIS worker's own store can be reached. One tenant's store not
// opening says nothing about that, and must not refuse new durable calls for runs
// whose own beat landed.

func ageOutTheWorkerWideReadings(w *Worker) {
	old := time.Now().Add(-time.Hour).UnixNano()
	w.lastHeartbeatOK.Store(old)
	w.lastDBContactOK.Store(old)
	w.lastDBTrouble.Store(old)
}

func TestHeartbeatOneTenantsSilenceDoesNotRefuseAnotherTenantsNewWork(t *testing.T) {
	own := newHeartbeatProbe(nil, nil)
	other := newHeartbeatProbe(nil, errors.New("pool for this tenant is down"))
	w := newTestWorker(own.mockStore)
	defer w.cancel()
	w.storeTenantID = tenantSelf
	w.reclaimTimeout = time.Second
	w.storeFactory = &recordingFactory{store: other.mockStore}
	registerTenantExecution(w, "own-run", tenantSelf, 1)
	registerTenantExecution(w, "other-run", tenantOther, 1)

	ageOutTheWorkerWideReadings(w)
	troubleBefore := w.lastDBTrouble.Load()
	w.heartbeatAndFenceInFlight()

	if w.heartbeatPresumedLostFor(tenantSelf) {
		t.Error("the worker's own beat landed, and another tenant's silence made its new durable calls refused: " +
			"a worker-wide reading was held back by one group")
	}
	if got := w.lastDBTrouble.Load(); got != troubleBefore {
		t.Error("another tenant's store failing was recorded as this worker's own DB trouble, which holds its reaper back")
	}
	if w.heartbeatPresumedLostFor(tenantOther) {
		t.Error("a tenant first seen this tick is presumed lost at once; its clock should start now")
	}

	// The silent tenant's OWN clock does run out, and only its own.
	w.groupHeartbeatOK.Store(tenantOther, time.Now().Add(-time.Hour).UnixNano())
	if !w.heartbeatPresumedLostFor(tenantOther) {
		t.Error("a tenant whose store has not answered for longer than reclaimAfter is not presumed lost")
	}
	if w.heartbeatPresumedLostFor(tenantSelf) {
		t.Error("the silent tenant's clock running out refused the worker's own runs")
	}
}

func TestHeartbeatWithNoOwnRunsStillProvesTheOwnStoreAndAFailingTenantDoesNotCountAgainstIt(t *testing.T) {
	pinging := &pingingMockStore{mockStore: &mockStore{}}
	w := newTestWorkerFromStore(pinging)
	defer w.cancel()
	w.storeTenantID = tenantSelf
	w.reclaimTimeout = time.Second
	w.storeFactory = &recordingFactory{openErr: errors.New("no pool for this tenant")}
	registerTenantExecution(w, "other-run", tenantOther, 1)

	ageOutTheWorkerWideReadings(w)
	troubleBefore := w.lastDBTrouble.Load()
	if landed := w.heartbeatAndFenceInFlight(); landed {
		t.Error("the tenant's store could not be opened, so the call did not land")
	}
	if w.heartbeatPresumedLostFor(tenantSelf) {
		t.Error("no run of the worker's own was in flight, its own store answered a ping, and it was still presumed lost")
	}
	if w.lastDBContactOK.Load() <= troubleBefore {
		t.Error("the own store's ping answered and was not recorded as confirmed contact")
	}
	if got := w.lastDBTrouble.Load(); got != troubleBefore {
		t.Error("a tenant's unopenable store was recorded as this worker's DB trouble")
	}
}

func TestHeartbeatWhenTheOwnStoreFailsThatIsTheWorkersTroubleAndOnlyThatTenantsRunsAreAtRisk(t *testing.T) {
	own := newHeartbeatProbe(nil, errors.New("connection reset by peer"))
	other := newHeartbeatProbe(nil, nil)
	w := newTestWorker(own.mockStore)
	defer w.cancel()
	w.storeTenantID = tenantSelf
	w.reclaimTimeout = time.Second
	w.storeFactory = &recordingFactory{store: other.mockStore}
	registerTenantExecution(w, "own-run", tenantSelf, 1)
	registerTenantExecution(w, "other-run", tenantOther, 1)

	ageOutTheWorkerWideReadings(w)
	troubleBefore := w.lastDBTrouble.Load()
	w.heartbeatAndFenceInFlight()

	if got := w.lastDBTrouble.Load(); got == troubleBefore {
		t.Error("the worker's own store failed and no DB trouble was recorded")
	}
	if !w.heartbeatPresumedLostFor(tenantSelf) {
		t.Error("the worker's own beat has not landed for longer than reclaimAfter and its runs are not presumed lost")
	}
	if w.heartbeatPresumedLostFor(tenantOther) {
		t.Error("the other tenant's beat landed, and the worker's own failure refused its new durable calls")
	}
}
