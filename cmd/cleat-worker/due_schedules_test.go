package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// ---------------------------------------------------------------------------
// Which read the loop uses
// ---------------------------------------------------------------------------

// A real read failure is propagated, not swallowed.
//
// SUPERSEDED IN PART. Its sibling asserted that the flag chose between a
// cross-tenant read and a scoped one; that choice is gone with the widened
// query, and what replaced it -- does the flag decide whether other tenants
// are served at all -- is TestBothLoopsUseTheRotationOnlyWhenAsked.
//
// This half survives unchanged in meaning: a read that fails must reach the
// schedule loop as an error. Returning (nil, nil) would read as "nothing is
// due", which is indistinguishable from a quiet period, so a database outage
// would look like a deployment with no cron.
func TestDueSchedules_PropagatesARealFailure(t *testing.T) {
	w, factory, _ := newRotatingWorker(t, map[string]int{"a": 0, "b": 0})
	factory.openErr["a"] = fmt.Errorf("connection refused")
	factory.openErr["b"] = fmt.Errorf("connection refused")

	if _, err := w.dueSchedules(); err == nil {
		t.Fatal("a real read failure was swallowed; the loop would report an idle scheduler " +
			"while the database was unreachable")
	}
}

// ---------------------------------------------------------------------------
// Which store the firing runs through
// ---------------------------------------------------------------------------

// scheduleFireProbe records the store a firing actually went through.
type scheduleFireProbe struct {
	mu             sync.Mutex
	startedTenants []string
	claimed        []string
}

func (p *scheduleFireProbe) record(tenant, schedule string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.startedTenants = append(p.startedTenants, tenant)
	p.claimed = append(p.claimed, schedule)
}

func (p *scheduleFireProbe) snapshot() ([]string, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.startedTenants...), append([]string(nil), p.claimed...)
}

// fireProbeStore is a mockStore that answers everything scheduleLoop needs and
// reports which store instance was used.
func fireProbeStore(label string, probe *scheduleFireProbe, onStart func(string)) *mockStore {
	ms := &mockStore{}
	ms.listVersionsFn = func(_ context.Context, _ string) ([]int, error) { return []int{1}, nil }
	ms.startNewRunFn = func(_ context.Context, _, _ string, _ int, _ json.RawMessage,
		_, tenantID string, _ int) (string, bool, error) {
		onStart(label)
		probe.record(tenantID, label)
		return "run-1", false, nil
	}
	return ms
}

// TestScheduleLoop_FiresThroughTheSchedulesOwnTenantStore is the assertion the
// whole cross-tenant schedule read depends on.
//
// The read spans tenants -- one pass, every tenant's due schedules. Everything
// after it must be scoped again immediately, or the loop starts one tenant's
// run through another tenant's store, with that store's isolation applied and
// the wrong tenant's quota consumed. Nothing else in the tree checks that.
//
// It read through admin.get_due_schedules when this was written, and now reads
// per tenant through the rotation. The property is unchanged by that, which is
// the point: what must hold is where the FIRING goes, not where the reading
// came from.
func TestScheduleLoop_FiresThroughTheSchedulesOwnTenantStore(t *testing.T) {
	const otherTenant = "22222222-2222-2222-2222-222222222222"
	const ownTenant = "00000000-0000-0000-0000-000000000000"

	probe := &scheduleFireProbe{}
	fired := make(chan string, 4)

	// The worker's OWN store. It must never be the one that starts this run.
	own := fireProbeStore("own-store", probe, func(l string) {
		select {
		case fired <- l:
		default:
		}
	})
	own.getDueSchedulesFn = func(_ context.Context) ([]engine.Schedule, error) { return nil, nil }

	w := newTestWorker(own)
	defer w.cancel()
	w.storeTenantID = ownTenant
	w.claimAcrossTenants = true
	w.scheduleInterval = 5 * time.Millisecond

	// The other tenant's store, handed out by the factory. This is the one the
	// firing must go through.
	tenantStore := fireProbeStore("tenant-store", probe, func(l string) {
		select {
		case fired <- l:
		default:
		}
	})
	w.storeFactory = &fixedTenantFactory{tenantID: otherTenant, store: tenantStore}

	// The other tenant's store is where its due schedule comes from now: the
	// rotation enumerates tenants and reads each one's schedules through its
	// own store. Attaching the due set HERE rather than to the worker's own
	// store is what makes this test exercise the path that ships.
	tenantStore.getDueSchedulesFn = func(context.Context) ([]engine.Schedule, error) {
		return []engine.Schedule{{
			Name:           "xts-loop",
			DefName:        "sched-wf",
			CronExpression: "* * * * *",
			Input:          json.RawMessage(`{}`),
			NextRunAt:      time.Now().Add(-time.Minute),
			Timezone:       "UTC",
			TenantID:       otherTenant,
			MisfirePolicy:  "catch_up",
			OverlapPolicy:  "allow",
		}}, nil
	}
	w.store = &listingStore{mockStore: own, tenants: []string{otherTenant}}
	w.claimAcrossTenants = true
	w.storeTenantID = ""

	// No registerLoopFunc: newTestWorker leaves loopFuncs nil, and scheduleLoop
	// does not need it. Matches TestScheduleLoop_StopsOnCancel.
	w.wg.Add(1)
	go w.scheduleLoop()

	select {
	case label := <-fired:
		if label != "tenant-store" {
			t.Errorf("the run was started through %q; a schedule owned by %s must be fired "+
				"through that tenant's own store, not the worker's", label, otherTenant)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no schedule fired within 3s")
	}
	w.cancel()
	w.wg.Wait()

	tenants, labels := probe.snapshot()
	for i, l := range labels {
		if l == "own-store" {
			t.Errorf("the worker's own store started a run for tenant %s", tenants[i])
		}
	}
	for _, tid := range tenants {
		if tid != otherTenant {
			t.Errorf("StartNewRun was passed tenant %q, want %q -- the run would be recorded "+
				"under the wrong tenant", tid, otherTenant)
		}
	}
	if len(tenants) == 0 {
		t.Error("no run was started at all")
	}
}

// fixedTenantFactory hands out one store, for one tenant.
type fixedTenantFactory struct {
	tenantID string
	store    engine.WorkflowStore
}

func (f *fixedTenantFactory) OpenStore(_ context.Context, tenantID string, _ ...string) (engine.WorkflowStore, io.Closer, error) {
	if tenantID != f.tenantID {
		return nil, nil, fmt.Errorf("fixedTenantFactory: no store for tenant %s", tenantID)
	}
	return f.store, nopCloserT{}, nil
}

func (f *fixedTenantFactory) Close() error            { return nil }
func (f *fixedTenantFactory) DriverName() string      { return "test" }
func (f *fixedTenantFactory) Dialect() engine.Dialect { return engine.DialectPostgres }

type nopCloserT struct{}

func (nopCloserT) Close() error { return nil }
