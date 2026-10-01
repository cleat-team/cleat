package engine

// cleat#2897, decision (c) on cleat#1984. Mirrors callintent_test.go's
// TestResolveAmbiguity_* suite for the sibling mechanism: re-dispatching a
// pending call under its ORIGINAL idempotency key, rather than asking a
// separate lookup operation.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeKeyReplayer records what it was asked and answers as configured, with
// a queue of outcomes so a 409-then-200 sequence can be exercised without a
// second type.
type fakeKeyReplayer struct {
	outcomes []IdempotencyReplayOutcome
	response string
	err      error

	calls int
	keys  []string
	ops   []string
	reqs  []string
}

func (r *fakeKeyReplayer) ReplayUnderOriginalKey(_ context.Context, service, operation, requestJSON, idempotencyKey string) (string, IdempotencyReplayOutcome, error) {
	r.calls++
	r.keys = append(r.keys, idempotencyKey)
	r.ops = append(r.ops, service+"."+operation)
	r.reqs = append(r.reqs, requestJSON)
	if r.err != nil {
		return "", IdempotencyReplayCannotSay, r.err
	}
	idx := r.calls - 1
	if idx >= len(r.outcomes) {
		idx = len(r.outcomes) - 1
	}
	return r.response, r.outcomes[idx], nil
}

// TestResolveAmbiguityViaKeyReplay_CompletesTheStepAndPersistsIt is phase
// (c)'s point: the service's own key table resolves the ambiguity, and the
// engine never falls through to [AMBIGUOUS] or to a lookup-based resolver.
func TestResolveAmbiguityViaKeyReplay_CompletesTheStepAndPersistsIt(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		wfID := newIntentWorkflow(t, ctx, store, "keyreplay-ok")
		writePendingCall(t, ctx, store, wfID)

		replayer := &fakeKeyReplayer{response: `{"charged":true,"via":"keyreplay"}`, outcomes: []IdempotencyReplayOutcome{IdempotencyReplayResolved}}
		ops := map[string]bool{intentService + "." + intentOperation: true}
		s, caller := replaySessionFor(t, ctx, store, wfID,
			WithIdempotencyKeyReplayer(replayer), WithIdempotencyKeyOps(ops))

		result := s.DurableCall(ctx, nil, intentService, intentOperation, `{"amount":100}`, 0, 0)
		if errCodeOf(result) != 0 {
			t.Fatalf("a resolved call still reported failure (code %d)", callErrorCodeOf(result))
		}
		if caller.calls != 0 {
			t.Errorf("the service's ordinary Call path was used %d times; resolving via key replay "+
				"goes through ReplayUnderOriginalKey, not Call", caller.calls)
		}
		if replayer.calls != 1 {
			t.Fatalf("the replayer was asked %d times, want 1", replayer.calls)
		}
		if got, want := replayer.ops[0], intentService+"."+intentOperation; got != want {
			t.Errorf("the replayer was asked about %q, want %q", got, want)
		}
		if got, want := replayer.reqs[0], `{"amount":100}`; got != want {
			t.Errorf("the replayer was given request %q, want the ORIGINAL request %q", got, want)
		}
		if want := DurableCallIdempotencyKey(wfID, "", 0); replayer.keys[0] != want {
			t.Errorf("replayer was given key %q, want %q -- the same derivation callService uses",
				replayer.keys[0], want)
		}

		after := stepRecord(t, ctx, store, wfID, 0)
		if after.Pending {
			t.Error("the row is still pending after resolution, so every later replay would ask again")
		}
		if !strings.Contains(after.Response, "keyreplay") {
			t.Errorf("stored response = %q, want the resolved outcome", after.Response)
		}
		if err := store.VerifyWorkflowEvents(ctx, wfID); err != nil {
			t.Errorf("VerifyWorkflowEvents after resolution: %v", err)
		}

		// A second replay reads the now-completed row and does not consult the
		// replayer again.
		s2, _ := replaySessionFor(t, ctx, store, wfID, WithIdempotencyKeyReplayer(replayer), WithIdempotencyKeyOps(ops))
		if r2 := s2.DurableCall(ctx, nil, intentService, intentOperation, `{"amount":100}`, 0, 0); errCodeOf(r2) != 0 {
			t.Errorf("the second replay reported failure (code %d)", callErrorCodeOf(r2))
		}
		if replayer.calls != 1 {
			t.Errorf("the replayer was asked %d times across two replays, want 1", replayer.calls)
		}
	})
}

