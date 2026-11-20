package engine

import (
	"context"
	"fmt"
	"time"
)

// A wait that cannot be answered yet either SUSPENDS (the run leaves the worker and a later claim replays it) or PARKS (the run stays where it is
// and a wake source resumes it). parkOrSuspend is where that choice is made, and it is the only place a wait sets suspendErr once the waits have
// moved onto it (cleat#4152, W2b; the design is docs/design/replay-step-dispatcher.md, the parking mechanism is WS-cleat-2's).
//
// In this change the choice is always SUSPEND: there is no wake source in production, so every call returns parkRefused after setting exactly the
// suspension the call site used to set inline, and a characterization table (a_sleep_behaves_as_recorded_before_the_park_helper_test.go) shows
// the first caller, DurableSleep, behaves as it did. What this adds is the shape the parking work builds against, with a seam to test it: the
// engine's wakeSource field, which only tests assign until the real one exists.

// wakeKind says what a parked wait is waiting FOR. The values are the label set the parking metrics use, and String() is what they read, so there
// is one name for each and not a second free-text one that could disagree. The zero value is not a kind: a request that forgets to set one is
// invalid and suspends.
type wakeKind int

const (
	wakeTimer wakeKind = iota + 1
	wakeSignal
	wakePromise
	wakeChild
	wakeUpdate
)

func (k wakeKind) String() string {
	switch k {
	case wakeTimer:
		return "sleep"
	case wakeSignal:
		return "signals"
	case wakePromise:
		return "promise"
	case wakeChild:
		return "child"
	case wakeUpdate:
		return "update"
	}
	return fmt.Sprintf("wakeKind(%d)", int(k))
}

// parkOutcome is what a wait learns. The zero value is the safe one: a caller that does not look still suspends.
type parkOutcome int

const (
	// parkRefused: the run was not parked. The suspension has been set; the caller returns its suspend result. Every call returns this today.
	parkRefused parkOutcome = iota
	// For a TIMER (wakeTimer) the outcome is advisory and the CLOCK decides: a caller completes only when the clock says the deadline has passed,
	// and an early wake suspends as a refusal would. A timer has no store to re-poll, and a sleep is served by the clock on replay.
	//
	// parkWoke: something may have changed. The caller RE-POLLS the live store through the same read a replay uses, and the poll may find nothing
	// (a wake for another wait, a batched check that saw another run's sequence move), in which case it parks again with the same deadline and an
	// updated Baseline. The wake carries no payload, so it cannot disagree with the store.
	parkWoke
	// parkDeadline: the deadline passed while parked. The caller takes its timed-out result.
	parkDeadline
)

// parkRequest is everything a wake source needs to decide when to resume the run, and everything a refusal needs to suspend it.
type parkRequest struct {
	Kind wakeKind
	// Key names what is waited on: the promise id, the child id, the signal-name set. Empty for a timer.
	Key string
	// Baseline is RESERVED and unused in v1: every call passes 0. It was meant to carry what the call's last poll read, so that a wake source
	// could compare after registering and catch a change made in the lost-wakeup window; no store read exposes signal_seq or promise_seq to a call
	// (they are bumped on delivery and snapshotted at claim, in SQL only), and adding one on three dialects was not worth it. The window is closed
	// by ORDERING instead (WS-cleat-2, #4152): the wake source registers on the first Wait and answers parkWoke at once, and the call re-polls
	// through the same live read a replay uses, so anything delivered before the registration is found there. A kind that needs a compared value
	// (the child and update waits are still undecided: W3, W1) may use this field again; until then it is the zero it always was.
	Baseline int64
	// DeadlineMs is the absolute wake time in unix ms, 0 for none. A timer must have one.
	DeadlineMs int64
	// Reason and Until are the suspension a refusal sets, so that each call keeps its own message and wake time exactly as it had them inline.
	Reason string
	Until  time.Time
}

func (r parkRequest) validate() error {
	switch {
	case r.Kind < wakeTimer || r.Kind > wakeUpdate:
		return fmt.Errorf("park request has no valid kind (%d)", int(r.Kind))
	case r.DeadlineMs < 0:
		return fmt.Errorf("park request for %s has a negative deadline (%d)", r.Kind, r.DeadlineMs)
	case r.Kind == wakeTimer && r.DeadlineMs == 0:
		return fmt.Errorf("a %s park request must carry its deadline", r.Kind)
	}
	return nil
}

// wakeSource resumes parked runs. There is none in production yet; the engine's wakeSource field is nil, which means every wait suspends.
type wakeSource interface {
	// Wait blocks until the run should be resumed or its deadline passes, registering BEFORE it compares req.Baseline against the live value, so a
	// change made between the caller's poll and this registration is seen and not lost. Anything other than parkWoke or parkDeadline suspends.
	Wait(ctx context.Context, req parkRequest) parkOutcome
}

// canPark reports whether this session may hold a run in memory instead of suspending it. A refusal is always safe, so this is deliberately the
// conservative reading of "is the worker free to keep waiting here":
//   - not in a defer segment, not after a failed event write, not on a worker that is shutting down: exactly stopBeforeNewWork, so there is one
//     list of those and not a second that could drift from it;
//   - not while draining a segment's defers, because that run is already stopping;
//   - not mid-history. A wait that reaches the END of history is at the frontier and may park, and that includes a sleep, which records nothing
//     and so never calls exitReplay: after a wake, the sleep is the call that reaches the frontier while isReplay is still true.
func (s *execSession) canPark() bool {
	if s.engine == nil || s.engine.wakeSource == nil {
		return false
	}
	if s.stopBeforeNewWork() || s.inDeferDrain {
		return false
	}
	if s.isReplay && s.stepCount < len(s.history) {
		return false
	}
	return true
}

// parkOrSuspend is the one place a wait decides between parking and suspending. It returns parkRefused after setting the suspension from req, or
// parkWoke / parkDeadline from the wake source when the run was parked and has been resumed.
//
// An invalid request suspends anyway and is noted in parkViolations: suspending is the safe failure, and a caller that built a bad request has
// still waited.
func (s *execSession) parkOrSuspend(ctx context.Context, req parkRequest) parkOutcome {
	if err := req.validate(); err != nil {
		s.parkViolations = append(s.parkViolations, err.Error())
		return s.suspendFor(req)
	}
	if s.canPark() {
		// The EXECUTION's context when the session has one, not the host call's: the latter is context.Background() on every wasmtime path, so waiting
		// on it would hold a run through its own ceiling, a cancel and a terminate (execSession.execCtx).
		waitCtx := ctx
		if s.execCtx != nil {
			waitCtx = s.execCtx
		}
		switch out := s.engine.wakeSource.Wait(waitCtx, req); out {
		case parkWoke, parkDeadline:
			return out
		}
	}
	return s.suspendFor(req)
}

// suspendFor is the suspension a refused wait sets.
func (s *execSession) suspendFor(req parkRequest) parkOutcome {
	s.suspendErr = &SuspendError{Reason: req.Reason, Until: req.Until}
	return parkRefused
}
