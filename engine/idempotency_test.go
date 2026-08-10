package engine

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
)

// TestIdempotencyKeyIsStableAndDistinct pins the two properties the whole
// mechanism rests on: the same logical step always produces the same key, and
// anything that is a different logical step produces a different one.
//
// The first property is what makes replay safe. The second is what stops
// unrelated calls deduplicating against each other, which would be far worse
// than the duplicates the key exists to prevent — a workflow would silently
// receive another call's response.
func TestIdempotencyKeyIsStableAndDistinct(t *testing.T) {
	const (
		wf  = "wf-1"
		run = "run-1"
	)

	base := DurableCallIdempotencyKey(wf, run, 0)

	t.Run("stable across calls", func(t *testing.T) {
		for i := 0; i < 100; i++ {
			if got := DurableCallIdempotencyKey(wf, run, 0); got != base {
				t.Fatalf("key changed between calls: %q then %q", base, got)
			}
		}
	})

	t.Run("distinct per step", func(t *testing.T) {
		seen := map[string]int{base: 0}
		for step := 1; step < 500; step++ {
			k := DurableCallIdempotencyKey(wf, run, step)
			if prev, dup := seen[k]; dup {
				t.Fatalf("step %d collides with step %d (%q)", step, prev, k)
			}
			seen[k] = step
		}
	})

	t.Run("distinct per run", func(t *testing.T) {
		// ContinueAsNew is genuinely new work and must not reuse the previous
		// run's keys, or the new run's first call would deduplicate against the
		// old run's and never execute.
		if k := DurableCallIdempotencyKey(wf, "run-2", 0); k == base {
			t.Error("a different runID produced the same key; ContinueAsNew would " +
				"collide with the run it continues from")
		}
	})

	t.Run("distinct per workflow", func(t *testing.T) {
		if k := DurableCallIdempotencyKey("wf-2", run, 0); k == base {
			t.Error("a different workflowID produced the same key")
		}
	})

	// The separator test. Without the NUL bytes, ("ab","c") and ("a","bc")
	// concatenate identically, and two unrelated workflows' calls would
	// deduplicate against each other.
	t.Run("unambiguous concatenation", func(t *testing.T) {
		if DurableCallIdempotencyKey("ab", "c", 1) == DurableCallIdempotencyKey("a", "bc", 1) {
			t.Error(`("ab","c") and ("a","bc") produced the same key: the ` +
				"separators are not doing their job, and unrelated calls would " +
				"deduplicate against each other")
		}
	})

	t.Run("header safe", func(t *testing.T) {
		// The key travels in an HTTP header value. Unpadded base32 is A-Z2-7,
		// so there is nothing to escape and no '=' for an intermediary to trim.
		const allowed = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
		if strings.ContainsFunc(base, func(r rune) bool {
			return !strings.ContainsRune(allowed, r)
		}) {
			t.Errorf("key %q contains a character outside unpadded base32", base)
		}
	})
}

// recordingCaller implements IdempotentCaller and deduplicates on the key, the
// way a service that honours idempotency keys would.
type recordingCaller struct {
	mu       sync.Mutex
	attempts int            // every invocation, including deduplicated ones
	executed map[string]int // key -> how many times work was actually performed
	keys     []string
}

func newRecordingCaller() *recordingCaller {
	return &recordingCaller{executed: make(map[string]int)}
}

func (c *recordingCaller) Call(_ context.Context, _, _, _ string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attempts++
	c.executed[""]++ // no key: every attempt is fresh work
	return `{"ok":true}`, nil
}

func (c *recordingCaller) CallWithIdempotencyKey(_ context.Context, _, _, _, key string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attempts++
	c.keys = append(c.keys, key)
	if _, seen := c.executed[key]; !seen {
		c.executed[key] = 0
	}
	c.executed[key]++
	return `{"ok":true}`, nil
}

// plainCaller implements only ServiceCaller.
type plainCaller struct{ calls int }

func (c *plainCaller) Call(_ context.Context, _, _, _ string) (string, error) {
	c.calls++
	return `{"ok":true}`, nil
}

// TestCallerHonoursIdempotencyKeys checks the detection, because the failure
// mode of an optional interface is silence: a caller that does not implement it
// is not a compile error, it just never receives a key.
func TestCallerHonoursIdempotencyKeys(t *testing.T) {
	withKeys := NewEngine(nil, newRecordingCaller())
	if !withKeys.CallerHonoursIdempotencyKeys() {
		t.Error("a caller implementing IdempotentCaller was not detected")
	}

	without := NewEngine(nil, &plainCaller{})
	if without.CallerHonoursIdempotencyKeys() {
		t.Error("a caller implementing only ServiceCaller was reported as " +
			"honouring keys")
	}
}

