//go:build chaos

package chaos

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// cleat#4275: a worker is KILLED while it holds a run through a sleep, and nothing ages the lease: the run is reclaimed by the real reaper of a live worker, at the HELD window.
//
// TestChaosSigkillWhileAHeldRunSleepsThenReclaim ages the dead worker's heartbeat (ageLease), because the ordinary reclaim window cannot go below about 37 s and a sweep over it would be
// unaffordable. That is exactly what it cannot say anything about: how long the reclaim really takes. This case does not age it. What it asserts is that the run finishes with A and B once
// each, and that B ran well inside the ordinary window, which is the only observable that a held run was reclaimed by the fast path and not by the ordinary one.
//
// THE BOUND. The harness's workers heartbeat every 1 s, which puts the ordinary window at no less than ~37 s (minimumReclaimAfter; the README's "cannot go below"). The held window is that
// minus the 30 s database-stall allowance, about 7 s, and the reaper ticks every 10 s, so a held run is expected back inside roughly 7 + 10 + 1 s of the kill. The bound is 30 s: far
// enough below 37 s that the ordinary path cannot meet it, and far enough above ~18 s that a loaded machine does not fail it.
const heldReclaimBound = 30 * time.Second

func TestChaosSigkillWhileAHeldRunSleepsIsReclaimedAtTheHeldWindowWithoutAgeingTheLease(t *testing.T) {
	e := newEnv(t)
	// The reclaimer is the warm reaper (a live worker whose reaper is past its start-up grace), as a production fleet's other replicas are. A freshly started worker would wait out its
	// own grace first and measure the wrong thing.
	warmReaper(t, e)
	awaitWarmReaper(t)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	queue, run := "chaos-q-"+suffix, "chaos-run-"+suffix
	deploy(t, e.db, e.wasm, queue)

	addr := freeLoopbackAddr(t)
	first := startWorker(t, e.bin, queue, e.svc.srv.URL, "--api-addr", addr, "--resident-max-runs", "2")
	startRun(t, e.db, run, scriptInput(run, heldScenario), queue)

	sawA := func() bool {
		for _, r := range e.svc.requestsOf(run) {
			if r.Op == "A" && !r.Failed {
				return true
			}
		}
		return false
	}
	var detector string
	held := false
	for deadline := time.Now().Add(runBudget); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if !sawA() {
			continue
		}
		n, ok := residentRuns(addr)
		parked := parkedNow(e.db, run)
		detector = fmt.Sprintf("cleat_resident_runs=%v (readable=%v), parked in the store=%v", n, ok, parked)
		if ok && n >= 1 && !parked {
			held = true
			break
		}
	}
	if !held {
		t.Fatalf("UNMEASURED: the run was never in the state this case needs (%s), so the kill measured nothing", detector)
	}

	killedAt := time.Now()
	first.kill()
	// A same-queue worker to EXECUTE the run once the reaper has made it ready; residency off, so the replay does not park again. It is not the reclaimer.
	second := startWorker(t, e.bin, queue, e.svc.srv.URL, residencyOff...)

	status, msg, _, ok := awaitStatus(e.db, run, runBudget, func(s string) bool { return s != "ready" && s != "running" })
	if !ok {
		t.Fatalf("the run was not terminal (%q) within %s of starting; second worker log:\n%s", status, runBudget, tail(second.output(), 1800))
	}
	reqs := e.svc.requestsOf(run)
	successes, _, _, _ := tally(reqs)
	var viol []string
	if status != "done" {
		viol = append(viol, fmt.Sprintf("final status %q, want done (error: %s)", status, msg))
	}
	for _, name := range []string{"A", "B"} {
		if successes[name] != 1 {
			viol = append(viol, fmt.Sprintf("effect %q succeeded %d time(s), want exactly 1", name, successes[name]))
		}
	}
	var tB time.Time
	for _, r := range reqs {
		if r.Op == "B" && !r.Failed && tB.IsZero() {
			tB = r.At
		}
	}
	if tB.IsZero() {
		viol = append(viol, "B never ran")
	} else if d := tB.Sub(killedAt); d > heldReclaimBound {
		viol = append(viol, fmt.Sprintf("B ran %s after the kill, want within %s: the ordinary window is at least ~37 s, so this is the ordinary path, not the held one", d.Round(100*time.Millisecond), heldReclaimBound))
	} else {
		t.Logf("held run killed; B ran %s later (reclaimed by the reaper of a live worker, lease not aged; bound %s)", d.Round(100*time.Millisecond), heldReclaimBound)
	}
	if len(viol) > 0 {
		t.Errorf("%s\nrequests: %s\nlog tail:\n%s", strings.Join(viol, "\n  "), seqOf(reqs), tail(first.output()+"\n--- second worker ---\n"+second.output(), 1800))
	}
}
