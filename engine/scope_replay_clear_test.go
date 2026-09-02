package engine

import (
	"context"
	"testing"
	"time"
)

// Replay left a cleared scope in heldScopes, so end-of-segment cleanup
// released a concurrency key the workflow had explicitly given up.
//
// SetScope has a fresh path and a replay path. Clearing a scope
// (objectType == "" && instanceKey == "") does two things on the fresh path,
// via ClearScope: it releases the key on the concurrency-key store, and it
// splices the key out of s.heldScopes. The replay path zeroed the four scope
// fields inline and did neither -- correctly skipping the release, which is a
// side effect that already happened, but also skipping the bookkeeping, which
// every replay of that step owes.
//
// The leftover entry is not inert. releaseHeldScopes runs at the end of every
// execution and releases everything in the slice. Virtual objects use these
// keys to serialise access to one object instance, so once another workflow
// has acquired vo:<type>:<key> in the interval, this workflow's cleanup
// releases *that* workflow's lock, and two workflows are inside the same
// virtual object at once.
//
// Measured 2026-09-01, the same workflow driven both ways:
//
//	FRESH  after acquire+clear: heldScopes=[]string(nil)          scopeSet=false
//	REPLAY after acquire:       heldScopes=[]string{"vo:cart:c1"} scopeSet=true
//	REPLAY after clear:         heldScopes=[]string{"vo:cart:c1"} scopeSet=false
//
// Same class as IMPROVEMENT-PLAN 3.66: the fresh path mutates Go-side session
// state alongside recordEvent, and the replay branch reproduces the event but
// not the state. Found by auditing every execSession method for that shape
// after 3.66; these are the two instances of it.

// recordingKeyStore records which concurrency keys were released, so the
// tests can assert the thing that actually matters -- which locks this
// workflow frees -- rather than the length of a slice.
type recordingKeyStore struct{ released []string }

func (r *recordingKeyStore) AcquireConcurrencyKey(ctx context.Context, key, workflowID string, ttl time.Duration) (bool, error) {
	return true, nil
}

func (r *recordingKeyStore) ReleaseConcurrencyKey(ctx context.Context, key string) error {
	r.released = append(r.released, key)
	return nil
}

func scopeSession(t *testing.T) (*execSession, *recordingKeyStore) {
	t.Helper()
	s := newTestExecSession()
	store := &recordingKeyStore{}
	s.engine.concurrencyKeyStore = store
	return s, store
}

// TestReplayDoesNotReleaseAScopeItAlreadyCleared is the regression test.
//
// It asserts on releases rather than on heldScopes, because a release is the
// externally visible act: freeing vo:cart:c1 while another workflow holds it
// is what lets two workflows into the same virtual object.
func TestReplayDoesNotReleaseAScopeItAlreadyCleared(t *testing.T) {
	ctx := context.Background()

	s, store := scopeSession(t)
	s.isReplay = true
	s.history = []EventRecord{{Step: 0, EventType: EventTypeScopeAcquired}}

	s.SetScope(ctx, nil, "cart", "c1", 0, 0)
	if len(s.heldScopes) != 1 {
		t.Fatalf("the replayed acquisition did not record the held scope: %#v\n\n"+
			"Then the clear below has nothing to forget and this test is vacuous.", s.heldScopes)
	}
	if !s.isReplay {
		t.Fatal("the acquisition fell through to the fresh path, so the replay " +
			"clear branch under test never ran")
	}

	s.SetScope(ctx, nil, "", "", 0, 0)
	s.releaseHeldScopes(ctx)

	if len(store.released) != 0 {
		t.Errorf("replay released %v; it must release nothing.\n\n"+
			"The segment that originally ran this step already released the key "+
			"when the workflow cleared the scope. Releasing it again at the end "+
			"of a replayed segment frees whatever workflow holds that virtual "+
			"object now.", store.released)
	}
	if s.scopeSet {
		t.Error("scope is still set after clearing it on the replay path")
	}
}

// TestFreshClearReleasesExactlyOnce pins the behaviour replay is supposed to
// be reproducing, so "replay releases nothing" cannot become correct by the
// fresh path forgetting to release at all.
func TestFreshClearReleasesExactlyOnce(t *testing.T) {
	ctx := context.Background()
	s, store := scopeSession(t)

	s.SetScope(ctx, nil, "cart", "c1", 0, 0)
	if len(s.heldScopes) != 1 {
		t.Fatalf("fresh acquisition did not record the held scope: %#v", s.heldScopes)
	}

	s.SetScope(ctx, nil, "", "", 0, 0)
	if got := len(store.released); got != 1 {
		t.Fatalf("clearing a scope released %d keys, want 1: %v", got, store.released)
	}
	if store.released[0] != "vo:cart:c1" {
		t.Errorf("released %q, want %q", store.released[0], "vo:cart:c1")
	}

	s.releaseHeldScopes(ctx)
	if got := len(store.released); got != 1 {
		t.Errorf("end-of-segment cleanup released the key a second time: %v\n\n"+
			"ClearScope must forget it as well as release it.", store.released)
	}
}

// TestScopeStillHeldIsStillReleased is the control that keeps the fix from
// degenerating into "never release anything".
//
// A workflow that sets a scope and does NOT clear it must still have the key
// freed by end-of-segment cleanup, or every virtual object it touched stays
// locked until the TTL expires.
func TestScopeStillHeldIsStillReleased(t *testing.T) {
	ctx := context.Background()
	s, store := scopeSession(t)

	s.SetScope(ctx, nil, "cart", "c1", 0, 0)
	s.releaseHeldScopes(ctx)

	if len(store.released) != 1 || store.released[0] != "vo:cart:c1" {
		t.Errorf("a scope that was never cleared was not released at end of "+
			"segment: %v", store.released)
	}
}

// TestForgetHeldScopeLeavesOtherScopesAlone is the control for the splice.
//
// A workflow that switches between virtual objects holds several keys over its
// life, and an over-broad forget would silently stop releasing them -- a leak
// that the assertions above would not notice, because they only ever look at
// one key.
func TestForgetHeldScopeLeavesOtherScopesAlone(t *testing.T) {
	s := newTestExecSession()
	s.heldScopes = []string{"vo:cart:c1", "vo:order:o9", "vo:user:u3"}

	s.forgetHeldScope("vo:order:o9")

	want := []string{"vo:cart:c1", "vo:user:u3"}
	if len(s.heldScopes) != len(want) {
		t.Fatalf("heldScopes = %#v, want %#v", s.heldScopes, want)
	}
	for i := range want {
		if s.heldScopes[i] != want[i] {
			t.Errorf("heldScopes[%d] = %q, want %q", i, s.heldScopes[i], want[i])
		}
	}

	// Forgetting an unheld key must be a no-op, not a panic or a truncation:
	// the replay clear branch calls this whenever a scope is set, including in
	// histories where the acquisition was never recorded.
	s.forgetHeldScope("vo:nothing:here")
	if len(s.heldScopes) != 2 {
		t.Errorf("forgetting an unheld key changed the set: %#v", s.heldScopes)
	}
}
