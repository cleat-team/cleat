package engine

// What SetScope does today, taken from the code BEFORE it moves onto the step dispatcher (cleat#4211, the scope half). It is the table the move is held
// to, in the shape of a_lock_call_in_each_stop_state_test.go for the lock calls: the two have the same four stop states and the same two arrivals.
//
// Three groups, because SetScope is three things at once:
//
//	taking a scope   refused in the same four states as every other new work, on both arrivals, and before it touches the store;
//	clearing one     SetScope("", ""): no event, never refused, a release when fresh and only the bookkeeping when replayed;
//	replaying one    over a recorded run of failed attempts and then the acquisition, over a record that names another scope, and over a run that
//	                 is followed by something that is not this call's.
//
// The last group has three rows the move CHANGED, and their names keep what each was ("(was: ...)"). They were written as "(today)" rows on the old
// code, so the change is a diff to a table that said what the behaviour was: a first record that names another scope was accepted as this call's,
// and a failed run followed by another scope's record, or by another type, was answered with a bare error that consumed the failed attempts and
// counted nothing. All three are now a divergence, counted once, with nothing consumed, like every other replay's.

import (
	"context"
	"errors"
	"testing"
	"time"
)

type recordingScopeStore struct {
	acquires, releases int
	keys               []string
}

func (p *recordingScopeStore) AcquireConcurrencyKey(_ context.Context, key, _ string, _ time.Duration) (bool, error) {
	p.acquires++
	p.keys = append(p.keys, key)
	return true, nil
}

func (p *recordingScopeStore) ReleaseConcurrencyKey(context.Context, string, string) (bool, error) {
	p.releases++
	return true, nil
}

type scopeStopState struct {
	name string
	opts func() []EngineOption
	mut  func(s *execSession)
	// refused: taking a scope returns the suspend sentinel and does nothing. Clearing one is never refused.
	refused bool
}

func scopeStopStates() []scopeStopState {
	shut := func() []EngineOption {
		ch := make(chan struct{})
		close(ch)
		return []EngineOption{WithShutdownSignal(ch)}
	}
	return []scopeStopState{
		{name: "normal"},
		{name: "a segment's body (defer phase, not draining)", opts: func() []EngineOption { return []EngineOption{WithDeferPhase()} }, refused: true},
		{name: "a segment's defers draining", opts: func() []EngineOption { return []EngineOption{WithDeferPhase()} }, mut: func(s *execSession) { s.inDeferDrain = true }},
		{name: "after a shutdown", opts: shut, refused: true},
		{name: "a defer draining on a shutting-down worker", opts: shut, mut: func(s *execSession) { s.inDeferDrain = true }, refused: true},
		{name: "after an event that could not be written", mut: func(s *execSession) { s.eventWriteFailure = errors.New("write failed") }, refused: true},
	}
}

func scopeSessionIn(st scopeStopState, replay bool) (*execSession, *recordingScopeStore) {
	store := &recordingScopeStore{}
	opts := []EngineOption{WithWorkflowID("scope-states"), WithConcurrencyKeyStore(store)}
	if st.opts != nil {
		opts = append(opts, st.opts()...)
	}
	s := &execSession{
		engine: NewEngine(nil, nil, opts...), workflowID: "scope-states", nowMs: time.Now().UnixMilli(),
		deferrals: map[string]string{}, queryState: map[string]string{}, isReplay: replay,
	}
	if st.mut != nil {
		st.mut(s)
	}
	return s, store
}

func holdScope(s *execSession, objType, instKey string) {
	s.scopeSet = true
	s.scopeObjType, s.scopeInstKey = objType, instKey
	s.scopePrefix = "vo:" + objType + ":" + instKey + ":"
	s.heldScopes = []string{"vo:" + objType + ":" + instKey}
}

