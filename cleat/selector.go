package cleat

import (
	"time"
)

// SelectorTimer is returned by Selector.Select when the timer fires.
const SelectorTimer = "__selector_timer__"

// SelectorError is returned by Selector.Select when an error occurs.
const SelectorError = "__selector_error__"

// Selector waits for one of multiple futures to resolve. It provides
// a durable equivalent of Go's select statement for workflow code.
//
// Usage:
//
//	sel := cleat.NewSelector(h)
//	var signalPayload string
//	var timerFired bool
//
//	sel.AddSignal("driver_accepted", &signalPayload)
//	sel.AddTimer(5*time.Minute, &timerFired)
//
//	winner := sel.Select()
//	switch winner {
//	case "driver_accepted":
//	    // handle signal, signalPayload is populated
//	case cleat.SelectorTimer:
//	    // handle timeout, timerFired is true
//	}
//
// Note: AddChildWorkflow support requires host-side infrastructure
// (non-blocking child poll). Without it, child workflow futures are
// not supported and will cause Select to return an error immediately.
type Selector struct {
	h            HostCalls
	signals      []signalFuture
	children     []childFuture
	timers       []timerFuture
	pollInterval time.Duration
	err          error
}

type signalFuture struct {
	name string
	dest *string
}

type childFuture struct {
	runID string
	dest  *string
}

type timerFuture struct {
	deadline time.Time
	fired    *bool
}

// NewSelector creates a Selector backed by the given HostCalls.
func NewSelector(h HostCalls) *Selector {
	return &Selector{
		h:            h,
		pollInterval: 100 * time.Millisecond,
	}
}

// AddSignal adds a signal future. When the named signal arrives before
// Select returns, *dest is populated with the payload and Select returns
// the signal name.
func (s *Selector) AddSignal(name string, dest *string) {
	s.signals = append(s.signals, signalFuture{name: name, dest: dest})
}

// AddChildWorkflow adds a child workflow future. When the child completes
// before Select returns, *dest is populated with the result and Select
// returns the runID.
//
// IMPORTANT: Requires host-side non-blocking child poll support. Without it,
// child futures cause Select to return an error. For now, prefer signals
// and timers.
func (s *Selector) AddChildWorkflow(runID string, dest *string) {
	s.children = append(s.children, childFuture{runID: runID, dest: dest})
}

// AddTimer adds a timer future. When the timeout elapses before Select
// returns, *fired is set to true and Select returns SelectorTimer.
//
// Timers ACCUMULATE, like signals and child workflows. Racing two deadlines
// is the point of having more than one:
//
//	sel.AddTimer(30*time.Second, &soft)
//	sel.AddTimer(5*time.Minute, &hard)
//
// The earliest deadline wins regardless of the order they were added, and
// only the winner's *fired is set — so a caller can tell which deadline woke
// it. Select still returns SelectorTimer for any of them.
//
// Until cleat#1129 this assigned rather than appended, so the second call
// silently discarded the first: no error, no log, nothing at build time, and
// a deadline that simply never fired.
func (s *Selector) AddTimer(timeout time.Duration, fired *bool) {
	s.timers = append(s.timers, timerFuture{
		deadline: s.h.Now().Add(timeout),
		fired:    fired,
	})
}

// earliestTimer returns the index of the timer with the earliest deadline, or
// -1 when none has been added. Ties go to the first added, which keeps Select
// deterministic -- a workflow replaying the same history must take the same
// branch, so "whichever the map iteration reached first" is not available here.
func (s *Selector) earliestTimer() int {
	best := -1
	for i := range s.timers {
		if best == -1 || s.timers[i].deadline.Before(s.timers[best].deadline) {
			best = i
		}
	}
	return best
}

// Err returns the error from the last Select call, if any.
func (s *Selector) Err() error {
	return s.err
}

// Select blocks until one future resolves. It returns the signal name,
// the child workflow runID, SelectorTimer, or SelectorError. The
// corresponding destination pointer is populated before Select returns.
//
// Note: child workflow futures use AwaitChild which suspends the workflow
// if the child has not yet completed. When mixed with signals, signals
// are polled non-blocking first; children are checked via AwaitChild
// which will suspend if no child has completed yet.
func (s *Selector) Select() string {
	for {
		// Check signals non-blocking.
		for i := range s.signals {
			sf := &s.signals[i]
			payload, found, _ := s.h.PollSignal(sf.name)
			if found {
				if sf.dest != nil {
					*sf.dest = payload
				}
				return sf.name
			}
		}

		// Check children. Uses AwaitChild which returns immediately if
		// the child has completed (cached from replay or fresh from store),
		// or suspends the workflow if the child is still running.
		for i := range s.children {
			cf := &s.children[i]
			result, err := s.h.AwaitChild(cf.runID)
			if err == nil {
				if cf.dest != nil {
					*cf.dest = result
				}
				return cf.runID
			}
		}

		// Check timers. The earliest deadline wins, so a caller racing a
		// short deadline against a long one is woken by the short one.
		if i := s.earliestTimer(); i >= 0 {
			if !s.h.Now().Before(s.timers[i].deadline) {
				if s.timers[i].fired != nil {
					*s.timers[i].fired = true
				}
				return SelectorTimer
			}
		}

		// If we have signals to wait for, use AwaitSignals with a timeout
		// set to the nearest deadline.
		if len(s.signals) > 0 {
			names := make([]string, len(s.signals))
			for i, sf := range s.signals {
				names[i] = sf.name
			}

			timeout := 24 * time.Hour // effectively no timeout
			if i := s.earliestTimer(); i >= 0 {
				remaining := s.timers[i].deadline.Sub(s.h.Now())
				if remaining < timeout {
					timeout = remaining
				}
				if timeout < 0 {
					timeout = 0
				}
			}

			result := s.h.AwaitSignals(names, timeout)
			if result.Err != nil {
				// RECORD IT. This returned result.Name and nothing else, and
				// on the error path result.Name is "" -- so a caller switching
				// over the winners it added fell through every case, with
				// Err() still nil and no way to learn anything had gone wrong
				// (cleat#1404).
				//
				// Returning "" is kept: there is no winner, and inventing one
				// would put a caller into a branch whose future never
				// resolved. The empty string plus a non-nil Err() is the
				// distinguishable pair; the empty string alone was not.
				s.err = result.Err
				return ""
			}
			if !result.TimedOut {
				for i := range s.signals {
					if s.signals[i].name == result.Name {
						if s.signals[i].dest != nil {
							*s.signals[i].dest = result.Payload
						}
						return result.Name
					}
				}
			}
			// Timed out — loop back. The timer check at the top of the
			// loop will fire if the deadline has passed.
			continue
		}

		// No signals to wait for -- sleep until the EARLIEST deadline.
		// Sleeping to the last-added one instead is the same defect as the
		// discard it replaced, arriving late rather than never.
		if i := s.earliestTimer(); i >= 0 {
			remaining := s.timers[i].deadline.Sub(s.h.Now())
			if remaining > 0 {
				s.h.DurableSleep(remaining)
			}
			if s.timers[i].fired != nil {
				*s.timers[i].fired = true
			}
			return SelectorTimer
		}

		// Nothing to wait for.
		return ""
	}
}
