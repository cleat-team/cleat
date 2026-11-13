package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// cleat#3774: a run is never replayed without its compaction state.
//
// Compaction deletes the events it summarises from event_history in the
// transaction that writes the state, so afterwards the prefix of the run's
// history exists only in workflow_payloads.compaction_state. executeWorkflow used
// to log a warning and carry on with a NIL state for any error loading it, which
// replays the tail alone and diverges from what the run already did. These tests
// pin what happens instead, for each kind of error: the run is released, not
// replayed and not failed.
//
// executeWorkflow needs a real WASM binary to get anywhere (see
// a_worker_that_cannot_serve_a_run_releases_it_test.go), so the decision lives in
// loadCompactionStateOrRelease and TestExecuteWorkflowLoadsItsCompactionStateThroughTheGuard
// reads the source to keep the call on the path.

func compactionLoadFixture(t *testing.T, loadErr error, state *engine.CompactionState) (*Worker, *[]releaseRecord) {
	t.Helper()
	var mu sync.Mutex
	var releases []releaseRecord
	ms := &mockStore{
		loadCompactionStateFn: func(context.Context, string) (*engine.CompactionState, error) {
			return state, loadErr
		},
		releaseWorkflowFn: func(_ context.Context, id, workerID string, gen int64, next time.Time) error {
			mu.Lock()
			releases = append(releases, releaseRecord{id, workerID, gen, next})
			mu.Unlock()
			return nil
		},
		failWorkflowFn: func(_ context.Context, id, _ string, _ int64, msg, code, op string, _ map[string]string) error {
			t.Errorf("FailWorkflow was called for %s (code=%q op=%q msg=%q): an unreadable compaction state is "+
				"retryable, not a reason to destroy the run", id, code, op, msg)
			return nil
		},
		moveToDeadLetterQueueFn: func(_ context.Context, id, _ string, _ int64, _, _, _ string, _ map[string]string) error {
			t.Errorf("MoveToDeadLetterQueue was called for %s", id)
			return nil
		},
	}
	w := newTestWorker(ms)
	w.unservableBackoff = 30 * time.Second
	return w, &releases
}

func TestARunIsReleasedNotReplayedWhenItsCompactionStateCannotBeRead(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"a decryption failure", fmt.Errorf("load compaction state: %w: payload column: cipher: message authentication failed", engine.ErrPayloadDecryption)},
		{"a state that does not unmarshal", errors.New("unmarshal compaction state: invalid character 'x' looking for beginning of value")},
		{"a failed query", errors.New("load compaction state: pq: permission denied for table workflow_payloads")},
		{"the database being down", errors.New("load compaction state: dial tcp 10.0.0.5:5432: connect: connection refused")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, releases := compactionLoadFixture(t, c.err, nil)
			wf := unservableRun()

			cs, ok := w.loadCompactionStateOrRelease(w.store, wf)

			if ok || cs != nil {
				t.Fatalf("got state %v, ok=%v: the run would be replayed without its compaction state", cs, ok)
			}
			if len(*releases) != 1 {
				t.Fatalf("ReleaseWorkflow was called %d times, want 1: the run must go back to the queue", len(*releases))
			}
			if got := (*releases)[0]; got.workflowID != wf.ID || got.generation != wf.Generation {
				t.Errorf("released (%s, gen %d), want (%s, gen %d): the release must be fenced on the claim it gives up",
					got.workflowID, got.generation, wf.ID, wf.Generation)
			}
		})
	}
}

func TestARunWithAReadableCompactionStateIsServed(t *testing.T) {
	want := &engine.CompactionState{Version: 1, CompactedStep: 10}
	w, releases := compactionLoadFixture(t, nil, want)

	cs, ok := w.loadCompactionStateOrRelease(w.store, unservableRun())
	if !ok || cs != want {
		t.Fatalf("got (%v, %v), want the state it loaded and ok=true", cs, ok)
	}
	if len(*releases) != 0 {
		t.Errorf("a run whose state loaded was released %d times", len(*releases))
	}
}

func TestARunWithNoCompactionStateIsServed(t *testing.T) {
	w, releases := compactionLoadFixture(t, nil, nil)

	cs, ok := w.loadCompactionStateOrRelease(w.store, unservableRun())
	if !ok || cs != nil {
		t.Fatalf("got (%v, %v), want nil state and ok=true: an uncompacted run has none, and that is not an error", cs, ok)
	}
	if len(*releases) != 0 {
		t.Errorf("an uncompacted run was released %d times", len(*releases))
	}
}

// executeWorkflow cannot be driven without a WASM binary, so a regression that
// goes back to swallowing the error there would be invisible to the tests above.
// This reads the source for what that regression has to carry: the old warning,
// and a call that is not the guard.
func TestExecuteWorkflowLoadsItsCompactionStateThroughTheGuard(t *testing.T) {
	raw, err := os.ReadFile("setup.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	if n := strings.Count(src, "LoadCompactionState(w.ctx"); n != 1 {
		t.Errorf("setup.go calls LoadCompactionState(w.ctx ...) %d times, want exactly 1, inside loadCompactionStateOrRelease: "+
			"any other call can swallow its error and replay the tail alone", n)
	}
	if !strings.Contains(src, "w.loadCompactionStateOrRelease(execStore, wf)") {
		t.Error("executeWorkflow does not load the compaction state through loadCompactionStateOrRelease")
	}
}
