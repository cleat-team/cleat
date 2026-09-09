package engine

import (
	"context"
	"strings"
	"testing"
)

// A failed scope acquisition must stop the workflow, not return to it.
//
// cleat#1062. cleat_set_scope packs an errCode that no SDK decodes: Rust binds
// it as `_err_code`, AssemblyScript reads only `decoded.extra`, Java ignores
// the result entirely and returns its own `_scopePrefix`, Python's import is a
// stub, and Go discards it to match Rust. So the store-failure branch of
// freshSetScope returned errCode 1 into five discards and the guest ran on
// believing it held the concurrency key -- the mutual-exclusion violation of
// the guarantee scope exists to provide.
//
// The fix arms s.suspendErr, which is what the contention branch beside it
// already does for the same condition. These tests assert the host refuses,
// and -- via the two controls -- that it refuses only when it should.

// The two stores these tests need already exist in locking_test.go, which is
// the point: freshAcquireLock and freshSetScope are the only two callers of
// AcquireConcurrencyKey, and they were given opposite treatments of the same
// two outcomes.
//
//	acquireErrorStore        -> (false, "store error")  the store is broken
//	acquireNotAcquiredStore  -> (false, nil)            somebody else holds it

func TestSetScopeStoreFailureSuspends(t *testing.T) {
	s := newTestExecSession()
	s.engine.concurrencyKeyStore = &acquireErrorStore{}

	result := s.SetScope(context.Background(), nil, "account", "acct-123", 0, 0)

	// The workflow must not proceed. This is the assertion the defect fails:
	// before the fix suspendErr was nil and the guest continued.
	if s.suspendErr == nil {
		t.Fatal("a concurrency-key store failure returned to the guest without suspending: " +
			"the workflow proceeds believing it holds the scope")
	}

	// Distinguishable from contention. Both suspend; an operator reading a
	// suspend reason has no other way to tell a busy virtual object from a
	// broken store, and this repo's rule is that a failure mode needs a stated
	// way to tell it apart from its neighbours.
	if !strings.Contains(s.suspendErr.Reason, "store error") ||
		!strings.Contains(s.suspendErr.Reason, "vo:account:acct-123") {
		t.Errorf("suspend reason must name the scope key and the store error so it is not "+
			"read as contention, got %q", s.suspendErr.Reason)
	}
	if strings.Contains(s.suspendErr.Reason, "held by another workflow") {
		t.Errorf("a store failure must not report itself as contention, got %q", s.suspendErr.Reason)
	}

	// Host-side scope state must be untouched: the key was not acquired.
	if s.scopeSet {
		t.Error("scopeSet must stay false when acquisition failed")
	}
	if len(s.heldScopes) != 0 {
		t.Errorf("a scope that was not acquired must not be held, got %v", s.heldScopes)
	}

	// errCode 1 stays on the wire. Nothing reads it today, and this pins that
	// the fix did not change the ABI while adding the host-side refusal.
	if errCode := uint32(result & 0xFFFFFFFF); errCode != 1 {
		t.Errorf("expected errCode 1 preserved on the store-failure path, got %d", errCode)
	}

	// The failure is recorded, which is what the replay retry keys on.
	if len(s.history) != 1 {
		t.Fatalf("expected 1 recorded event, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypeScopeAcquired || s.history[0].Err == "" {
		t.Errorf("expected a ScopeAcquired event carrying Err, got %+v", s.history[0])
	}
}

// Control: contention is a different branch and keeps its own behaviour.
//
// Without this, a fix that suspended on every non-acquisition would pass the
// test above while erasing the distinction between the two cases.
func TestSetScopeContentionStillSuspendsAsContention(t *testing.T) {
	s := newTestExecSession()
	s.engine.concurrencyKeyStore = &acquireNotAcquiredStore{}

	result := s.SetScope(context.Background(), nil, "account", "acct-123", 0, 0)

	if s.suspendErr == nil {
		t.Fatal("contention must still suspend")
	}
	if !strings.Contains(s.suspendErr.Reason, "held by another workflow") {
		t.Errorf("expected the contention reason, got %q", s.suspendErr.Reason)
	}
	// Contention reports errCode ZERO -- apparent success -- and relies wholly
	// on the suspension. Unchanged by cleat#1062.
	if errCode := uint32(result & 0xFFFFFFFF); errCode != 0 {
		t.Errorf("contention must keep returning errCode 0, got %d", errCode)
	}
}

// Control: the store working must not suspend anything.
func TestSetScopeSuccessDoesNotSuspend(t *testing.T) {
	s := newTestExecSession()
	s.engine.concurrencyKeyStore = &mockConcurrencyKeyStore{}

	if result := s.SetScope(context.Background(), nil, "account", "acct-123", 0, 0); result != 0 {
		t.Errorf("expected 0 from a successful acquisition, got %d", result)
	}
	if s.suspendErr != nil {
		t.Fatalf("a successful acquisition must not suspend, got %v", s.suspendErr)
	}
	if !s.scopeSet {
		t.Error("expected scopeSet=true")
	}
}

// The retry the suspension exists to reach.
//
// replaySetScope's `rec.Err != ""` branch declines to set the scope fields and
// re-enters freshSetScope, commented "switch to fresh to retry acquisition".
// Before cleat#1062 nothing produced the suspension that leads to a replay, so
// that branch was reachable only by accident. This walks the whole path: a
// recorded failure, replayed against a store that now works, acquires.
func TestSetScopeReplayOfRecordedFailureRetriesAcquisition(t *testing.T) {
	s := newTestExecSession()
	s.engine.concurrencyKeyStore = &mockConcurrencyKeyStore{}
	s.isReplay = true
	s.history = []EventRecord{{
		Step:      0,
		EventType: EventTypeScopeAcquired,
		ScopeKey:  "vo:account:acct-123",
		Err:       "store error",
	}}

	if result := s.SetScope(context.Background(), nil, "account", "acct-123", 0, 0); result != 0 {
		t.Errorf("expected the retry to succeed, got result %d", result)
	}
	if !s.scopeSet {
		t.Fatal("replaying a recorded acquisition failure must retry and acquire")
	}
	if s.scopePrefix != "vo:account:acct-123:" {
		t.Errorf("expected scopePrefix 'vo:account:acct-123:', got %q", s.scopePrefix)
	}
	if len(s.heldScopes) != 1 || s.heldScopes[0] != "vo:account:acct-123" {
		t.Errorf("expected the retried key to be held, got %v", s.heldScopes)
	}
	if s.suspendErr != nil {
		t.Errorf("a successful retry must not suspend, got %v", s.suspendErr)
	}
	if s.isReplay {
		t.Error("expected the session to have left replay to redo the acquisition")
	}
}