func TestSetScopeBehavesAsMeasuredInEachStopState(t *testing.T) {
	ctx := context.Background()
	for _, replay := range []bool{false, true} {
		arrival := map[bool]string{false: "fresh", true: "at the replay frontier"}[replay]
		for _, st := range scopeStopStates() {
			t.Run("a scope taken "+arrival+", "+st.name, func(t *testing.T) {
				s, store := scopeSessionIn(st, replay)
				got := s.SetScope(ctx, nil, "order", "o1", 0, 0)
				if st.refused {
					if got != callSuspendSentinel || len(s.history) != 0 || store.acquires != 0 || s.scopeSet {
						t.Fatalf("want the suspend sentinel, no event, no store call and no scope, got result %d, %d event(s), %d acquire(s), scopeSet %v", got, len(s.history), store.acquires, s.scopeSet)
					}
				} else {
					if got != packSimpleResult(0, 0) || len(s.history) != 1 || store.acquires != 1 || !s.scopeSet {
						t.Fatalf("want a scope taken (result %d, one event, one acquire), got result %d, %d event(s), %d acquire(s), scopeSet %v", packSimpleResult(0, 0), got, len(s.history), store.acquires, s.scopeSet)
					}
					if ev := s.history[0]; ev.EventType != EventTypeScopeAcquired || ev.ScopeKey != "vo:order:o1" || ev.Err != "" {
						t.Fatalf("the recorded event is %+v, want a scope_acquired for vo:order:o1 with no error", ev)
					}
					if s.scopePrefix != "vo:order:o1:" || len(s.heldScopes) != 1 || s.heldScopes[0] != "vo:order:o1" {
						t.Fatalf("scope state after the call: prefix %q, held %v", s.scopePrefix, s.heldScopes)
					}
				}
				if len(s.stepViolations) != 0 {
					t.Fatalf("a spec reported itself in violation: %v", s.stepViolations)
				}
			})
			t.Run("a scope cleared "+arrival+", "+st.name, func(t *testing.T) {
				s, store := scopeSessionIn(st, replay)
				holdScope(s, "order", "o1")
				got := s.SetScope(ctx, nil, "", "", 0, 0)
				// Cleared and never refused, in EVERY state, and it records nothing. A fresh clear releases the key; a replayed one only forgets it,
				// because the segment that ran the step originally already released it.
				wantReleases := 1
				if replay {
					wantReleases = 0
				}
				if got != 0 || len(s.history) != 0 || s.scopeSet || len(s.heldScopes) != 0 || store.releases != wantReleases {
					t.Fatalf("want a clear (result 0, no event, no scope, nothing held, %d release(s)), got result %d, %d event(s), scopeSet %v, held %v, %d release(s)",
						wantReleases, got, len(s.history), s.scopeSet, s.heldScopes, store.releases)
				}
			})
		}
	}
}

// The previous scope is written into the guest's buffer BEFORE history is read and before the call can be refused, and its length is the result.
// The write is not the call's own outcome, so a move must keep it first: a replayed call has to hand the guest what the fresh one did, and a refused
// one still wrote it. The fresh, accepted case is pinned in empty_payload_abi_test.go; these are the two that file does not reach.
func TestSetScopeWritesThePreviousScopeOnReplayAndBeforeARefusal(t *testing.T) {
	const prevPtr, prevMax = 2048, 256
	const want = "vo:cart:c1:"
	read := func(t *testing.T, h *testHostFuncHarness) string {
		t.Helper()
		buf, ok := h.mem.Read(prevPtr, uint32(len(want)))
		if !ok {
			t.Fatal("could not read the previous-scope buffer")
		}
		return string(buf)
	}
	t.Run("replayed over the recorded acquisition", func(t *testing.T) {
		s := replaySessionOver([]EventRecord{{Step: 0, EventType: EventTypeScopeAcquired, ScopeKey: "vo:order:o1"}})
		s.engine.concurrencyKeyStore = &recordingScopeStore{}
		holdScope(s, "cart", "c1")
		h := newTestHostFuncHarness(t, "cleat_set_scope", []byte{wasmI32, wasmI32, wasmI32, wasmI32, wasmI32, wasmI32}, []byte{wasmI64}, true, s)
		got := s.SetScope(h.ctx, h.mod, "order", "o1", prevPtr, prevMax)
		if uint32(got>>32) != uint32(len(want)) || byte(got&0xff) != 0 || read(t, h) != want {
			t.Fatalf("replay answered %#x and left %q in the buffer, want length %d, error code 0 and %q", got, read(t, h), len(want), want)
		}
		if s.stepCount != 1 || s.scopePrefix != "vo:order:o1:" {
			t.Fatalf("the replay did not adopt the recorded scope: stepCount %d, prefix %q", s.stepCount, s.scopePrefix)
		}
	})
	t.Run("a refused call has already written it", func(t *testing.T) {
		s := newTestExecSession()
		s.engine.concurrencyKeyStore = &recordingScopeStore{}
		s.eventWriteFailure = errors.New("write failed")
		holdScope(s, "cart", "c1")
		h := newTestHostFuncHarness(t, "cleat_set_scope", []byte{wasmI32, wasmI32, wasmI32, wasmI32, wasmI32, wasmI32}, []byte{wasmI64}, true, s)
		got := s.SetScope(h.ctx, h.mod, "order", "o1", prevPtr, prevMax)
		if got != callSuspendSentinel {
			t.Fatalf("a call refused for an unwritable event returned %#x, want the suspend sentinel", got)
		}
		if read(t, h) != want {
			t.Fatalf("the buffer holds %q after a refusal, want %q: the previous scope is written before the call can be refused", read(t, h), want)
		}
	})
}

