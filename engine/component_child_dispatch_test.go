//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// cleat#3390: durable-poll-child and durable-await-any-child were registered
// with the component linker but had no witTypeMap entry, so they linked to the
// deprecated cbTypeDefault fallback and a Python guest on a worker never reached
// PollChild or AwaitAnyChild.
//
// These drive the dispatchers over a REAL execSession and a child-store double,
// not over mockHostHandler. The mock returns a canned word and writes nothing to
// the buffer, which is how TestDispatchAwaitChild can assert only "a string came
// back"; here the string's content is the claim.

// childDispatchSession returns a backend whose handler is a real session over
// the given child store, and the session so a test can read what it recorded.
func childDispatchSession(store *mockChildStore) (*wasmtimeBackend, *execSession) {
	s := newTestExecSession()
	s.nowMs = 2_000
	s.engine.childWfStore = store
	return &wasmtimeBackend{handler: s}, s
}

func dispatchChildCall(t *testing.T, b *wasmtimeBackend, method int, arg string) string {
	t.Helper()
	strPtr, _, free := cgotestMakeStrArgs(arg)
	defer free()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchStr(method, strPtr, 1, resultPtr); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	got, ok := cgotestReadResultString(resultPtr)
	if !ok {
		t.Fatal("the dispatcher wrote no string into the result slot -- the cbTypeDefault " +
			"fallback writes a u64, which is what these two calls got before cleat#3390")
	}
	return got
}

const (
	dispatchPollChildMethod     = 26
	dispatchAwaitAnyChildMethod = 27
)

func TestDispatchPollChildReturnsTheHostsDocument(t *testing.T) {
	completed := &mockChildStore{
		getChildResultFn: func(ctx context.Context, runID string) (ChildOutcome, error) {
			return ChildOutcome{Completed: true, Result: `{"ok":true}`}, nil
		},
		getChildCompletedAtMsFn: func(ctx context.Context, runID string) (int64, bool, error) {
			return 1_000, true, nil
		},
	}
	b, _ := childDispatchSession(completed)
	got := dispatchChildCall(t, b, dispatchPollChildMethod, "run-1")
	var doc struct {
		Status string `json:"status"`
		Result string `json:"result"`
	}
	if err := json.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatalf("poll_child answered %q, which is not the host's JSON document: %v", got, err)
	}
	if doc.Status != "completed" || doc.Result != `{"ok":true}` {
		t.Errorf("poll_child answered %+v, want status=completed result={\"ok\":true}", doc)
	}

	running := &mockChildStore{getChildResultFn: func(ctx context.Context, runID string) (ChildOutcome, error) {
		return ChildOutcome{}, nil
	}}
	b, _ = childDispatchSession(running)
	if got := dispatchChildCall(t, b, dispatchPollChildMethod, "run-1"); got != `{"status":"running"}` {
		t.Errorf("poll_child on a running child answered %q, want {\"status\":\"running\"}", got)
	}
}

// A store that cannot answer is a HOST error, not a child status. WIT types the
// result as a bare string, so the guest receives the host's message: not valid
// JSON, so the Python parse fails loudly instead of reading a status.
func TestDispatchPollChildStoreErrorArrivesAsTheHostsMessage(t *testing.T) {
	broken := &mockChildStore{getChildResultFn: func(ctx context.Context, runID string) (ChildOutcome, error) {
		return ChildOutcome{}, fmt.Errorf("connection refused")
	}}
	b, _ := childDispatchSession(broken)
	got := dispatchChildCall(t, b, dispatchPollChildMethod, "run-1")
	if !strings.Contains(got, "connection refused") {
		t.Errorf("poll_child answered %q, want the host's message naming the store error", got)
	}
	if json.Valid([]byte(got)) {
		t.Errorf("the host's error message %q parses as JSON, so a guest could read it as a status", got)
	}
}

func TestDispatchAwaitAnyChildReturnsTheWinner(t *testing.T) {
	done := &mockChildStore{getChildResultFn: func(ctx context.Context, runID string) (ChildOutcome, error) {
		return ChildOutcome{Completed: true, Result: `{"v":1}`}, nil
	}}
	b, s := childDispatchSession(done)
	got := dispatchChildCall(t, b, dispatchAwaitAnyChildMethod, `["run-b","run-a"]`)
	var doc struct {
		RunID  string `json:"run_id"`
		Result string `json:"result"`
	}
	if err := json.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatalf("await_any_child answered %q, not the winner's document: %v", got, err)
	}
	if doc.RunID != "run-a" || doc.Result != `{"v":1}` {
		t.Errorf("await_any_child answered %+v, want run-a (the sorted-first completed child) with {\"v\":1}", doc)
	}
	if s.suspendErr != nil {
		t.Errorf("a completed child suspended the run: %+v", s.suspendErr)
	}
}

// The pending case is the one packAwaitChildResultSuspend (1<<62) answers.
// extractStringFromSimplePacked reads that word's length field as 1<<30,
// clamps it to the buffer, and returns 64 KiB of zero bytes -- which is not
// what a guest should be handed as a winner's document.
func TestDispatchAwaitAnyChildPendingIsEmptyAndSuspends(t *testing.T) {
	pending := &mockChildStore{getChildResultFn: func(ctx context.Context, runID string) (ChildOutcome, error) {
		return ChildOutcome{}, nil
	}}
	b, s := childDispatchSession(pending)
	got := dispatchChildCall(t, b, dispatchAwaitAnyChildMethod, `["run-a"]`)
	if got != "" {
		t.Errorf("a pending await_any_child answered %d bytes (%q...), want the empty string", len(got), got[:min(len(got), 16)])
	}
	if s.suspendErr == nil {
		t.Error("a pending await_any_child did not suspend the run; an empty answer alone would read as a result")
	}
}

func TestExtractChildAnswerIsNotTheSimpleExtractorForTheSuspendWord(t *testing.T) {
	buf := make([]byte, 65536)
	suspend := packAwaitChildResultSuspend()

	// The control: the extractor the other child dispatchers use turns the
	// suspend word into a buffer-sized string. If it ever stops doing so this
	// test is no longer about anything.
	if got := extractStringFromSimplePacked(suspend, buf); len(got) != len(buf) {
		t.Fatalf("extractStringFromSimplePacked(suspend) = %d bytes, want %d; the premise of "+
			"extractChildAnswer has changed", len(got), len(buf))
	}
	if got := extractChildAnswer(suspend, buf); got != "" {
		t.Errorf("extractChildAnswer(suspend) = %d bytes, want the empty string", len(got))
	}

	// And it still reads an ordinary answer.
	copy(buf, `{"a":1}`)
	if got := extractChildAnswer(packSimpleResult(0, 7), buf); got != `{"a":1}` {
		t.Errorf("extractChildAnswer(ordinary) = %q, want {\"a\":1}", got)
	}
}