// TestRetryAttemptsShareOneIdempotencyKey is the property that makes retries
// safe against a key-honouring service.
//
// DurableCallWithRetry may invoke the caller several times for one logical
// step. Every one of those attempts must carry the same key — a retry after an
// ambiguous failure is precisely the case the key exists to collapse. If each
// attempt derived its own key, retrying a call that had in fact succeeded would
// perform the work again, which is the duplicate this phase exists to remove.
func TestRetryAttemptsShareOneIdempotencyKey(t *testing.T) {
	caller := newRecordingCaller()
	s := newTestExecSession()
	s.engine.caller = caller
	s.workflowID = "wf-retry"
	s.execRunID = "run-retry"

	ctx := context.Background()
	const step = 3
	for attempt := 0; attempt < 4; attempt++ {
		if _, err := s.callService(ctx, "svc", "op", "{}", step); err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
	}

	if caller.attempts != 4 {
		t.Fatalf("caller saw %d attempts, want 4", caller.attempts)
	}
	if len(caller.executed) != 1 {
		t.Errorf("attempts of one logical step produced %d distinct keys, want 1: %v",
			len(caller.executed), caller.keys)
	}
	want := DurableCallIdempotencyKey("wf-retry", "run-retry", step)
	if _, ok := caller.executed[want]; !ok {
		t.Errorf("attempts did not use the derived key %q; got %v", want, caller.keys)
	}
}

// TestCallServiceFallsBackForPlainCallers makes sure a caller that cannot
// deduplicate still works. The optional interface must not become a silent
// requirement.
func TestCallServiceFallsBackForPlainCallers(t *testing.T) {
	caller := &plainCaller{}
	s := newTestExecSession()
	s.engine.caller = caller

	if _, err := s.callService(context.Background(), "svc", "op", "{}", 0); err != nil {
		t.Fatalf("callService: %v", err)
	}
	if caller.calls != 1 {
		t.Errorf("plain caller received %d calls, want 1", caller.calls)
	}
}

// TestIdempotencyKeyIsStableAcrossReplay is the property the mechanism actually
// depends on, and the one the unit tests above cannot establish.
//
// Repeating a call in one process with the same step argument proves only that
// a pure function is pure. What has to hold is that a *resumed* workflow derives
// the same key for a step as the original run did — otherwise a crash mid-call
// re-issues it under a new key and the service, honouring keys correctly,
// performs the work a second time.
//
// So: run place_order to completion and record the key for every call. Then
// truncate the history to the first two events, as a crash after two completed
// calls would leave it, and resume. The keys for the resumed steps must match
// the original run's exactly.
func TestIdempotencyKeyIsStableAcrossReplay(t *testing.T) {
	wasmPath := buildTestWasm(t)
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read WASM: %v", err)
	}

	ctx := context.Background()
	input := []byte(`{"userID":"user-1","cart":[{"sku":"ABC-123","quantity":1}]}`)

	run := func(history []EventRecord) (*orderCaller, []EventRecord) {
		rt, err := NewRuntime(ctx, 0, 0)
		if err != nil {
			t.Fatalf("NewRuntime: %v", err)
		}
		defer rt.Close(ctx)

		backend, err := NewWasmtimeBackend(ctx)
		if err != nil {
			t.Fatalf("wasmtime backend unavailable: %v (if this build disabled "+
				"CGO, that is the defect: it removes the primary backend)", err)
		}
		defer backend.Close(ctx)

		caller := &orderCaller{}
		eng := NewEngine(rt, caller,
			WithBackend("go", backend),
			WithWorkflowID("wf-replay"),
		)
		var out []EventRecord
		if history == nil {
			_, out, _, _, _, err = eng.Execute(ctx, wasmBytes, "place_order", input)
		} else {
			_, out, _, _, _, err = eng.Replay(ctx, wasmBytes, "place_order", input, history)
		}
		if err != nil {
			t.Fatalf("execute/replay: %v", err)
		}
		return caller, out
	}

	fresh, history := run(nil)
	if len(fresh.keys) < 3 {
		t.Fatalf("fresh run made %d keyed calls, need at least 3 to truncate "+
			"meaningfully", len(fresh.keys))
	}

	// A crash after the first two calls were recorded.
	const recorded = 2
	if len(history) < recorded {
		t.Fatalf("fresh run produced %d events, need at least %d", len(history), recorded)
	}
	resumed, _ := run(history[:recorded])

	// The resumed run re-issues every call the truncated history does not
	// cover. Those must carry the keys the original run used for the same steps.
	want := fresh.keys[recorded:]
	got := resumed.keys
	if len(got) != len(want) {
		t.Fatalf("resumed run made %d keyed calls, want %d (fresh: %v, resumed: %v)",
			len(got), len(want), fresh.keys, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("resumed call %d used key %q, the original run used %q — a "+
				"crash would re-issue this call under a new key and a "+
				"key-honouring service would perform the work twice",
				i, got[i], want[i])
		}
	}
}

// orderCaller records the idempotency key of every call and returns the
// canned responses testdata/basic expects.
type orderCaller struct {
	mu   sync.Mutex
	keys []string
}

func (c *orderCaller) Call(_ context.Context, service, operation, _ string) (string, error) {
	return mockResponse(service, operation), nil
}

func (c *orderCaller) CallWithIdempotencyKey(_ context.Context, service, operation, _, key string) (string, error) {
	c.mu.Lock()
	c.keys = append(c.keys, key)
	c.mu.Unlock()
	return mockResponse(service, operation), nil
}
