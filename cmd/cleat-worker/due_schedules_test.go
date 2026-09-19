package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
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

// ---------------------------------------------------------------------------
// The startup report
// ---------------------------------------------------------------------------

type capabilityStore struct {
	*mockStore
	capability engine.CrossTenantCapability
	mu         sync.Mutex
	checks     int
}

func (m *capabilityStore) CheckCrossTenantCapability(context.Context) engine.CrossTenantCapability {
	m.mu.Lock()
	m.checks++
	m.mu.Unlock()
	return m.capability
}

// TestReportCrossTenantCapability_SaysWhichModeTheWorkerIsIn.
//
// Both cross-tenant paths degrade rather than fail, which is the right default
// and is what makes this report necessary: an operator who set the flag and saw
// nothing cannot tell "working" from "the warning already scrolled past". So the
// outcome is stated in both directions, at startup, before either loop ticks.
//
// The unavailable case must name the reason, because the runtime error for the
// worst version of it -- a lost BYPASSRLS -- is "cleat.tenant_id is not set",
// which names neither the function nor the attribute.
func TestReportCrossTenantCapability_SaysWhichModeTheWorkerIsIn(t *testing.T) {
	for _, tc := range []struct {
		name       string
		flag       bool
		capability engine.CrossTenantCapability
		wantChecks int
		wantLevel  string
		wantIn     []string
	}{
		{
			name:       "flag off: nothing is probed and nothing is said",
			flag:       false,
			wantChecks: 0,
		},
		{
			name:       "granted: reported available, so silence is not the only evidence",
			flag:       true,
			capability: engine.CrossTenantCapability{Claim: true, Schedules: true},
			wantChecks: 1,
			wantLevel:  "INFO",
			wantIn:     []string{"cross-tenant workflow claim is available", "cross-tenant due-schedule read is available"},
		},
		{
			name: "ungranted: reported unavailable, with the reason and the consequence",
			flag: true,
			capability: engine.CrossTenantCapability{
				ClaimReason:     "admin.claim_workflows does not exist; apply 023",
				SchedulesReason: "owner does not have BYPASSRLS",
			},
			wantChecks: 1,
			wantLevel:  "WARN",
			wantIn: []string{
				"only this worker's own tenant's workflows will execute",
				"only this worker's own tenant's cron will fire",
				"BYPASSRLS",
			},
		},
		{
			name: "partially granted: the claim works and cron does not, said separately",
			flag: true,
			capability: engine.CrossTenantCapability{
				Claim:           true,
				SchedulesReason: "admin.get_due_schedules does not exist; apply 024",
			},
			wantChecks: 1,
			wantIn: []string{
				"cross-tenant workflow claim is available",
				"cross-tenant due-schedule read is NOT available",
				"024",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			st := &capabilityStore{mockStore: &mockStore{}, capability: tc.capability}
			w := newTestWorker(st.mockStore)
			defer w.cancel()
			w.store = st
			w.claimAcrossTenants = tc.flag
			// This table is about the report for the WIDENED QUERY, whose
			// availability is a property of the 023/024 grants. Since the
			// rotating claim became the default it has its own report, backed
			// by a different question -- "can this worker enumerate tenants
			// and open a store per tenant" rather than "was a grant made" --
			// and it is covered by TestReportCrossTenantCapability_RotatingPath
			// below. Naming the mechanism here keeps each report asserted
			// against the thing it actually reports on.
			w.claimStrategy = claimStrategyGlobal
			w.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

			w.reportCrossTenantCapability()

			st.mu.Lock()
			checks := st.checks
			st.mu.Unlock()
			if checks != tc.wantChecks {
				t.Errorf("capability checked %d time(s), want %d", checks, tc.wantChecks)
			}
			out := buf.String()
			if tc.wantChecks == 0 && out != "" {
				t.Errorf("flag off but the worker logged: %s", out)
			}
			for _, want := range tc.wantIn {
				if !strings.Contains(out, want) {
					t.Errorf("startup report does not mention %q\n%s", want, out)
				}
			}
			if tc.wantLevel != "" && !strings.Contains(out, "level="+tc.wantLevel) {
				t.Errorf("expected a %s line, got:\n%s", tc.wantLevel, out)
			}
		})
	}
}

