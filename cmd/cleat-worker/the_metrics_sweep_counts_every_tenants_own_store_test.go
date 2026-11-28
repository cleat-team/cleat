package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// cleat#3572. Four of the five gauges metricsSweepLoop publishes are tenant-scoped
// counts, and the sweep ran them through the worker's own store only, so on a
// worker serving other tenants each described one tenant. They are now the sum
// over every tenant the worker serves, published only when the pass is complete.
// These are the routing and publication halves, against recording stores and the
// doubles from the_reaper_visits_each_tenants_own_store_test.go; what is asserted
// is what a SCRAPE of the worker's metrics reports, not what was called.

// metricsProbe is a store that answers the five MetricsStore queries.
type metricsProbe struct {
	*mockStore
	mu    sync.Mutex
	vals  [gaugeCount]int64
	errs  [gaugeCount]error
	calls [gaugeCount]int
}

func newMetricsProbe(stuck, rows, size, keys, expiring int64) *metricsProbe {
	return &metricsProbe{mockStore: &mockStore{}, vals: [gaugeCount]int64{stuck, rows, size, keys, expiring}}
}

func (p *metricsProbe) answer(g int) (int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls[g]++
	if p.errs[g] != nil {
		return 0, p.errs[g]
	}
	return p.vals[g], nil
}

func (p *metricsProbe) called(g int) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[g]
}

func (p *metricsProbe) CountStalledWorkflows(context.Context, time.Duration) (int, error) {
	n, err := p.answer(gaugeStuck)
	return int(n), err
}
func (p *metricsProbe) CountEventHistoryTotal(context.Context) (int, error) {
	n, err := p.answer(gaugeEventRows)
	return int(n), err
}
func (p *metricsProbe) EstimateEventHistorySize(context.Context) (int64, error) {
	return p.answer(gaugeEventSize)
}
func (p *metricsProbe) CountActiveConcurrencyKeys(context.Context) (int, error) {
	n, err := p.answer(gaugeKeysTotal)
	return int(n), err
}
func (p *metricsProbe) CountConcurrencyKeysExpiringSoon(context.Context, time.Duration) (int, error) {
	n, err := p.answer(gaugeKeysExpiring)
	return int(n), err
}

// ownMetrics is the worker's own store: a metricsProbe that also enumerates tenants.
type ownMetrics struct {
	*metricsProbe
	tenants []string
	listErr error
}

func (s *ownMetrics) ListTenantIDs(context.Context) ([]string, error) {
	return s.tenants, s.listErr
}

func newMetricsWorker(own *ownMetrics, f *perTenantFactory) *Worker {
	w := newTestWorkerFromStore(own)
	w.storeTenantID = tenantSelf
	w.claimAcrossTenants = true
	w.storeFactory = f
	w.stallThreshold = time.Minute
	w.keyExpiryWindow = time.Hour
	return w
}

// scrape returns the worker's metrics as the /metrics endpoint serves them.
func scrape(t *testing.T, w *Worker) string {
	t.Helper()
	rec := httptest.NewRecorder()
	w.Metrics.ServeHTTP().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

// gauge returns the value of a series by metric name (the sum of its samples), and
// whether it is present at all: an unset gauge emits NO series, which is the state
// "never published" and must be told apart from a published zero.
func gauge(text, name string) (float64, bool) {
	var sum float64
	found := false
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "#") || !(strings.HasPrefix(line, name+" ") || strings.HasPrefix(line, name+"{")) {
			continue
		}
		f := strings.Fields(line)
		v, err := strconv.ParseFloat(f[len(f)-1], 64)
		if err != nil {
			continue
		}
		sum += v
		found = true
	}
	return sum, found
}

// loopRuns reads cleat_background_loops_total for the metrics sweep and one status.
func loopRuns(text, status string) float64 {
	var sum float64
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "cleat_background_loops_total{") &&
			strings.Contains(line, `loop_name="metrics_sweep"`) && strings.Contains(line, `status="`+status+`"`) {
			f := strings.Fields(line)
			v, _ := strconv.ParseFloat(f[len(f)-1], 64)
			sum += v
		}
	}
	return sum
}

var gaugeMetric = [gaugeCount]string{
	"cleat_workflows_stuck", "cleat_event_history_row_count", "cleat_event_history_size_bytes",
	"cleat_concurrency_keys_total", "cleat_concurrency_keys_expiring_soon",
}

