// Package awaitpromisebranch is cleat#3383's fixture: AwaitPromise, then --
// only on a timeout -- a DurableCall with a real side effect.
//
// The defect was that a FRESH await which had to wait returned a word
// identical to a genuine timeout, and no SDK's await_promise binding checked
// for a stop, so this code took its timeout branch and the call executed
// before the run suspended. A compiled guest is what the test needs, not a
// hand-built session: the question is what the Go SDK does with the word the
// host returns.
package awaitpromisebranch

import (
	"time"

	"github.com/cleat-team/cleat/cleat"
)

// WaitInput is an object rather than a string so the entry binds by field name
// like every generated Go entry point.
type WaitInput struct {
	Attempt int `json:"attempt"`
}

// Wait awaits a promise nobody resolves. On a timeout it reports through a
// DurableCall; on resolution it reports that instead. It returns which outcome
// it saw.
//
//cleat:entry
func Wait(h cleat.HostCalls, input WaitInput) (string, error) {
	pid, err := h.CreatePromise("never-resolved")
	if err != nil {
		return "", err
	}
	_, timedOut, err := h.AwaitPromise(pid, 60*time.Second)
	if err != nil {
		return "", err
	}
	if timedOut {
		if _, err := h.DurableCall("report", "timed_out", `{"attempt":1}`); err != nil {
			return "", err
		}
		return "timed_out", nil
	}
	return "resolved", nil
}
