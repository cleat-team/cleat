//go:build chaos && timing

package chaos

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// cleat#4306, the gate PR 3 ships on. The bar (coordinator, 2026-11-21) is the END-TO-END time from a signal's commit to the guest's next step, at p50 AND p99, counting the replay a hold
// saves: on PostgreSQL a delivery's NOTIFY redispatches a suspended waiter at once, so a held run has to beat notify + claim + instantiate + replay, not merely a poll.
//
// Real worker binaries, real PostgreSQL with the DEFAULT --notify-channel listener on, the real HTTP API delivering the signal (so the production pg_notify path runs), and the chaos service
// recording WHEN the guest's next step (the call named for the branch it took) arrived. Three arms, INTERLEAVED trial by trial so drift in the machine cancels:
//
//	held     --resident-max-runs 8: the wait is held, and the NOTIFY nudge wakes it
//	off      stock flags: the wait suspends and the NOTIFY redispatches the run, which replays
//	control  held, but --notify-channel "" and --poll 5s: the nudge is gone, so the hold waits for its tick. This arm EXISTS so the row can fail: it must come out LATER than off.
//
// WHAT THIS ROW MEASURES IS THE END-TO-END BAR, NOT THE NUDGE. At the production default poll a hold that waited only for its tick is roughly level with a suspended run on a minimal
// guest, so removing the nudge makes this row fail only marginally (measured: at p99, by 6 ms). The nudge itself is pinned elsewhere (engine/a_notify_wakes_a_held_signal_wait_on_real_postgres_test.go:
// 9 ms against a 3 s interval, 2.8 s with it off). Do not read a green here as evidence the nudge works, or a thin margin as evidence it does not.
//
// WHAT THIS DOES NOT SHOW: one worker per arm with --concurrency 1 and a service on the same machine, so queueing, a loaded database and a second worker process are not in it. The first trial of
// every arm is discarded (it compiles the module). N and the margin are in the log line.

const (
	timingTrials   = 100 // measured trials per arm, after one discarded warm-up: enough that p99 is a percentile and not just the maximum of a few. CLEAT_TIMING_TRIALS overrides it for a quick local run.
	timingControl  = 8   // control trials, spread evenly across the run: each waits for a 5 s tick, so it is the expensive arm
	timingMargin   = 50 * time.Millisecond
	timingAwaitMs  = 8000                   // inside the default 10 s window, so the held arms hold it
	timingPoll     = 500 * time.Millisecond // the worker's default --poll: the held arm's recheck tick and the off arm's claim poll
	timingCtlPoll  = 5 * time.Second
	timingStepWait = 60 * time.Second
)

type timingArm struct {
	name    string
	flags   []string
	held    bool // the worker is expected to hold the run
	addr    string
	queue   string
	samples []time.Duration
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted))*p + 0.999999999) // nearest rank, rounded up
	if idx < 1 {
		idx = 1
	}
	if idx > len(sorted) {
		idx = len(sorted)
	}
	return sorted[idx-1]
}

func summarise(d []time.Duration) (p50, p99, mx time.Duration) {
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return percentile(s, 0.50), percentile(s, 0.99), s[len(s)-1]
}

// signalOverHTTP delivers the signal through the worker's own API, the way a client does, so the store it writes through is the production one with its notify channel.
func signalOverHTTP(addr, key, run, name string) error {
	body := fmt.Sprintf(`{"signal_name":%q,"payload":"{}"}`, name)
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/api/workflows/"+run+"/signal", bytes.NewBufferString(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("POST signal: HTTP %d", resp.StatusCode)
	}
	return nil
}

// apiKey mints a key for the default tenant, the way tests/typescript does: the worker binary's own --generate-api-key against the app DSN. It needs the schema, which a worker applies at
// start (--migrate-on-start), so it is retried until the first worker has finished migrating.
func apiKey(t *testing.T, bin string) string {
	t.Helper()
	const defaultTenant = "00000000-0000-0000-0000-000000000000"
	var last string
	for deadline := time.Now().Add(timingStepWait); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		out, err := exec.Command(bin, "--driver=postgres", "--db="+appDSN(t), "--generate-api-key="+defaultTenant).CombinedOutput()
		last = string(out)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "Key:") {
				if f := strings.Fields(line); len(f) == 2 {
					return f[1]
				}
			}
		}
	}
	t.Fatalf("UNMEASURED: no API key could be minted for the signal calls:\n%s", last)
	return ""
}

