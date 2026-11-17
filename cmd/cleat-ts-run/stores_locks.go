// The `locks` store for cleat-ts-run (cleat#3827, group E): an in-memory engine.ConcurrencyKeyStore.
//
// WHY IT EXISTS. With NO ConcurrencyKeyStore the engine answers every acquire "not acquired" and every release "ok", and records
// the events as though a store had been asked (engine/locking.go: `acquired` stays false when the store is nil). A test of a
// workflow that takes a lock would therefore run to completion on its did-not-get-it branch and pass, having exercised nothing.
// Supplying any `-script locks=<file>` (an empty `{}` is enough) installs this store.
//
// WHAT IT MIRRORS, from the shipped stores: a key is held until its TTL passes or its holder releases it; an expired key is
// taken by the next acquire; acquire is NEVER re-entrant (a run asking for a key it already holds is refused, as on all three
// dialects, engine/concurrency_key_reentrancy_test.go); and release removes a key only for the run that holds it, so a run cannot
// free another's lock (cleat#1188). Expiry reads the wall clock, as the databases do.
//
// WHAT IT IS NOT. One process, one tenant, no database: it does not exercise a dialect's SQL, which is what the three-dialect test
// in tests/typescript is for.
//
// FIXTURE: the keys that other runs already hold when this run starts.
//
//	{"held": [{"key": "report-lock", "holder": "some-other-run", "ttlMs": 60000}]}
//
// `ttlMs` 0 or absent means the key never expires within the run. The run's own id is whatever -workflow-id says.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/cleat-team/cleat/engine"
)

func init() { registerStore("locks", loadLocksStore) }

type heldScript struct {
	Key    string `json:"key"`
	Holder string `json:"holder"`
	TTLMs  int64  `json:"ttlMs"`
}

type lockScript struct {
	Held []heldScript `json:"held"`
}

type lockEntry struct {
	holder  string
	expires time.Time // zero: never
}

// lockAcquire and lockRelease are the store's record of what the workflow asked, in order.
type lockAcquire struct {
	Key      string `json:"key"`
	TTLMs    int64  `json:"ttlMs"`
	Acquired bool   `json:"acquired"`
}

type lockRelease struct {
	Key      string `json:"key"`
	Released bool   `json:"released"`
}

type heldNow struct {
	Key    string `json:"key"`
	Holder string `json:"holder"`
}

type memoryLocks struct {
	mu       sync.Mutex
	now      func() time.Time
	held     map[string]lockEntry
	acquires []lockAcquire
	releases []lockRelease
}

func loadLocksStore(path string) (storeSpec, error) {
	m, err := newMemoryLocksFromFile(path)
	if err != nil {
		return storeSpec{}, err
	}
	return locksSpec(m), nil
}

func locksSpec(m *memoryLocks) storeSpec {
	return storeSpec{
		Option: engine.WithConcurrencyKeyStore(m),
		Extra:  func() (string, any) { return "locks", m.report() },
	}
}

func newMemoryLocksFromFile(path string) (*memoryLocks, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the locks script %s: %w", path, err)
	}
	var script lockScript
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields() // a misspelt "holder" would otherwise script a key nobody holds, and the run would pass
	if err := dec.Decode(&script); err != nil {
		return nil, fmt.Errorf("the locks script %s is not {\"held\": [{key, holder, ttlMs}]}: %w", path, err)
	}
	m := &memoryLocks{now: time.Now, held: map[string]lockEntry{}}
	for i, h := range script.Held {
		if h.Key == "" || h.Holder == "" {
			return nil, fmt.Errorf("the locks script %s: held[%d] needs both a key and a holder", path, i)
		}
		e := lockEntry{holder: h.Holder}
		if h.TTLMs > 0 {
			e.expires = m.now().Add(time.Duration(h.TTLMs) * time.Millisecond)
		}
		m.held[h.Key] = e
	}
	return m, nil
}

func (m *memoryLocks) live(key string) (lockEntry, bool) {
	e, ok := m.held[key]
	if ok && !e.expires.IsZero() && !m.now().Before(e.expires) {
		delete(m.held, key) // an expired key is released by the next acquire, as the databases do
		return lockEntry{}, false
	}
	return e, ok
}

func (m *memoryLocks) AcquireConcurrencyKey(_ context.Context, key, workflowID string, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, taken := m.live(key)
	if !taken {
		e := lockEntry{holder: workflowID}
		if ttl > 0 {
			e.expires = m.now().Add(ttl)
		}
		m.held[key] = e
	}
	m.acquires = append(m.acquires, lockAcquire{Key: key, TTLMs: ttl.Milliseconds(), Acquired: !taken})
	return !taken, nil
}

func (m *memoryLocks) ReleaseConcurrencyKey(_ context.Context, key, workflowID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.live(key)
	released := ok && e.holder == workflowID
	if released {
		delete(m.held, key)
	}
	m.releases = append(m.releases, lockRelease{Key: key, Released: released})
	return released, nil
}

// report is what the store saw, read after the run: every acquire and release in order, and the keys still held.
func (m *memoryLocks) report() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	still := []heldNow{}
	for k := range m.held {
		if e, ok := m.live(k); ok {
			still = append(still, heldNow{Key: k, Holder: e.holder})
		}
	}
	sort.Slice(still, func(i, j int) bool { return still[i].Key < still[j].Key })
	return map[string]any{
		"acquires": append([]lockAcquire{}, m.acquires...),
		"releases": append([]lockRelease{}, m.releases...),
		"held":     still,
	}
}
