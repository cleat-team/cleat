// Package promisetimeout is cleat#2020's fixture for AwaitPromise's timeout
// path.
//
// It exists because a real guest is what the issue's own methodology
// requires: #2008's execCtx claim survived a code reading and a unit test
// that called the wasmtime hostfunc closure directly, and was only
// disproved by an end-to-end acceptance test making real calls through a
// real compiled module. A survey that reasons about AwaitPromise's suspend
// path from engine/promises.go alone would repeat exactly that mistake.
package promisetimeout

import (
	"time"

	"github.com/cleat-team/cleat/cleat"
)

// Entry creates a promise nobody ever resolves and awaits it with a short
// timeout, then reports which outcome it saw via a DurableCall -- so a test
// can read the outcome off the service caller's recorded calls rather than
// reaching into engine internals.
//
//cleat:entry
func Entry(h cleat.HostCalls, input string) (string, error) {
	pid, err := h.CreatePromise("never-resolved")
	if err != nil {
		return "", err
	}
	_, timedOut, err := h.AwaitPromise(pid, 200*time.Millisecond)
	if err != nil {
		return "", err
	}
	if timedOut {
		_, _ = h.DurableCall("report", "timed_out", "{}")
		return "timed_out", nil
	}
	_, _ = h.DurableCall("report", "resolved", "{}")
	return "resolved", nil
}
