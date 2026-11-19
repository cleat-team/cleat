// Package chaosscript is the guest for the fault-injection harness (cleat#4169): ONE workflow that interprets a list of operations given
// as its input, so a scenario is data. The harness generates the list from a seed, runs it fault-free to get the golden record, then runs
// it again with a fault injected at each step boundary and compares.
//
// Every operation that has an effect on the outside world goes through the one service the harness records ("chaos"), under an operation
// name derived from the script's own step names, so "every effect happened exactly once" is a count of what the service received.
//
// Operations:
//
//	{"op":"call","name":"A"}               a durable call; the service records name and run id
//	{"op":"side_effect","name":"S"}        a frozen non-reproducible value (the clock), then a call that reports the value the guest HELD
//	{"op":"call_retry","name":"R","attempts":5,"fail":2}   a call under a retry policy; the service fails its first `fail` requests
//	{"op":"sleep","ms":300}                a durable sleep
//	{"op":"defer","name":"D"}              registers a cleanup that makes a durable call named "defer_D" when the run ends
//
// The result is a JSON object of what the guest observed, so a replay that hands the guest a different answer shows in the output.
package chaosscript

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/cleat-team/cleat/cleat"
)

type op struct {
	Op       string `json:"op"`
	Name     string `json:"name"`
	Ms       int64  `json:"ms"`
	Attempts int    `json:"attempts"`
	Fail     int    `json:"fail"`
}

type scriptInput struct {
	Run string `json:"run"`
	Ops []op   `json:"ops"`
}

func request(run, name, held string) string {
	b, _ := json.Marshal(map[string]string{"run": run, "name": name, "held": held})
	return string(b)
}

// retryRequest asks the service to fail the first `fail` requests for this operation, so the retry policy has something to do.
func retryRequest(run, name string, fail int) string {
	b, _ := json.Marshal(map[string]any{"run": run, "name": name, "fail": fail})
	return string(b)
}

// Script runs the operations in order.
func Script(h cleat.HostCalls, input string) (string, error) {
	var s scriptInput
	if err := json.Unmarshal([]byte(input), &s); err != nil {
		return "", fmt.Errorf("chaosscript: input is not a script: %w", err)
	}
	seen := map[string]string{}
	for _, o := range s.Ops {
		switch o.Op {
		case "call":
			resp, err := h.DurableCall("chaos", o.Name, request(s.Run, o.Name, ""))
			if err != nil {
				return "", err
			}
			seen[o.Name] = resp
		case "side_effect":
			held, err := h.SideEffect(func() (string, error) {
				return fmt.Sprintf("%d", time.Now().UnixNano()), nil
			})
			if err != nil {
				return "", err
			}
			// The service sees the value the guest HELD. A replay that recomputed it would report a different one for the same step.
			if _, err := h.DurableCall("chaos", o.Name, request(s.Run, o.Name, held)); err != nil {
				return "", err
			}
			seen[o.Name] = held
		case "call_retry":
			resp, err := h.DurableCallWithRetry("chaos", o.Name, retryRequest(s.Run, o.Name, o.Fail), cleat.RetryPolicy{
				MaxAttempts:        o.Attempts,
				InitialInterval:    100 * time.Millisecond,
				BackoffCoefficient: 1.0,
				MaxInterval:        100 * time.Millisecond,
			})
			if err != nil {
				return "", err
			}
			seen[o.Name] = resp
		case "sleep":
			h.DurableSleepMs(o.Ms)
		case "defer":
			name := o.Name
			if _, err := h.DurableDeferFunc(func() {
				_, _ = h.DurableCall("chaos", "defer_"+name, request(s.Run, "defer_"+name, ""))
			}); err != nil {
				return "", err
			}
		default:
			return "", fmt.Errorf("chaosscript: unknown op %q", o.Op)
		}
	}
	out, _ := json.Marshal(seen)
	return string(out), nil
}