func TestAHeldSignalWaitResumesNoLaterThanASuspendedOneOnPostgres(t *testing.T) {
	env := newEnv(t)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	arms := []*timingArm{
		// --poll 500ms is the PRODUCTION default; the harness's own 100ms would let the held arm's tick alone keep up with a suspended run, and the row could not tell the nudge from it.
		{name: "held", held: true, flags: []string{"--resident-max-runs", "8", "--poll", timingPoll.String()}},
		{name: "off", held: false, flags: []string{"--poll", timingPoll.String()}},
		{name: "control", held: true, flags: []string{"--resident-max-runs", "8", "--notify-channel", "", "--poll", timingCtlPoll.String()}},
	}
	for _, a := range arms {
		a.queue = "chaos-timing-" + a.name + "-" + suffix
		a.addr = freeLoopbackAddr(t)
		deploy(t, env.db, env.wasm, a.queue)
		flags := append([]string{"--api-addr", a.addr}, a.flags...)
		startWorker(t, env.bin, a.queue, env.svc.srv.URL, flags...)
	}
	key := apiKey(t, env.bin)

	script := []op{{Op: "call", Name: "A"}, {Op: "await_signal", Name: "go", Ms: timingAwaitMs, Hit: "HIT", Miss: "MISS"}, {Op: "call", Name: "B"}}
	sc := scenario{"await_signal_then_call", script, nil}

	trial := func(a *timingArm, n int) (time.Duration, string) {
		run := fmt.Sprintf("chaos-timing-%s-%d-%s", a.name, n, suffix)
		startRun(t, env.db, run, scriptInput(run, sc), a.queue)
		// The run has made its first call, then is asleep in the way this arm puts it asleep: HELD (the worker says it holds a run and the store still has it running) or
		// PARKED (the worker holds nothing and the store has it ready and unassigned).
		deadline := time.Now().Add(timingStepWait)
		asleep := false
		detail := ""
		for time.Now().Before(deadline) {
			sawA := false
			for _, r := range env.svc.requestsOf(run) {
				if r.Op == "A" && !r.Failed {
					sawA = true
				}
			}
			if sawA {
				n, ok := residentRuns(a.addr)
				parked := parkedNow(env.db, run)
				if a.held {
					asleep = ok && n >= 1 && !parked
				} else {
					asleep = ok && n == 0 && parked
				}
				detail = fmt.Sprintf("cleat_resident_runs=%v (readable=%v) parked in the store=%v", n, ok, parked)
				if asleep {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !asleep {
			return 0, "UNMEASURED: the run was never asleep in the way the " + a.name + " arm needs (" + detail + ")"
		}
		time.Sleep(150 * time.Millisecond) // the hold has made its admission recheck and is waiting; the parked run is settled
		t0 := time.Now()
		if err := signalOverHTTP(a.addr, key, run, "go"); err != nil {
			return 0, "UNMEASURED: delivering the signal: " + err.Error()
		}
		var at time.Time
		for time.Now().Before(t0.Add(timingStepWait)) {
			for _, r := range env.svc.requestsOf(run) {
				if r.Op == "HIT" && !r.Failed {
					at = r.At
				}
			}
			if !at.IsZero() {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if at.IsZero() {
			return 0, "UNMEASURED: the guest's next step (HIT) never arrived within " + timingStepWait.String()
		}
		// Let the run finish so a concurrency-1 worker is free for the next trial.
		if _, _, _, ok := awaitStatus(env.db, run, timingStepWait, func(s string) bool { return s != "ready" && s != "running" }); !ok {
			return 0, "UNMEASURED: the run did not finish"
		}
		return at.Sub(t0), ""
	}

	// Interleaved: held, off, then (every few rounds) control, so a slow patch lands on every arm alike. Trial 0 of each arm is the discarded warm-up.
	trials := timingTrials
	if v, err := strconv.Atoi(os.Getenv("CLEAT_TIMING_TRIALS")); err == nil && v > 0 {
		trials = v
	}
	rounds := trials + 1
	ctl := 0
	every := rounds / timingControl // the control runs in round 0 (its warm-up) and then every `every` rounds, about timingControl times in all
	if every < 1 {
		every = 1
	}
	for i := 0; i < rounds; i++ {
		for _, a := range arms {
			if a.name == "control" && i != 0 && i%every != 0 {
				continue
			}
			d, why := trial(a, i)
			if why != "" {
				t.Fatalf("%s arm, trial %d: %s", a.name, i, why)
			}
			if a.name == "control" {
				ctl++
			}
			if i == 0 {
				continue // warm-up
			}
			a.samples = append(a.samples, d)
		}
	}

	var held, off, control *timingArm
	for _, a := range arms {
		switch a.name {
		case "held":
			held = a
		case "off":
			off = a
		case "control":
			control = a
		}
	}
	rep := func(a *timingArm) (p50, p99 time.Duration) {
		p50, p99, mx := summarise(a.samples)
		t.Logf("%-8s N=%-3d p50=%-8s p99=%-8s max=%s", a.name, len(a.samples), p50.Round(time.Millisecond), p99.Round(time.Millisecond), mx.Round(time.Millisecond))
		return p50, p99
	}
	hp50, hp99 := rep(held)
	op50, op99 := rep(off)
	cp50, _ := rep(control)
	t.Logf("margin %s (noise allowance; the measured gap is far larger than this when the mechanism works)", timingMargin)

	if len(held.samples) < trials || len(off.samples) < trials {
		t.Fatalf("UNMEASURED: held N=%d off N=%d, want %d each", len(held.samples), len(off.samples), trials)
	}
	if len(control.samples) < 3 {
		t.Fatalf("UNMEASURED: the control arm has only %d samples, too few to tell it from noise", len(control.samples))
	}
	// THE INSTRUMENT CAN SEE LATENESS: with the nudge gone and a 5 s tick the held run is later than a suspended one. Without this, "held is not later" is also what a harness that
	// cannot tell the arms apart would report.
	if cp50 <= op50 {
		t.Fatalf("CONTROL FAILED: the held arm with no nudge (p50 %s) is not later than the suspended arm (p50 %s): the row cannot tell a late hold from a prompt one", cp50, op50)
	}
	if hp50 > op50+timingMargin {
		t.Errorf("a held signal wait resumed LATER than a suspended one at p50: held %s, off %s (margin %s)", hp50, op50, timingMargin)
	}
	if hp99 > op99+timingMargin {
		t.Errorf("a held signal wait resumed LATER than a suspended one at p99: held %s, off %s (margin %s): PR 3 does not ship on Postgres without a fix", hp99, op99, timingMargin)
	}
}
