// Package crashcall is the fixture for IMPROVEMENT-PLAN §2.4: a workflow whose
// every side effect is externally countable, so that a crash mid-call can be
// scored by asking the service what it was asked to do.
//
// Three sequential calls, not one. One call cannot distinguish the two
// hypotheses that matter:
//
//	at-least-once as documented — steps 1 and 2 are durable, so a crash during
//	step 3 re-runs only step 3.
//	no durable history at all   — a crash during step 3 re-runs all three.
//
// With a single call both predict "the call happens twice". With three, they
// predict (1,1,2) and (2,2,2), and the counts say which is true.
package crashcall

import (
	"encoding/json"
	"time"

	"github.com/cleat-team/cleat/cleat"
)

// ThreeCharges makes three distinct durable calls in order. Distinct operations
// so the service can count them separately.
func ThreeCharges(h cleat.HostCalls, orderID string) (string, error) {
	// Marshal rather than concatenate. A lone string parameter receives the
	// entire input JSON verbatim (wasm/exports.go special-cases it), so orderID
	// contains quotes, and string-building here produces invalid JSON.
	for _, op := range []string{"Reserve", "Charge", "Ship"} {
		req, err := json.Marshal(map[string]string{"order_id": orderID, "op": op})
		if err != nil {
			return "", err
		}
		if _, err := h.DurableCall("payments", op, string(req)); err != nil {
			return "", err
		}
	}
	return `{"status":"completed"}`, nil
}

// Compensating is cleat#2285's saga: Reserve, Charge, then Ship under a retry policy, and a Refund if Ship
// fails. A shutdown that interrupts Ship's backoff and reaches the guest as a failed call makes this run its
// Refund and finish COMPLETED on a fault that never happened.
func Compensating(h cleat.HostCalls, orderID string) (string, error) {
	for _, op := range []string{"Reserve", "Charge"} {
		if _, err := h.DurableCall("payments", op, mustReq(orderID, op)); err != nil {
			return "", err
		}
	}
	_, err := h.DurableCallWithOptions(cleat.CallOptions{
		Retry: &cleat.RetryPolicy{
			MaxAttempts:        3,
			InitialInterval:    10 * time.Second,
			BackoffCoefficient: 1.0,
			MaxInterval:        10 * time.Second,
		},
	}, "payments", "Ship", mustReq(orderID, "Ship"))
	if err != nil {
		if _, cerr := h.DurableCall("payments", "Refund", mustReq(orderID, "Refund")); cerr != nil {
			return "", cerr
		}
		return `{"status":"compensated"}`, nil
	}
	return `{"status":"completed"}`, nil
}

// WithCleanup is ThreeCharges with a deferred Cleanup call: a run cut off by shutdown must not run its cleanup
// (the run is being handed to another worker, and cleanup is what a run does when it is FINISHED).
func WithCleanup(h cleat.HostCalls, orderID string) (string, error) {
	if _, err := h.DurableDeferFunc(func() {
		_, _ = h.DurableCall("payments", "Cleanup", mustReq(orderID, "Cleanup"))
	}); err != nil {
		return "", err
	}
	return ThreeCharges(h, orderID)
}

func mustReq(orderID, op string) string {
	req, err := json.Marshal(map[string]string{"order_id": orderID, "op": op})
	if err != nil {
		return "{}"
	}
	return string(req)
}

// ContinuesAsNew makes one call and then continues as a new run of ThreeCharges: cleat#2285's ContinueAsNew
// in flight when the grace ends. The new run's input is fixed because a lone string parameter receives the
// whole input JSON.
func ContinuesAsNew(h cleat.HostCalls, orderID string) (string, error) {
	if _, err := h.DurableCall("payments", "Reserve", mustReq(orderID, "Reserve")); err != nil {
		return "", err
	}
	if err := h.ContinueAsNew(`{"__entry_point":"three_charges","orderID":"continued"}`); err != nil {
		return "", err
	}
	return "", nil
}
