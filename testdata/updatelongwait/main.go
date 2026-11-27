// Package updatelongwait is the guest of cleat#4511's regression tests: the human-in-the-loop shape update handlers exist for, with ONE long wait.
//
// It registers an update handler and then waits an hour for a "done" signal, in a loop. There is no slicing: testdata/updatedispatch waits in
// one-second slices, which takes the path that always worked (every slice that times out is followed by a fresh poll), so it could not show that an
// update arriving AFTER the run had suspended in a long wait was never handled.
//
// The guest sees how many updates were handled only when its wait returns: the SDK dispatches updates BEFORE a wait, so a handler runs and the run goes
// back to the same wait, and the loop's own condition is read when the wait ends. That is why it ends on a signal and not on a count.
package updatelongwait

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/cleat-team/cleat/cleat"
)

var applied int

//cleat:entry
func UpdateInALongWait(h cleat.HostCalls, input string) (string, error) {
	// The wait is an hour unless the input says {"wait_ms": N}: a wait due within the residency window is HELD, and an hour is not, so one guest serves both rows.
	wait := time.Hour
	var in struct {
		WaitMs int64 `json:"wait_ms"`
	}
	if err := json.Unmarshal([]byte(input), &in); err == nil && in.WaitMs > 0 {
		wait = time.Duration(in.WaitMs) * time.Millisecond
	}
	h.RegisterUpdateHandler("bump",
		func(payload string) (string, error) {
			applied++
			return fmt.Sprintf(`{"n":%d}`, applied), nil
		}, nil)
	for {
		if r := h.AwaitSignals([]string{"done"}, wait); !r.TimedOut {
			break
		}
	}
	return fmt.Sprintf(`{"n":%d}`, applied), nil
}
