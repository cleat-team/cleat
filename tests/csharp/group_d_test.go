//go:build cgo

package csharp

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// Group D of cleat#4080's host-call coverage: durable-acquire-lock, durable-release-lock, set-scope, get-scope and uuid, each driven through
// the real engine and wasmtime backend by a named entry of the conformance component (packages/cleat-csharp/conformance/GroupD.cs).
// Nothing here claims C# is supported.

// fakeKeyStore is the concurrency-key store the lock and scope calls need: without one the host reports every acquire as "not acquired".
// It records what it was asked, so a test can tell a store call from a replayed answer.
type fakeKeyStore struct {
	mu       sync.Mutex
	holder   map[string]string
	ttl      map[string]time.Duration
	acquires []string // "key by holder"
	releases []string
	failWith error
}

func newFakeKeyStore() *fakeKeyStore {
	return &fakeKeyStore{holder: map[string]string{}, ttl: map[string]time.Duration{}}
}

func (f *fakeKeyStore) AcquireConcurrencyKey(_ context.Context, key, workflowID string, ttl time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquires = append(f.acquires, key+" by "+workflowID)
	if f.failWith != nil {
		return false, f.failWith
	}
	if h, held := f.holder[key]; held && h != workflowID {
		return false, nil
	}
	f.holder[key], f.ttl[key] = workflowID, ttl
	return true, nil
}

func (f *fakeKeyStore) ReleaseConcurrencyKey(_ context.Context, key, workflowID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releases = append(f.releases, key+" by "+workflowID)
	if f.failWith != nil {
		return false, f.failWith
	}
	if f.holder[key] != workflowID {
		return false, nil
	}
	delete(f.holder, key)
	return true, nil
}

func (f *fakeKeyStore) heldBy(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.holder[key]
}

func withStore(f *fakeKeyStore) []engine.EngineOption {
	return []engine.EngineOption{engine.WithConcurrencyKeyStore(f)}
}

func TestCSharpGroupDAcquireTakesAFreeLockAndPassesTheTTL(t *testing.T) {
	st := newFakeKeyStore()
	res := runConformanceEntry(t, "d_acquire", entryRun{WorkflowID: "wf-d-1", Input: `{"key":"orders","ttlMs":90000}`, Options: withStore(st)})
	if mustFinish(t, res)["acquired"] != true {
		t.Fatal("a free lock was not reported as acquired")
	}
	if got := st.heldBy("orders"); got != "wf-d-1" {
		t.Errorf("the store says %q holds the lock, want wf-d-1", got)
	}
	if got := st.ttl["orders"]; got != 90*time.Second {
		t.Errorf("the store was given a TTL of %v, want 90s", got)
	}
}

// The CONTROL for the "not acquired" answer: the same call, with the lock held by someone else, must say false, and must not fail.
func TestCSharpGroupDAcquireReportsFalseWhenAnotherWorkflowHoldsTheLock(t *testing.T) {
	st := newFakeKeyStore()
	st.holder["orders"] = "someone-else"
	res := runConformanceEntry(t, "d_acquire", entryRun{WorkflowID: "wf-d-2", Input: `{"key":"orders","ttlMs":1000}`, Options: withStore(st)})
	m := mustFinish(t, res)
	if m["acquired"] != false || m["failed"] != nil {
		t.Errorf("got %v, want acquired=false and no failure: a held lock is an ordinary answer", m)
	}
	if got := st.heldBy("orders"); got != "someone-else" {
		t.Errorf("the holder changed to %q", got)
	}
}

func TestCSharpGroupDAcquireAStoreFailureIsAnErrorNotAFalse(t *testing.T) {
	st := newFakeKeyStore()
	st.failWith = errors.New("the lock store is down")
	m := mustFinish(t, runConformanceEntry(t, "d_acquire", entryRun{Input: `{"key":"orders","ttlMs":1000}`, Options: withStore(st)}))
	if m["failed"] != true || m["type"] != "CleatCallException" || m["code"] != float64(0) {
		t.Errorf("got %v, want the BASE CleatCallException with code 0 (the host's status 1 is not a call-error code)", m)
	}
	if !strings.Contains(fmt.Sprint(m["message"]), "host status code 1") {
		t.Errorf("message = %q, want the host status named", m["message"])
	}
}

// A lock that expires as it is taken excludes nobody and would still report true, so the SDK refuses it. A positive TTL under a
// millisecond is rounded up rather than to zero.
func TestCSharpGroupDAcquireRefusesATTLThatCouldNotHoldAndRoundsASubMillisecondUp(t *testing.T) {
	for _, ttl := range []string{"0", "-5"} {
		st := newFakeKeyStore()
		m := mustFinish(t, runConformanceEntry(t, "d_acquire", entryRun{Input: `{"key":"k","ttlMs":` + ttl + `}`, Options: withStore(st)}))
		if m["invalid"] != "ttl" {
			t.Errorf("ttl %s ms: got %v, want the SDK to refuse it", ttl, m)
		}
		if len(st.acquires) != 0 {
			t.Errorf("ttl %s ms reached the store: %v", ttl, st.acquires)
		}
	}
	st := newFakeKeyStore()
	mustFinish(t, runConformanceEntry(t, "d_acquire", entryRun{Input: `{"key":"k","ttlMs":0.2}`, Options: withStore(st)}))
	if got := st.ttl["k"]; got != time.Millisecond {
		t.Errorf("a 0.2 ms TTL reached the store as %v, want 1ms (rounded UP, never to zero)", got)
	}
}

