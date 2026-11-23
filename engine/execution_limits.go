package engine

import (
	"context"
	"sync"
	"time"
)

// The wall-clock limits of ONE execution -- --max-workflow-duration (the workflow deadline) and --wasm-wall-clock-ceiling -- are per SEGMENT, and a segment
// that suspends and resumes gets a fresh deadline each time (the flag's own help says so, cmd/cleat-worker/config.go). A run that is HELD on the worker
// instead of suspended (resident.go) has to get the same treatment, or residency changes what a limit means: work before the wait and work after it would add
// up against one limit where a suspension would have given the work after the wait a limit of its own (cleat#4162).
//
// So these limits are contexts that can be PAUSED while a run is held and RESTARTED when it is woken. A paused limit does not run, so a hold cannot expire
// it; a restarted one has its full duration again, which is what the resumed segment of a suspended run would have. Only the deadline contexts are
// special: cancellation (a terminate, a cancel, the worker's own shutdown) still reaches a held run at once, because the parent's cancellation is propagated
// whether or not the deadline is running.
//
// The registry is carried in the context as a value, and not found by asserting the context's type, because a context.WithValue between two limits (the
// workflow span is one) hides the method set of the one beneath it and a type assertion would silently pause only the outer limit.

type executionLimitsKey struct{}

// executionLimits is the set of restartable deadlines of one execution, and the things that must be told when they restart.
type executionLimits struct {
	mu        sync.Mutex
	limits    []*restartableDeadline
	onRestart []func()
}

func withExecutionLimits(ctx context.Context, l *executionLimits) context.Context {
	return context.WithValue(ctx, executionLimitsKey{}, l)
}

// executionLimitsFrom is nil when the execution has none registered, which every caller tolerates: nothing is paused, nothing restarts.
func executionLimitsFrom(ctx context.Context) *executionLimits {
	if ctx == nil {
		return nil
	}
	l, _ := ctx.Value(executionLimitsKey{}).(*executionLimits)
	return l
}

// withRestartableTimeout is context.WithTimeout whose deadline the execution's registry can pause and restart. Without a registry in ctx it IS
// context.WithTimeout, so a caller outside an execution (a test, the wazero path before it is wired) behaves exactly as it did.
func withRestartableTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	reg := executionLimitsFrom(ctx)
	if reg == nil {
		return context.WithTimeout(ctx, d)
	}
	c := &restartableDeadline{parent: ctx, d: d, done: make(chan struct{}), deadline: time.Now().Add(d)}
	// Both callbacks can run before this function returns (a parent that has already ended, a duration of nanoseconds), and both take c.mu, so the fields
	// they read are assigned under it.
	c.mu.Lock()
	c.stopParent = context.AfterFunc(ctx, func() { c.finish(ctx.Err()) })
	c.timer = time.AfterFunc(d, func() { c.finish(context.DeadlineExceeded) })
	c.mu.Unlock()
	// context.WithDeadline ends a deadline that is already past SYNCHRONOUSLY, before it returns, and callers rely on it (the executor reports a spent deadline as a
	// timeout by reading Err straight after: a_spent_deadline_is_not_a_one_millisecond_budget_test.go uses a 1ns workflow timeout). An asynchronous timer alone is
	// not that, so this matches it.
	if d <= 0 || time.Until(c.deadline) <= 0 {
		c.finish(context.DeadlineExceeded)
	}
	reg.mu.Lock()
	reg.limits = append(reg.limits, c)
	reg.mu.Unlock()
	return c, func() { c.finish(context.Canceled) }
}

// restartableDeadline is a context with a deadline of d that can be paused and restarted. It is a context.Context in full: Err reports
// context.DeadlineExceeded when its own timer ended it, which is what the executor's expiry branches compare against.
type restartableDeadline struct {
	parent     context.Context
	d          time.Duration
	stopParent func() bool

	mu       sync.Mutex
	timer    *time.Timer
	deadline time.Time
	paused   bool
	pausedAt time.Time
	done     chan struct{}
	err      error
}

