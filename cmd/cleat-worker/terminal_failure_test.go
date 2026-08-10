package main

// Regression tests for the IMPROVEMENT-PLAN.md 1.2 caller residual in the
// dispatch paths: ~15 terminal writes that passed correct fence arguments and
// discarded the result.
//
// Unlike the concurrency-key defect (see concurrency_conflict_test.go), these
// were not data loss -- the store skipped the write correctly. The cost was
// that a lost fence was invisible, and that RecordWorkflowFailed was emitted
// *before* the store call, so a workflow another worker went on to complete
// was still counted as failed. The failure counter disagreed with the
// database, and the disagreement grew with exactly the thing that causes lost
// fences: workers stalling and being reaped.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// failedTotalFor scrapes the worker's own /metrics endpoint and returns the
// cleat_workflows_failed_total sample lines mentioning defName. Reading the
// published metric rather than an internal counter keeps the assertion on
// what an operator would actually see.
func failedTotalFor(t *testing.T, w *Worker, defName string) []string {
	t.Helper()
	rec := httptest.NewRecorder()
	w.Metrics.ServeHTTP().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	var out []string
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(line, "cleat_workflows_failed_total") && strings.Contains(line, defName) {
			out = append(out, line)
		}
	}
	return out
}

func testInstance(defName string) *engine.WorkflowInstance {
	return &engine.WorkflowInstance{
		ID:         "wf-under-test",
		DefName:    defName,
		Generation: 7,
		TenantID:   "00000000-0000-0000-0000-000000000000",
	}
}

// TestRecordTerminalFailure_FenceLostIsNotCountedAsFailed is the regression.
// A lost fence means another worker legitimately owns the workflow and may
// complete it successfully; counting it as failed here makes the metric wrong.
func TestRecordTerminalFailure_FenceLostIsNotCountedAsFailed(t *testing.T) {
	const defName = "fence-lost-wf"

	ms := &mockStore{}
	ms.failWorkflowFn = func(ctx context.Context, workflowID, workerID string, generation int64, errorMsg, errorCode, errorOp string, queryState map[string]string) error {
		return engine.ErrFenceLost
	}
	w := newTestWorker(ms)

	w.recordTerminalFailure(testInstance(defName), time.Now(), "boom", engine.ErrUnknown.String(), "")

	if samples := failedTotalFor(t, w, defName); len(samples) > 0 {
		t.Errorf("workflow counted as failed although the fenced write did not apply; "+
			"another worker owns it and may complete it successfully:\n  %s", strings.Join(samples, "\n  "))
	}
}

// TestRecordTerminalFailure_AppliedWriteIsCountedAsFailed is the positive
// control. Without it the test above would pass against a worker that never
// records anything at all.
func TestRecordTerminalFailure_AppliedWriteIsCountedAsFailed(t *testing.T) {
	const defName = "fence-held-wf"

	ms := &mockStore{}
	ms.failWorkflowFn = func(ctx context.Context, workflowID, workerID string, generation int64, errorMsg, errorCode, errorOp string, queryState map[string]string) error {
		return nil // the fence held
	}
	w := newTestWorker(ms)

	w.recordTerminalFailure(testInstance(defName), time.Now(), "boom", engine.ErrUnknown.String(), "")

	if samples := failedTotalFor(t, w, defName); len(samples) == 0 {
		t.Error("a terminal failure that applied was not counted as failed")
	}
}

// TestRecordTerminalFailure_UsesTheWorkflowsFence checks the arguments, not
// just the outcome. The concurrency-key defect in #263 was a caller passing
// ("", 0) to a fenced write; nothing in the dispatch path stops a future edit
// doing the same, and the store would silently skip every write again.
func TestRecordTerminalFailure_UsesTheWorkflowsFence(t *testing.T) {
	var gotWorker string
	var gotGeneration int64
	var called bool

	ms := &mockStore{}
	ms.failWorkflowFn = func(ctx context.Context, workflowID, workerID string, generation int64, errorMsg, errorCode, errorOp string, queryState map[string]string) error {
		called = true
		gotWorker, gotGeneration = workerID, generation
		return nil
	}
	w := newTestWorker(ms)
	wf := testInstance("fence-args-wf")

	w.recordTerminalFailure(wf, time.Now(), "boom", engine.ErrUnknown.String(), "")

	if !called {
		t.Fatal("FailWorkflow was never called")
	}
	if gotWorker != w.id || gotGeneration != wf.Generation {
		t.Errorf("fenced write used (workerID=%q, generation=%d), want (%q, %d) -- "+
			"a write that cannot match its row is skipped by the store and reports nothing",
			gotWorker, gotGeneration, w.id, wf.Generation)
	}
}

// TestRecordTerminalFailure_RetriesExhaustedDeadLetters pins the routing that
// used to live inline at each call site, so folding it into the helper did not
// quietly change which workflows are dead-lettered.
func TestRecordTerminalFailure_RetriesExhaustedDeadLetters(t *testing.T) {
	for _, tc := range []struct {
		name       string
		errMsg     string
		wantDLQ    bool
		wantFailed bool
	}{
		{"retries exhausted", "step failed: retries exhausted after 5 attempts", true, false},
		{"ordinary failure", "step failed: connection refused", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sawDLQ, sawFail bool
			ms := &mockStore{}
			ms.moveToDeadLetterQueueFn = func(ctx context.Context, workflowID, workerID string, generation int64, errorMsg, errorCode, errorOp string) error {
				sawDLQ = true
				return nil
			}
			ms.failWorkflowFn = func(ctx context.Context, workflowID, workerID string, generation int64, errorMsg, errorCode, errorOp string, queryState map[string]string) error {
				sawFail = true
				return nil
			}
			w := newTestWorker(ms)

			w.recordTerminalFailure(testInstance("dlq-routing-wf"), time.Now(), tc.errMsg, "", "")

			if sawDLQ != tc.wantDLQ || sawFail != tc.wantFailed {
				t.Errorf("routing for %q: dead-letter=%v fail=%v, want dead-letter=%v fail=%v",
					tc.errMsg, sawDLQ, sawFail, tc.wantDLQ, tc.wantFailed)
			}
		})
	}
}

// TestReleaseWorkflow_FenceLostIsNotAnError covers the release path: losing
// the fence there means there is nothing to release, which is not a failure.
func TestReleaseWorkflow_FenceLostIsNotAnError(t *testing.T) {
	const defName = "release-fence-wf"

	ms := &mockStore{}
	ms.releaseWorkflowFn = func(ctx context.Context, workflowID, workerID string, generation int64, nextWakeAt time.Time) error {
		return engine.ErrFenceLost
	}
	w := newTestWorker(ms)

	w.releaseWorkflow(testInstance(defName))

	if samples := failedTotalFor(t, w, defName); len(samples) > 0 {
		t.Errorf("a lost fence on release was counted as a workflow failure:\n  %s", strings.Join(samples, "\n  "))
	}
}
