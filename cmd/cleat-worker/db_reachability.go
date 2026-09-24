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
}

// dbTransition says what an observation changed.
type dbTransition int

const (
	dbNoChange dbTransition = iota
	dbBecameUnreachable
	dbBecameReachable
)

// observe records one bounded call and reports whether it flipped the state. The first observation
// of a good call is a change from unknown, not a recovery, and reports dbNoChange.
func (r *dbReachability) observe(now time.Time, elapsed time.Duration, err error) (t dbTransition, outage time.Duration, snap dbSnapshot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	wasKnown, wasReachable := r.known, r.reachable
	r.known = true
	r.lastElapsed = elapsed
	if err == nil {
		r.reachable = true
		r.failures = 0
		r.lastSuccess = now
		r.lastErr = ""
		if wasKnown && !wasReachable {
			t, outage = dbBecameReachable, now.Sub(r.failingSince)
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
func (w *Worker) observeDBProbe(elapsed time.Duration, err error) {
	now := time.Now()
	t, outage, snap := w.dbReach.observe(now, elapsed, err)
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
