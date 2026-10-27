package engine

import (
	"sync"
	"sync/atomic"
	"testing"
)

func mustRing(t *testing.T, current VersionedKey, previous ...VersionedKey) *KeyRing {
	t.Helper()
	r, err := NewKeyRing(current, previous...)
	if err != nil {
		t.Fatalf("NewKeyRing: %v", err)
	}
	return r
}

func key32(b byte) []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = b
	}
	return k
}

func TestReloadableKeyRingLoadReturnsTheInitialRing(t *testing.T) {
	ring := mustRing(t, VersionedKey{Version: 1, Key: key32(0x01)})
	r := NewReloadableKeyRing(ring)
	if got := r.Load(); got != ring {
		t.Errorf("Load() = %p, want the initial ring %p", got, ring)
	}
}

func TestReloadableKeyRingLoadOnNilRingIsNilSafe(t *testing.T) {
	r := NewReloadableKeyRing(nil)
	if got := r.Load(); got != nil {
		t.Errorf("Load() on a nil-wrapped ring = %v, want nil", got)
	}
	var nilR *ReloadableKeyRing
	if got := nilR.Load(); got != nil {
		t.Errorf("Load() on a nil *ReloadableKeyRing = %v, want nil", got)
	}
}

// TestReloadSwapsToTheNewRing is the literal acceptance criterion at the
// engine level: after Reload, Load returns the NEW ring, and a value sealed
// under the old current key still opens (it is now the previous key).
func TestReloadSwapsToTheNewRing(t *testing.T) {
	keyA := key32(0xAA)
	keyB := key32(0xBB)
	oldRing := mustRing(t, VersionedKey{Version: 1, Key: keyA})
	newRing := mustRing(t, VersionedKey{Version: 2, Key: keyB}, VersionedKey{Version: 1, Key: keyA})

	r := NewReloadableKeyRing(oldRing)
	if got := r.Load().Current().Version; got != 1 {
		t.Fatalf("before Reload: current version = %d, want 1", got)
	}

	result, err := r.Reload(newRing)
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if len(result.Dropped) != 0 {
		t.Errorf("Dropped = %v, want none (version 1 carried forward as previous)", result.Dropped)
	}

	got := r.Load()
	if got != newRing {
		t.Fatalf("Load() after Reload = %p, want the new ring %p", got, newRing)
	}
	if got.Current().Version != 2 {
		t.Errorf("current version after Reload = %d, want 2", got.Current().Version)
	}
	if _, ok := got.Key(1); !ok {
		t.Errorf("version 1 (the old current key) is not openable after Reload")
	}
}

// TestReloadRefusesAReusedVersionWithDifferentBytes is M3: the one case
// Reload must refuse rather than merely log.
func TestReloadRefusesAReusedVersionWithDifferentBytes(t *testing.T) {
	keyA := key32(0xAA)
	keyC := key32(0xCC)
	oldRing := mustRing(t, VersionedKey{Version: 2, Key: keyA}, VersionedKey{Version: 1, Key: key32(0x11)})
	// A slipped flag: a brand-new key written to the same version number
	// (2) without demoting the old one first.
	badRing := mustRing(t, VersionedKey{Version: 2, Key: keyC}, VersionedKey{Version: 1, Key: key32(0x11)})

	r := NewReloadableKeyRing(oldRing)
	_, err := r.Reload(badRing)
	if err == nil {
		t.Fatal("Reload: want a refusal for a reused version with different bytes, got nil error")
	}

	// THE PROPERTY A REFUSAL EXISTS TO GUARANTEE: the live ring is UNCHANGED.
	if got := r.Load(); got != oldRing {
		t.Errorf("Load() after a refused Reload = %p, want the untouched old ring %p", got, oldRing)
	}
}

// TestReloadAllowsTheSameVersionWithTheSameBytes is the negative control for
// the test above: re-submitting an IDENTICAL ring (same version, same key
// bytes) is not a reuse and must succeed, because a reload whose candidate
// happens to match what is already live is not an operator mistake.
func TestReloadAllowsTheSameVersionWithTheSameBytes(t *testing.T) {
	keyA := key32(0xAA)
	ring1 := mustRing(t, VersionedKey{Version: 1, Key: keyA})
	ring2 := mustRing(t, VersionedKey{Version: 1, Key: append([]byte(nil), keyA...)})

	r := NewReloadableKeyRing(ring1)
	if _, err := r.Reload(ring2); err != nil {
		t.Fatalf("Reload with identical key bytes under the same version: %v", err)
	}
	if got := r.Load(); got != ring2 {
		t.Errorf("Load() after Reload = %p, want the new (byte-identical) ring %p", got, ring2)
	}
}

