package main

import (
	"os"
	"time"
)

// connectionShare decides what fraction of the configured connection budget
// this worker may use, given how many workers are currently live.
//
// cleat#1487, second half. The first half (admin.workers) made workers
// countable; this turns a count into a number of connections.
//
// Equal share, per the owner's decision of 2026-09-13: every live worker gets
// budget/N, with a floor of 1 so the reserved bootstrap connection is never
// negotiated away. A worker that cannot get one connection cannot participate
// in the protocol that would tell it to ask for fewer, and the floor is what
// keeps that from being circular.
//
// SHRINKING IS FREE AND GROWING IS NOT. That asymmetry is the whole of this
// type, so it is worth stating why rather than leaving it in the branch
// structure.
//
// Lowering database/sql's SetMaxOpenConns does not interrupt work in flight --
// measured against PostgreSQL 16 with three transactions held open across the
// change: all three stayed usable, and the pool settled to the new cap as they
// were returned. So a worker can always give connections back immediately and
// cannot break a running workflow by doing so.
//
// Taking connections is different, and the reason is a limit of what the
// registry can see: IT KNOWS WHEN A WORKER STOPPED HEARTBEATING, NOT WHEN THAT
// WORKER'S CONNECTIONS WERE RELEASED. A clean shutdown deregisters and its
// connections go with the process; a crashed worker stops heartbeating at once
// and its connections survive until the SERVER reaps them, on timeouts that
// have nothing to do with the expiry window. Growing the moment the count drops
// would claim connections the database still has allocated -- the exact
// overcommit the budget exists to prevent.
//
// So an increase in the worker count is applied immediately and a decrease is
// applied only after holdDown has passed with the count staying down. Being
// slow to grow costs throughput; being slow to shrink costs correctness.
//
// Owner decision, 2026-09-14: ONE conservative hold-down, and no distinction
// between a clean departure and a crash. Deregister is positive evidence that
// connections are gone and expiry is not, so the two could in principle be
// treated differently -- that was considered and declined, because it means
// carrying departure state for a case that does not clearly earn it.
type connectionShare struct {
	budget   int
	holdDown time.Duration
	now      func() time.Time

	// current is the share in force. Zero means nothing has been applied yet,
	// which is distinct from a share of zero -- the floor makes that
	// unreachable.
	current int

	// pending is a larger share observed but not yet taken, and since is when
	// it was first observed.
	pending int
	since   time.Time
}

func newConnectionShare(budget int, holdDown time.Duration, now func() time.Time) *connectionShare {
	if now == nil {
		now = time.Now
	}
	return &connectionShare{budget: budget, holdDown: holdDown, now: now}
}

// want is the share this worker would have if the count were stable.
func (s *connectionShare) want(liveWorkers int) int {
	if liveWorkers < 1 {
		// A count of zero means this worker did not see its own row -- a
		// sweep that raced its registration, or a read against a database
		// that has lost the table. Treating it as "I am alone, take
		// everything" is the one answer that is certainly wrong, so it is
		// read as "I am the only one I can prove", which is the same as 1.
		liveWorkers = 1
	}
	share := s.budget / liveWorkers
	if share < 1 {
		// The reserved bootstrap connection. If the budget cannot cover one
		// per worker the cluster is misconfigured rather than merely
		// contended -- a condition the caller reports; this type does not
		// silently hand back zero.
		return 1
	}
	return share
}

// Observe records a worker count and returns the share to apply now.
func (s *connectionShare) Observe(liveWorkers int) int {
	w := s.want(liveWorkers)

	// Nothing applied yet: take it. This is not "growing" -- the worker
	// registered BEFORE counting, so w already accounts for itself and it is
	// claiming its own share rather than a departed worker's.
	if s.current == 0 {
		s.current = w
		s.pending, s.since = 0, time.Time{}
		return s.current
	}

	if w <= s.current {
		s.current = w
		s.pending, s.since = 0, time.Time{}
		return s.current
	}

	// w > current: a worker left, or the budget rose. Hold.
	if s.pending != w {
		// A different target than the one being held. Restart the clock
		// rather than inheriting it, so a flapping count delays growth
		// instead of accumulating credit toward it.
		s.pending, s.since = w, s.now()
		return s.current
	}
	if s.now().Sub(s.since) >= s.holdDown {
		s.current = w
		s.pending, s.since = 0, time.Time{}
	}
	return s.current
}

// Current is the share in force, or 0 before the first Observe.
func (s *connectionShare) Current() int { return s.current }

// Pending reports a larger share being held down, and how long it has been
// held. Returns 0 when nothing is pending. For logging: a worker sitting below
// its entitlement should be able to say so.
func (s *connectionShare) Pending() (share int, heldFor time.Duration) {
	if s.pending == 0 {
		return 0, 0
	}
	return s.pending, s.now().Sub(s.since)
}

// connectionShareGrowHoldDown is how long a worker waits before taking a
// larger share after the live-worker count drops.
//
// Deliberately longer than the registry's own expiry window, and NOT derived
// from it, because the two answer different questions. A lease is stale at
// twice the heartbeat; that says the worker stopped talking. It does not say
// its connections are gone -- a crashed process leaves them allocated on the
// server until the server's own timeouts reclaim them, which is unrelated to
// anything cleat controls.
//
// Owner decision, 2026-09-14: one conservative hold-down, with no distinction
// between a clean departure and a crash. Deregister IS positive evidence that
// connections were released and could justify growing sooner, but telling the
// two apart means carrying departure state for a case that does not clearly
// earn it. Slow to grow costs throughput; slow to shrink costs correctness.
const connectionShareGrowHoldDown = 2 * time.Minute

// hostnameOrEmpty is os.Hostname without the error, which is diagnostic here:
// a worker whose hostname cannot be read is still a worker.
func hostnameOrEmpty() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}
