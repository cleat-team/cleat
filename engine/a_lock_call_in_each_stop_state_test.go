package engine

import (
	"context"
	"errors"
	"testing"
	"time"
)

// cleat#4211 (the lock half). AcquireLock and ReleaseLock move onto the step dispatcher, and the migration is meant to change nothing but the
// identity check, so this test records what the two calls DO in each state stopBeforeNewWork distinguishes, as measured on develop 6fc6340d BEFORE the
// migration (2026-11-20, an execSession with a recording lock store), and holds the migrated code to the same table.
//
// The table is not what the dispatcher design assumed. Its rule is "refuse every non-Pure step at the frontier in a segment body", argued from a
// segment's body alone. stopBeforeNewWork is also true after a worker shutdown and after an event that could not be written, and ReleaseLock is
// refused in none of the four states below, where AcquireLock is refused in all four:
//
//	a segment's body         AcquireLock refused   ReleaseLock released and recorded
//	a shutdown               AcquireLock refused   ReleaseLock released and recorded
//	a defer draining on a    AcquireLock refused   ReleaseLock released and recorded   <- the case that decides it
//	shutting-down worker
//	an unwritable event      AcquireLock refused   ReleaseLock released and recorded
//
// A cleanup that is draining on a shutting-down worker cannot take a lock but can give one back, and applying the blanket rule would leave the lock
// held until its lease ran out. So ReleaseLock keeps its behaviour by declaring `Refuse: neverRefuse`, and whether a segment's body should
// release at all is a separate decision. Each row is run twice: the call as the guest makes it fresh, and the same call reached at the replay frontier
// (a defer segment starts in replay, so its body's first new call arrives that way, cleat#4101).

type recordingLockStore struct {
	acquires, releases int
	ttl                time.Duration
}

func (p *recordingLockStore) AcquireConcurrencyKey(_ context.Context, _, _ string, ttl time.Duration) (bool, error) {
	p.acquires++
	p.ttl = ttl
	return true, nil
}

func (p *recordingLockStore) ReleaseConcurrencyKey(context.Context, string, string) (bool, error) {
	p.releases++
	return true, nil
}

type lockStopState struct {
	name string
	opts func() []EngineOption
	mut  func(s *execSession)
	// acquireRefused: AcquireLock returns the suspend sentinel and does nothing. ReleaseLock is never refused.
	acquireRefused bool
}

func lockStopStates() []lockStopState {
	shut := func() []EngineOption {
		ch := make(chan struct{})
		close(ch)
		return []EngineOption{WithShutdownSignal(ch)}
	}
	return []lockStopState{
		{name: "normal"},
		{name: "a segment's body (defer phase, not draining)", opts: func() []EngineOption { return []EngineOption{WithDeferPhase()} }, acquireRefused: true},
		{name: "a segment's defers draining", opts: func() []EngineOption { return []EngineOption{WithDeferPhase()} }, mut: func(s *execSession) { s.inDeferDrain = true }},
		{name: "after a shutdown", opts: shut, acquireRefused: true},
		{name: "a defer draining on a shutting-down worker", opts: shut, mut: func(s *execSession) { s.inDeferDrain = true }, acquireRefused: true},
		{name: "after an event that could not be written", mut: func(s *execSession) { s.eventWriteFailure = errors.New("write failed") }, acquireRefused: true},
	}
}

func lockSessionIn(st lockStopState, replay bool) (*execSession, *recordingLockStore) {
	store := &recordingLockStore{}
	opts := []EngineOption{WithWorkflowID("lock-states"), WithConcurrencyKeyStore(store)}
	if st.opts != nil {
		opts = append(opts, st.opts()...)
	}
	s := &execSession{
		engine: NewEngine(nil, nil, opts...), workflowID: "lock-states", nowMs: time.Now().UnixMilli(),
		deferrals: map[string]string{}, queryState: map[string]string{}, isReplay: replay,
	}
	if st.mut != nil {
		st.mut(s)
	}
	return s, store
}

func TestAcquireLockAndReleaseLockBehaveAsMeasuredInEachStopState(t *testing.T) {
	ctx := context.Background()
	for _, replay := range []bool{false, true} {
		arrival := map[bool]string{false: "fresh", true: "at the replay frontier"}[replay]
		for _, st := range lockStopStates() {
			t.Run("AcquireLock "+arrival+", "+st.name, func(t *testing.T) {
				s, store := lockSessionIn(st, replay)
				got := s.AcquireLock(ctx, nil, "k", 1000)
				if st.acquireRefused {
					if got != callSuspendSentinel || len(s.history) != 0 || store.acquires != 0 {
						t.Fatalf("want the suspend sentinel and no event and no store call, got result %d, %d event(s), %d acquire(s)", got, len(s.history), store.acquires)
					}
				} else {
					if got != packAcquireLockResult(true, 0) || len(s.history) != 1 || store.acquires != 1 {
						t.Fatalf("want acquired (%d), one event, one acquire, got result %d, %d event(s), %d acquire(s)", packAcquireLockResult(true, 0), got, len(s.history), store.acquires)
					}
					// The lock's time to live reaches the store and the recorded event. Found by falsification: dropping it from the step's arguments
					// turned nothing red, so a move onto the dispatcher could have lost it without a test saying so.
					if store.ttl != guestMsDuration(1000) || s.history[0].LockTTLMs != 1000 || s.history[0].LockKey != "k" {
						t.Fatalf("the TTL or key did not reach the store and the event: store ttl %v (want %v), event ttl %d (want 1000), event key %q (want k)", store.ttl, guestMsDuration(1000), s.history[0].LockTTLMs, s.history[0].LockKey)
					}
				}
				if len(s.stepViolations) != 0 {
					t.Fatalf("a spec reported itself in violation: %v", s.stepViolations)
				}
			})
			t.Run("ReleaseLock "+arrival+", "+st.name, func(t *testing.T) {
				s, store := lockSessionIn(st, replay)
				got := s.ReleaseLock(ctx, nil, "k")
				// Released and recorded in EVERY state: this is the row that the blanket refusal rule would have changed.
				if got != 0 || len(s.history) != 1 || store.releases != 1 || s.history[0].EventType != EventTypeReleaseLock || s.history[0].LockKey != "k" {
					t.Fatalf("want a release recorded (result 0, one release_lock event for k, one store release), got result %d, %d event(s), %d release(s)", got, len(s.history), store.releases)
				}
				if len(s.stepViolations) != 0 {
					t.Fatalf("a spec reported itself in violation: %v", s.stepViolations)
				}
			})
		}
	}
}
