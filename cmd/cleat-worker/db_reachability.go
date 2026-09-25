package main

import (
	"context"
	"errors"
	"sync"
	"time"
)

// dbReachability is what this worker knows about whether its database answers (cleat#2007).
//
// It is fed by every deadline-bounded database call the worker already makes (the heartbeat, the idle
// ping, the reaper: Worker.probeBoundedCall), so there is no second probe and no second connection. A
// call counts as failed when it returns an error OR when it returns success after its deadline, which
// is #2005's rule: a database that delays rather than refuses (docker pause) never returns a
// connection error at all, and only the deadline catches it.
//
// The zero value is ready: nothing observed yet, which is `known == false` and reads as "starting".
type dbReachability struct {
	mu           sync.Mutex
	known        bool
	reachable    bool
	failures     int64 // failed calls in a row, 0 after a success
	lastSuccess  time.Time
	failingSince time.Time
	lastErr      string // for an authenticated admin only: it can contain a host name or a DSN fragment
	lastElapsed  time.Duration

	// Three more facts, each closing a hole cleat-review measured in the first version (cleat#2007).
	//
	// lastSuccessStart orders evidence. A failure from a call that STARTED before the latest successful
	// call began is older than the success and is dropped: call A hangs, call B succeeds, and A's deadline
	// timer would otherwise mark a database that just answered as unreachable.
	lastSuccessStart time.Time
	superseded       int64 // failures dropped for that reason, so a test can see it happened
	// lastObserved and inFlight say whether an "unreachable" verdict is still evidence. It is fresh while
	// a call is in flight (a paused database holds the call, which is the only observation there will be)
	// or while the last observation is recent; otherwise nothing is watching the database and the verdict
	// has expired.
	lastObserved time.Time
	inFlight     int
	// recoveredAt is when the database last went from unreachable to reachable, for the recovery grace.
	recoveredAt time.Time

	// clock is time.Now in production; tests replace it.
	clock func() time.Time
}

func (r *dbReachability) now() time.Time {
	if r.clock != nil {
		return r.clock()
	}
	return time.Now()
}

// dbSnapshot is a copy of the state, for the health handlers and the metrics.
type dbSnapshot struct {
	Known        bool
	Reachable    bool
	Failures     int64
	LastSuccess  time.Time
	FailingSince time.Time
	LastError    string
	LastElapsed  time.Duration
	LastObserved time.Time
	RecoveredAt  time.Time
	InFlight     int
	Superseded   int64
	// LastSuccessStart is when the latest successful bounded call BEGAN.
	LastSuccessStart time.Time
}

// dbTransition says what an observation changed.
type dbTransition int

const (
	dbNoChange dbTransition = iota
	dbBecameUnreachable
	dbBecameReachable
)

// beginCall and endCall bracket one bounded call, so a call that never returns is visible as such.
func (r *dbReachability) beginCall() {
	r.mu.Lock()
	r.inFlight++
	r.mu.Unlock()
}

func (r *dbReachability) endCall() {
	r.mu.Lock()
	r.inFlight--
	r.mu.Unlock()
}

// touch records that a call which had already been reported failed has now returned. It changes nothing
// about reachability (the deadline verdict stands), but the call was what kept the evidence live while it
// hung, and its return must not make the verdict look abandoned before the next probe can refresh it.
// Measured against a real `docker pause`: without this, /livez answered 503 half a second after the
// database came back.
func (r *dbReachability) touch() {
	r.mu.Lock()
	r.lastObserved = r.now()
	r.mu.Unlock()
}

// observe records one bounded call, which began at started, and reports whether it flipped the state.
// The first observation of a good call is a change from unknown, not a recovery, and reports dbNoChange.
func (r *dbReachability) observe(started, now time.Time, elapsed time.Duration, err error) (t dbTransition, outage time.Duration, snap dbSnapshot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil && started.Before(r.lastSuccessStart) {
		// Older than a success we already have: not evidence about the database NOW.
		r.superseded++
		return dbNoChange, 0, r.snapshotLocked()
	}
	wasKnown, wasReachable := r.known, r.reachable
	r.known = true
	r.lastElapsed = elapsed
	r.lastObserved = now
	if err == nil {
		r.reachable = true
		r.failures = 0
		r.lastSuccess = now
		if started.After(r.lastSuccessStart) {
			r.lastSuccessStart = started
		}
		r.lastErr = ""
		if wasKnown && !wasReachable {
			t, outage = dbBecameReachable, now.Sub(r.failingSince)
			r.recoveredAt = now
		}
		r.failingSince = time.Time{}
	} else {
		r.reachable = false
		r.failures++
		r.lastErr = err.Error()
		if !wasKnown || wasReachable {
			r.failingSince = now
			t = dbBecameUnreachable
		}
	}
	return t, outage, r.snapshotLocked()
}

func (r *dbReachability) snapshot() dbSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshotLocked()
}

func (r *dbReachability) snapshotLocked() dbSnapshot {
	return dbSnapshot{
		Known: r.known, Reachable: r.reachable, Failures: r.failures,
		LastSuccess: r.lastSuccess, FailingSince: r.failingSince,
		LastError: r.lastErr, LastElapsed: r.lastElapsed,
		LastObserved: r.lastObserved, RecoveredAt: r.recoveredAt, InFlight: r.inFlight, Superseded: r.superseded,
		LastSuccessStart: r.lastSuccessStart,
	}
}

// dbFailureClass is the short, fixed-vocabulary reason for a failed call, for the log line an
// operator greps for. The full error text is kept separately and is not put in a public body.
func dbFailureClass(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline exceeded"
	case isConnectionError(err):
		return "connection error"
	default:
		return "error or ran past its deadline"
	}
}

// observeDBProbe folds one bounded call into the reachability state, exports it, and logs the two
// transitions an operator needs at 3am: the first failure, and the recovery with the outage length.
func (w *Worker) observeDBProbe(started time.Time, elapsed time.Duration, err error) {
	now := w.dbReach.now()
	t, outage, snap := w.dbReach.observe(started, now, elapsed, err)
	if w.Metrics != nil {
		w.Metrics.RecordDBProbe(context.Background(), w.dbDialect, elapsed, snap.Reachable, snap.Failures, snap.LastSuccess)
	}
	if w.logger == nil {
		return
	}
	switch t {
	case dbBecameUnreachable:
		w.logger.WarnContext(context.Background(), "database unreachable ("+dbFailureClass(err)+")",
			"worker_id", w.id, "elapsed", elapsed.Round(time.Millisecond), "error", err)
	case dbBecameReachable:
		w.logger.InfoContext(context.Background(), "database reachable again after "+outage.Round(time.Second).String(),
			"worker_id", w.id)
	}
}