func TestTheMetricsSweepPublishesTheSumOverEveryTenantAndTakesTheSizeFromTheOwnStoreOnly(t *testing.T) {
	// distinct, far-apart values per tenant and per gauge, so a total that misses a
	// tenant or counts one twice cannot coincide with the right one.
	own := &ownMetrics{metricsProbe: newMetricsProbe(1, 10, 1_000_000, 100, 7), tenants: []string{tenantSelf, tenantOther, "tenant-c"}}
	a, c := newMetricsProbe(20, 200, 999, 2_000, 30), newMetricsProbe(300, 3_000, 888, 40_000, 400)
	f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: a, "tenant-c": c}}
	w := newMetricsWorker(own, f)
	defer w.cancel()

	w.metricsSweepOnce(own)
	text := scrape(t, w)

	want := [gaugeCount]float64{1 + 20 + 300, 10 + 200 + 3_000, 1_000_000, 100 + 2_000 + 40_000, 7 + 30 + 400}
	for g := 0; g < gaugeCount; g++ {
		got, ok := gauge(text, gaugeMetric[g])
		if !ok || got != want[g] {
			t.Errorf("%s = %v (present=%v), want %v. Through the worker's own store alone (the bug) the three tenant-scoped "+
				"gauges describe one tenant; and the SIZE is whole-table, so it must be the own store's figure and NOT the sum (cleat#3572)",
				gaugeMetric[g], got, ok, want[g])
		}
	}
	// The size is asked of the worker's own store only.
	if a.called(gaugeEventSize) != 0 || c.called(gaugeEventSize) != 0 {
		t.Errorf("the whole-table size was asked of another tenant's store (%d, %d times): summing it multiplies it by the tenant count",
			a.called(gaugeEventSize), c.called(gaugeEventSize))
	}
	if got := loopRuns(text, "ok"); got != 1 {
		t.Errorf("a complete pass recorded %v 'ok' runs, want 1", got)
	}
	if got := loopRuns(text, "incomplete"); got != 0 {
		t.Errorf("a complete pass recorded %v 'incomplete' runs, want 0", got)
	}
}

// A partial sum must not be published as the total: the previous value stands.
func TestAnIncompleteMetricsPassKeepsTheLastTotalAndCountsItself(t *testing.T) {
	own := &ownMetrics{metricsProbe: newMetricsProbe(1, 10, 1_000_000, 100, 7), tenants: []string{tenantSelf, tenantOther, "tenant-c"}}
	a, c := newMetricsProbe(20, 200, 999, 2_000, 30), newMetricsProbe(300, 3_000, 888, 40_000, 400)
	f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: a, "tenant-c": c}}
	w := newMetricsWorker(own, f)
	defer w.cancel()

	w.metricsSweepOnce(own) // a complete pass: the totals are 321, 3210, ...
	w.lastDBTrouble.Store(1)
	troubleBefore := w.lastDBTrouble.Load()

	// Every value changes, and one tenant's stuck-workflow count now fails.
	own.vals = [gaugeCount]int64{2, 20, 2_000_000, 200, 14}
	a.vals = [gaugeCount]int64{40, 400, 999, 4_000, 60}
	c.vals = [gaugeCount]int64{600, 6_000, 888, 80_000, 800}
	c.errs[gaugeStuck] = errors.New("this tenant's pool is down")
	w.metricsSweepOnce(own)
	text := scrape(t, w)

	// The stuck total keeps its previous value (321), and is NOT the partial sum 2+40.
	if got, _ := gauge(text, gaugeMetric[gaugeStuck]); got != 321 {
		t.Errorf("cleat_workflows_stuck = %v after one tenant's count failed, want the previous total 321 and not a partial sum (2+40=42)", got)
	}
	// The gauges every tenant DID answer are complete, and are published.
	if got, _ := gauge(text, gaugeMetric[gaugeEventRows]); got != 20+400+6_000 {
		t.Errorf("cleat_event_history_row_count = %v, want %v: its queries all answered, so one tenant's other failure must not hold it back", got, 20+400+6_000)
	}
	if got := loopRuns(text, "incomplete"); got != 1 {
		t.Errorf("incomplete passes counted: %v, want 1: a stale total is otherwise invisible", got)
	}
	if got := w.lastDBTrouble.Load(); got != troubleBefore {
		t.Error("one tenant's count failing was recorded as this worker's database trouble")
	}
}

