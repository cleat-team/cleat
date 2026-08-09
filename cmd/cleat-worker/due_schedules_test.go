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

type crossTenantScheduleStore struct {
	*mockStore
	mu     sync.Mutex
	cross  int
	scoped int
	// crossErr, when set, is what the cross-tenant read returns.
	crossErr error
}

func (m *crossTenantScheduleStore) GetDueSchedules(ctx context.Context) ([]engine.Schedule, error) {
	m.mu.Lock()
	m.scoped++
	m.mu.Unlock()
	return nil, nil
}

func (m *crossTenantScheduleStore) GetDueSchedulesAcrossTenants(ctx context.Context) ([]engine.Schedule, error) {
	m.mu.Lock()
	m.cross++
	m.mu.Unlock()
	return nil, m.crossErr
}

func (m *crossTenantScheduleStore) counts() (cross, scoped int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cross, m.scoped
}

// TestDueSchedules_UsesTheCrossTenantReadOnlyWhenAsked.
//
// The flag is the whole safety story for this feature: a deployment that has
// not granted the exemption must keep the pre-existing behaviour exactly.
func TestDueSchedules_UsesTheCrossTenantReadOnlyWhenAsked(t *testing.T) {
	for _, tc := range []struct {
		name             string
		flag             bool
		wantCross        int
		wantScopedAtMost int
	}{
		{"flag off: the scoped read, as before", false, 0, 1},
		{"flag on: the cross-tenant read", true, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &crossTenantScheduleStore{mockStore: &mockStore{}}
			w := newTestWorker(st.mockStore)
			defer w.cancel()
			w.store = st
			w.claimAcrossTenants = tc.flag

			if _, err := w.dueSchedules(); err != nil {
				t.Fatalf("dueSchedules: %v", err)
			}
			cross, scoped := st.counts()
			if cross != tc.wantCross {
				t.Errorf("cross-tenant reads = %d, want %d", cross, tc.wantCross)
			}
			if scoped > tc.wantScopedAtMost {
				t.Errorf("scoped reads = %d, want at most %d", scoped, tc.wantScopedAtMost)
			}
		})
	}
}

// TestDueSchedules_FallsBackWhenTheStoreCannotReadAcrossTenants.
//
// A missing GRANT must narrow the worker, not stop it firing anything at all.
// The assertion that matters is the scoped read happening AFTER the refusal:
// without it the loop returns an error every tick and no schedule fires,
// including the worker's own tenant's, which is strictly worse than the
// behaviour before this feature existed.
func TestDueSchedules_FallsBackWhenTheStoreCannotReadAcrossTenants(t *testing.T) {
	st := &crossTenantScheduleStore{
		mockStore: &mockStore{},
		crossErr: fmt.Errorf("admin.get_due_schedules does not exist: %w",
			engine.ErrCrossTenantClaimUnsupported),
	}
	w := newTestWorker(st.mockStore)
	defer w.cancel()
	w.store = st
	w.claimAcrossTenants = true

	if _, err := w.dueSchedules(); err != nil {
		t.Fatalf("a refusal must be answered by falling back, not propagated: %v", err)
	}
	cross, scoped := st.counts()
	if cross != 1 {
		t.Errorf("cross-tenant reads = %d, want 1", cross)
	}
	if scoped != 1 {
		t.Errorf("scoped reads = %d, want 1 -- the fallback did not happen, so no schedule "+
			"fires at all on a deployment that merely has not applied migration 024", scoped)
	}
}

// TestDueSchedules_PropagatesARealFailure is the false-positive half. A
// fallback that swallowed every error would satisfy the test above and would
// hide a database outage as "no schedules are due".
func TestDueSchedules_PropagatesARealFailure(t *testing.T) {
	st := &crossTenantScheduleStore{
		mockStore: &mockStore{},
		crossErr:  fmt.Errorf("connection refused"),
	}
	w := newTestWorker(st.mockStore)
	defer w.cancel()
	w.store = st
	w.claimAcrossTenants = true

	if _, err := w.dueSchedules(); err == nil {
		t.Fatal("a real read failure was swallowed; the loop would report an idle scheduler " +
			"while the database was unreachable")
	}
	if _, scoped := st.counts(); scoped != 0 {
		t.Errorf("scoped reads = %d, want 0 -- a real failure must not be retried as a fallback", scoped)
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
// The read is deliberately unscoped -- it returns every tenant's due schedules
// through one connection. Everything after it must be scoped again immediately,
// or the loop starts one tenant's run through another tenant's store, with that
// store's isolation applied and the wrong tenant's quota consumed. Nothing else
// in the tree checks that.
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

	// The cross-tenant read returns a schedule belonging to the OTHER tenant.
	xt := &scheduleReadStore{
		mockStore: own,
		due: []engine.Schedule{{
			Name:           "xts-loop",
			DefName:        "sched-wf",
			CronExpression: "* * * * *",
			Input:          json.RawMessage(`{}`),
			Enabled:        true,
			NextRunAt:      time.Now().Add(-time.Minute),
			Timezone:       "UTC",
			TenantID:       otherTenant,
			MisfirePolicy:  "catch_up",
			OverlapPolicy:  "allow",
		}},
	}
	w.store = xt

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

// scheduleReadStore returns a fixed due set from the cross-tenant read.
type scheduleReadStore struct {
	*mockStore
	due  []engine.Schedule
	once sync.Once
}

func (m *scheduleReadStore) GetDueSchedulesAcrossTenants(ctx context.Context) ([]engine.Schedule, error) {
	// Once: the loop would otherwise re-fire the same schedule every tick,
	// since nothing here advances next_run_at.
	var out []engine.Schedule
	m.once.Do(func() { out = m.due })
	return out, nil
}

// fixedTenantFactory hands out one store for one tenant and fails for any
// other, so a lookup for the wrong tenant is a visible error rather than a
// silent fallback to the worker's own store.
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
