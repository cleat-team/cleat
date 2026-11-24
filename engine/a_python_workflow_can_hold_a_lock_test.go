//go:build cgo

package engine

// cleat#4081: a Python workflow could not take a lock.
//
// acquire_lock, acquire_lock_ms and release_lock called the component binding
// with a core-module argument list -- (scratch pointer, key length[, ttl]) --
// where the binding takes (key[, ttl_ms]). On a component build all three
// raised TypeError before anything reached the host.
//
// WHAT THIS ASSERTS, and why it is not just the workflow's return value. The
// store is a fake that records what the ENGINE was asked: which key, which
// holder, and what TTL. A workflow that reported True from a binding that
// never reached the host would satisfy a result-only assertion, and "30
// seconds became 30000 ms" is a claim about the conversion that only the
// store can see.

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

type heldKeysStore struct {
	mu       sync.Mutex
	held     map[string]string // key -> holder
	acquired []lockAsk
	released []string
}

type lockAsk struct {
	key, holder string
	ttl         time.Duration
}

func (s *heldKeysStore) AcquireConcurrencyKey(_ context.Context, key, workflowID string, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acquired = append(s.acquired, lockAsk{key, workflowID, ttl})
	if _, taken := s.held[key]; taken {
		return false, nil
	}
	if s.held == nil {
		s.held = map[string]string{}
	}
	s.held[key] = workflowID
	return true, nil
}

func (s *heldKeysStore) ReleaseConcurrencyKey(_ context.Context, key, workflowID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.released = append(s.released, key)
	if s.held[key] != workflowID {
		return false, nil
	}
	delete(s.held, key)
	return true, nil
}

func TestAPythonWorkflowCanHoldAndReleaseALock(t *testing.T) {
	store := &heldKeysStore{}
	res, susp, _, err := pythonExecuteFixture(t, "wf-python-locks", "locks_workflow.py", "locks",
		WithConcurrencyKeyStore(store))
	if err != nil {
		t.Fatalf("the Python lock workflow failed: %v\n\n"+
			"cleat#4081 had two causes, and either one fails this test. A TypeError naming "+
			"durable_acquire_lock means the SDK passed core-module arguments (a scratch pointer "+
			"and a length) to the component binding. A trap saying `expected s64, found u64` "+
			"means the engine set a u64 result on an import the WIT declares `-> s64` "+
			"(setResultS64 in component_cgo.go).", err)
	}
	if susp != nil {
		t.Fatalf("the Python lock workflow suspended (%v); nothing in it should", susp.Reason)
	}

	var got map[string]bool
	if err := json.Unmarshal([]byte(res), &got); err != nil {
		t.Fatalf("result %q is not an object of booleans: %v", res, err)
	}
	want := map[string]bool{"first": true, "second": false, "ms": true, "third": true}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: the workflow reported %v, want %v (result %s)", k, got[k], w, res)
		}
	}

	// What the engine was asked. The TTLs are the point: 30 seconds must reach
	// the store as 30000 ms, and acquire_lock_ms's 45000 must arrive untouched.
	wantAsks := []lockAsk{
		{"k", "wf-python-locks", 30 * time.Second},
		{"k", "wf-python-locks", 30 * time.Second},
		{"k-ms", "wf-python-locks", 45 * time.Second},
		{"k", "wf-python-locks", 30 * time.Second},
	}
	if len(store.acquired) != len(wantAsks) {
		t.Fatalf("the store was asked to acquire %d times, want %d: %+v", len(store.acquired), len(wantAsks), store.acquired)
	}
	for i, w := range wantAsks {
		if store.acquired[i] != w {
			t.Errorf("acquire #%d reached the store as %+v, want %+v", i+1, store.acquired[i], w)
		}
	}
	if len(store.released) != 1 || store.released[0] != "k" {
		t.Errorf("release_lock('k') reached the store as %v, want exactly [k]", store.released)
	}
}