// TestResolveAmbiguityViaKeyReplay_409RetriesThenResolves is item 2's
// contract: a 409 means "a request under this key is still being processed",
// not a failure -- the engine retries the SAME dispatch, under the SAME key,
// rather than giving up on the first one.
func TestResolveAmbiguityViaKeyReplay_409RetriesThenResolves(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		wfID := newIntentWorkflow(t, ctx, store, "keyreplay-409")
		writePendingCall(t, ctx, store, wfID)

		replayer := &fakeKeyReplayer{
			response: `{"charged":true}`,
			outcomes: []IdempotencyReplayOutcome{IdempotencyReplayRetryLater, IdempotencyReplayResolved},
		}
		ops := map[string]bool{intentService + "." + intentOperation: true}
		s, _ := replaySessionFor(t, ctx, store, wfID, WithIdempotencyKeyReplayer(replayer), WithIdempotencyKeyOps(ops))

		result := s.DurableCall(ctx, nil, intentService, intentOperation, `{"amount":100}`, 0, 0)
		if errCodeOf(result) != 0 {
			t.Fatalf("a 409-then-resolved sequence still reported failure (code %d)", callErrorCodeOf(result))
		}
		if replayer.calls != 2 {
			t.Fatalf("the replayer was asked %d times, want 2 (the 409 retried once)", replayer.calls)
		}
		if replayer.keys[0] != replayer.keys[1] {
			t.Error("the retry used a DIFFERENT key than the first attempt -- the whole point of a " +
				"409 retry is reusing the SAME key")
		}
	})
}

// TestResolveAmbiguityViaKeyReplay_409ExhaustsIntoAmbiguous pins the bound:
// a 409 that never resolves must not retry forever inside replay, which runs
// synchronously on a worker goroutine a workflow instance is waiting on.
func TestResolveAmbiguityViaKeyReplay_409ExhaustsIntoAmbiguous(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		wfID := newIntentWorkflow(t, ctx, store, "keyreplay-409-exhaust")
		writePendingCall(t, ctx, store, wfID)

		replayer := &fakeKeyReplayer{outcomes: []IdempotencyReplayOutcome{IdempotencyReplayRetryLater}}
		ops := map[string]bool{intentService + "." + intentOperation: true}
		s, _ := replaySessionFor(t, ctx, store, wfID, WithIdempotencyKeyReplayer(replayer), WithIdempotencyKeyOps(ops))

		result := s.DurableCall(ctx, nil, intentService, intentOperation, `{"amount":100}`, 0, 0)
		if errCodeOf(result) == 0 {
			t.Fatal("an exhausted 409 retry reported success")
		}
		if got := callErrorCodeOf(result); got != callErrorUnknown {
			t.Errorf("callErrorCode = %d, want callErrorUnknown (%d) -- a replay attempt that never "+
				"got a definite answer must fall back to [AMBIGUOUS], same as no replayer at all", got, callErrorUnknown)
		}
		if replayer.calls != idempotencyReplayMaxAttempts {
			t.Errorf("the replayer was asked %d times, want the bound (%d)", replayer.calls, idempotencyReplayMaxAttempts)
		}
		if rec := stepRecord(t, ctx, store, wfID, 0); !rec.Pending {
			t.Error("the row stopped being pending even though nothing resolved it")
		}
	})
}

// TestResolveAmbiguityViaKeyReplay_RetentionBoundSkipsAStaleRow is item 3's
// contract: beyond the configured retention, a resend risks being a NEW call
// under a key the service may have already forgotten, so the engine must not
// attempt it at all -- not even once.
func TestResolveAmbiguityViaKeyReplay_RetentionBoundSkipsAStaleRow(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		wfID := newIntentWorkflow(t, ctx, store, "keyreplay-stale")
		writePendingCall(t, ctx, store, wfID)
		// Give the written row's created_at time to be strictly in the past
		// relative to a 1ns retention -- the DB round trip above already
		// guarantees this in practice, but a sleep makes it true by
		// construction rather than by timing luck.
		time.Sleep(5 * time.Millisecond)

		replayer := &fakeKeyReplayer{outcomes: []IdempotencyReplayOutcome{IdempotencyReplayResolved}, response: `{"charged":true}`}
		ops := map[string]bool{intentService + "." + intentOperation: true}
		s, _ := replaySessionFor(t, ctx, store, wfID,
			WithIdempotencyKeyReplayer(replayer), WithIdempotencyKeyOps(ops), WithIdempotencyKeyRetention(1*time.Nanosecond))

		result := s.DurableCall(ctx, nil, intentService, intentOperation, `{"amount":100}`, 0, 0)
		if errCodeOf(result) == 0 {
			t.Fatal("a stale pending row was resolved anyway, past its retention bound")
		}
		if got := callErrorCodeOf(result); got != callErrorUnknown {
			t.Errorf("callErrorCode = %d, want callErrorUnknown (%d) -- a retention-expired row falls "+
				"back to [AMBIGUOUS]", got, callErrorUnknown)
		}
		if replayer.calls != 0 {
			t.Errorf("the replayer was consulted %d times; a row past its retention bound must not be "+
				"replayed at all, not even once", replayer.calls)
		}
	})
}

