//go:build chaos

package chaos

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

type op struct {
	Op       string `json:"op"`
	Name     string `json:"name,omitempty"`
	Ms       int64  `json:"ms,omitempty"`
	Attempts int    `json:"attempts,omitempty"`
	Fail     int    `json:"fail,omitempty"`
}

type scenario struct {
	Name string
	Ops  []op
}

// The table: every op the script guest knows, in the combinations whose boundaries differ. cleat#4169 adds scripts generated from a seed.
var scenarios = []scenario{
	{"calls", []op{{Op: "call", Name: "A"}, {Op: "call", Name: "B"}, {Op: "call", Name: "C"}}},
	{"side_effect_and_sleep", []op{{Op: "call", Name: "A"}, {Op: "side_effect", Name: "S"}, {Op: "sleep", Ms: 300}, {Op: "call", Name: "B"}}},
	{"defer_first", []op{{Op: "defer", Name: "D"}, {Op: "call", Name: "A"}, {Op: "call", Name: "B"}}},
	{"mixed", []op{{Op: "call", Name: "A"}, {Op: "sleep", Ms: 500}, {Op: "side_effect", Name: "S"}, {Op: "defer", Name: "D"}, {Op: "call", Name: "B"}}},
	// A retried call whose first two attempts fail: the policy has work to do, so a kill lands INSIDE it (cleat#4152).
	{"four_calls", []op{{Op: "call", Name: "A"}, {Op: "call", Name: "B"}, {Op: "call", Name: "C"}, {Op: "call", Name: "D"}}},
	{"retry", []op{{Op: "call", Name: "A"}, {Op: "call_retry", Name: "R", Attempts: 5, Fail: 2}, {Op: "call", Name: "B"}}},
}

// fault is one injected failure at one step boundary: the nth request of an operation, held at "before" or "after" its effect, the worker
// SIGKILLed there, and then what happens to the run before the next worker takes it over.
type fault struct {
	Op    string
	Nth   int
	Mode  string   // "before" or "after"
	Then  string   // "reclaim" (the lease expires) or "terminate" (the run is terminated before anyone reclaims it)
	Flags []string // extra worker flags for BOTH workers: a fault that is a configuration (a write quota) rather than an action
}

func (f *fault) String() string {
	return fmt.Sprintf("sigkill@%s:%s#%d+%s", f.Mode, f.Op, f.Nth, f.Then)
}

type outcome struct {
	Status, ErrMsg, Result string
	Requests               []request
	Events                 int
	EventsAfterHold        int
	ReleaseAt, KillAt      time.Time
	Applied                bool // the fault actually fired: a fault that never did measures nothing
	Log                    string
}

const runBudget = 60 * time.Second

// dbWriteHold is how long the event writes stay refused. CLEAT_CHAOS_DBHOLD overrides it (a Go duration), so the same boundary can be run as a
// transient failure (well inside the flush retry window) and as one that outlasts it.
var dbWriteHold = func() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("CLEAT_CHAOS_DBHOLD")); err == nil {
		return d
	}
	return 5 * time.Second
}()

func scriptInput(run string, sc scenario) string {
	b, _ := json.Marshal(map[string]any{"__entry_point": "script", "run": run, "ops": sc.Ops})
	return string(b)
}

