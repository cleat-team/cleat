package typescript

// The lock bindings (cleat#3827, group E): acquireLock and releaseLock. Two layers, because they answer different questions:
//
//   - in-process, against a ConcurrencyKeyStore the test controls: what the GUEST sees and what the host RECORDS (the answer,
//     the events, the replay), including every case a store cannot easily be made to produce;
//   - on a real worker against each dialect's real store (TestTypeScriptLocksOnARealWorker): that the answers a guest gets are
//     the ones the databases give, which no in-memory store can say.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

type memLocksE2E struct {
	mu       sync.Mutex
	held     map[string]string // key -> holder
	acquires []string          // "key:ttlMs:true|false"
	releases []string
}

func newMemLocksE2E(held map[string]string) *memLocksE2E {
	m := &memLocksE2E{held: map[string]string{}}
	for k, v := range held {
		m.held[k] = v
	}
	return m
}

func (m *memLocksE2E) AcquireConcurrencyKey(_ context.Context, key, wf string, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, taken := m.held[key]
	if !taken {
		m.held[key] = wf
	}
	m.acquires = append(m.acquires, key+":"+ttl.String()+":"+boolWord(!taken))
	return !taken, nil
}

func (m *memLocksE2E) ReleaseConcurrencyKey(_ context.Context, key, wf string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ok := m.held[key] == wf
	if ok {
		delete(m.held, key)
	}
	m.releases = append(m.releases, key+":"+boolWord(ok))
	return ok, nil
}

func boolWord(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

const lockEntries = "locks_round_trip,locks_holder,locks_contender,locks_expiry,locks_bad_ttl,locks_late"

func buildLocks(t *testing.T) []byte {
	t.Helper()
	root := needToolchain(t)
	src := "tests/typescript/testdata/workflows/locks.ts"
	res := build(t, root, src, lockEntries)
	if res.exit != 0 {
		t.Fatalf("building %s failed (exit %d):\n%s", src, res.exit, res.output)
	}
	wasm, err := os.ReadFile(res.module)
	if err != nil {
		t.Fatal(err)
	}
	return wasm
}

func lockEvents(h []engine.EventRecord) (acquires, releases int) {
	for _, ev := range h {
		switch ev.EventType {
		case engine.EventTypeAcquireLock:
			acquires++
		case engine.EventTypeReleaseLock:
			releases++
		}
	}
	return
}

func TestTypeScriptAcquireLockAnswersAcquiredAndNotAcquiredWithoutThrowing(t *testing.T) {
	wasm := buildLocks(t)
	store := newMemLocksE2E(map[string]string{"theirs": "another-run"})
	e, closeRT := newEngineWith(t, engine.WithConcurrencyKeyStore(store), engine.WithWorkflowID("run-self"))
	defer closeRT()
	raw, history, susp, _, _, err := e.Execute(context.Background(), wasm, "locks_round_trip", json.RawMessage(`{"key":"mine","other":"theirs"}`))
	if err != nil || susp != nil {
		t.Fatalf("susp=%v err=%v", susp, err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"first": true, "again": false, "afterRelease": true, "heldElsewhere": false, "stillHeldElsewhere": true}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %v, want %v (result %s): first acquire true, a second is refused (never re-entrant), release makes it free again, a key held elsewhere is false and releasing it frees nothing", k, got[k], w, raw)
		}
	}
	// What the STORE saw, in order, and the events the host recorded: a result alone cannot tell a binding that asked the store
	// from one that invented the answers.
	if len(store.acquires) != 5 || !strings.HasPrefix(store.acquires[0], "mine:1m0s:true") {
		t.Errorf("the store saw acquires %v, want five, the first being mine with a one-minute TTL (60000 ms crossed as a Duration)", store.acquires)
	}
	if got := strings.Join(store.releases, ","); got != "mine:true,theirs:false" {
		t.Errorf("the store saw releases %q, want mine:true,theirs:false (the second is a key this run does not hold)", got)
	}
	if a, r := lockEvents(history); a != 5 || r != 2 {
		t.Errorf("history has %d acquire and %d release events, want 5 and 2", a, r)
	}
}

func TestTypeScriptALockIsNotAskedForAgainOnReplay(t *testing.T) {
	wasm := buildLocks(t)
	store := newMemLocksE2E(nil)
	e, closeRT := newEngineWith(t, engine.WithConcurrencyKeyStore(store), engine.WithWorkflowID("run-self"))
	defer closeRT()
	input := json.RawMessage(`{"key":"mine"}`)
	raw, history, _, _, _, err := e.Execute(context.Background(), wasm, "locks_round_trip", input)
	if err != nil {
		t.Fatal(err)
	}
	before := len(store.acquires) + len(store.releases)
	// The store changes between the runs: another run now holds the key. A replay that asked the store again would be refused.
	store.held["mine"] = "an-interloper"
	raw2, _, _, _, _, err := e.Replay(context.Background(), wasm, "locks_round_trip", input, history)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if after := len(store.acquires) + len(store.releases); after != before {
		t.Errorf("replay asked the store %d more time(s); a recorded answer is returned and the store is not consulted", after-before)
	}
	if raw2 != raw {
		t.Errorf("replay answered %s, the first run %s: the recorded answers must be returned as they were", raw2, raw)
	}
}

func TestTypeScriptATtlBelowOneMillisecondIsAnErrorNotALockBornExpired(t *testing.T) {
	wasm := buildLocks(t)
	store := newMemLocksE2E(nil)
	e, closeRT := newEngineWith(t, engine.WithConcurrencyKeyStore(store))
	defer closeRT()
	for _, ttl := range []string{"0", "0.5", "-5"} {
		raw, history, _, _, _, err := e.Execute(context.Background(), wasm, "locks_bad_ttl", json.RawMessage(`{"ttlMs":`+ttl+`}`))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(raw, "ttlMs must be at least 1 millisecond") {
			t.Errorf("ttlMs=%s: result %s, want the refusal naming ttlMs", ttl, raw)
		}
		if len(store.acquires) != 0 {
			t.Fatalf("ttlMs=%s reached the store (%v): a TTL that truncates to zero makes a lock born expired", ttl, store.acquires)
		}
		if a, _ := lockEvents(history); a != 0 {
			t.Errorf("ttlMs=%s recorded %d acquire event(s) for a call the host never saw", ttl, a)
		}
	}
}

// What `guarded` buys on both calls: nothing of any type is recorded after the call that suspended the segment, and the store
// is never asked. Without the guard a lock would be taken for a run that is parked and will never reach its release.
func TestTypeScriptALockCallAfterASuspensionRecordsNothing(t *testing.T) {
	wasm := buildLocks(t)
	store := newMemLocksE2E(nil)
	e, closeRT := newEngineWith(t, engine.WithConcurrencyKeyStore(store))
	defer closeRT()
	_, history, susp, _, _, err := e.Execute(context.Background(), wasm, "locks_late", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if susp == nil || !strings.Contains(susp.Reason, "cleat_sleep(60000ms)") {
		t.Fatalf("the workflow did not suspend at the sleep (%v); the ordering below is then unmeasured", susp)
	}
	if len(history) != 0 {
		var types []string
		for _, ev := range history {
			types = append(types, string(ev.EventType))
		}
		t.Fatalf("the segment recorded %v AFTER the call that suspended it; acquire_lock and release_lock must be guarded", types)
	}
	if len(store.acquires)+len(store.releases) != 0 {
		t.Fatalf("a call after the suspension reached the store: acquires %v releases %v", store.acquires, store.releases)
	}
}