func TestCSharpGroupDAcquireThenReleaseFreesTheLock(t *testing.T) {
	st := newFakeKeyStore()
	res := runConformanceEntry(t, "d_acquire_release", entryRun{WorkflowID: "wf-d-3", Input: `{"key":"orders"}`, Options: withStore(st)})
	if m := mustFinish(t, res); m["acquired"] != true || m["released"] != true {
		t.Fatalf("got %v", m)
	}
	if got := st.heldBy("orders"); got != "" {
		t.Errorf("the lock is still held by %q after ReleaseLock", got)
	}
	if len(st.releases) != 1 || st.releases[0] != "orders by wf-d-3" {
		t.Errorf("releases = %v, want one release of orders by the workflow itself (not a bare key)", st.releases)
	}
}

// Releasing a lock someone else holds is a success that frees nothing: the guest cannot act on it, and the host must not take the lock from
// its holder.
func TestCSharpGroupDReleaseOfALockNotHeldSucceedsAndLeavesTheHolder(t *testing.T) {
	st := newFakeKeyStore()
	st.holder["orders"] = "someone-else"
	m := mustFinish(t, runConformanceEntry(t, "d_release", entryRun{Input: `{"key":"orders"}`, Options: withStore(st)}))
	if m["released"] != true || m["failed"] != nil {
		t.Errorf("got %v, want a quiet success", m)
	}
	if got := st.heldBy("orders"); got != "someone-else" {
		t.Errorf("the holder is now %q, want someone-else to keep it", got)
	}
}

func TestCSharpGroupDReleaseAStoreFailureIsAnError(t *testing.T) {
	st := newFakeKeyStore()
	st.failWith = errors.New("the lock store is down")
	m := mustFinish(t, runConformanceEntry(t, "d_release", entryRun{Input: `{"key":"orders"}`, Options: withStore(st)}))
	if m["failed"] != true || m["type"] != "CleatCallException" {
		t.Errorf("got %v, want a CleatCallException", m)
	}
}

// Replay returns the RECORDED answer and does not ask the store: the store is made to deny every acquire for the replay, and the answer must
// still be true. Without this, a replay could silently flip a workflow onto its "did not get the lock" branch.
func TestCSharpGroupDAcquireReplaysTheRecordedAnswerWithoutAskingTheStore(t *testing.T) {
	fresh := runConformanceEntry(t, "d_acquire", entryRun{WorkflowID: "wf-d-4", Input: `{"key":"orders","ttlMs":1000}`, Options: withStore(newFakeKeyStore())})
	if mustFinish(t, fresh)["acquired"] != true {
		t.Fatal("the fresh run did not acquire")
	}
	denying := newFakeKeyStore()
	denying.holder["orders"] = "someone-else"
	replay := runConformanceEntry(t, "d_acquire", entryRun{WorkflowID: "wf-d-4", Input: `{"key":"orders","ttlMs":1000}`, Replay: fresh.History, Options: withStore(denying)})
	if mustFinish(t, replay)["acquired"] != true {
		t.Error("the replay answered false: it asked the store instead of reading the recorded answer")
	}
	if len(denying.acquires) != 0 {
		t.Errorf("the replay asked the store %v, want it to ask nothing", denying.acquires)
	}
}

func TestCSharpGroupDScopeWalkSetsSwitchesAndClears(t *testing.T) {
	st := newFakeKeyStore()
	in := `{"first":{"type":"order","key":"o-1"},"second":{"type":"cart","key":"c-9"}}`
	m := mustFinish(t, runConformanceEntry(t, "d_scope_walk", entryRun{WorkflowID: "wf-d-5", Input: in, Options: withStore(st)}))
	empty := map[string]any{"type": "", "key": ""}
	want := map[string]any{
		"initial":     empty,
		"prevOnFirst": "",
		"afterFirst":  map[string]any{"type": "order", "key": "o-1"},
		// The host's previous-scope text is its own prefix spelling, not JSON (the WIT comment says JSON; it is wrong).
		"prevOnSecond": "vo:order:o-1:",
		"afterSecond":  map[string]any{"type": "cart", "key": "c-9"},
		"prevOnClear":  "vo:cart:c-9:",
		"afterClear":   empty,
	}
	for k, w := range want {
		if fmt.Sprint(m[k]) != fmt.Sprint(w) {
			t.Errorf("%s = %v, want %v", k, m[k], w)
		}
	}
	// Switching released the first key, and clearing released the second: nothing is left held.
	if h1, h2 := st.heldBy("vo:order:o-1"), st.heldBy("vo:cart:c-9"); h1 != "" || h2 != "" {
		t.Errorf("after the walk the store still shows holders %q and %q", h1, h2)
	}
	if len(st.releases) != 2 {
		t.Errorf("releases = %v, want exactly the switch and the clear", st.releases)
	}
}

