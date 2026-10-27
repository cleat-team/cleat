// Package awaittimeoutbranch is cleat#3378's fixture: the lease-and-escalate
// shape the clew-service port runs, reduced to its three host calls.
//
// AwaitSignals, then -- only on a timeout -- a PluginCall with a real side
// effect, then AwaitSignals again. The defect was that a FRESH await which had
// to wait returned a word identical to a genuine timeout, so this code took its
// timeout branch and the plugin call executed before the run suspended. A
// compiled guest is what the test needs, not a hand-built session: the
// question is what the Go SDK does with the word the host returns.
package awaittimeoutbranch

import (
	"time"

	"github.com/cleat-team/cleat/cleat"
)

// EscalateInput is an object rather than a string because SQL Server's input
// CHECK constraint refuses a bare JSON scalar, and the test runs on all three
// dialects.
type EscalateInput struct {
	Attempt int `json:"attempt"`
}

// Escalate waits for "ack"; on a timeout it re-dispatches through a plugin and
// waits again. It returns which outcome each wait saw.
//
//cleat:entry
func Escalate(h cleat.HostCalls, input EscalateInput) (string, error) {
	first := h.AwaitSignals([]string{"ack"}, 60*time.Second)
	if first.TimedOut {
		if _, err := h.PluginCall("notify", "redispatch", `{"attempt":2}`); err != nil {
			return "", err
		}
	}
	second := h.AwaitSignals([]string{"ack"}, 60*time.Second)
	if second.TimedOut {
		return "timed_out_twice", nil
	}
	return "acked", nil
}
