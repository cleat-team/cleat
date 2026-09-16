package jobqueue

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// decodeJob pulls run_id out of a response without a typed struct, so this test
// fails if the FIELD IS RENAMED as well as if it is dropped. Decoding into
// JobResponse would follow a rename and keep passing, which is the shape of
// check this whole issue is about.
func decodeJob(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decoding job response: %v\n%s", err, body)
	}
	return m
}

// TestAJobNamesTheRunItStarted covers the half of cleat#1715 that needed no
// decision: the link from a job to its workflow run existed as a column and was
// reachable through no API.
//
// THE DISPATCHER HAS ALWAYS WRITTEN task_queue.run_id. What was missing is that
// neither read path selected it and JobResponse had no field for it, so an
// operator holding a job row had no way to reach the run. That is what made the
// job's own status unfalsifiable from outside: a job whose workflow failed and
// one whose workflow did the work both report "completed", because status is
// written when the run is STARTED, and the one field that could tell them apart
// was not returned.
//
// This does not fix the status semantics -- that is the rest of cleat#1715 --
// it makes the claim checkable, which is the precondition for anyone noticing
// it is wrong.
func TestAJobNamesTheRunItStarted(t *testing.T) {
	p, handler, store, _, _ := setupTestPlugin(t)

	jobID := uuid.New().String()
	defName := "my-workflow"
	insertPendingJob(store, "wf-queue", jobID, []byte(`{"source":"test"}`), &defName, []byte(`{"x":1}`))

	if _, dispatched, _, err := p.pollPending(context.Background()); err != nil || dispatched != 1 {
		t.Fatalf("UNMEASURED: pollPending dispatched=%d err=%v, want 1 and nil -- "+
			"nothing was dispatched, so there is no run_id to expose", dispatched, err)
	}

	// The value the dispatcher actually stored, read from the fake store rather
	// than from the response, so the assertion compares two independent reads.
	store.mu.RLock()
	row, ok := store.rows[rowKey(testTenantID.String(), "wf-queue", jobID)]
	var stored string
	if ok && row.runID != nil {
		stored = *row.runID
	}
	store.mu.RUnlock()
	if stored == "" {
		t.Fatal("UNMEASURED: the dispatcher stored no run_id, so this test cannot " +
			"distinguish a read path that drops it from one that had nothing to read")
	}

	t.Run("the single-job read returns it", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, authedRequest("GET", "/jobqueue/wf-queue/jobs/"+jobID, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET job: %d: %s", rec.Code, rec.Body.String())
		}
		got := decodeJob(t, rec.Body.Bytes())["run_id"]
		if got != stored {
			t.Errorf("run_id = %v, want %q.\n\nThe job is reported as %v, and without "+
				"run_id nothing in this response says whether that run succeeded.",
				got, stored, decodeJob(t, rec.Body.Bytes())["status"])
		}
	})

	t.Run("the listing returns it", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, authedRequest("GET", "/jobqueue/wf-queue/jobs", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("list jobs: %d: %s", rec.Code, rec.Body.String())
		}
		var jobs []map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &jobs); err != nil {
			t.Fatalf("decoding listing: %v", err)
		}
		if len(jobs) != 1 {
			t.Fatalf("expected 1 job in the listing, got %d", len(jobs))
		}
		// BOTH read paths, deliberately. They are separate queries over the
		// same table and the column was missing from both; fixing one and
		// asserting only that one is how the other stays broken.
		if got := jobs[0]["run_id"]; got != stored {
			t.Errorf("listing run_id = %v, want %q", got, stored)
		}
	})
}

// TestAJobThatStartedNoRunReportsNoRunID is the control.
//
// Without it, "run_id is present" is also what a response that hardcodes a
// value, or that echoes a field for every row regardless, would produce. A job
// with no def_name dispatches no workflow, so its run_id must be ABSENT rather
// than empty-but-present -- which is what `omitempty` on a string buys, and why
// the field is a plain string instead of a *string.
func TestAJobThatStartedNoRunReportsNoRunID(t *testing.T) {
	p, handler, store, _, _ := setupTestPlugin(t)

	jobID := uuid.New().String()
	// No def_name: the dispatcher marks it completed without starting anything.
	insertPendingJob(store, "plain-queue", jobID, []byte(`{"source":"test"}`), nil, nil)

	if _, _, _, err := p.pollPending(context.Background()); err != nil {
		t.Fatalf("pollPending: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authedRequest("GET", "/jobqueue/plain-queue/jobs/"+jobID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET job: %d: %s", rec.Code, rec.Body.String())
	}

	m := decodeJob(t, rec.Body.Bytes())
	if v, present := m["run_id"]; present {
		t.Errorf("run_id is present as %v on a job that started no workflow. "+
			"Absent is the honest answer: there is no run, which is not the same "+
			"as a run whose id is the empty string.", v)
	}
}