// TestReportCrossTenantCapability_SaysSoWhenItCannotTell is the inconclusive
// case. A store that cannot answer must not be reported as either working or
// broken -- asserting a capability nobody established is the failure this whole
// report exists to prevent, arriving through the report itself.
func TestReportCrossTenantCapability_SaysSoWhenItCannotTell(t *testing.T) {
	var buf bytes.Buffer
	// A plain mockStore implements neither CrossTenantCapabilityChecker nor the
	// cross-tenant paths.
	ms := &mockStore{}
	w := newTestWorker(ms)
	defer w.cancel()
	w.claimAcrossTenants = true
	w.claimStrategy = claimStrategyGlobal // see the table above: this is the widened query's report
	w.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	w.reportCrossTenantCapability()

	out := buf.String()
	if !strings.Contains(out, "cannot report whether it supports it") {
		t.Errorf("a store that cannot answer was not reported as such:\n%s", out)
	}
	if strings.Contains(out, "is available") {
		t.Errorf("an unanswerable check was reported as available:\n%s", out)
	}
}

// The rotating claim reports its own availability at startup, and does NOT
// report a missing 023 grant it will never use.
//
// The second half is the point. On managed PostgreSQL the BYPASSRLS role
// cannot be created at all, so a worker there will never have 023 -- and a
// startup line warning that "only this worker's own tenant's workflows will
// execute" would be both alarming and false on every healthy worker in the
// deployment.
//
// The SCHEDULE half is still reported, because the rotation does not replace
// it: 024 remains the only way a non-default tenant's cron is seen.
func TestReportCrossTenantCapability_RotatingPath(t *testing.T) {
	report := func(t *testing.T, withFactory bool, capability engine.CrossTenantCapability) string {
		t.Helper()
		var buf bytes.Buffer
		// BOTH interfaces on one store, as PostgresStore has them. A fixture
		// that split them would have the schedule report silently skipped for
		// a reason no real deployment has.
		st := &listingCapabilityStore{
			capabilityStore: &capabilityStore{mockStore: &mockStore{}, capability: capability},
			tenants:         []string{"t1", "t2"},
		}
		w := newTestWorker(st.mockStore)
		defer w.cancel()
		w.store = st
		w.claimAcrossTenants = true
		w.claimStrategy = claimStrategyRotate
		if withFactory {
			w.storeFactory = &tenantStoreFactory{
				stores: map[string]*queuedTenantStore{}, openErr: map[string]error{},
			}
		}
		w.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		w.reportCrossTenantCapability()
		return buf.String()
	}

	t.Run("available, and says no grant is needed", func(t *testing.T) {
		// Claim ungranted on purpose: the rotating path must not care.
		out := report(t, true, engine.CrossTenantCapability{
			Schedules:   true,
			ClaimReason: "admin.claim_workflows does not exist; apply 023",
		})
		if !strings.Contains(out, "by tenant rotation") {
			t.Errorf("the rotating claim was not reported as available:\n%s", out)
		}
		if strings.Contains(out, "only this worker's own tenant's workflows will execute") {
			t.Errorf("warned about a missing 023 grant the rotating claim never uses:\n%s", out)
		}
	})

	t.Run("says so when it cannot rotate", func(t *testing.T) {
		out := report(t, false, engine.CrossTenantCapability{Schedules: true})
		if !strings.Contains(out, "cannot rotate") {
			t.Errorf("a worker with no store factory was not told it cannot rotate:\n%s", out)
		}
	})

	t.Run("still reports a missing schedule grant", func(t *testing.T) {
		out := report(t, true, engine.CrossTenantCapability{
			SchedulesReason: "admin.get_due_schedules does not exist; apply 024",
		})
		for _, want := range []string{"cron will fire", "024", "does not cover this"} {
			if !strings.Contains(out, want) {
				t.Errorf("the schedule report does not mention %q:\n%s", want, out)
			}
		}
	})
}

// listingCapabilityStore implements TenantLister and
// CrossTenantCapabilityChecker together, which is what every real store does.
type listingCapabilityStore struct {
	*capabilityStore
	tenants []string
}

func (s *listingCapabilityStore) ListTenantIDs(context.Context) ([]string, error) {
	return s.tenants, nil
}
