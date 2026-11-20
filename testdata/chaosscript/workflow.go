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
//	{"op":"call","name":"SLOW","delay":40000}   a call the service answers after 40 s, longer than the instance timeout
//	{"op":"side_effect","name":"S"}        a frozen non-reproducible value (the clock), then a call that reports the value the guest HELD
//	{"op":"call_retry","name":"R","attempts":5,"fail":2}   a call under a retry policy; the service fails its first `fail` requests
//	{"op":"await0","name":"go","hit":"HIT","miss":"MISS"}   a signal wait with no wait, then a call named for the branch taken
//	{"op":"continue_as_new","name":"POST"}   continues as a new run whose only operation is a call named POST
//	{"op":"await_promise","name":"p","ms":1500,"hit":"RES","miss":"TIMEDOUT"}   await an unresolved promise, then a call named for the outcome
//	{"op":"set_scope","name":"k"}   enter the virtual object ("chaos","k"): the host takes its key, or suspends while another run holds it
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
	Hit      string `json:"hit"`
	Miss     string `json:"miss"`
	Delay    int    `json:"delay"`
}

type scriptInput struct {
	Run string `json:"run"`
	Ops []op   `json:"ops"`
}

func request(run, name, held string) string {
	b, _ := json.Marshal(map[string]string{"run": run, "name": name, "held": held})
	return string(b)
}

// slowRequest asks the service to answer after delayMs: a host call that blocks for longer than any instance timeout.
func slowRequest(run, name string, delayMs int) string {
	b, _ := json.Marshal(map[string]any{"run": run, "name": name, "delay": delayMs})
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
			req := request(s.Run, o.Name, "")
			if o.Delay > 0 {
				req = slowRequest(s.Run, o.Name, o.Delay)
			}
			resp, err := h.DurableCall("chaos", o.Name, req)
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
		case "await0":
			// A signal wait with NO wait (timeout 0): it returns at once, with a signal if one is already there and a timeout if not. The branch
			// taken is made visible to the service as a call named for it, so a replay that takes the other branch performs a different effect.
			_, _, timedOut, err := h.DurableAwaitSignals([]string{o.Name}, 0)
			if err != nil {
				return "", err
			}
			branch := o.Hit
			if timedOut {
				branch = o.Miss
			}
			if _, err := h.DurableCall("chaos", branch, request(s.Run, branch, "")); err != nil {
				return "", err
			}
			seen[branch] = "1"
		case "await_promise":
			// Create a promise nobody resolves and await it for o.Ms. It times out; the branch call is named for it (o.Miss). A promise a
			// harness never resolves exercises the TIMED-OUT outcome, which cleat#4177 found is not recorded as its own step.
			pid, err := h.CreatePromise("p-" + o.Name)
			if err != nil {
				return "", err
			}
			_, timedOut, err := h.AwaitPromise(pid, time.Duration(o.Ms)*time.Millisecond)
			if err != nil {
				return "", err
			}
			branch := o.Hit
			if timedOut {
				branch = o.Miss
			}
			if _, err := h.DurableCall("chaos", branch, request(s.Run, branch, "")); err != nil {
				return "", err
			}
			seen[branch] = "1"
		case "promise_create", "promise_resolve", "promise_reject":
			// A promise operation the recording service cannot see: the promise store is the only place its effect lands, so the harness reads
			// the workflow_promises rows after the run (cleat#4207, #4152). The promise is named "p-<name>"; a rerun that created it twice leaves
			// two rows under that name, and a resolve or reject that did not take leaves it pending.
			pid, err := h.CreatePromise("p-" + o.Name)
			if err != nil {
				return "", err
			}
			switch o.Op {
			case "promise_resolve":
				err = h.ResolvePromise(pid, `"ok"`)
			case "promise_reject":
				err = h.RejectPromise(pid, "rejected")
			}
			if err != nil {
				return "", err
			}
		case "set_scope":
			// Enter the virtual object ("chaos", o.Name): the host takes its concurrency key, or suspends the run while another run holds it.
			h.SetScope("chaos", o.Name)
		case "continue_as_new":
			// Continue as a new run that makes one call named o.Name. The service sees the same run label for both runs.
			next, _ := json.Marshal(map[string]any{"__entry_point": "script", "run": s.Run, "ops": []map[string]string{{"op": "call", "name": o.Name}}})
			if err := h.ContinueAsNew(string(next)); err != nil {
				return "", err
			}
			return "{}", nil
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