// TestReloadReportsADroppedVersionButDoesNotRefuseIt is the companion to the
// reuse test: retiring a key on purpose -- present in the old ring, absent
// from the new one -- is how a rotation finishes, and must succeed while
// still being reported so the caller can log it loudly (S3).
func TestReloadReportsADroppedVersionButDoesNotRefuseIt(t *testing.T) {
	oldRing := mustRing(t, VersionedKey{Version: 2, Key: key32(0xBB)}, VersionedKey{Version: 1, Key: key32(0xAA)})
	newRing := mustRing(t, VersionedKey{Version: 2, Key: key32(0xBB)}) // version 1 retired

	r := NewReloadableKeyRing(oldRing)
	result, err := r.Reload(newRing)
	if err != nil {
		t.Fatalf("Reload (retiring a version on purpose): %v", err)
	}
	if len(result.Dropped) != 1 || result.Dropped[0] != 1 {
		t.Errorf("Dropped = %v, want [1]", result.Dropped)
	}
	if got := r.Load(); got != newRing {
		t.Errorf("Load() after Reload = %p, want the new ring %p", got, newRing)
	}
}

// TestReloadFromNilToARingSucceeds covers going from "no key configured" to
// a configured ring -- the very first reload a deployment that started with
// no master key at all would ever do. Nothing in the old (nil) ring can
// conflict with anything in the new one.
func TestReloadFromNilToARingSucceeds(t *testing.T) {
	newRing := mustRing(t, VersionedKey{Version: 1, Key: key32(0x01)})
	r := NewReloadableKeyRing(nil)
	result, err := r.Reload(newRing)
	if err != nil {
		t.Fatalf("Reload from nil: %v", err)
	}
	if len(result.Dropped) != 0 {
		t.Errorf("Dropped = %v, want none", result.Dropped)
	}
	if got := r.Load(); got != newRing {
		t.Errorf("Load() after Reload = %p, want %p", got, newRing)
	}
}

// TestReloadToNilDropsEveryVersionAndSucceeds is the inverse: an operator
// can reload AWAY from having a master key entirely (a legal state every
// store already handles as "secrets unusable"). Not this check's job to
// forbid -- only a REUSED version is refused.
func TestReloadToNilDropsEveryVersionAndSucceeds(t *testing.T) {
	oldRing := mustRing(t, VersionedKey{Version: 1, Key: key32(0x01)})
	r := NewReloadableKeyRing(oldRing)
	result, err := r.Reload(nil)
	if err != nil {
		t.Fatalf("Reload to nil: %v", err)
	}
	if len(result.Dropped) != 1 || result.Dropped[0] != 1 {
		t.Errorf("Dropped = %v, want [1]", result.Dropped)
	}
	if got := r.Load(); got != nil {
		t.Errorf("Load() after Reload to nil = %v, want nil", got)
	}
}

// TestReloadableKeyRingConcurrentLoadDuringReload is S2's race assertion:
// readers calling Load in a tight loop while another goroutine calls Reload
// repeatedly must never observe anything but a complete, internally
// consistent ring -- true by construction with atomic.Pointer, asserted
// anyway and run under -race so a future change that breaks the invariant
// (e.g. swapping to a mutex-protected struct field written in two steps)
// is caught rather than merely believed.
func TestReloadableKeyRingConcurrentLoadDuringReload(t *testing.T) {
	// DISJOINT version numbers between the two rings -- 1 vs. 2 -- on
	// purpose: reusing one version number across them would trip the
	// M3 refusal this same type enforces (TestReloadRefusesAReusedVersion...),
	// which is a correctness check, not the race this test is about. A
	// version going 1 -> 2 -> 1 -> 2... reports a "dropped" version each
	// time and never a refusal, so every Reload call here actually swaps.
	ringA := mustRing(t, VersionedKey{Version: 1, Key: key32(0xAA)})
	ringB := mustRing(t, VersionedKey{Version: 2, Key: key32(0xBB)})
	r := NewReloadableKeyRing(ringA)

	const iterations = 2000
	var wg sync.WaitGroup
	var torn atomic.Bool

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			got := r.Load()
			if got == nil {
				torn.Store(true)
				return
			}
			// A torn read would show up here as a ring whose Current()
			// version/key pairing matches neither ringA's (1, 0xAA...) nor
			// ringB's (2, 0xBB...) -- impossible under atomic.Pointer,
			// which is the property this test pins.
			cur := got.Current()
			switch cur.Version {
			case 1:
				if len(cur.Key) == 0 || cur.Key[0] != 0xAA {
					torn.Store(true)
				}
			case 2:
				if len(cur.Key) == 0 || cur.Key[0] != 0xBB {
					torn.Store(true)
				}
			default:
				torn.Store(true)
			}
			_ = got.Versions()
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			if i%2 == 0 {
				_, _ = r.Reload(ringB)
			} else {
				_, _ = r.Reload(ringA)
			}
		}
	}()

	wg.Wait()
	if torn.Load() {
		t.Error("a concurrent Load observed an inconsistent ring during Reload")
	}
}