// TestResolveAmbiguityViaKeyReplay_UnconfiguredOperationCannotSay pins that
// this mechanism only answers for an operation WithIdempotencyKeyOps named --
// the engine-level half of
// TestReplayUnderOriginalKey_UnconfiguredOperationCannotSay in
// cmd/cleat-worker.
func TestResolveAmbiguityViaKeyReplay_UnconfiguredOperationCannotSay(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		wfID := newIntentWorkflow(t, ctx, store, "keyreplay-unconfigured")
		writePendingCall(t, ctx, store, wfID)

		replayer := &fakeKeyReplayer{outcomes: []IdempotencyReplayOutcome{IdempotencyReplayResolved}, response: `{"charged":true}`}
		// Declared for a DIFFERENT operation, not intentService.intentOperation.
		ops := map[string]bool{"shipping.dispatch": true}
		s, _ := replaySessionFor(t, ctx, store, wfID, WithIdempotencyKeyReplayer(replayer), WithIdempotencyKeyOps(ops))

		result := s.DurableCall(ctx, nil, intentService, intentOperation, `{"amount":100}`, 0, 0)
		if errCodeOf(result) == 0 {
			t.Fatal("an unconfigured operation was resolved anyway")
		}
		if replayer.calls != 0 {
			t.Errorf("the replayer was consulted %d times for an operation nobody declared it for", replayer.calls)
		}
	})
}

// TestResolveAmbiguityViaKeyReplay_ReplayerErrorFallsBackToAmbiguous mirrors
// TestResolveAmbiguity_FallsBackWhenItCannotSay's "the resolver itself
// failed" case: a transport-level error answers nothing about the call's
// outcome, so it must degrade to [AMBIGUOUS] rather than being treated as a
// resolved call or a licence to repeat the dispatch.
func TestResolveAmbiguityViaKeyReplay_ReplayerErrorFallsBackToAmbiguous(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		wfID := newIntentWorkflow(t, ctx, store, "keyreplay-err")
		writePendingCall(t, ctx, store, wfID)

		replayer := &fakeKeyReplayer{err: errors.New("replay service down")}
		ops := map[string]bool{intentService + "." + intentOperation: true}
		s, _ := replaySessionFor(t, ctx, store, wfID, WithIdempotencyKeyReplayer(replayer), WithIdempotencyKeyOps(ops))

		result := s.DurableCall(ctx, nil, intentService, intentOperation, `{"amount":100}`, 0, 0)
		if errCodeOf(result) == 0 {
			t.Fatal("a replayer error reported success")
		}
		if got := callErrorCodeOf(result); got != callErrorUnknown {
			t.Errorf("callErrorCode = %d, want callErrorUnknown (%d)", got, callErrorUnknown)
		}
		if replayer.calls != 1 {
			t.Errorf("the replayer was consulted %d times, want 1 -- an error is not retried inside "+
				"this loop, the same way ResolveCall's err path is not", replayer.calls)
		}
		if rec := stepRecord(t, ctx, store, wfID, 0); !rec.Pending {
			t.Error("the row stopped being pending even though nothing resolved it")
		}
	})
}

