package cleat

import (
	"fmt"
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
// Child workflows race the same way (AddChildWorkflow): a child that is
// still running does not stop a timer or a signal from winning. See Select
// for how a pending child is waited on.
type Selector struct {
	h        HostCalls
	signals  []signalFuture
	children []childFuture
	timers   []timerFuture
	err      error

	// While a child is pending alongside a timer or a signal, Select polls it
	// (PollChild) between durable waits: the first wait is minChildPoll, each
	// one after doubles, up to maxChildPoll. A child's completion does not
	// wake a signal wait, so this bounds how late a completed child is seen;
	// the doubling bounds how many durable waits a long race records.
	minChildPoll time.Duration
	maxChildPoll time.Duration
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
		minChildPoll: time.Second,
		maxChildPoll: 30 * time.Second,
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
// returns the runID. A child that FAILED also resolves its future: Select
// returns its runID, *dest is left alone, and Err() reports the failure.
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
// the child workflow runID, SelectorTimer, or "" with a non-nil Err() when
// the host reports an error. The corresponding destination pointer is
// populated before Select returns.
//
// Every future is checked without blocking -- signals with PollSignal,
// children with PollChild, timers against the durable clock -- and only then
// does Select wait, so whichever resolves first wins:
//
//   - signals pending: AwaitSignals until the earliest timer, or until the
//     next child poll if a child is pending;
//   - no signals, a timer or a pending child to wait out: DurableSleep for
//     the same bound;
//   - only children: AwaitAnyChild, which suspends until one completes and
//     needs no polling.
//
// Until cleat#3369 the children were checked with AwaitChild, which suspends
// on a running child: the workflow then woke only for that child, and a
// timer or signal added beside it could never win.
func (s *Selector) Select() string {
	s.err = nil
	poll := s.minChildPoll
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

		// Check children non-blocking. PollChild answers as of the durable
		// clock, so a replay sees the same "running" the first run did.
		var pending []string
		for i := range s.children {
			cf := &s.children[i]
			status, result, err := s.h.PollChild(cf.runID)
			switch status {
			case "completed":
				if cf.dest != nil {
					*cf.dest = result
				}
				return cf.runID
			case "failed":
				s.err = fmt.Errorf("child workflow %s failed: %v", cf.runID, err)
				return cf.runID
			case "running":
				pending = append(pending, cf.runID)
			default:
				s.err = fmt.Errorf("polling child workflow %s: status %q: %v", cf.runID, status, err)
				return ""
			}
		}

		// Check timers. The earliest deadline wins, so a caller racing a
		// short deadline against a long one is woken by the short one.
		timer := s.earliestTimer()
		if timer >= 0 && !s.h.Now().Before(s.timers[timer].deadline) {
			if s.timers[timer].fired != nil {
				*s.timers[timer].fired = true
			}
			return SelectorTimer
		}

		// Only children left to wait for: AwaitAnyChild suspends until one
		// completes, with nothing to poll.
		if len(s.signals) == 0 && timer < 0 && len(pending) > 0 {
			runID, result, err := s.h.AwaitAnyChild(pending)
			for i := range s.children {
				cf := &s.children[i]
				if cf.runID != runID {
					continue
				}
				if err != nil {
					s.err = fmt.Errorf("child workflow %s failed: %v", runID, err)
				} else if cf.dest != nil {
					*cf.dest = result
				}
				return runID
			}
			s.err = fmt.Errorf("awaiting child workflows %v: %v", pending, err)
			return ""
		}

		// How long the next wait may last: until the earliest deadline, and
		// no longer than the next child poll while a child is pending.
		wait := time.Duration(-1)
		if timer >= 0 {
			wait = s.timers[timer].deadline.Sub(s.h.Now())
		}
		toDeadline := timer >= 0
		if len(pending) > 0 && (wait < 0 || poll < wait) {
			wait, toDeadline = poll, false
			if poll *= 2; poll > s.maxChildPoll {
				poll = s.maxChildPoll
			}
		}

		if len(s.signals) > 0 {
			names := make([]string, len(s.signals))
			for i, sf := range s.signals {
				names[i] = sf.name
			}
			if wait < 0 {
				wait = 24 * time.Hour // effectively no timeout
			}
			result := s.h.AwaitSignals(names, wait)
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
			// Timed out -- loop back. A timed-out wait is not proof the
			// deadline passed (the clock decides that at the top of the
			// loop), and a pending child is polled again.
			continue
		}

		if wait < 0 {
			// Nothing to wait for.
			return ""
		}
		// No signals -- sleep until the EARLIEST deadline, or the next child
		// poll. Sleeping to the last-added deadline instead is the same
		// defect as the discard it replaced, arriving late rather than never.
		if wait > 0 {
			s.h.DurableSleep(wait)
		}
		if toDeadline {
			if s.timers[timer].fired != nil {
				*s.timers[timer].fired = true
			}
			return SelectorTimer
		}
		// Woken to poll a pending child: check everything again.
	}
}
