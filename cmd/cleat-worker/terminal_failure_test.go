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

// TestDeadLetteringIsDecidedByARecordedFactNotAPhrase is cleat#902's other half.
//
// Routing used to be `strings.Contains(errMsg, "retries exhausted")` over a
// human-readable string the guest could have written. It is now
// EventRecord.RetriesExhausted -- set by engine/durablecalls.go where the
// engine knows, persisted, and read back out of the segment history -- with
// the text used only to tie the terminal error to that specific event.
//
// The table is the difference between those two, case by case. Cases 2 and 3
// are the two defects; case 6 is the near-miss that makes RetriesExhausted a
// field of its own rather than a re-reading of ErrNonRetryable.
func TestDeadLetteringIsDecidedByARecordedFactNotAPhrase(t *testing.T) {
	// The text engine/durablecalls.go would have recorded and handed to the guest.
	const engineText = "retries exhausted: connection refused"

	exhaustion := []engine.EventRecord{{
		EventType: engine.EventTypeCall, Service: "svc", Op: "op",
		Err: engineText, RetriesExhausted: true,
	}}

	for _, tc := range []struct {
		name    string
		history []engine.EventRecord
		errMsg  string
		wantDLQ bool
		why     string
	}{
		{
			name: "the exhaustion that ended the workflow", history: exhaustion,
			errMsg: engineText, wantDLQ: true,
			why: "the case that must keep working -- a real exhaustion still reaches the DLQ",
		},
		{
			name: "the guest wrapped the engine's text", history: exhaustion,
			errMsg: "step 3: " + engineText, wantDLQ: true,
			why: "a workflow is free to wrap what it received and still died of the exhaustion",
		},
		{
			name: "the phrase, with no exhaustion behind it", history: nil,
			errMsg: "step failed: retries exhausted after 5 attempts", wantDLQ: false,
			why: "FALSE POSITIVE, fixed: text mentioning retry exhaustion no longer routes anything",
		},
		{
			name: "an exhaustion the guest caught, then an unrelated failure", history: exhaustion,
			errMsg: "step failed: invalid customer id", wantDLQ: false,
			why: "REGRESSION GUARD: history holds an exhaustion, but it is not what ended this workflow",
		},
		{
			name: "an ordinary failure", history: nil,
			errMsg: "step failed: connection refused", wantDLQ: false,
			why: "unchanged",
		},
		{
			name: "a failed call that was NOT an exhaustion, same text",
			history: []engine.EventRecord{{
				EventType: engine.EventTypeCall, Service: "svc", Op: "op",
				Err: engineText, RetriesExhausted: false,
			}},
			errMsg: engineText, wantDLQ: false,
			why: "the typed bit decides, not the text -- engine/callintent.go records exactly this shape",
		},
		{
			name: "no history at all", history: nil, errMsg: engineText, wantDLQ: false,
			why: "the pre-execution paths (WASM load, version check) cannot be exhaustions",
		},
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

			w.recordTerminalFailureWithHistory(testInstance("dlq-routing-wf"), time.Now(), tc.errMsg, "", "", tc.history)

			if sawDLQ != tc.wantDLQ || sawFail == tc.wantDLQ {
				t.Errorf("routing for %q: dead-letter=%v fail=%v, want dead-letter=%v fail=%v\n  %s",
					tc.errMsg, sawDLQ, sawFail, tc.wantDLQ, !tc.wantDLQ, tc.why)
			}
		})
	}
}

// TestAPanicIsNeverDeadLettered is cleat#902's unambiguous half.
//
// releaseOrFail has exactly one caller -- executeWorkflow's panic recovery,
// `w.releaseOrFail(wf, fmt.Sprintf("panic: %v", r))` -- so every message that
// reaches it is a recovered panic value. Until eligibleForDLQ existed, whether
// one was dead-lettered was decided by whether that VALUE happened to contain
// the words "retries exhausted".
//
// Both cases here are the same panic as far as the system is concerned. The
// first is the one that used to be routed to the dead-letter queue, and the
// only thing that put it there was a substring in text the guest chose.
func TestAPanicIsNeverDeadLettered(t *testing.T) {
	for _, tc := range []struct {
		name   string
		panicV string
	}{
		{"panic whose text happens to say retries exhausted",
			"panic: sending on closed channel: retries exhausted after 5 attempts"},
		{"ordinary panic", "panic: runtime error: index out of range [3] with length 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sawDLQ, sawFail bool
			var gotCode, gotOp string
			ms := &mockStore{}
			ms.moveToDeadLetterQueueFn = func(_ context.Context, _, _ string, _ int64, _, _, _ string) error {
				sawDLQ = true
				return nil
			}
			ms.failWorkflowFn = func(_ context.Context, _, _ string, _ int64, _, errorCode, errorOp string, _ map[string]string) error {
				sawFail, gotCode, gotOp = true, errorCode, errorOp
				return nil
			}
			w := newTestWorker(ms)

			w.releaseOrFail(testInstance("panic-routing-wf"), tc.panicV)

			if sawDLQ {
				t.Errorf("a recovered panic was dead-lettered. The dead-letter queue is for "+
					"work that exhausted its retries and can be reprocessed; a panic is a "+
					"crash, and it landed there only because its text contained a phrase: %q",
					tc.panicV)
			}
			if !sawFail {
				t.Fatal("the panic was neither dead-lettered nor failed, so nothing recorded it")
			}
			// The old call passed "", "" -- so every panicked workflow was
			// stored with a blank classification, indistinguishable in the
			// error_code column from a failure nobody had classified.
			if gotCode != engine.ErrUnknown.String() || gotOp != "panic" {
				t.Errorf("a panicked workflow was recorded with error_code=%q error_op=%q, "+
					"want %q/%q -- a blank code cannot be told from an unclassified failure",
					gotCode, gotOp, engine.ErrUnknown.String(), "panic")
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
