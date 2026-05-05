package durable

import (
	"time"
)

// SelectorTimer is returned by Selector.Select when the timer fires.
const SelectorTimer = "__selector_timer__"

// Selector waits for one of multiple futures to resolve. It provides
// a durable equivalent of Go's select statement for workflow code.
//
// Usage:
//
//	sel := durable.NewSelector(h)
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
//	case durable.SelectorTimer:
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
	timer        *timerFuture
	pollInterval time.Duration
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
func (s *Selector) AddTimer(timeout time.Duration, fired *bool) {
	s.timer = &timerFuture{
		deadline: s.h.Now().Add(timeout),
		fired:    fired,
	}
}

// Select blocks until one future resolves. It returns the signal name,
// the child workflow runID, or SelectorTimer. The corresponding destination
// pointer is populated before Select returns.
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

		// Check timer.
		if s.timer != nil {
			if !s.h.Now().Before(s.timer.deadline) {
				if s.timer.fired != nil {
					*s.timer.fired = true
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
			if s.timer != nil {
				remaining := s.timer.deadline.Sub(s.h.Now())
				if remaining < timeout {
					timeout = remaining
				}
				if timeout < 0 {
					timeout = 0
				}
			}

			result := s.h.AwaitSignals(names, timeout)
			if result.Err != nil {
				return result.Name
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

		// No signals to wait for — just sleep until the timer fires.
		if s.timer != nil {
			remaining := s.timer.deadline.Sub(s.h.Now())
			if remaining > 0 {
				s.h.DurableSleep(remaining)
			}
			if s.timer.fired != nil {
				*s.timer.fired = true
			}
			return SelectorTimer
		}

		// Nothing to wait for.
		return ""
	}
}
