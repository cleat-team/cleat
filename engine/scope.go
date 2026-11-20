package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/tetratelabs/wazero/api"
)

// SetScope enters or leaves a virtual object's scope. Three things happen here and only the middle one is a step:
//
//  1. The previous scope is written into the guest's buffer, on both arrivals, BEFORE history is read and before the call can be refused: the
//     guest has no other way to learn how many of those bytes are its previous scope, and a replayed call must hand it what the fresh one did.
//     The result of that write is the call's answer on every path that does not fail, so it travels to the step packed in args.i1.
//  2. SetScope("", "") clears the scope. It records no event and is never refused, so it is not a step: a fresh clear releases the key, and a
//     replayed one only forgets it, because the segment that ran the step originally already released it.
//  3. Anything else takes a scope, which is the step below.
func (s *execSession) SetScope(ctx context.Context, m api.Module, objectType, instanceKey string, prevScopePtr, prevScopeMaxLen uint32) int64 {
	var prevLen uint32
	var prevEC byte
	if s.scopeSet && s.scopePrefix != "" {
		prevLen, prevEC = s.writeOut(ctx, m, prevScopePtr, s.scopePrefix, prevScopeMaxLen)
	}
	prev := packSimpleResult(prevEC, prevLen)

	if objectType == "" && instanceKey == "" {
		if s.isReplay {
			s.replayClearScope()
		} else {
			s.ClearScope(ctx)
		}
		return prev
	}
	// outPtr and outMaxLen stay zero on purpose: that buffer holds the previous scope, and a divergence message written there would replace it.
	return s.step(ctx, m, &setScopeStep, stepArgs{s1: objectType, s2: instanceKey, i1: prev})
}

// scopeKeyOf is the concurrency key a scope is held under.
func scopeKeyOf(objectType, instanceKey string) string { return "vo:" + objectType + ":" + instanceKey }

// setScopeStep is SetScope taking a scope, as a step (cleat#4211). Its history is a RUN: any number of failed attempts, each a scope_acquired
// carrying an error, then the acquisition that ended them. Failed attempts are recorded at consecutive steps, and a replay that took only the first
// record left replay at the second on every later replay (cleat#4177), so the run is one call's history and the dispatcher reads it whole.
//
// Refused in the same four states as every other new work (stopBeforeNewWork, the default): taking a scope writes a 24-hour row to the
// concurrency-key store and blocks every live workflow that wants the same virtual object. Measured against a real store, cleat#4079 audit
// row set_scope. Clearing a scope is not this step and is never refused.
var setScopeStep = stepSpec{
	Name:    "set_scope",
	Effect:  effectEffect,
	Records: recordsOne,
	Want:    EventTypeScopeAcquired,
	// The scope key is what identifies the call: a record that names another scope is another call's. A first record used to escape this
	// comparison (cleat#4211) and was accepted as this call's, which adopted the guest's scope on the strength of a record for a different one.
	Identity: func(a stepArgs, rec *EventRecord) string {
		if want := scopeKeyOf(a.s1, a.s2); rec.ScopeKey != want {
			return fmt.Sprintf("scope %q, recorded %q", want, rec.ScopeKey)
		}
		return ""
	},
	// A recorded attempt that FAILED is a prefix record; the acquisition is the final. Whether it names this call's scope is Identity's.
	Role: func(_ stepArgs, rec *EventRecord) recordRole {
		switch {
		case rec.EventType != EventTypeScopeAcquired:
			return roleForeign
		case rec.Err != "":
			return rolePrefix
		default:
			return roleFinal
		}
	},
	// The failed attempts need no decoding: the retry that ended them is the acquisition, and what the replay owes is its bookkeeping.
	DecodeRun: func(c *stepCtx, _ []EventRecord) int64 {
		c.s.adoptReplayedScope(c.args.s1, c.args.s2)
		return c.args.i1
	},
	// The event is recorded HERE, in the spec's own Fresh, and not in a method it calls: see scopeAttempt.
	Fresh: func(c *stepCtx) int64 {
		a := c.s.attemptScope(c.ctx, c.args.s1, c.args.s2)
		c.Record(a.rec)
		return c.s.freshSetScope(c, a)
	},
}

// forgetHeldScope drops scopeKey from the held set WITHOUT releasing it.
//
// The two halves of "the workflow no longer holds this scope" are separable on
// purpose. Releasing is a side effect on the concurrency-key store and must
// happen exactly once, in the segment that originally ran the step; forgetting
// is Go-side bookkeeping that every replay of that step has to reproduce, or
// releaseHeldScopes frees a key the workflow already gave up.
func (s *execSession) forgetHeldScope(scopeKey string) {
	for i, held := range s.heldScopes {
		if held == scopeKey {
			s.heldScopes = append(s.heldScopes[:i], s.heldScopes[i+1:]...)
			return
		}
	}
}

