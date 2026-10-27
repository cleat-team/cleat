package engine

import (
	"crypto/subtle"
	"fmt"
	"sort"
	"sync/atomic"
)

// ReloadableKeyRing holds a *KeyRing that can be swapped for a new one while
// readers keep calling Load concurrently -- cleat#2298's mechanism for a
// SIGHUP-triggered key reload with no restart.
//
// ONE INSTANCE, SHARED ACROSS EVERY STORE THAT USES THE SAME KEY MATERIAL.
// SecretStore and DeploymentSecretStore share one ring today (both built
// from CLEAT_SECRET_MASTER_KEY and its _PREVIOUS pair -- see
// DeploymentSecretStore's own doc comment), so they are meant to share one
// *ReloadableKeyRing instance too: one Reload call updates both stores at
// once, with no window in which they disagree about which key is current.
// Two independently-swapped atomic cells fed the same ring value would allow
// exactly that window.
//
// atomic.Pointer, not a mutex: Load is a single atomic read with no
// blocking, which matters because it runs on every seal and every open --
// cleat's hottest crypto path, and under a `-race` reader this is also what
// makes every Load return an internally-consistent ring rather than a torn
// mix of two: a reader never dereferences the pointer twice for one ring.
type ReloadableKeyRing struct {
	ring atomic.Pointer[KeyRing]
}

// NewReloadableKeyRing wraps an initial ring, which may be nil -- "no master
// key configured" is a legal state every store built on KeyRing already
// handles, and it stays legal here.
func NewReloadableKeyRing(initial *KeyRing) *ReloadableKeyRing {
	r := &ReloadableKeyRing{}
	r.ring.Store(initial)
	return r
}

// Load returns the ring that is live right now. A nil receiver reads the
// same as a nil ring: a store wrapping a nil *ReloadableKeyRing (none of
// this package's constructors build one, but a zero-value struct embedding
// one would) does not need its own nil check before calling Load.
func (r *ReloadableKeyRing) Load() *KeyRing {
	if r == nil {
		return nil
	}
	return r.ring.Load()
}

// ReloadResult reports what a successful Reload changed.
type ReloadResult struct {
	// Dropped lists a key version the OLD ring could open that the NEW ring
	// cannot. This is not a refusal -- retiring a key on purpose, once
	// nothing is still sealed under it, is exactly how a rotation finishes
	// (see Reload's doc comment on the version-reuse check it DOES refuse).
	// The caller logs this loudly rather than silently: a dropped version
	// that still has live data under it just stopped resolving.
	Dropped []int
}

// Reload validates next against the ring that is live right now and, if it
// passes, installs it. On any refusal the live ring is left exactly as it
// was -- never a partial swap, and never two reads in a row (TOCTOU would
// let a concurrent Reload race this one's own validation against a ring
// neither validated against).
//
// THE ONE SAFETY CHECK BEYOND "is this a well-formed ring" -- NewKeyRing
// already enforces that on construction, rejecting a ring with a version
// collision or a duplicated key within ITSELF. Reload's job is the check
// NewKeyRing cannot do, because it only ever sees one ring: next must not
// carry a version that the CURRENTLY LIVE ring also carries, under
// DIFFERENT key bytes.
//
// THE MISTAKE THIS CATCHES, AND WHY IT REFUSES RATHER THAN LOGS. A key
// ring's version numbers are an operator-declared integer, not a
// fingerprint of the key -- which is what lets an operator retire a key by
// simply dropping it from the next ring (see Dropped, above). But a
// declared number can also be REUSED by accident: write a brand-new key to
// the path a version number already names, without first demoting the old
// key under a new number, and the resulting ring's version N is a
// DIFFERENT key than the version N every row sealed a moment ago was
// written under. Installing it would not merely make those rows
// unreadable -- a request racing the swap could seal a NEW row under
// version N using the new key while an older reader (or this same process,
// a moment later) still believes version N means the old key, silently
// returning the wrong plaintext for a ciphertext written an instant
// earlier. That is worse than refusing the reload outright, which is why
// this is the one check Reload refuses on rather than logs through.
func (r *ReloadableKeyRing) Reload(next *KeyRing) (ReloadResult, error) {
	if r == nil {
		return ReloadResult{}, fmt.Errorf("reload key ring: nil ReloadableKeyRing")
	}
	current := r.ring.Load()
	if err := refuseReusedVersion(current, next); err != nil {
		return ReloadResult{}, err
	}
	result := ReloadResult{Dropped: droppedVersions(current, next)}
	r.ring.Store(next)
	return result, nil
}

// refuseReusedVersion returns an error if next carries a version that
// current also carries under different key bytes. Either ring may be nil
// (KeyRing.Versions/Key are both nil-safe); a nil ring has no versions to
// conflict with and never causes a refusal.
func refuseReusedVersion(current, next *KeyRing) error {
	for _, v := range current.Versions() {
		oldKey, ok := current.Key(v)
		if !ok {
			continue // unreachable: v was read from current.Versions() itself
		}
		newKey, ok := next.Key(v)
		if !ok {
			continue // absent from next -- a drop, not a reuse; see Dropped
		}
		// subtle.ConstantTimeCompare reports unequal for mismatched lengths
		// without a separate length check, and timing-safety costs nothing
		// here that a plain bytes.Equal would not already cost in clarity.
		if subtle.ConstantTimeCompare(oldKey.Key, newKey.Key) != 1 {
			return fmt.Errorf("reload refused: key version %d is already live under different key "+
				"bytes than the ones just configured -- a version number must never be reused for a "+
				"different key, because a request racing this reload could seal a new row under it "+
				"with one key while a reader expects the other; retire version %d (drop it from the "+
				"ring) and give the new key an unused version number instead", v, v)
		}
	}
	return nil
}

// droppedVersions lists, ascending, every version current could open that
// next cannot.
func droppedVersions(current, next *KeyRing) []int {
	var dropped []int
	for _, v := range current.Versions() {
		if _, ok := next.Key(v); !ok {
			dropped = append(dropped, v)
		}
	}
	sort.Ints(dropped)
	return dropped
}
