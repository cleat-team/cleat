package jobqueue

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// dispatchOneJob enqueues a job with a workflow target, dispatches it, and
// returns the run_id the dispatcher stored. Fails the test (not just
// UNMEASURED) on anything short of a clean single dispatch, since every test
// below depends on there being exactly one dispatched row to observe.
func dispatchOneJob(t *testing.T, p *Plugin, store *fakeJobQueueStore, queueName, jobID string) string {
	t.Helper()
	defName := "my-workflow"
	insertPendingJob(store, queueName, jobID, []byte(`{"source":"test"}`), &defName, []byte(`{}`))

	if _, dispatched, _, err := p.pollPending(context.Background()); err != nil || dispatched != 1 {
		t.Fatalf("UNMEASURED: pollPending dispatched=%d err=%v, want 1 and nil", dispatched, err)
	}

	store.mu.RLock()
	row, ok := store.rows[rowKey(testTenantID.String(), queueName, jobID)]
	var runID string
	if ok && row.runID != nil {
		runID = *row.runID
	}
	gotStatus := ""
	if ok {
		gotStatus = row.status
	}
	store.mu.RUnlock()
	if runID == "" {
		t.Fatal("UNMEASURED: dispatch stored no run_id")
	}
	if gotStatus != "dispatched" {
		t.Fatalf("after dispatch, status = %q, want \"dispatched\"", gotStatus)
	}
	return runID
}

func getJobStatus(t *testing.T, handler http.Handler, queueName, jobID string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authedRequest("GET", "/jobqueue/"+queueName+"/jobs/"+jobID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET job: %d: %s", rec.Code, rec.Body.String())
	}
	return decodeJob(t, rec.Body.Bytes())
}

// TestAJobWhoseWorkflowFailed is cleat#1715's own named acceptance test: a
// job whose workflow fails must be distinguishable, through the read API,
// from one whose workflow succeeded. Before this fix both read "completed"
// the moment the workflow was started, because that is when the write
// happened.
func TestAJobWhoseWorkflowFailed(t *testing.T) {
	p, handler, store, _, _ := setupTestPlugin(t)

	jobID := uuid.New().String()
	runID := dispatchOneJob(t, p, store, "wf-queue", jobID)

	// The workflow this job started ran and failed. Nothing about that is
	// visible to the job row until the finalize observer says so.
	if err := p.ObserveFinalize(context.Background(), runID, "failed"); err != nil {
		t.Fatalf("ObserveFinalize: %v", err)
	}

	m := getJobStatus(t, handler, "wf-queue", jobID)
	if got := m["status"]; got != "failed" {
		t.Errorf("status = %v, want %q -- the workflow failed and the job still "+
			"reports it as if it succeeded", got, "failed")
	}
}

// TestAJobWhoseWorkflowSucceeded is the positive control for the test above:
// without it, a finalize observer that always writes "failed" would also
// pass TestAJobWhoseWorkflowFailed.
func TestAJobWhoseWorkflowSucceeded(t *testing.T) {
	p, handler, store, _, _ := setupTestPlugin(t)

	jobID := uuid.New().String()
	runID := dispatchOneJob(t, p, store, "wf-queue", jobID)

	if err := p.ObserveFinalize(context.Background(), runID, "done"); err != nil {
		t.Fatalf("ObserveFinalize: %v", err)
	}

	m := getJobStatus(t, handler, "wf-queue", jobID)
	if got := m["status"]; got != "completed" {
		t.Errorf("status = %v, want %q", got, "completed")
	}
}

// TestAJobWhoseWorkflowWasDeadLettered is cleat#1976's acceptance criterion
// for dead-lettering: a run that exhausted its retries must be recorded
// distinguishably from an ordinary failure, not folded into "failed" the way
// it was before this issue (when it reached ObserveFinalize at all -- before
// #1976 it did not, on any of the paths that can dead-letter a run).
func TestAJobWhoseWorkflowWasDeadLettered(t *testing.T) {
	p, handler, store, _, _ := setupTestPlugin(t)

	jobID := uuid.New().String()
	runID := dispatchOneJob(t, p, store, "wf-queue", jobID)

	if err := p.ObserveFinalize(context.Background(), runID, "dead_lettered"); err != nil {
		t.Fatalf("ObserveFinalize: %v", err)
	}

	m := getJobStatus(t, handler, "wf-queue", jobID)
	if got := m["status"]; got != "dead_lettered" {
		t.Errorf("status = %v, want %q -- a dead-lettered run must not read the same "+
			"as an ordinary failure", got, "dead_lettered")
	}
}

// TestAJobWhoseWorkflowWasCancelled covers the two operator-initiated
// terminal outcomes (CancelWorkflow, TerminateWorkflow) that reach
// ObserveFinalize as of cleat#1976. task_queue has no separate lifecycle for
// them, unlike workflow_instances, so both bucket to "failed" -- a decision,
// not a gap; see finalize_observer.go's doc comment.
func TestAJobWhoseWorkflowWasCancelled(t *testing.T) {
	p, handler, store, _, _ := setupTestPlugin(t)

	jobID := uuid.New().String()
	runID := dispatchOneJob(t, p, store, "wf-queue", jobID)

	if err := p.ObserveFinalize(context.Background(), runID, "cancelled"); err != nil {
		t.Fatalf("ObserveFinalize: %v", err)
	}

	m := getJobStatus(t, handler, "wf-queue", jobID)
	if got := m["status"]; got != "failed" {
		t.Errorf("status = %v, want %q", got, "failed")
	}
}