func (s *execSession) ClearScope(ctx context.Context) {
	if s.scopeSet && s.scopePrefix != "" {
		scopeKey := "vo:" + s.scopeObjType + ":" + s.scopeInstKey
		if s.engine.concurrencyKeyStore != nil {
			// s.workflowID: this session acquired the scope key, so it is the
			// holder. Passing it is a tightening -- the release can no longer
			// match a row another workflow owns (cleat#1188).
			if _, err := s.engine.concurrencyKeyStore.ReleaseConcurrencyKey(ctx, scopeKey, s.workflowID); err != nil {
				s.engine.log().ErrorContext(ctx, "release_concurrency_key failed", "workflow_id", s.workflowID, "tenant_id", s.tenantID, "error", err)
			}
		}
		s.forgetHeldScope(scopeKey)
	}
	s.scopeSet = false
	s.scopePrefix = ""
	s.scopeObjType = ""
	s.scopeInstKey = ""
}

// scopeAttempt is what trying to take a scope came to: the event to record, and which way it went. It exists so that the event is recorded by the
// step's Fresh and nowhere else: the dispatcher refuses before Fresh runs, and a Record reached from any other function could skip that refusal
// (the cleat#4143 shape, held by TestTheDispatcherExemptionsAreTrueOfTheirCallers). What the call returns and the suspension it arms stay in
// freshSetScope, which the sites guard counts by name.
type scopeAttempt struct {
	rec EventRecord
	// storeErr is the concurrency-key store's failure, or nil; held says another workflow holds the key. At most one is set, and neither means the
	// key was acquired (or that there is no store to ask, which this call has always treated as acquired).
	storeErr error
	held     bool
}

// attemptScope releases the scope being switched away from and tries to acquire the new key. It records nothing, sets no scope state and arms no
// suspension: the caller records a.rec and then hands the attempt to freshSetScope. It does append a key it acquired to the held set, before the
// event is recorded, as this call always did.
func (s *execSession) attemptScope(ctx context.Context, objectType, instanceKey string) scopeAttempt {
	// If switching from an existing scope, release the old key first.
	if s.scopeSet && s.scopePrefix != "" {
		oldKey := "vo:" + s.scopeObjType + ":" + s.scopeInstKey
		if s.engine.concurrencyKeyStore != nil {
			if _, err := s.engine.concurrencyKeyStore.ReleaseConcurrencyKey(ctx, oldKey, s.workflowID); err != nil {
				s.engine.log().ErrorContext(ctx, "release_concurrency_key failed", "workflow_id", s.workflowID, "tenant_id", s.tenantID, "error", err)
			}
		}
		s.forgetHeldScope(oldKey)
	}

	// Acquire new scope key.
	scopeKey := "vo:" + objectType + ":" + instanceKey
	a := scopeAttempt{rec: EventRecord{Step: s.stepCount, EventType: EventTypeScopeAcquired, ScopeKey: scopeKey}}
	if s.engine.concurrencyKeyStore != nil {
		acquired, err := s.engine.concurrencyKeyStore.AcquireConcurrencyKey(ctx, scopeKey, s.workflowID, 24*time.Hour)
		switch {
		case err != nil:
			a.storeErr = err
			a.rec.Err = err.Error()
		case !acquired:
			a.held = true
			a.rec.Err = "scope held by another workflow"
		default:
			s.heldScopes = append(s.heldScopes, scopeKey)
		}
	}
	return a
}

