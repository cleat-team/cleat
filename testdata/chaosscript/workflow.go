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
//	{"op":"await_signal","name":"go","ms":8000,"hit":"HIT","miss":"MISS"}   a signal wait that WAITS up to ms, then a call named for the branch taken (cleat#4306)
//	{"op":"continue_as_new","name":"POST"}   continues as a new run whose only operation is a call named POST
//	{"op":"await_promise","name":"p","ms":1500,"hit":"RES","miss":"TIMEDOUT"}   await an unresolved promise, then a call named for the outcome
//	{"op":"set_scope","name":"k"}   enter the virtual object ("chaos","k"): the host takes its key, or suspends while another run holds it
//	{"op":"send","name":"S"}        a fire-and-forget request to the service, named "send_S": recorded first, dispatched after, so at most once
//	{"op":"signal","name":"S"}      signal THIS run with the signal "sig-S": the effect lands in the signal table, which the recording service cannot see
//	{"op":"cron_create","name":"C"} schedule a yearly cron for the workflow "chaos-cron-<run>", input {"n":"C"}: the effect lands in workflow_schedules
//	{"op":"cron_delete","name":"C"} delete the schedule the earlier cron_create C made
//	{"op":"cron_list","name":"L"}   list the schedules, then make two calls, L and L2, that report how many belong to this run (the value the guest HELD)
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
	// The schedule ids the run's own cron_create ops got, by name, so a later cron_delete can delete the one it means.
	cronIDs := map[string]string{}
	cronWorkflow := "chaos-cron-" + s.Run
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
		case "await_signal":
			// A signal wait that WAITS for up to o.Ms (cleat#4306). It is await0 with a real timeout: the branch taken is made visible to the service as a call named for it
			// (o.Hit if the signal arrived, o.Miss if the wait timed out), and the service records WHEN that call arrived, which is how a harness measures the time from a
			// signal's commit to the guest's next step. A held wait and a suspended-and-replayed one both end in the same call.
			_, _, timedOut, err := h.DurableAwaitSignals([]string{o.Name}, o.Ms)
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
		case "send":
			// Fire-and-forget: the event is recorded first and the request dispatched on a goroutine after, so the service sees it as an effect
			// named "send_<name>" that happens AT MOST once (a kill between the record and the dispatch loses it, and a replay never sends it).
			if err := h.DurableSend("chaos", "send_"+o.Name, request(s.Run, "send_"+o.Name, "")); err != nil {
				return "", err
			}
		case "signal":
			// A signal to THIS run, which nothing waits for: the only place its effect lands is the signal table, so the harness reads
			// workflow_signals after the run (cleat#4241). A replay that sent it again leaves a second row under the name.
			if err := h.SignalWorkflow(s.Run, "sig-"+o.Name, `{"n":"`+o.Name+`"}`); err != nil {
				return "", err
			}
		case "cron_create":
			// A yearly schedule, so nothing fires during a run. The effect lands in workflow_schedules (cleat#4241).
			id, err := h.ScheduleCron(cronWorkflow, "0 0 1 1 *", "UTC", `{"n":"`+o.Name+`"}`)
			if err != nil {
				return "", err
			}
			cronIDs[o.Name] = id
			seen[o.Name] = id
		case "cron_delete":
			if err := h.DeleteCron(cronIDs[o.Name]); err != nil {
				return "", err
			}
		case "cron_list":
			// The list is a READ the engine records, so a replay must answer the RECORDED list and not the table as it is now. The count of
			// this run's schedules is what the guest held, reported to the service: a replay that listed afresh reports a different one.
			raw, err := h.ListCrons()
			if err != nil {
				return "", err
			}
			var list []struct {
				WorkflowName string `json:"workflow_name"`
			}
			if err := json.Unmarshal([]byte(raw), &list); err != nil {
				return "", fmt.Errorf("chaosscript: ListCrons did not return a JSON list: %w", err)
			}
			n := 0
			for _, c := range list {
				if c.WorkflowName == cronWorkflow {
					n++
				}
			}
			held := fmt.Sprintf("n=%d", n)
			// TWO calls carry the held count: a replayed list is observable only through a request made AFTER it, and the first call is
			// replayed from its record at a boundary past it. A kill between the two leaves the second to be requested fresh, with the
			// value the replay's list answered.
			for _, name := range []string{o.Name, o.Name + "2"} {
				if _, err := h.DurableCall("chaos", name, request(s.Run, name, held)); err != nil {
					return "", err
				}
			}
			seen[o.Name] = held
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
