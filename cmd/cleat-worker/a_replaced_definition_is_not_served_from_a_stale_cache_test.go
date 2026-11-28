package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// cleat#3516: Worker.loadWASM kept serving a definition's old bytes after they
// were replaced in the store, which is what cleat#3497's `--force` does on
// purpose.
//
// Measured before the fix, with a stub store whose bytes change between two
// loads:
//
//	memory only, different length   new bytes   (the control)
//	memory only, same length        OLD bytes
//	memory + disk, different length OLD bytes
//	memory + disk, same length      OLD bytes
//
// The control is the row that shows the harness can see a refresh, so the other
// three are not an artefact of it.

// swappableStore holds one definition whose bytes the test replaces.
type swappableStore struct {
	*mockStore
	mu          sync.Mutex
	bytes       []byte
	loadCalls   int
	digestCalls int
}

func newSwappableStore(initial []byte) *swappableStore {
	s := &swappableStore{mockStore: &mockStore{}, bytes: initial}
	s.mockStore.loadWASMFn = func(context.Context, string, int) ([]byte, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.loadCalls++
		return append([]byte(nil), s.bytes...), nil
	}
	s.mockStore.getWASMLengthFn = func(context.Context, string, int) (int64, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		return int64(len(s.bytes)), nil
	}
	return s
}

func (s *swappableStore) replace(b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bytes = b
}

func (s *swappableStore) counts() (loads, digests int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadCalls, s.digestCalls
}

// digestingStore is the same store as one that can hash in the database, which
// is what every real dialect is. swappableStore alone is the store that cannot,
// so each case runs against both.
type digestingStore struct{ *swappableStore }

func (d digestingStore) GetWASMDigest(context.Context, string, int) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.digestCalls++
	return engine.WASMDigest(d.bytes), nil
}

func revalidateEveryCall(t *testing.T) {
	t.Helper()
	old := wasmRevalidateInterval
	wasmRevalidateInterval = 0
	t.Cleanup(func() { wasmRevalidateInterval = old })
}

func TestAReplacedDefinitionIsNotServedFromAStaleCache(t *testing.T) {
	revalidateEveryCall(t)

	oldB := []byte("the module as first deployed")
	sameLen := []byte("the module as hot-fixed now!") // same length as oldB
	diffLen := []byte("the module as hot-fixed, and longer")
	if len(sameLen) != len(oldB) || len(diffLen) == len(oldB) {
		t.Fatalf("fixture is wrong: lengths %d, %d, %d", len(oldB), len(sameLen), len(diffLen))
	}

	for _, withDisk := range []bool{false, true} {
		for _, digester := range []bool{true, false} {
			for _, c := range []struct {
				name string
				next []byte
			}{{"same length", sameLen}, {"different length", diffLen}} {
				name := fmt.Sprintf("disk=%v/store hashes=%v/%s", withDisk, digester, c.name)
				t.Run(name, func(t *testing.T) {
					ss := newSwappableStore(oldB)
					var store engine.WorkflowStore = ss.mockStore
					if digester {
						store = digestingStore{ss}
					}
					w := newTestWorker(ss.mockStore)
					defer w.cancel()
					if withDisk {
						w.wasmDiskCache = engine.NewWasmDiskCache(t.TempDir(), 100)
					}

					got, err := w.loadWASM(store, "t", "orders", 1)
					if err != nil || string(got) != string(oldB) {
						t.Fatalf("first load = %q, %v; want %q", got, err, oldB)
					}

					// The control: nothing replaced, so the second load must be
					// served from the cache. Without it, a loadWASM that reloads
					// every time passes everything below.
					loadsBefore, _ := ss.counts()
					if got, _ = w.loadWASM(store, "t", "orders", 1); string(got) != string(oldB) {
						t.Fatalf("unchanged second load = %q, want %q", got, oldB)
					}
					if loadsAfter, _ := ss.counts(); digester && loadsAfter != loadsBefore {
						t.Fatalf("an unchanged definition was read from the store again (%d -> %d loads), so the cache is not doing its job", loadsBefore, loadsAfter)
					}

					ss.replace(c.next)
					got, err = w.loadWASM(store, "t", "orders", 1)
					if err != nil {
						t.Fatalf("load after replacement: %v", err)
					}
					if string(got) != string(c.next) {
						t.Errorf("after the store's bytes were replaced, loadWASM returned %q, want %q: a replacement never reached the worker", got, c.next)
					}
				})
			}
		}
	}
}

// A worker that restarts keeps its disk cache and loses its memory. This is the
// path where the index, not the memory entry, is what is stale.
func TestAReplacementOutlivesARestartThroughTheDiskCache(t *testing.T) {
	revalidateEveryCall(t)
	dir := t.TempDir()

	oldB := []byte("the module as first deployed")
	newB := []byte("the module as hot-fixed now!") // same length

	ss := newSwappableStore(oldB)
	w1 := newTestWorker(ss.mockStore)
	w1.wasmDiskCache = engine.NewWasmDiskCache(dir, 100)
	if _, err := w1.loadWASM(digestingStore{ss}, "t", "orders", 1); err != nil {
		t.Fatal(err)
	}
	w1.cancel()

	ss.replace(newB)

	// Same directory, empty memory.
	w2 := newTestWorker(ss.mockStore)
	defer w2.cancel()
	w2.wasmDiskCache = engine.NewWasmDiskCache(dir, 100)
	got, err := w2.loadWASM(digestingStore{ss}, "t", "orders", 1)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(newB) {
		t.Fatalf("a restarted worker served %q from its disk cache, want the replacement %q", got, newB)
	}

	// And the index was repointed, so a third process does not have to be told again.
	if got := w2.wasmDiskCache.LookupDef("t", "orders", 1); string(got) != string(newB) {
		t.Errorf("the disk index still names the old bytes: %q", got)
	}
}

// The digest is a read of the whole row, so it is asked for once per interval,
// not once per task. This is also the honest statement of the bound: inside the
// interval a same-length replacement is not yet seen.
func TestTheContentCheckIsAskedOncePerInterval(t *testing.T) {
	old := wasmRevalidateInterval
	wasmRevalidateInterval = time.Hour
	t.Cleanup(func() { wasmRevalidateInterval = old })

	oldB := []byte("the module as first deployed")
	newB := []byte("the module as hot-fixed now!") // same length
	ss := newSwappableStore(oldB)
	store := digestingStore{ss}
	w := newTestWorker(ss.mockStore)
	defer w.cancel()

	for i := 0; i < 5; i++ {
		if _, err := w.loadWASM(store, "t", "orders", 1); err != nil {
			t.Fatal(err)
		}
	}
	if _, digests := ss.counts(); digests != 0 {
		t.Errorf("the store was asked to hash %d times inside the interval; want 0 (the entry was verified when it was loaded)", digests)
	}

	ss.replace(newB)
	got, _ := w.loadWASM(store, "t", "orders", 1)
	if string(got) != string(oldB) {
		t.Fatalf("inside the interval a same-length replacement was already served (%q); this test pins the documented bound, so either the bound moved or the check ran", got)
	}

	// Let the interval lapse: now it is seen, with one hash.
	wasmRevalidateInterval = 0
	got, _ = w.loadWASM(store, "t", "orders", 1)
	if string(got) != string(newB) {
		t.Errorf("after the interval the replacement was not served: %q", got)
	}
	if _, digests := ss.counts(); digests != 1 {
		t.Errorf("hashed %d times to notice one replacement, want 1", digests)
	}
}