// freshSetScope is what the step's fresh half does with an attempt once it is recorded: it arms the suspension for a failure or a contended key,
// and otherwise takes the scope. The dispatcher has already refused the call where it must be refused, and SetScope has already written the
// previous scope into the guest's buffer. Both suspensions are set here, in a function of this name, which the sites guard counts.
func (s *execSession) freshSetScope(c *stepCtx, a scopeAttempt) int64 {
	objectType, instanceKey, prev := c.args.s1, c.args.s2, c.args.i1
	scopeKey := a.rec.ScopeKey
	switch {
	case a.storeErr != nil:
		// Suspend, exactly as the contention case below does.
		//
		// The errCode this returns is decoded by no SDK -- Rust names it
		// `_err_code`, AssemblyScript reads only `decoded.extra`, Java
		// ignores the result and returns its own `_scopePrefix`, Python's
		// import is a stub, and Go discards it to match Rust (cleat#1062).
		// So until this line, a store failure returned to the guest as an
		// ordinary success and the workflow ran on believing it held the
		// key -- a mutual-exclusion violation of the one guarantee scope
		// exists to provide.
		//
		// Reporting it through the return value cannot fix that on its
		// own: it makes the guarantee contingent on five SDKs each
		// choosing to check. The host is the only party that can refuse,
		// and it already refuses for the neighbouring case -- a contended
		// key returns errCode ZERO and stops the run through
		// s.suspendErr. This is the same condition ("we do not hold this
		// key") and now takes the same exit.
		//
		// The retry side already existed: a replay of a recorded failed
		// attempt declined to set the scope fields and went fresh to
		// reacquire (it is setScopeStep's run of prefix records now).
		// Nothing reached it, because nothing suspended. errCode 1 stays
		// on the wire for any SDK that later decides to read it; ABI.md is
		// unchanged.
		//
		// The Reason is deliberately distinguishable from contention's:
		// both suspend the workflow, and an operator reading a suspend
		// reason has no other way to tell a busy virtual object from a
		// broken concurrency-key store.
		s.suspendErr = &SuspendError{
			Reason: fmt.Sprintf("virtual object scope %s could not be acquired: %v", scopeKey, a.storeErr),
			Until:  time.UnixMilli(s.nowMs).Add(5 * time.Second),
		}
		return packSimpleResult(1, 0)
	case a.held:
		s.suspendErr = &SuspendError{
			Reason: fmt.Sprintf("virtual object scope %s held by another workflow", scopeKey),
			Until:  time.UnixMilli(s.nowMs).Add(5 * time.Second),
		}
		return prev
	}

	s.scopeSet = true
	s.scopeObjType = objectType
	s.scopeInstKey = instanceKey
	s.scopePrefix = "vo:" + objectType + ":" + instanceKey + ":"
	return prev
}

// replayClearScope reproduces ClearScope's bookkeeping, not its release. The fresh path released this key in the segment that originally ran
// the step; re-releasing here would be a side effect during replay. Dropping it from heldScopes is what replay owes, and skipping that left
// releaseHeldScopes to free a key the workflow had explicitly cleared -- which, once another workflow has acquired that virtual object, is
// releasing somebody else's lock.
func (s *execSession) replayClearScope() {
	if s.scopeSet && s.scopePrefix != "" {
		s.forgetHeldScope(scopeKeyOf(s.scopeObjType, s.scopeInstKey))
	}
	s.scopeSet = false
	s.scopePrefix = ""
	s.scopeObjType = ""
	s.scopeInstKey = ""
}

// adoptReplayedScope applies the bookkeeping of a recorded SUCCESSFUL acquisition: drop the scope it switched away from, and hold the new one.
// It acquires nothing; the acquisition happened in the segment that recorded it. The fresh path released the previous key and dropped it from
// the held set; replay reproduces the second half only, or releaseHeldScopes frees a key this workflow gave up when it switched away, which is
// another workflow's lock once that object has been re-acquired (found by the fresh-vs-replay parity property test; IMPROVEMENT-PLAN 3.69).
func (s *execSession) adoptReplayedScope(objectType, instanceKey string) {
	if s.scopeSet && s.scopePrefix != "" {
		s.forgetHeldScope(scopeKeyOf(s.scopeObjType, s.scopeInstKey))
	}
	s.scopeSet = true
	s.scopeObjType = objectType
	s.scopeInstKey = instanceKey
	s.scopePrefix = "vo:" + objectType + ":" + instanceKey + ":"
	s.heldScopes = append(s.heldScopes, scopeKeyOf(objectType, instanceKey))
}

func (s *execSession) releaseHeldScopes(ctx context.Context) {
	if s.engine.concurrencyKeyStore == nil {
		return
	}
	for _, scopeKey := range s.heldScopes {
		if _, err := s.engine.concurrencyKeyStore.ReleaseConcurrencyKey(ctx, scopeKey, s.workflowID); err != nil {
			s.engine.log().ErrorContext(ctx, "release_concurrency_key failed", "workflow_id", s.workflowID, "tenant_id", s.tenantID, "error", err)
		}
	}
	s.heldScopes = nil
}

func (s *execSession) GetScope(ctx context.Context, m api.Module, objTypePtr, objTypeMaxLen, instKeyPtr, instKeyMaxLen uint32) int64 {

	var objTypeLen, instKeyLen uint32
	if s.scopeSet {
		objTypeLen, _ = s.writeResult(ctx, m, objTypePtr, s.scopeObjType, objTypeMaxLen)
		instKeyLen, _ = s.writeResult(ctx, m, instKeyPtr, s.scopeInstKey, instKeyMaxLen)
	}

	return int64(uint64(objTypeLen)<<32 | uint64(instKeyLen))
}
