//go:build ignore

package main

import (
	"encoding/json"
	"fmt"

	"github.com/cleat-team/cleat/cleat"
)

// order is what `make run` and web/index.html POST as the run's input.
type order struct {
	Item string `json:"item"`
	Qty  int    `json:"qty"`
}

// SubmitOrder is the command side of a full-stack application: an HTTP request
// starts it, and everything after that is durable.
//
// The browser never waits for this to finish. It POSTs, gets a run id back,
// and polls the published state below. That split is the point of the
// template -- see README.md, "Why the browser does not wait".
//
// This runs to `done` on a fresh checkout with nothing else installed, so the
// first run you start proves the path works. The one step that would call your
// payment provider is a durable sleep standing in for it: see "Replace the
// placeholder" in README.md for the DurableCall that replaces it.
//
// @cleatEntry(name="submit_order")
func SubmitOrder(h cleat.HostCalls, input string) (string, error) {
	h.LogKV("order_received", "input", input)

	// Publish state the front-end can poll. Each SetQueryState is readable at
	// GET /api/workflows/{id}/query?key=status -- one key per read, by design.
	h.SetQueryState("status", "validating")

	var o order
	if err := json.Unmarshal([]byte(input), &o); err != nil || o.Item == "" || o.Qty < 1 {
		// A returned error ends the run `failed`, with this message. (Only a
		// durable call that exhausts its retries is dead-lettered.)
		h.SetQueryState("status", "rejected")
		return "", fmt.Errorf(`invalid order: want {"item": "<name>", "qty": <1 or more>}, got %s`, input)
	}

	h.SetQueryState("status", "charging")

	// PLACEHOLDER for the payment provider. A durable sleep is recorded like any
	// other step: if the worker dies during it, another resumes AFTER it rather
	// than starting the order over. It also leaves `charging` visible to the
	// browser for a few seconds, which is what the polling in web/index.html is
	// for. Replace it with a DurableCall (README.md shows one), and pass your
	// own idempotency key downstream -- cleat's Idempotency-Key protects the
	// START of this workflow, not your provider's charge endpoint.
	h.DurableSleepMs(3000)

	h.SetQueryState("status", "complete")
	h.LogKV("order_complete")
	return `{"ok":true}`, nil
}

// No func main here on purpose: `cleat build` generates gen_main_stub.go,
// which declares it. A main in this file collides with the generated one --
// "other declaration of main" -- and is why this template did not build
// (cleat#1888). basic and agent have never declared one.
