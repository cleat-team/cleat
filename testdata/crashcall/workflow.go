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
	return "completed", nil
}
