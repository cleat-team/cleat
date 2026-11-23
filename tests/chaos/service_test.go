//go:build chaos

package chaos

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"sync"
	"testing"
	"time"
)

// request is one request the recording service handled on a worker's behalf, in arrival order. A request is an EFFECT when the service
// performed it and answered success; a request it failed on purpose (to exercise a retry policy) is an attempt. The counts of these are the
// ground truth: "exactly once" is never inferred from the event history.
type request struct {
	Op, Run, Held string
	Nth           int  // 1-based arrival ordinal of this operation within the run
	Failed        bool // answered with a transient error on purpose
	At            time.Time
}

// gate holds the nth arrival of an operation at a chosen boundary until the harness aborts it.
//
//	before   the request has ARRIVED and nothing has been performed: a kill here is "after the previous step was recorded, before this effect"
//	after    the effect has been PERFORMED and the reply is withheld: a kill here is the documented at-least-once window (effect done, event not
//	         recorded, because the worker never received the reply)
type gate struct {
	run, op, mode string
	nth           int
	arrived       chan struct{}
	release       chan struct{} // let the held ("before") call proceed
	abort         chan struct{} // the worker was killed: the held call must not proceed, whatever the socket says about it
	fired         bool
}

type service struct {
	srv      *httptest.Server
	mu       sync.Mutex
	requests []request
	arrivals map[string]int // run|op -> arrivals so far
	g        *gate
}

func newService(t *testing.T) *service {
	t.Helper()
	s := &service{arrivals: map[string]int{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *service) handle(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	var body struct {
		Run, Name, Held string
		Fail            int
		Delay           int
	}
	_ = json.Unmarshal(raw, &body)
	op := path.Base(r.URL.Path)

	s.mu.Lock()
	key := body.Run + "|" + op
	s.arrivals[key]++
	nth := s.arrivals[key]
	g := s.g
	hold := g != nil && !g.fired && g.run == body.Run && g.op == op && g.nth == nth
	if hold {
		g.fired = true
	}
	s.mu.Unlock()

	if hold && g.mode == "before" {
		close(g.arrived)
		select {
		case <-g.release:
			// proceed and perform the effect (the harness has changed something underneath the worker first)
		case <-g.abort:
			return // nothing was performed
		case <-r.Context().Done():
			return // nothing was performed
		}
	}
	failed := nth <= body.Fail
	s.mu.Lock()
	s.requests = append(s.requests, request{Op: op, Run: body.Run, Held: body.Held, Nth: nth, Failed: failed, At: time.Now()})
	s.mu.Unlock()
	if hold && g.mode == "after" {
		close(g.arrived)
		select {
		case <-g.abort:
		case <-r.Context().Done():
		}
		return // performed, and the worker never saw the reply
	}
	if body.Delay > 0 {
		select {
		case <-time.After(time.Duration(body.Delay) * time.Millisecond):
		case <-r.Context().Done():
			return
		}
	}
	if failed {
		http.Error(w, `{"error":"transient"}`, http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func (s *service) arm(g *gate) {
	g.arrived, g.release, g.abort = make(chan struct{}), make(chan struct{}), make(chan struct{})
	s.mu.Lock()
	s.g = g
	s.mu.Unlock()
}

func (s *service) disarm() {
	s.mu.Lock()
	s.g = nil
	s.mu.Unlock()
}

func (s *service) requestsOf(run string) []request {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []request
	for _, e := range s.requests {
		if e.Run == run {
			out = append(out, e)
		}
	}
	return out
}
