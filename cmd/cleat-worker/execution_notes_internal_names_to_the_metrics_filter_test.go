package main

// cleat#3001, the worker half of the /metrics member: executeWorkflow resolves
// the tenant's internal definition names and hands them to the metrics filter,
// because the class lives on the definition row and the metric record path
// holds only the run (wf), never the definition.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

func scrapeWorkerMetrics(t *testing.T, w *Worker) string {
	t.Helper()
	rec := httptest.NewRecorder()
	w.Metrics.ServeHTTP().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics = %d: %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// The tenant's internal names reach the filter, so an internal definition's
// series is dropped while a public one survives.
func TestExecutionNotesTheTenantsInternalNamesToTheMetricsFilter(t *testing.T) {
	ms := &mockStore{
		listWorkflowDefsFn: func(_ context.Context, _ string) ([]engine.WorkflowDef, error) {
			return []engine.WorkflowDef{
				{Name: "secret-def", Version: 1, Exposure: engine.ExposureInternal},
				{Name: "public-def", Version: 1, Exposure: engine.ExposurePublic},
			}, nil
		},
	}
	w := &Worker{store: ms, Metrics: newTestPrometheus()}

	w.noteInternalDefs(context.Background(), &engine.WorkflowInstance{DefName: "secret-def"})

	// Both series exist; the filter is what keeps one out of the scrape.
	w.Metrics.RecordWorkflowStarted(context.Background(), "secret-def")
	w.Metrics.RecordWorkflowStarted(context.Background(), "public-def")

	body := scrapeWorkerMetrics(t, w)
	if strings.Contains(body, "secret-def") {
		t.Errorf("the tenant's resolved internal name did not reach the filter:\n%s", body)
	}
	if !strings.Contains(body, "public-def") {
		t.Errorf("a public definition was hidden too, so the filter is not keyed on the class:\n%s", body)
	}
}

// A store that will not resolve must not put a query on every execution: the
// second call inside the refresh window is skipped. Asserted by counting the
// store call, which is the observable the throttle controls.
func TestTheResolutionIsThrottledPerTenant(t *testing.T) {
	var calls int
	ms := &mockStore{
		listWorkflowDefsFn: func(_ context.Context, _ string) ([]engine.WorkflowDef, error) {
			calls++
			return nil, nil
		},
	}
	w := &Worker{store: ms, Metrics: newTestPrometheus()}
	wf := &engine.WorkflowInstance{DefName: "d"}

	w.noteInternalDefs(context.Background(), wf)
	w.noteInternalDefs(context.Background(), wf)

	if calls != 1 {
		t.Errorf("the tenant's names were resolved %d times across two executions in one window, "+
			"want 1 -- a resolution per execution is the cost the throttle exists to avoid", calls)
	}
}

// The sweep is the half that makes the throttle a BOUND rather than a window: a
// definition that executes while public and is then TIGHTENED to internal --
// with no further execution -- is never re-resolved by the execution path, so
// without the sweep its series keeps naming it until the process restarts.
// cleat-review reproduced exactly this against the throttle alone.
func TestTheSweepReMarksADefinitionTightenedAfterItsLastExecution(t *testing.T) {
	var internal atomic.Bool
	ms := &mockStore{
		listWorkflowDefsFn: func(_ context.Context, _ string) ([]engine.WorkflowDef, error) {
			class := engine.ExposurePublic
			if internal.Load() {
				class = engine.ExposureInternal
			}
			return []engine.WorkflowDef{{Name: "tightened-def", Version: 1, Exposure: class}}, nil
		},
	}
	w := &Worker{store: ms, Metrics: newTestPrometheus()}
	ctx := context.Background()

	// It executes while public.
	w.noteInternalDefs(ctx, &engine.WorkflowInstance{DefName: "tightened-def"})
	w.Metrics.RecordWorkflowStarted(ctx, "tightened-def")
	if body := scrapeWorkerMetrics(t, w); !strings.Contains(body, "tightened-def") {
		t.Fatalf("setup: a public definition's series is absent from the scrape:\n%s", body)
	}

	// It is tightened to internal and the tenant executes NOTHING further, so
	// the execution-path throttle never re-resolves inside its window.
	internal.Store(true)

	// The sweep is what closes it.
	w.sweepInternalDefs(ctx)

	if body := scrapeWorkerMetrics(t, w); strings.Contains(body, "tightened-def") {
		t.Errorf("a definition tightened after its last execution is still named by the scrape, so the "+
			"throttle's window is not a bound:\n%s", body)
	}
}
