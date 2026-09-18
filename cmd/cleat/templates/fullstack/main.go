//go:build ignore

package main

import (
	"github.com/cleat-team/cleat/cleat"
)

// SubmitOrder is the command side of a full-stack application: an HTTP request
// starts it, and everything after that is durable.
//
// The browser never waits for this to finish. It POSTs, gets a run id back,
// and polls the published state below. That split is the point of the
// template -- see README.md, "Why the browser does not wait".
//
// @cleatEntry(name="submit_order")
func SubmitOrder(h cleat.HostCalls, input string) (string, error) {
	h.LogKV("order_received", "input", input)

	// Publish state the front-end can poll. Each SetQueryState is readable at
	// GET /api/workflows/{id}/state?key=status -- one key per read, by design.
	h.SetQueryState("status", "validating")

	// Each DurableCall is recorded. If this worker dies here, another resumes
	// AFTER this step rather than repeating it, so the charge below happens
	// once even across a crash.
	if _, err := h.DurableCall("http", "fetch", `{"url":"https://example.invalid/validate","method":"GET"}`); err != nil {
		// A permanent error dead-letters the run with its full history, which
		// is what you inspect at GET /api/dead-letters.
		h.SetQueryState("status", "rejected")
		return "", err
	}

	h.SetQueryState("status", "charging")

	// TODO: replace with your payment provider. Pass your own idempotency key
	// downstream -- cleat's Idempotency-Key protects the START of this
	// workflow, not your provider's charge endpoint.

	h.SetQueryState("status", "complete")
	h.LogKV("order_complete")
	return `{"ok":true}`, nil
}

// No func main here on purpose: `cleat build` generates gen_main_stub.go,
// which declares it. A main in this file collides with the generated one --
// "other declaration of main" -- and is why this template did not build
// (cleat#1888). basic and agent have never declared one.