// TestResolveAmbiguityViaKeyReplay_TriedBeforeAmbiguityResolver pins the
// precedence --idempotency-key-ops gets over --ambiguity-lookup when (through
// a misconfiguration the worker's own boot validation refuses, but the
// engine itself does not enforce) both were somehow set for the same
// operation: key replay answers first, and the lookup-based resolver is
// never reached once it does.
func TestResolveAmbiguityViaKeyReplay_TriedBeforeAmbiguityResolver(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		wfID := newIntentWorkflow(t, ctx, store, "keyreplay-precedence")
		writePendingCall(t, ctx, store, wfID)

		replayer := &fakeKeyReplayer{response: `{"charged":true,"via":"keyreplay"}`, outcomes: []IdempotencyReplayOutcome{IdempotencyReplayResolved}}
		lookupResolver := &fakeResolver{response: `{"charged":true,"via":"lookup"}`, outcome: AmbiguityResolved}
		ops := map[string]bool{intentService + "." + intentOperation: true}
		s, _ := replaySessionFor(t, ctx, store, wfID,
			WithIdempotencyKeyReplayer(replayer), WithIdempotencyKeyOps(ops), WithAmbiguityResolver(lookupResolver))

		result := s.DurableCall(ctx, nil, intentService, intentOperation, `{"amount":100}`, 0, 0)
		if errCodeOf(result) != 0 {
			t.Fatalf("code %d", callErrorCodeOf(result))
		}
		if replayer.calls != 1 {
			t.Errorf("the key replayer was asked %d times, want 1", replayer.calls)
		}
		if lookupResolver.calls != 0 {
			t.Errorf("the lookup resolver was asked %d times, want 0 -- key replay resolved it first", lookupResolver.calls)
		}
		after := stepRecord(t, ctx, store, wfID, 0)
		if !strings.Contains(after.Response, "keyreplay") {
			t.Errorf("stored response = %q, want the KEY-REPLAY outcome, not the lookup's", after.Response)
		}
	})
}

// TestResolveAmbiguityViaKeyReplay_UnrecordableResolutionIsNotUsed mirrors
// TestResolveAmbiguity_UnrecordableResolutionIsNotUsed: a resolution that
// cannot be persisted must not be used, or the next replay would find the
// row still pending and a service that answers differently (or not at all,
// for a now-expired key) would diverge from what this replay already told
// the guest.
func TestResolveAmbiguityViaKeyReplay_UnrecordableResolutionIsNotUsed(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		wfID := newIntentWorkflow(t, ctx, store, "keyreplay-unrecordable")
		writePendingCall(t, ctx, store, wfID)

		// Complete the row for real first, then hand the session a STALE
		// in-memory history that still shows it pending -- same technique as
		// TestResolveAmbiguity_UnrecordableResolutionIsNotUsed. The resolving
		// UPDATE (conditioned on intent_at IS NOT NULL AND checksum IS NULL)
		// then matches no row, which is what a concurrent resolution looks
		// like from here.
		done := EventRecord{
			Step: 0, EventType: EventTypeCall, Service: intentService, Op: intentOperation,
			Request: `{"amount":100}`, Response: `{"charged":true}`, TimestampMs: time.Now().UnixMilli(),
		}
		payload, _ := eventRecordToPayload(done)
		if err := intentStoreOf(t, store).CompleteCallIntent(ctx, wfID, done, payload,
			computeEventChecksum(done, ""), "", 0); err != nil {
			t.Fatalf("CompleteCallIntent: %v", err)
		}

		replayer := &fakeKeyReplayer{response: `{"charged":true,"via":"keyreplay"}`, outcomes: []IdempotencyReplayOutcome{IdempotencyReplayResolved}}
		ops := map[string]bool{intentService + "." + intentOperation: true}
		caller := &intentTestCaller{store: store, workflowID: wfID}
		s := &execSession{
			engine: NewEngine(nil, caller, WithWorkflowStore(store), WithWorkflowID(wfID),
				WithIdempotencyKeyReplayer(replayer), WithIdempotencyKeyOps(ops)),
			workflowID: wfID,
			isReplay:   true,
			history: []EventRecord{{
				Step: 0, EventType: EventTypeCall, Service: intentService, Op: intentOperation,
				Request: `{"amount":100}`, Pending: true,
			}},
		}

		result := s.DurableCall(ctx, nil, intentService, intentOperation, `{"amount":100}`, 0, 0)
		if errCodeOf(result) == 0 {
			t.Error("a resolution that could not be recorded was used anyway; the next replay would " +
				"find the step unresolved and ask again")
		}
		if !strings.Contains(stepRecord(t, ctx, store, wfID, 0).Response, "charged") {
			t.Error("the already-completed outcome was overwritten")
		}
	})
}
