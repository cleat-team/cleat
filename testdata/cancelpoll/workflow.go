// Package cancelpoll is a fixture for the guest-facing cancellation API.
//
// testdata/basic covers the engine's own pre-call cancellation check, which
// fires inside freshCall before a durable call goes out. It cannot cover
// h.PollCancellation() — the API examples/subscription and examples/travel
// actually use — because no fixture called it. That is why the host function at
// engine/signaller.go:121 had no end-to-end coverage: nothing compiled to WASM
// invoked it.
package cancelpoll

import (
	"github.com/cleat-team/cleat/cleat"
)

// PollThenCall polls for cancellation and returns early with the reason if it
// finds one, making a durable call only if it does not. The call is what makes
// the test meaningful: an assertion on the return value alone cannot tell a
// workflow that stopped from one that reported cancellation and carried on.
func PollThenCall(h cleat.HostCalls, orderID string) (string, error) {
	if cancelled, reason := h.PollCancellation(); cancelled {
		return "stopped: " + reason, nil
	}

	if _, err := h.DurableCall("notifications", "SendEmail", `{"order_id":"`+orderID+`"}`); err != nil {
		return "", err
	}
	return "completed", nil
}