// terminate is phase 1 of the two-phase transition engine.preemptivelySettle writes for a run that owes a defer phase, as the crash tests do.
func terminate(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	if _, err := db.Exec(`UPDATE workflow_payloads SET error_msg = 'chaos terminate' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE workflow_leases
		SET status = 'terminating', pending_terminal_status = 'terminated',
		    defer_phase_deadline = now() + interval '300 seconds',
		    next_wake_at = now(), assigned_to = NULL, generation = generation + 1
		WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
}

// execute runs one scenario once, with or without a fault, on a fresh task queue and run id.
func execute(t *testing.T, env *env, sc scenario, f *fault) outcome {
	t.Helper()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	queue, run := "chaos-q-"+suffix, "chaos-run-"+suffix
	deploy(t, env.db, env.wasm, queue)

	first := startWorker(t, env.bin, queue, env.svc.srv.URL, flagsOf(f)...)
	startRun(t, env.db, run, scriptInput(run, sc), queue)

	var out outcome
	var second *worker
	wantTerminal := func(s string) bool { return s != "ready" && s != "running" }
	if f != nil {
		g := &gate{run: run, op: f.Op, nth: f.Nth, mode: f.Mode}
		env.svc.arm(g)
		defer env.svc.disarm()
		select {
		case <-g.arrived:
			out.Applied = true
			out.Events = eventCount(env.db, run)
			if f.Then == "dbwrite" {
				// The event write is refused while the call proceeds: the worker performs the effect, cannot record it, and (if it does not stop)
				// goes on. After a hold long enough to outlast the flush retry, the worker is killed and the privilege restored.
				denyEventWrites(t, env.db)
				out.ReleaseAt = time.Now()
				close(g.release)
				time.Sleep(dbWriteHold)
				out.EventsAfterHold = eventCount(env.db, run)
				out.KillAt = time.Now()
				first.kill()
				grantEventWrites(t, env.db)
			} else {
				first.kill()
			}
			close(g.abort) // after the kill, so a held call can never be performed by a worker that no longer exists
			ageLease(t, env.db, run)
			if f.Then == "terminate" {
				terminate(t, env.db, run)
				wantTerminal = func(s string) bool { return s == "terminated" || s == "failed" || s == "done" || s == "completed" }
			}
			second = startWorker(t, env.bin, queue, env.svc.srv.URL, flagsOf(f)...)
		case <-time.After(runBudget):
			first.kill() // the run finished or stalled without ever reaching the boundary
		}
	}
	status, msg, result, ok := awaitStatus(env.db, run, runBudget, wantTerminal)
	out.Status, out.ErrMsg, out.Result = status, msg, result
	if !ok {
		out.Status = "NOT-TERMINAL:" + status
	}
	out.Requests = env.svc.requestsOf(run)
	out.Log = first.output()
	if second != nil {
		out.Log += "\n--- second worker ---\n" + second.output()
	}
	return out
}

func flagsOf(f *fault) []string {
	if f == nil {
		return nil
	}
	return f.Flags
}

// goldenRecord is what the fault-free run did.
type goldenRecord struct {
	Reqs   []request // every request, in arrival order
	Status string
}

func (g goldenRecord) successes() map[string]int {
	m := map[string]int{}
	for _, r := range g.Reqs {
		if !r.Failed {
			m[r.Op]++
		}
	}
	return m
}

// boundaries lists every request of the golden run as a place to put a fault.
func (g goldenRecord) boundaries() []request { return g.Reqs }

func tally(reqs []request) (successes, attempts map[string]int, helds map[string]map[string]bool, firstOrder []string) {
	successes, attempts, helds = map[string]int{}, map[string]int{}, map[string]map[string]bool{}
	seen := map[string]bool{}
	for _, r := range reqs {
		attempts[r.Op]++
		if !r.Failed {
			successes[r.Op]++
		}
		if r.Held != "" {
			if helds[r.Op] == nil {
				helds[r.Op] = map[string]bool{}
			}
			helds[r.Op][r.Held] = true
		}
		if !seen[r.Op] {
			seen[r.Op] = true
			firstOrder = append(firstOrder, r.Op)
		}
	}
	return
}

// verifyReclaim checks a run that was killed at a boundary and then RECLAIMED: it must finish as the golden run did, every effect exactly
// once, except the one the kill left unrecorded (performed, reply withheld), which the documented at-least-once window allows to repeat once.
func verifyReclaim(sc scenario, g goldenRecord, f *fault, b request, o outcome) []string {
	var v []string
	if !o.Applied {
		return []string{"UNMEASURED: the fault never fired (the run did not reach the boundary), so nothing here is evidence"}
	}
	if o.Status != g.Status {
		v = append(v, fmt.Sprintf("final status %q, golden %q (error: %s)", o.Status, g.Status, o.ErrMsg))
	}
	gs := g.successes()
	successes, attempts, helds, first := tally(o.Requests)
	for op, want := range gs {
		w := want
		if f.Op == op && f.Mode == "after" && !b.Failed {
			w = want + 1 // the documented at-least-once window: performed, never recorded, so replayed once
		}
		if successes[op] != w {
			v = append(v, fmt.Sprintf("effect %q succeeded %d time(s), want %d", op, successes[op], w))
		}
	}
	for op := range successes {
		if _, ok := gs[op]; !ok {
			v = append(v, fmt.Sprintf("effect %q happened but the golden run never did it", op))
		}
	}
	for _, o2 := range sc.Ops {
		if o2.Op == "call_retry" && attempts[o2.Name] > o2.Attempts+1 {
			v = append(v, fmt.Sprintf("%q made %d attempts under a policy of %d (one more is allowed for the kill)", o2.Name, attempts[o2.Name], o2.Attempts))
		}
	}
	var gfirst []string
	{
		_, _, _, gfirst = tally(g.Reqs)
	}
	if strings.Join(first, ",") != strings.Join(gfirst, ",") {
		v = append(v, fmt.Sprintf("effects first happened in order %v, golden %v", first, gfirst))
	}
	for op, hs := range helds {
		if len(hs) > 1 {
			v = append(v, fmt.Sprintf("side effect %q reported %d different held values %v: a replay recomputed what should have been frozen", op, len(hs), hs))
		}
	}
	return v
}

// verifyTerminate checks a run killed at a boundary and then TERMINATED before anyone reclaimed it. Its defer segment runs the registered
// cleanups and NOTHING ELSE: the body must not make a call it had not made (cleat#4143). So the expected effects are exactly those the body had
// performed by the kill, each once, plus each cleanup registered by then, once.
func verifyTerminate(sc scenario, g goldenRecord, f *fault, b request, o outcome) []string {
	var v []string
	if !o.Applied {
		return []string{"UNMEASURED: the fault never fired (the run did not reach the boundary), so nothing here is evidence"}
	}
	if o.Status != "terminated" {
		v = append(v, fmt.Sprintf("final status %q, want terminated (error: %s)", o.Status, o.ErrMsg))
	}
	// What the body had done at the kill: every golden success before the boundary request, and the boundary one itself if it was performed.
	want := map[string]int{}
	performedBody := map[string]bool{}
	for _, r := range g.Reqs {
		if r.Op == b.Op && r.Nth == b.Nth {
			if f.Mode == "after" && !r.Failed {
				want[r.Op]++
			}
			break
		}
		if !r.Failed && !strings.HasPrefix(r.Op, "defer_") {
			want[r.Op]++
			performedBody[r.Op] = true
		}
	}
	// Cleanups registered before the boundary run once each in the defer segment: a defer scripted before the op the boundary belongs to.
	for _, o2 := range sc.Ops {
		if o2.Op != "defer" && o2.Name == b.Op {
			break
		}
		if o2.Op == "defer" {
			want["defer_"+o2.Name]++
		}
	}
	// The loop above counts every defer up to the boundary's op, and stops there. A defer scripted after it is not registered by the kill.
	successes, _, _, _ := tally(o.Requests)
	for op, w := range want {
		if successes[op] != w {
			v = append(v, fmt.Sprintf("effect %q succeeded %d time(s), want %d (terminated body: nothing new, each cleanup once)", op, successes[op], w))
		}
	}
	for op, n := range successes {
		if _, ok := want[op]; !ok && n > 0 {
			v = append(v, fmt.Sprintf("effect %q happened %d time(s) in a terminated run's defer segment; the body had not made it and must not", op, n))
		}
	}
	return v
}

type env struct {
	db   *sql.DB
	bin  string
	wasm []byte
	svc  *service
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db := ownerDB(t)
	bin, wasm := build(t)
	return &env{db: db, bin: bin, wasm: wasm, svc: newService(t)}
}

// runBoundariesBefore is runBoundaries for faults that are placed only at "before" a request.
func runBoundariesBefore(t *testing.T, then string, verify func(scenario, goldenRecord, *fault, request, outcome) []string) {
	onlyBefore = true
	defer func() { onlyBefore = false }()
	runBoundaries(t, then, verify)
}

var onlyBefore bool

func runBoundaries(t *testing.T, then string, verify func(scenario, goldenRecord, *fault, request, outcome) []string, flags ...string) {
	e := newEnv(t)
	only := os.Getenv("CLEAT_CHAOS_SCENARIO")
	for _, sc := range scenarios {
		if only != "" && only != sc.Name {
			continue
		}
		sc := sc
		t.Run(sc.Name, func(t *testing.T) {
			gold := execute(t, e, sc, nil)
			if gold.Status != "done" && gold.Status != "completed" {
				t.Fatalf("the FAULT-FREE run ended %q (%s): the golden record is not a record\n--- worker log (tail) ---\n%s", gold.Status, gold.ErrMsg, tail(gold.Log, 3000))
			}
			g := goldenRecord{Reqs: gold.Requests, Status: gold.Status}
			t.Logf("golden: %d requests status=%s result=%s", len(g.Reqs), g.Status, gold.Result)
			for _, b := range g.boundaries() {
				if then == "terminate" && strings.HasPrefix(b.Op, "defer_") {
					continue // a kill during a run's own defer drain is a different scenario from terminating a body mid-flight
				}
				for _, mode := range []string{"before", "after"} {
					if onlyBefore && mode == "after" {
						continue
					}
					f := &fault{Op: b.Op, Nth: b.Nth, Mode: mode, Then: then, Flags: flags}
					b := b
					t.Run(f.String(), func(t *testing.T) {
						o := execute(t, e, sc, f)
						viol := verify(sc, g, f, b, o)
						t.Logf("applied=%v events-at-kill=%d events-after-hold=%d status=%s requests=%s err=%.160s", o.Applied, o.Events, o.EventsAfterHold, o.Status, seqOf(o.Requests), o.ErrMsg)
						if len(viol) > 0 {
							seq := make([]string, 0, len(o.Requests))
							for _, r := range o.Requests {
								s := r.Op
								if r.Failed {
									s += "(fail)"
								}
								seq = append(seq, s)
							}
							if len(seq) > 40 {
								seq = append(seq[:40], fmt.Sprintf("... (%d requests in all)", len(o.Requests)))
							}
							t.Errorf("scenario=%s fault=%s\n  %s\n  requests in order: %v\n  result: %s\n  --- worker log (tail) ---\n%s", sc.Name, f, strings.Join(viol, "\n  "), seq, o.Result, tail(o.Log, 2500))
						}
					})
				}
			}
		})
	}
}

func TestChaosSigkillThenReclaimAtEveryBoundary(t *testing.T) {
	runBoundaries(t, "reclaim", verifyReclaim)
}

// verifyNoUnrecordedEffectRepeats is the oracle for a write that is REFUSED (a quota): whatever the run does about it, an effect must not be
// performed twice because its event was never durable (cleat#4160). Status is not asserted: stopping the run is a correct answer; carrying on
// with the step missing from history, so that a reclaim performs it again, is not.
func verifyNoUnrecordedEffectRepeats(sc scenario, g goldenRecord, f *fault, b request, o outcome) []string {
	var v []string
	if !o.Applied {
		// The run never reached the boundary. Usually that is UNMEASURED; but a run that made MORE requests than the golden run without ever
		// reaching it has repeated effects, which is a finding in its own right.
		if len(o.Requests) > len(g.Reqs) {
			return []string{fmt.Sprintf("the run never reached the boundary and made %d requests (the golden run made %d in all): effects were repeated before the fault could be placed", len(o.Requests), len(g.Reqs))}
		}
		return []string{"UNMEASURED: the fault never fired (the run did not reach the boundary), so nothing here is evidence"}
	}
	successes, _, _, _ := tally(o.Requests)
	for op, n := range successes {
		w := 1
		if f.Op == op && (f.Mode == "after" || f.Then == "dbwrite") {
			// The documented at-least-once window: the call performed, its event never became durable, the worker died, the reclaim performs it
			// once more. A "dbwrite" fault RELEASES the held call, so the call it is placed at is in that window whatever the mode.
			w = 2
		}
		if n > w {
			v = append(v, fmt.Sprintf("effect %q succeeded %d time(s), at most %d allowed: it was performed again on the reclaim because its event never became durable (status %s)", op, n, w, o.Status))
		}
	}
	return v
}

// verifyRefusedWriteStopsTheGuest is the oracle for an event write that is refused for longer than the flush retry window (cleat#4160): once
// the durable write of a step has failed, NO FURTHER effect may be performed by that run, because a step that is missing from the history will
// be performed again by the next worker. The released call itself is the one the write was refused for, so it is the only one allowed.
func verifyRefusedWriteStopsTheGuest(sc scenario, g goldenRecord, f *fault, b request, o outcome) []string {
	v := verifyNoUnrecordedEffectRepeats(sc, g, f, b, o)
	if !o.Applied {
		return v
	}
	var after []string
	for _, r := range o.Requests {
		if r.At.After(o.ReleaseAt) && r.At.Before(o.KillAt) && !(r.Op == f.Op && r.Nth == f.Nth) {
			s := r.Op
			if r.Failed {
				s += "(fail)"
			}
			after = append(after, s)
		}
	}
	if len(after) > 0 {
		v = append(v, fmt.Sprintf("after the durable write of step %q was refused for %v the run went on to perform %v: none of those steps is in event_history (%d event(s) durable), "+
			"so a reclaim performs every one of them again", f.Op, dbWriteHold, after, o.EventsAfterHold))
	}
	return v
}

func TestChaosEventWriteRefusedThenReclaimAtEveryBoundary(t *testing.T) {
	runBoundariesBefore(t, "dbwrite", verifyRefusedWriteStopsTheGuest)
}

func TestChaosSigkillThenTerminateAtEveryBoundary(t *testing.T) {
	runBoundaries(t, "terminate", verifyTerminate)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func seqOf(reqs []request) string {
	var b []string
	for _, r := range reqs {
		s := r.Op
		if r.Failed {
			s += "(fail)"
		}
		b = append(b, s)
	}
	return strings.Join(b, " ")
}
