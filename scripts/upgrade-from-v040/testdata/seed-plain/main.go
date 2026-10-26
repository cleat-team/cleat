// seed-plain is the workflow that makes the finished runs: completed, failed,
// and dead-lettered. cleat#3386.
//
// Written against the v0.4.0 SDK only. It must not use anything added since,
// because the script compiles it with the v0.4.0 `cleat` and a newer call would
// be a build error rather than a seeded run.
package seedplain

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/cleat-team/cleat/cleat"
)

type input struct {
	N    int    `json:"n"`
	Mode string `json:"mode"` // "ok" (default), "fail", "deadletter", "spin"
}

// Plain records a few events, then ends the way Mode says. A string parameter
// receives the whole input JSON verbatim, and the result must be a string.
func Plain(h cleat.HostCalls, raw string) (string, error) {
	var in input
	_ = json.Unmarshal([]byte(raw), &in)

	h.SetQueryState("stage", "started")
	h.DurableLog(fmt.Sprintf("seed-plain n=%d mode=%s", in.N, in.Mode))

	switch in.Mode {
	case "spin":
		// CPU-bound and silent: no host call, so the worker is mid-execution of
		// this run rather than parked or between steps when it is killed.
		// Counted, not timed: time.Now is refused by cleat vet (E003), and the
		// count is sized to be a few seconds, not tens.
		var acc uint64
		for i := uint64(0); i < 2_000_000_000; i++ {
			acc += i ^ (acc >> 3)
		}
		h.DurableLog(fmt.Sprintf("spun %d", acc&1))
	case "fail":
		return "", fmt.Errorf("seeded failure n=%d", in.N)
	case "deadletter":
		// Retries exhausted against a service that is reachable and keeps
		// answering 503 (the seed script runs the stub). A missing service is
		// a permanent error and ends "failed"; only a call whose retries
		// really ran out is dead-lettered (endedOnAnExhaustedCall).
		_, err := h.DurableCallWithOptions(cleat.CallOptions{
			Retry: &cleat.RetryPolicy{
				MaxAttempts:        2,
				InitialInterval:    100 * time.Millisecond,
				BackoffCoefficient: 1.0,
				MaxInterval:        100 * time.Millisecond,
			},
		}, "seed-flaky", "Poison", `{}`)
		if err != nil {
			return "", err
		}
	}

	h.SetQueryState("stage", "done")
	return fmt.Sprintf(`{"n":%d}`, in.N), nil
}
