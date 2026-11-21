//go:build chaos

package chaos

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// cleat#4162, PR 2d: a worker is KILLED while it holds a run in memory through a sleep.
//
// The harness's other faults land on a step boundary: a request to the recording service, held before or after its effect. A held run makes no request while it
// is held, so none of those boundaries is "while held". This is the fault they cannot reach (the README listed it under "Not yet"): the worker is SIGKILLed in the middle
// of a hold, after one effect has been recorded and before the next, with the sleep still running.
//
// What it must show is what the reaper's design already promises for any run in flight and nothing more: the run is reclaimed after the kill, replays from its recorded
// history, and finishes with every effect exactly once. A held run is a run in flight whose sleep happens to be waiting in memory instead of in the store, so there
// is nothing to recover that the store does not have: the sleep records no event, and its deadline is served by the clock on replay.
//
// THE PRECONDITION IS THE HARD PART. A kill that lands after the run has been SUSPENDED (no worker holds it) would pass for the same reason a kill of an idle worker
// does, and measure nothing about residency. So the run is killed only once the worker itself reports that it is holding one (cleat_resident_runs on its /metrics),
// and the control, the same scenario with residency off, has to read the opposite on the same detector: not held, parked in the store.

const heldSleepMs = 8000

var heldScenario = scenario{"call_sleep_call", []op{{Op: "call", Name: "A"}, {Op: "sleep", Ms: heldSleepMs}, {Op: "call", Name: "B"}}, nil}

// freeLoopbackAddr returns an address nothing is listening on, for a worker's --api-addr.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding a free port: %v", err)
	}
	defer l.Close()
	return l.Addr().String()
}

// residentRuns reads cleat_resident_runs from a worker's /metrics. ok is false when the page cannot be read or is not a real metrics page, which is NOT zero: an
// unreadable gauge says nothing about whether a run is held, and the caller reports UNMEASURED for it.
//
// A worker that has never held a run exports NO cleat_resident_runs series at all (the counter is created on its first increment), so "the page is real and the series is
// absent" is zero. "Real" is checked, and not assumed from a 200: the page must carry some other cleat_ series, so an empty body or another service's page on that port
// cannot read as "holds nothing". The first version of this treated absence as unreadable and so could not tell its own control apart from a broken scrape.
func residentRuns(addr string) (n float64, ok bool) {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://" + addr + "/metrics")
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		return 0, false
	}
	real := false
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		if strings.HasPrefix(line, "cleat_") {
			real = true
		}
		if !strings.HasPrefix(line, "cleat_resident_runs") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if v, err := strconv.ParseFloat(fields[len(fields)-1], 64); err == nil {
			return v, true
		}
	}
	return 0, real
}

// heldOutcome is what a kill-while-held run reports.
type heldOutcome struct {
	outcome
	Detector string // what the worker said about the run at the moment of the kill, for the failure message
	Held     bool   // the detector says a run was held (residency on) / was parked in the store (residency off)
}

// executeKilledWhileHeld runs call A, sleep, call B on a worker with residency on or off, waits until the run is asleep in the way that variant puts it asleep,
// SIGKILLs the worker, ages the lease, starts a second worker (residency off, so the second half is the ordinary reclaim and not a second variable) and waits for the end.
func executeKilledWhileHeld(t *testing.T, env *env, residency bool) heldOutcome {
	t.Helper()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	queue, run := "chaos-q-"+suffix, "chaos-run-"+suffix
	deploy(t, env.db, env.wasm, queue)

	addr := freeLoopbackAddr(t)
	flags := []string{"--api-addr", addr}
	if residency {
		flags = append(flags, "--resident-max-runs", "2")
	}
	first := startWorker(t, env.bin, queue, env.svc.srv.URL, flags...)
	startRun(t, env.db, run, scriptInput(run, heldScenario), queue)

	var out heldOutcome
	// Wait for effect A to have happened, then for the run to be asleep in the way this variant puts it asleep.
	deadline := time.Now().Add(runBudget)
	sawA := func() bool {
		for _, r := range env.svc.requestsOf(run) {
			if r.Op == "A" && !r.Failed {
				return true
			}
		}
		return false
	}
	asleep := func() (bool, string) {
		n, ok := residentRuns(addr)
		if residency {
			// Held: the worker says it holds a run, and the store still has the run RUNNING and assigned to it.
			// parkedNow swallows its query error as false, so on this residency-on side a store error reads as "not parked". The metric still has to say held (n >= 1), so
			// that cannot make the detector pass on its own; it can only make the precondition succeed when the store was unreadable and the run was in fact parked.
			held := ok && n >= 1 && !parkedNow(env.db, run)
			return held, fmt.Sprintf("cleat_resident_runs=%v (readable=%v), parked in the store=%v", n, ok, parkedNow(env.db, run))
		}
		// Control: the worker says it holds nothing, and the store has the run parked (ready, unassigned, a wake time ahead).
		parked := parkedNow(env.db, run)
		return ok && n == 0 && parked, fmt.Sprintf("cleat_resident_runs=%v (readable=%v), parked in the store=%v", n, ok, parked)
	}
	for time.Now().Before(deadline) {
		if sawA() {
			if h, d := asleep(); h {
				out.Held, out.Detector = true, d
				break
			} else {
				out.Detector = d
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	out.Applied = out.Held
	out.Events = eventCount(env.db, run)
	first.kill()
	ageLease(t, env.db, run)

	second := startWorker(t, env.bin, queue, env.svc.srv.URL)
	wantTerminal := func(s string) bool { return s != "ready" && s != "running" }
	status, msg, result, ok := awaitStatus(env.db, run, runBudget, wantTerminal)
	out.Status, out.ErrMsg, out.Result = status, msg, result
	if !ok {
		out.Status = "NOT-TERMINAL:" + status
	}
	out.Requests = env.svc.requestsOf(run)
	out.Log = first.output() + "\n--- second worker ---\n" + second.output()
	return out
}

// verifyKilledWhileHeld is the harness's exactly-once oracle for this scenario: both effects once, the run done.
func verifyKilledWhileHeld(o heldOutcome) []string {
	if !o.Applied {
		return []string{"UNMEASURED: the run was never in the state this case needs (" + o.Detector + "), so the kill measured nothing"}
	}
	var v []string
	if o.Status != "done" {
		v = append(v, fmt.Sprintf("final status %q, want done (error: %s)", o.Status, o.ErrMsg))
	}
	successes, _, _, _ := tally(o.Requests)
	for _, name := range []string{"A", "B"} {
		if successes[name] != 1 {
			v = append(v, fmt.Sprintf("effect %q succeeded %d time(s), want exactly 1", name, successes[name]))
		}
	}
	return v
}

func TestChaosSigkillWhileAHeldRunSleepsThenReclaim(t *testing.T) {
	e := newEnv(t)
	for _, residency := range []bool{false, true} {
		name := "residency off (control, the run is parked in the store)"
		if residency {
			name = "residency on (the run is held in memory)"
		}
		t.Run(name, func(t *testing.T) {
			o := executeKilledWhileHeld(t, e, residency)
			t.Logf("%s: events at the kill %d, detector: %s", name, o.Events, o.Detector)
			if viol := verifyKilledWhileHeld(o); len(viol) > 0 {
				t.Errorf("%s:\n  %s\nrequests: %s\nlog tail:\n%s", name, strings.Join(viol, "\n  "), seqOf(o.Requests), tail(o.Log, 1800))
			}
		})
	}
}
