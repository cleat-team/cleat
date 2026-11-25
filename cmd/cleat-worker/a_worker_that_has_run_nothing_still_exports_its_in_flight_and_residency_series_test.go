package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// cleat#4474, through the worker's own /metrics handler. The in-flight gauge is started at zero by the worker and the residency series by the set it builds; this builds both the
// way main does, on a worker that has run nothing, and reads the scrape. The control is the same scrape without the in-flight call, which does not carry the gauge: that is
// the state an operator saw, and it is what shows the assertions below can fail.

func scrapeTheWorkersStartUpSeries(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	handleMetrics(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body, _ := io.ReadAll(rec.Result().Body)
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics answered %d: %s", rec.Code, body)
	}
	return string(body)
}

// startUpSeriesReading returns the scrape's lines that start with prefix and contain every label fragment after it, and the value when there is exactly one. Labels are written in
// name order, so worker_id comes before workflow_name and a fragment is matched anywhere in the line, not as a prefix.
func startUpSeriesReading(t *testing.T, scrape, prefix string, fragments ...string) (lines []string, value float64) {
	t.Helper()
lines:
	for _, line := range strings.Split(scrape, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		for _, f := range fragments {
			if !strings.Contains(line, f) {
				continue lines
			}
		}
		lines = append(lines, line)
	}
	if len(lines) == 1 {
		f := strings.Fields(lines[0])
		v, err := strconv.ParseFloat(f[len(f)-1], 64)
		if err != nil {
			t.Fatalf("cannot read the value of %q: %v", lines[0], err)
		}
		value = v
	}
	return lines, value
}

func TestAWorkerThatHasRunNothingStillExportsItsInFlightAndResidencySeries(t *testing.T) {
	build := func(t *testing.T, startInFlight bool) string {
		old := globalWorker
		t.Cleanup(func() { globalWorker = old })
		m := newTestPrometheus()
		globalWorker = &Worker{Metrics: m}
		cfg := engine.DefaultResidencyConfig()
		cfg.MaxRuns = 4
		cfg.Metrics = m
		if _, err := engine.NewResidentSet(cfg); err != nil {
			t.Fatalf("NewResidentSet: %v", err)
		}
		if startInFlight {
			startInFlightSeriesAtZero(m)
		}
		return scrapeTheWorkersStartUpSeries(t)
	}

	// The control: without the call the gauge is not on the scrape, though the residency series are.
	scrape := build(t, false)
	if lines, _ := startUpSeriesReading(t, scrape, "cleat_workflows_active{"); len(lines) != 0 {
		t.Fatalf("cleat_workflows_active is on the scrape before anything started it: %v; the control no longer shows the gap, so the case below proves nothing", lines)
	}

	scrape = build(t, true)
	for _, name := range []string{
		"cleat_workflows_active{",
		"cleat_resident_runs{",
		"cleat_resident_memory_charged_bytes{",
		`cleat_resident_evictions_total{reason="shutdown"`,
		`cleat_resident_refusals_total{kind="sleep",reason="window"`,
		`cleat_resident_parks_total{kind="signals",outcome="woke"`,
		`cleat_resident_rechecks_total{kind="signals",result="found"`,
	} {
		lines, v := startUpSeriesReading(t, scrape, name)
		if len(lines) != 1 {
			t.Errorf("%s: %d series on a worker that has run nothing, want 1 reading 0", name, len(lines))
		} else if v != 0 {
			t.Errorf("%s reads %v, want 0", name, v)
		}
	}
	if lines, v := startUpSeriesReading(t, scrape, "cleat_resident_memory_budget_bytes{"); len(lines) != 1 || v <= 0 {
		t.Errorf("the budget gauge is not a positive figure on this worker: %v", lines)
	}
}

// A run that starts moves the gauge off the start-up zero and back, and the start-up series stays at 0 beside it: the zero is a label set nothing real uses.
func TestAWorkflowActiveSeriesStartedAtZeroDoesNotHideTheRealOne(t *testing.T) {
	old := globalWorker
	t.Cleanup(func() { globalWorker = old })
	m := newTestPrometheus()
	globalWorker = &Worker{Metrics: m}
	startInFlightSeriesAtZero(m)
	m.AddWorkflowActive(t.Context(), 1, "order-flow")

	scrape := scrapeTheWorkersStartUpSeries(t)
	if lines, v := startUpSeriesReading(t, scrape, "cleat_workflows_active{", `workflow_name="order-flow"`); len(lines) != 1 || v != 1 {
		t.Errorf("a running order-flow: %v, want one series reading 1", lines)
	}
	if lines, v := startUpSeriesReading(t, scrape, "cleat_workflows_active{", `workflow_name=""`); len(lines) != 1 || v != 0 {
		t.Errorf("the start-up series after a run began: %v, want one series reading 0", lines)
	}
}
