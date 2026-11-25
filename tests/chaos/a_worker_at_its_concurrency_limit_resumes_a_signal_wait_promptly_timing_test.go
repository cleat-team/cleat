//go:build chaos && timing

package chaos

import (
	"fmt"
	"sort"
	"testing"
	"time"
)

// cleat#4311. A worker at its --concurrency limit slept for a whole --poll tick whenever it had no free slot, and nothing could end that sleep early. A signal's NOTIFY wakes the dispatch
// loop, but a loop that is asleep in the at-capacity branch is not listening, so on a --concurrency 1 worker a suspended run's resume waited for the tick: the slot the previous
// activity had just freed went unused until it passed.
//
// Real worker binary, real PostgreSQL with the default NOTIFY listener on, residency OFF (so the wait suspends and the signal redispatches the run), --concurrency 1 and the production
// --poll of 500 ms. The signal is sent 150 ms after the run parks: the worker has just finished its slot's work, which is the window the old sleep covered. Measured 2026-11-25 on an
// Apple M4 against PostgreSQL 16, 30 trials per side after a discarded warm-up each: develop 355-412 ms (median 385), with the fix 92-110 ms (median 103). The bar is 250 ms, which
// separates them by ~140 ms either way. The same row with a 1500 ms settle, or with --concurrency 4, comes out level on both sides, so those are not here.
//
// WHAT THIS DOES NOT SHOW: other machines (a slow runner can put the fixed side over the bar, which ends as a FAILURE that prints every sample, never a pass), MySQL and SQL Server (they
// have no NOTIFY, so their resume is poll-bound by design), or a worker with several busy slots. The first trial is discarded (it compiles the module).
const (
	atLimitTrials = 8
	atLimitSettle = 150 * time.Millisecond
	atLimitBar    = 250 * time.Millisecond
)

func TestAWorkerAtItsConcurrencyLimitResumesASignalWaitWithoutWaitingForItsPollTickOnPostgres(t *testing.T) {
	env := newEnv(t)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	queue := "chaos-atlimit-" + suffix
	addr := freeLoopbackAddr(t)
	deploy(t, env.db, env.wasm, queue)
	startWorker(t, env.bin, queue, env.svc.srv.URL, "--api-addr", addr, "--resident-max-runs", "0", "--poll", timingPoll.String(), "--concurrency", "1")
	key := apiKey(t, env.bin)
	sc := scenario{"await_signal_then_call", []op{{Op: "call", Name: "A"}, {Op: "await_signal", Name: "go", Ms: timingAwaitMs, Hit: "HIT", Miss: "MISS"}, {Op: "call", Name: "B"}}, nil}

	var samples []time.Duration
	for i := 0; i <= atLimitTrials; i++ {
		run := fmt.Sprintf("chaos-atlimit-%d-%s", i, suffix)
		startRun(t, env.db, run, scriptInput(run, sc), queue)
		asleep, detail := false, ""
		for deadline := time.Now().Add(timingStepWait); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			sawA := false
			for _, r := range env.svc.requestsOf(run) {
				if r.Op == "A" && !r.Failed {
					sawA = true
				}
			}
			if !sawA {
				continue
			}
			n, ok := residentRuns(addr)
			parked := parkedNow(env.db, run)
			asleep = ok && n == 0 && parked
			detail = fmt.Sprintf("cleat_resident_runs=%v (readable=%v) parked in the store=%v", n, ok, parked)
			if asleep {
				break
			}
		}
		if !asleep {
			t.Fatalf("UNMEASURED: trial %d: the run was never parked with the worker holding nothing (%s)", i, detail)
		}
		time.Sleep(atLimitSettle)
		t0 := time.Now()
		if err := signalOverHTTP(addr, key, run, "go"); err != nil {
			t.Fatalf("UNMEASURED: trial %d: delivering the signal: %v", i, err)
		}
		var at time.Time
		for time.Now().Before(t0.Add(timingStepWait)) && at.IsZero() {
			for _, r := range env.svc.requestsOf(run) {
				if r.Op == "HIT" && !r.Failed {
					at = r.At
				}
			}
			time.Sleep(2 * time.Millisecond)
		}
		if at.IsZero() {
			t.Fatalf("UNMEASURED: trial %d: the guest's next step (HIT) never arrived within %s", i, timingStepWait)
		}
		if _, _, _, ok := awaitStatus(env.db, run, timingStepWait, func(s string) bool { return s != "ready" && s != "running" }); !ok {
			t.Fatalf("UNMEASURED: trial %d: the run did not finish", i)
		}
		if i == 0 {
			continue // the warm-up
		}
		samples = append(samples, at.Sub(t0))
	}

	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	median := (sorted[len(sorted)/2-1] + sorted[len(sorted)/2]) / 2
	t.Logf("a signal's commit to the guest's next step on a --concurrency 1 worker, --poll %s, %s after the run parked: N=%d median=%s min=%s max=%s samples=%v",
		timingPoll, atLimitSettle, len(sorted), median.Round(time.Millisecond), sorted[0].Round(time.Millisecond), sorted[len(sorted)-1].Round(time.Millisecond), samples)
	if median >= atLimitBar {
		t.Fatalf("the median resume was %s (N=%d, bar %s, samples %v): a worker at its --concurrency limit is waiting for its --poll tick (%s) instead of waking when a slot is freed or a signal is delivered (cleat#4311). "+
			"If every sample is near 350-400 ms the dispatch loop's at-capacity wait has lost its wake; if they are all far above both, the machine is too loaded to measure and this is not a finding about the fix",
			median.Round(time.Millisecond), len(sorted), atLimitBar, samples, timingPoll)
	}
}