// TestObserveFinalizeDoesNotOverwriteATerminalStatus guards the crash-retry
// case the finalize observer's own doc comment calls out: a caller that
// fires ObserveFinalize twice (worker restart between the store write and
// the observer call, then a retry) must not be able to flip an
// already-recorded outcome.
func TestObserveFinalizeDoesNotOverwriteATerminalStatus(t *testing.T) {
	p, handler, store, _, _ := setupTestPlugin(t)

	jobID := uuid.New().String()
	runID := dispatchOneJob(t, p, store, "wf-queue", jobID)

	if err := p.ObserveFinalize(context.Background(), runID, "done"); err != nil {
		t.Fatalf("ObserveFinalize (first): %v", err)
	}
	// A second, contradictory call -- what a retried finalize would produce.
	if err := p.ObserveFinalize(context.Background(), runID, "failed"); err != nil {
		t.Fatalf("ObserveFinalize (second): %v", err)
	}

	m := getJobStatus(t, handler, "wf-queue", jobID)
	if got := m["status"]; got != "completed" {
		t.Errorf("status = %v, want %q -- a second ObserveFinalize call flipped "+
			"an already-recorded outcome", got, "completed")
	}
}

// TestObserveFinalizeOverwritesAnAbandonedVerdict covers the one case where a
// later ObserveFinalize call IS allowed to change status: the abandonment
// sweep already gave up and marked the row 'abandoned' on an inference from
// absence, and a genuine, authoritative outcome arrives after all. That is
// stronger evidence than the inference and must win.
func TestObserveFinalizeOverwritesAnAbandonedVerdict(t *testing.T) {
	p, handler, store, _, _ := setupTestPlugin(t)

	jobID := uuid.New().String()
	runID := dispatchOneJob(t, p, store, "wf-queue", jobID)

	// The run is no longer in flight (never added to inFlightRunIDs), so the
	// sweep concludes it is gone with no recorded outcome.
	if n := p.sweepAbandonedJobs(context.Background()); n != 1 {
		t.Fatalf("UNMEASURED: sweepAbandonedJobs = %d, want 1", n)
	}
	if got := getJobStatus(t, handler, "wf-queue", jobID)["status"]; got != "abandoned" {
		t.Fatalf("UNMEASURED: status after sweep = %v, want %q", got, "abandoned")
	}

	// The authoritative outcome arrives late.
	if err := p.ObserveFinalize(context.Background(), runID, "done"); err != nil {
		t.Fatalf("ObserveFinalize: %v", err)
	}

	m := getJobStatus(t, handler, "wf-queue", jobID)
	if got := m["status"]; got != "completed" {
		t.Errorf("status = %v, want %q -- a late authoritative outcome did not "+
			"overwrite the sweep's inference", got, "completed")
	}
}

// TestSweepAbandonedJobs is the positive control background.go's own comment
// demands of every caller: this reaper has silently no-opped twice before on
// this exact table (cleat#1133, #1134, #1141), so a run that engineers a real
// abandoned row and checks the count went to 1 is required, not optional.
//
// Three rows, three different fates, so a sweep that abandons everything
// dispatched (ignoring in-flight state) or abandons nothing (a dead no-op)
// both fail this test.
func TestSweepAbandonedJobs(t *testing.T) {
	p, handler, store, _, _ := setupTestPlugin(t)

	goneJobID := uuid.New().String()
	dispatchOneJob(t, p, store, "wf-queue", goneJobID)

	inFlightJobID := uuid.New().String()
	inFlightRunID := dispatchOneJob(t, p, store, "wf-queue", inFlightJobID)
	store.mu.Lock()
	store.inFlightRunIDs[inFlightRunID] = true
	store.mu.Unlock()

	doneJobID := uuid.New().String()
	doneRunID := dispatchOneJob(t, p, store, "wf-queue", doneJobID)
	if err := p.ObserveFinalize(context.Background(), doneRunID, "done"); err != nil {
		t.Fatalf("ObserveFinalize: %v", err)
	}

	if n := p.sweepAbandonedJobs(context.Background()); n != 1 {
		t.Fatalf("sweepAbandonedJobs = %d, want 1 (exactly the gone one; run_id %s "+
			"was in flight and run_id %s already had a recorded outcome)",
			n, inFlightRunID, doneRunID)
	}

	if got := getJobStatus(t, handler, "wf-queue", goneJobID)["status"]; got != "abandoned" {
		t.Errorf("gone job status = %v, want %q", got, "abandoned")
	}
	if got := getJobStatus(t, handler, "wf-queue", inFlightJobID)["status"]; got != "dispatched" {
		t.Errorf("in-flight job status = %v, want %q -- the sweep abandoned a "+
			"job whose run is still running", got, "dispatched")
	}
	if got := getJobStatus(t, handler, "wf-queue", doneJobID)["status"]; got != "completed" {
		t.Errorf("done job status = %v, want %q -- the sweep overwrote a real "+
			"recorded outcome", got, "completed")
	}
}