func TestCSharpGroupDSetScopeTakesTheKeyForTheWorkflow(t *testing.T) {
	st := newFakeKeyStore()
	// A scope set and never cleared is released when the workflow ends, so the holder is checked from the store's own record of the call.
	runConformanceEntry(t, "d_scope_then_call", entryRun{WorkflowID: "wf-d-6", Input: `{"type":"order","key":"o-1"}`, Options: withStore(st)})
	if len(st.acquires) != 1 || st.acquires[0] != "vo:order:o-1 by wf-d-6" {
		t.Errorf("acquires = %v, want the scope key taken once by the workflow", st.acquires)
	}
}

// A busy virtual object suspends the workflow, with the reason that names the scope. SetScope itself returns normally and the guest is NOT
// stopped: the call after it reaches the service. That second half is the open hazard cleat#3916 (measured here, 2026-11-19, on the C# SDK as
// on the others), and it is deliberately not asserted either way: a test pinned to it would break the day it is fixed, and one asserting the
// fix would fail today. The suspension is the half that is true now and must stay true.
func TestCSharpGroupDSetScopeOnABusyObjectSuspendsTheRun(t *testing.T) {
	st := newFakeKeyStore()
	st.holder["vo:order:o-1"] = "someone-else"
	res := runConformanceEntry(t, "d_scope_then_call", entryRun{WorkflowID: "wf-d-7", Input: `{"type":"order","key":"o-1"}`, Options: withStore(st)})
	if res.Err != nil {
		t.Fatalf("the run failed: %v", res.Err)
	}
	if res.Suspended == nil || !strings.Contains(res.Suspended.Reason, "vo:order:o-1 held by another workflow") {
		t.Fatalf("suspended = %+v result = %q, want a suspension naming the scope key and saying it is held by another workflow", res.Suspended, res.Result)
	}
	if got := st.heldBy("vo:order:o-1"); got != "someone-else" {
		t.Errorf("the scope key's holder is now %q, want someone-else to keep it", got)
	}
}

func TestCSharpGroupDHalfEmptyScopeIsRefusedBeforeTheHost(t *testing.T) {
	for _, in := range []string{`{"type":"order","key":""}`, `{"type":"","key":"o-1"}`} {
		st := newFakeKeyStore()
		m := mustFinish(t, runConformanceEntry(t, "d_scope_partial", entryRun{Input: in, Options: withStore(st)}))
		if m["refused"] != true {
			t.Errorf("%s: got %v, want the SDK to refuse it", in, m)
		}
		if len(st.acquires) != 0 {
			t.Errorf("%s reached the store: %v", in, st.acquires)
		}
	}
}

var uuidV5Shape = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// wantUUID is the engine's documented derivation, written down independently: the first 16 bytes of SHA-256("<workflow id>:<seed>") with
// the version and variant bits set. Ids are visible to users, so the derivation is a compatibility fact worth pinning.
func wantUUID(wfID, seed string) string {
	h := sha256.Sum256([]byte(wfID + ":" + seed))
	h[6] = (h[6] & 0x0f) | 0x50
	h[8] = (h[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

func TestCSharpGroupDUuidIsDeterministicPerSeedAndPerWorkflow(t *testing.T) {
	in := `{"seeds":["order","payment"]}`
	a := mustFinish(t, runConformanceEntry(t, "d_uuid", entryRun{WorkflowID: "wf-u-1", Input: in}))
	ids := a["ids"].([]any)
	if len(ids) != 2 {
		t.Fatalf("ids = %v", ids)
	}
	for i, seed := range []string{"order", "payment"} {
		if got := ids[i].(string); got != wantUUID("wf-u-1", seed) {
			t.Errorf("uuid(%q) = %s, want %s", seed, got, wantUUID("wf-u-1", seed))
		}
		if !uuidV5Shape.MatchString(ids[i].(string)) {
			t.Errorf("uuid(%q) = %s is not lower-case 8-4-4-4-12 with version 5 and variant 10", seed, ids[i])
		}
	}
	if ids[0] == ids[1] {
		t.Error("two different seeds gave one id")
	}
	if a["firstAgain"] != ids[0] {
		t.Errorf("the same seed asked again gave %v, want %v", a["firstAgain"], ids[0])
	}
	// A second run of the SAME workflow id is the replay case: nothing is recorded, so it must come out identical.
	b := mustFinish(t, runConformanceEntry(t, "d_uuid", entryRun{WorkflowID: "wf-u-1", Input: in}))
	if fmt.Sprint(b["ids"]) != fmt.Sprint(a["ids"]) {
		t.Errorf("a second run gave %v, want %v", b["ids"], a["ids"])
	}
	// A different workflow gets different ids for the same seeds.
	c := mustFinish(t, runConformanceEntry(t, "d_uuid", entryRun{WorkflowID: "wf-u-2", Input: in}))
	if c["ids"].([]any)[0] == ids[0] {
		t.Error("two workflows got the same id for one seed")
	}
}
