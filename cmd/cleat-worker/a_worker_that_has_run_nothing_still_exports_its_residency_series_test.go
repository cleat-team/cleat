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

// cleat#4474, through the worker's own /metrics handler: the residency set a worker builds starts its series at zero, so a worker that has run nothing reports them.
// cleat_workflows_active is deliberately NOT started at zero: it is labelled by definition name, which a fresh worker cannot know, and a placeholder series would show as a blank
// row in every by-workflow panel. It is exported as soon as a run starts, and a missing series there means 0 (docs/reference/worker-config.md, "Reading the residency metrics").

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
// name order, so worker_id comes after kind and reason and a fragment is matched anywhere in the line, not as a prefix.
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

func TestAWorkerThatHasRunNothingStillExportsItsResidencySeries(t *testing.T) {
	build := func(t *testing.T, maxRuns int) string {
		old := globalWorker
		t.Cleanup(func() { globalWorker = old })
		m := newTestPrometheus()
		globalWorker = &Worker{Metrics: m}
		cfg := engine.DefaultResidencyConfig()
		cfg.MaxRuns = maxRuns
		cfg.Metrics = m
		if _, err := engine.NewResidentSet(cfg); err != nil {
			t.Fatalf("NewResidentSet: %v", err)
		}
		return scrapeTheWorkersStartUpSeries(t)
	}

	// The control: with residency off the same worker exports no residency series, so the series below are the set's doing and not something every scrape carries.
	if lines, _ := startUpSeriesReading(t, build(t, 0), "cleat_resident_"); len(lines) != 0 {
		t.Fatalf("a worker with residency off exports residency series: %v", lines)
	}

	scrape := build(t, 4)
	for _, name := range []string{
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
