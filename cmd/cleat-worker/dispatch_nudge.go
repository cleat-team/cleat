package main

import (
	"sync/atomic"
	"time"
)

// A suspended run whose timer comes due used to be claimed at the next poll tick of the dispatch loop's idle backoff, however close its wake time was (cleat#4491). An idle
// worker sleeps idleTicks x --poll between empty claims, idleTicks growing to maxIdleTicks, and the sleep was cut short only by a workflow on THIS worker reaching a terminal
// status (parentWakeCh), a PostgreSQL NOTIFY (notifyCh), or finding work. A timer coming due sends none of those, so at the default --poll 500ms a suspended timer fired up to 3.0 s
// late on an idle worker (measured 1127 ms at p50 for a 2 s sleep, 1695 ms for a 12 s one, against about 9 ms for the same timer held in memory).
//
// The worker that suspends a run knows when it comes due: it writes the wake time itself. dispatchNudger schedules a nudge for that moment on a channel only the idle backoff reads, so the
// loop polls then instead of at the next rung of the ladder. Nothing about WHICH run is claimed changes: the claim is the same query at an earlier time, and a nudge that finds nothing
// (another worker took the run, or the clocks disagree by more than the slack) costs one claim and falls back to the ladder it was already on.
//
// WHY A SEPARATE CHANNEL and not parentWakeCh: the other selects that read parentWakeCh are DB-down backoffs, up to 30 s, and a nudge that cut one short would send an extra claim at a
// database that is down, once per timer, for as long as it stayed down.
const (
	// dispatchNudgeHorizon bounds how far ahead a nudge is scheduled. A timer is held in memory until it fires, one per suspension, so a run that sleeps for a day must not
	// pin one; and the ladder's lateness (at most maxIdleTicks x --poll) is under a percent of a sleep this long, which is the case it was never a problem for.
	dispatchNudgeHorizon = 15 * time.Minute
	// dispatchNudgeMax bounds the timers pending at once. A worker suspending runs this fast is not idle, and a busy loop resets its own backoff by finding work, so the nudge
	// has nothing to add; past the bound the ladder serves as it always did.
	dispatchNudgeMax = 4096
	// dispatchNudgeSlack is added to the wake time so the claim does not run a hair BEFORE next_wake_at and find nothing (the wake time is written by this worker's clock and
	// compared against the database's).
	dispatchNudgeSlack = 10 * time.Millisecond
)

type dispatchNudger struct {
	ch        chan<- struct{}
	now       func() time.Time
	afterFunc func(d time.Duration, f func()) // time.AfterFunc's shape, replaceable by a test
	pending   atomic.Int64
}

func newDispatchNudger(ch chan<- struct{}) *dispatchNudger {
	return &dispatchNudger{
		ch:        ch,
		now:       time.Now,
		afterFunc: func(d time.Duration, f func()) { time.AfterFunc(d, f) },
	}
}

// at schedules a nudge for wake and reports whether it did. A nil nudger, a nil channel, a zero wake time, a wake beyond dispatchNudgeHorizon and a full set of pending timers all
// return false and change nothing: the loop's ordinary backoff serves those, as it did before.
func (n *dispatchNudger) at(wake time.Time) bool {
	if n == nil || n.ch == nil || wake.IsZero() {
		return false
	}
	d := wake.Sub(n.now())
	if d > dispatchNudgeHorizon {
		return false
	}
	if d < 0 {
		d = 0
	}
	if n.pending.Add(1) > dispatchNudgeMax {
		n.pending.Add(-1)
		return false
	}
	n.afterFunc(d+dispatchNudgeSlack, func() {
		n.pending.Add(-1)
		select {
		case n.ch <- struct{}{}:
		default: // one token is already waiting: the loop will poll once for both
		}
	})
	return true
}