func (c *restartableDeadline) finish(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return
	}
	c.err = err
	close(c.done)
	if c.timer != nil {
		c.timer.Stop()
	}
	if c.stopParent != nil {
		c.stopParent()
	}
}

func (c *restartableDeadline) Done() <-chan struct{} { return c.done }
func (c *restartableDeadline) Value(key any) any     { return c.parent.Value(key) }

func (c *restartableDeadline) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Deadline is the EARLIER of this limit's own deadline and the parent's. While this limit is paused it has none of its own, so a paused run reports only the
// deadlines that cannot be paused, which is the thing a hold has to check before it starts.
func (c *restartableDeadline) Deadline() (time.Time, bool) {
	c.mu.Lock()
	own, running := c.deadline, !c.paused
	c.mu.Unlock()
	pd, pok := c.parent.Deadline()
	switch {
	case running && pok && pd.Before(own):
		return pd, true
	case running:
		return own, true
	}
	return pd, pok
}

type pauseResult int

const (
	pausedNow     pauseResult = iota + 1 // the clock was running and has been stopped
	alreadyPaused                        // not ours to end: another hold has it
	limitEnded                           // expired, cancelled, or its timer is firing: the run must not be held
)

// pause stops the clock. limitEnded includes a timer that has fired and not yet finished the limit (Stop reports false), because that expiry is already on
// its way and a hold that went ahead would end in the timeout this exists to prevent.
func (c *restartableDeadline) pause() pauseResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	// The err check and the Stop check overlap by design: a limit that ended has had its timer stopped, so either alone reports limitEnded. Both stay so that
	// removing one cannot reopen the case (a hold through an expiry that is already on its way).
	switch {
	case c.err != nil:
		return limitEnded
	case c.paused:
		return alreadyPaused
	case !c.timer.Stop():
		return limitEnded
	}
	c.paused, c.pausedAt = true, time.Now()
	return pausedNow
}

// resume ends a pause. restart gives the limit its full duration again (a hold that completed: the segment the wait ends is a fresh one); without it the
// limit carries on with the time it had left when it was paused (a hold that was never entered).
func (c *restartableDeadline) resume(restart bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil || !c.paused {
		return
	}
	c.paused = false
	left := c.d
	if !restart {
		left = time.Until(c.deadline.Add(time.Since(c.pausedAt)))
		if left < 0 {
			left = 0
		}
	}
	c.deadline = time.Now().Add(left)
	c.timer.Reset(left)
}

// pauseExecutionLimits pauses every restartable deadline of the execution ctx belongs to, and returns what ends the pause: resume(true) restarts them and
// tells the registered listeners (the backend's own fence budget), resume(false) puts them back as they were. ok is false when ctx has no registry (nothing
// to pause, and the caller is on its own for any deadline ctx carries) or when a limit has already ended; in the second case nothing is left paused.
func pauseExecutionLimits(ctx context.Context) (resume func(restart bool), ok bool) {
	reg := executionLimitsFrom(ctx)
	if reg == nil {
		return func(bool) {}, true
	}
	reg.mu.Lock()
	limits := append([]*restartableDeadline(nil), reg.limits...)
	listeners := append([]func(){}, reg.onRestart...)
	reg.mu.Unlock()

	var paused []*restartableDeadline
	for _, l := range limits {
		switch l.pause() {
		case pausedNow:
			paused = append(paused, l)
		case limitEnded:
			for _, p := range paused {
				p.resume(false)
			}
			return nil, false
		}
	}
	return func(restart bool) {
		for _, p := range paused {
			p.resume(restart)
		}
		if restart {
			for _, f := range listeners {
				f()
			}
		}
	}, true
}

// onLimitsRestart registers f to run when a hold restarts this execution's limits, after they have been restarted.
func onLimitsRestart(ctx context.Context, f func()) {
	if reg := executionLimitsFrom(ctx); reg != nil {
		reg.mu.Lock()
		reg.onRestart = append(reg.onRestart, f)
		reg.mu.Unlock()
	}
}
