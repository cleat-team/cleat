// Package killafterwork is the fixture for the failure-path event flush (cleat#4079, part 2).
//
// Each entry point makes one durable call and registers one defer, and then ends the run badly in a different way. What the
// engine had recorded by then is what a failed run's history is, and whether that reaches the database with per-step flush
// off is the question.
package killafterwork

import (
	"errors"
	"strconv"

	"github.com/cleat-team/cleat/cleat"
)

// CallThenSpin makes a call, registers a defer, and then never enters the host again, so the epoch fence is what stops it.
func CallThenSpin(h cleat.HostCalls) (string, error) {
	if _, err := h.DurableCall("kill-probe", "charge", `{}`); err != nil {
		return "", err
	}
	if _, err := h.DurableDeferFunc(func() {
		_, _ = h.DurableCall("kill-probe", "refund", `{}`)
	}); err != nil {
		return "", err
	}
	x := uint64(1)
	for i := 0; i < 100000000000; i++ {
		x = x*6364136223846793005 + 1442695040888963407
		x ^= x >> 33
	}
	return `{"value":` + strconv.FormatUint(x, 10) + `}`, nil
}

// CallThenFail makes a call and then returns an error: a terminal error that is not a kill.
func CallThenFail(h cleat.HostCalls) (string, error) {
	if _, err := h.DurableCall("kill-probe", "charge", `{}`); err != nil {
		return "", err
	}
	return "", errors.New("the workflow failed on purpose")
}
