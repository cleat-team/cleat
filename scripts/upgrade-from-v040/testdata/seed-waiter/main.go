// seed-waiter makes the runs that are still waiting when the v0.4.0 worker
// stops: parked on a signal, or on a timer. cleat#3386.
package seedwaiter

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/cleat-team/cleat/cleat"
)

type input struct {
	N       int    `json:"n"`
	Mode    string `json:"mode"` // "signal" (default) or "timer"
	TimerMs int64  `json:"timer_ms"`
}

// Wait parks, then finishes. A signal waiter ends when "go" arrives; a timer
// waiter ends when its sleep does. The 24h signal timeout is a ceiling nobody
// reaches: a run that ends by timing out has not been resumed by anything.
func Wait(h cleat.HostCalls, raw string) (string, error) {
	var in input
	_ = json.Unmarshal([]byte(raw), &in)

	h.SetQueryState("stage", "waiting")

	if in.Mode == "timer" {
		h.DurableSleepMs(in.TimerMs)
		h.SetQueryState("stage", "done")
		return fmt.Sprintf(`{"n":%d,"woke":"timer"}`, in.N), nil
	}

	res := h.AwaitSignals([]string{"go"}, 24*time.Hour)
	if res.TimedOut {
		return "", fmt.Errorf("n=%d timed out waiting for the go signal", in.N)
	}
	h.SetQueryState("stage", "done")
	return fmt.Sprintf(`{"n":%d,"woke":"signal"}`, in.N), nil
}