func scopeRec(step int, key, errText string) EventRecord {
	return EventRecord{Step: step, EventType: EventTypeScopeAcquired, ScopeKey: key, Err: errText}
}

// The replay of a run of recorded acquisitions, row by row. The three rows that say "(was: ...)" are the behaviour the move changed, and the
// names keep what each was: this file was written on the old code, and these rows are the diff the move makes to it.
func TestSetScopeReplaysARecordedRun(t *testing.T) {
	const mine = "vo:order:o1"
	const other = "vo:order:o2"
	rows := []struct {
		name        string
		history     []EventRecord
		result      int64
		stepCount   int
		stillReplay bool
		counted     int
		scopeSet    bool
		acquires    int
		// events is the length of history afterwards: a call that went fresh appended its own acquisition.
		events int
	}{
		{"a failed attempt then the acquisition adopts it and acquires nothing", []EventRecord{scopeRec(0, mine, "boom"), scopeRec(1, mine, "")}, 0, 2, true, 0, true, 0, 2},
		{"history ending inside the failed run goes fresh and acquires once", []EventRecord{scopeRec(0, mine, "boom"), scopeRec(1, mine, "boom")}, 0, 3, false, 0, true, 1, 3},
		{"a first record naming another scope is a divergence, counted once, and consumes nothing (was: accepted as this call's)", []EventRecord{scopeRec(0, other, "")}, 1, 0, true, 1, false, 0, 1},
		{"a failed run then another scope's acquisition is a divergence, counted once, and consumes nothing (was: a bare error that consumed the failure and counted nothing)",
			[]EventRecord{scopeRec(0, mine, "boom"), scopeRec(1, other, "")}, 1, 0, true, 1, false, 0, 2},
		{"a failed run then a record of another type is a divergence, counted once, and consumes nothing (was: a bare error that consumed the failure and counted nothing)",
			[]EventRecord{scopeRec(0, mine, "boom"), {Step: 1, EventType: EventTypeCreatePromise, PromiseID: "p"}}, 1, 0, true, 1, false, 0, 2},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			s := replaySessionOver(r.history)
			m := newReplayMetrics(t)
			s.engine.Metrics = m
			store := &recordingScopeStore{}
			s.engine.concurrencyKeyStore = store
			got := s.SetScope(context.Background(), nil, "order", "o1", 0, 0)
			if got != r.result || s.stepCount != r.stepCount || s.isReplay != r.stillReplay || s.scopeSet != r.scopeSet || store.acquires != r.acquires || len(s.history) != r.events {
				t.Fatalf("result %d, stepCount %d, isReplay %v, scopeSet %v, acquires %d, %d event(s); want %d, %d, %v, %v, %d, %d",
					got, s.stepCount, s.isReplay, s.scopeSet, store.acquires, len(s.history), r.result, r.stepCount, r.stillReplay, r.scopeSet, r.acquires, r.events)
			}
			if n := replayFailureCount(t, m); n != r.counted {
				t.Fatalf("cleat_replay_failures_total = %d, want %d", n, r.counted)
			}
		})
	}
}

// An attempt that did not get the scope is recorded as a FAILED attempt, carrying its error and the scope it was for. The error is what makes it a
// prefix record on replay: a record without one reads as the acquisition itself, and a replay would adopt a scope that was never taken. Only a real
// store round trip noticed a contended attempt recorded without it (step_dispatcher_scope_roundtrip_test.go), so this holds it where no database is needed.
func TestAScopeAttemptThatDidNotAcquireIsRecordedWithItsErrorAndItsScope(t *testing.T) {
	for _, r := range []struct {
		name  string
		store ConcurrencyKeyStore
		err   string
	}{
		{"held by another workflow", &acquireNotAcquiredStore{}, "scope held by another workflow"},
		{"a store failure", &acquireErrorStore{}, "store error"},
	} {
		t.Run(r.name, func(t *testing.T) {
			s := newTestExecSession()
			s.engine.concurrencyKeyStore = r.store
			s.SetScope(context.Background(), nil, "order", "o1", 0, 0)
			if len(s.history) != 1 {
				t.Fatalf("want one recorded attempt, got %d", len(s.history))
			}
			ev := s.history[0]
			if ev.EventType != EventTypeScopeAcquired || ev.ScopeKey != "vo:order:o1" || ev.Err != r.err {
				t.Fatalf("the attempt was recorded as %+v, want a scope_acquired for vo:order:o1 carrying the error %q", ev, r.err)
			}
			if s.scopeSet || len(s.heldScopes) != 0 || s.suspendErr == nil {
				t.Fatalf("a failed attempt must leave no scope held and must suspend: scopeSet %v, held %v, suspended %v", s.scopeSet, s.heldScopes, s.suspendErr != nil)
			}
		})
	}
}