func TestAGapInTheTenantsHoldsEveryTotal(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func() (*ownMetrics, *perTenantFactory)
	}{
		{"a tenant's store would not open", func() (*ownMetrics, *perTenantFactory) {
			own := &ownMetrics{metricsProbe: newMetricsProbe(2, 20, 2_000_000, 200, 14), tenants: []string{tenantSelf, tenantOther, "tenant-c"}}
			return own, &perTenantFactory{
				stores:  map[string]engine.WorkflowStore{"tenant-c": newMetricsProbe(600, 6_000, 888, 80_000, 800)},
				openErr: map[string]error{tenantOther: errors.New("no pool")},
			}
		}},
		{"the tenant list could not be read", func() (*ownMetrics, *perTenantFactory) {
			own := &ownMetrics{metricsProbe: newMetricsProbe(2, 20, 2_000_000, 200, 14), listErr: errors.New("admin.tenants unreadable")}
			return own, &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: newMetricsProbe(40, 400, 999, 4_000, 60)}}
		}},
		{"a tenant's store cannot answer MetricsStore", func() (*ownMetrics, *perTenantFactory) {
			own := &ownMetrics{metricsProbe: newMetricsProbe(2, 20, 2_000_000, 200, 14), tenants: []string{tenantSelf, tenantOther}}
			return own, &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: &mockStore{}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			own, f := tc.setup()
			w := newMetricsWorker(own, f)
			defer w.cancel()
			w.metricsSweepOnce(own)
			text := scrape(t, w)
			for g := 0; g < gaugeCount; g++ {
				v, ok := gauge(text, gaugeMetric[g])
				if g == gaugeEventSize {
					// Whole-table, taken from the own store: no tenant gap can make it partial.
					if !ok || v != 2_000_000 {
						t.Errorf("%s = %v (present=%v), want the own store's 2000000: it does not depend on the tenants", gaugeMetric[g], v, ok)
					}
					continue
				}
				if ok {
					t.Errorf("%s was published (%v) on a pass that could not count every tenant: it must keep its previous value, here none", gaugeMetric[g], v)
				}
			}
			if got := loopRuns(text, "incomplete"); got != 1 {
				t.Errorf("incomplete passes counted: %v, want 1", got)
			}
		})
	}
}

func TestTheMetricsSweepStaysOnItsOwnTenantWhenItCannotServeOthers(t *testing.T) {
	other := newMetricsProbe(20, 200, 999, 2_000, 30)
	f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: other}}

	check := func(t *testing.T, w *Worker, own *ownMetrics) {
		t.Helper()
		w.metricsSweepOnce(own)
		text := scrape(t, w)
		if got, ok := gauge(text, gaugeMetric[gaugeStuck]); !ok || got != 1 {
			t.Errorf("cleat_workflows_stuck = %v (present=%v), want the own value 1", got, ok)
		}
		if other.called(gaugeStuck) != 0 {
			t.Error("another tenant was counted by a worker that does not serve other tenants")
		}
		if got := loopRuns(text, "incomplete"); got != 0 {
			t.Errorf("a worker that serves its own tenant only recorded %v incomplete passes: that is a complete pass", got)
		}
	}
	t.Run("claim-across-tenants off", func(t *testing.T) {
		own := &ownMetrics{metricsProbe: newMetricsProbe(1, 10, 1_000_000, 100, 7), tenants: []string{tenantSelf, tenantOther}}
		w := newMetricsWorker(own, f)
		defer w.cancel()
		w.claimAcrossTenants = false
		check(t, w, own)
	})
	t.Run("no store factory", func(t *testing.T) {
		own := &ownMetrics{metricsProbe: newMetricsProbe(1, 10, 1_000_000, 100, 7), tenants: []string{tenantSelf, tenantOther}}
		w := newMetricsWorker(own, nil)
		w.storeFactory = nil
		defer w.cancel()
		check(t, w, own)
	})
}

// The worker's OWN failures keep their old meaning: that gauge keeps its last value,
// the others are published, and the pass counts as an error (not as incomplete).
func TestAnOwnQueryFailingStillLeavesOnlyThatGaugeAndCountsAsAnError(t *testing.T) {
	own := &ownMetrics{metricsProbe: newMetricsProbe(1, 10, 1_000_000, 100, 7), tenants: []string{tenantSelf, tenantOther}}
	a := newMetricsProbe(20, 200, 999, 2_000, 30)
	f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: a}}
	w := newMetricsWorker(own, f)
	defer w.cancel()
	own.errs[gaugeEventRows] = errors.New("count failed")

	w.metricsSweepOnce(own)
	text := scrape(t, w)

	if v, ok := gauge(text, gaugeMetric[gaugeEventRows]); ok {
		t.Errorf("cleat_event_history_row_count was published (%v) though the own query failed", v)
	}
	if got, _ := gauge(text, gaugeMetric[gaugeStuck]); got != 21 {
		t.Errorf("cleat_workflows_stuck = %v, want 21: an unrelated gauge's failure must not hold it back", got)
	}
	if loopRuns(text, "error") != 1 || loopRuns(text, "incomplete") != 0 {
		t.Errorf("status counts: error=%v incomplete=%v, want 1 and 0", loopRuns(text, "error"), loopRuns(text, "incomplete"))
	}
}
