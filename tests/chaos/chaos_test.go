//go:build chaos

package chaos

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
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
	Hit      string `json:"hit,omitempty"`
	Miss     string `json:"miss,omitempty"`
	Delay    int    `json:"delay,omitempty"`
}

type scenario struct {
	Name string
	Ops  []op
	// Drive, when set, runs beside the run in every execution of the scenario (the golden one included), so an outside event the script waits for
	// happens the same way each time. It must only read and write through the database and must not call t.
	Drive func(db *sql.DB, run string)
}

// The table: every op the script guest knows, in the combinations whose boundaries differ. cleat#4169 adds scripts generated from a seed.
var scenarios = []scenario{
	{"calls", []op{{Op: "call", Name: "A"}, {Op: "call", Name: "B"}, {Op: "call", Name: "C"}}, nil},
	{"side_effect_and_sleep", []op{{Op: "call", Name: "A"}, {Op: "side_effect", Name: "S"}, {Op: "sleep", Ms: 300}, {Op: "call", Name: "B"}}, nil},
	{"defer_first", []op{{Op: "defer", Name: "D"}, {Op: "call", Name: "A"}, {Op: "call", Name: "B"}}, nil},
	{"mixed", []op{{Op: "call", Name: "A"}, {Op: "sleep", Ms: 500}, {Op: "side_effect", Name: "S"}, {Op: "defer", Name: "D"}, {Op: "call", Name: "B"}}, nil},
	// A retried call whose first two attempts fail: the policy has work to do, so a kill lands INSIDE it (cleat#4152).
	{"four_calls", []op{{Op: "call", Name: "A"}, {Op: "call", Name: "B"}, {Op: "call", Name: "C"}, {Op: "call", Name: "D"}}, nil},
	// A signal wait with no wait, then a call named for the branch taken. With no signal the golden run takes MISS. A signal that arrives while
	// the worker is dead must not change the branch a replay takes (cleat#4153).
	{"poll_signal", []op{{Op: "await0", Name: "go", Hit: "HIT", Miss: "MISS"}, {Op: "call", Name: "B"}}, nil},
	// An await of a promise nobody resolves times out; the call after it must be performed once however often the run is replayed (cleat#4177).
	{"await_promise", []op{{Op: "await_promise", Name: "p", Ms: 1500, Hit: "RES", Miss: "TIMEDOUT"}, {Op: "call", Name: "B"}}, nil},
	// A poll with no wait, three calls, a sleep and a call: the sleep suspends the run and the wake REPLAYS the whole prefix. Found by the seeded
	// generator on develop (seed 4175): the fault-free run repeated the prefix 130+ times. A timed-out poll records nothing (cleat#4153, #4177).
	{"poll_then_sleep", []op{{Op: "await0", Name: "go", Hit: "HIT", Miss: "MISS"}, {Op: "call", Name: "C"}, {Op: "call", Name: "D"}, {Op: "sleep", Ms: 300}, {Op: "call", Name: "E"}}, nil},
	// The promise IS resolved, after the run has suspended in the await: resume with the resolution, then a call, then replay again (cleat#4177).
	// The effect count is the oracle: the recorded history can look right while a replay re-runs the call.
	{"await_promise_resolved", []op{{Op: "await_promise", Name: "p", Ms: 20000, Hit: "RES", Miss: "TIMEDOUT"}, {Op: "call", Name: "B"}}, resolveWhenSuspended},
	{"retry", []op{{Op: "call", Name: "A"}, {Op: "call_retry", Name: "R", Attempts: 5, Fail: 2}, {Op: "call", Name: "B"}}, nil},
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

const runBudget = 120 * time.Second

// dbWriteHold is how long the event writes stay refused. CLEAT_CHAOS_DBHOLD overrides it (a Go duration), so the same boundary can be run as a
// transient failure (well inside the flush retry window) and as one that outlasts it.
var dbWriteHold = func() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("CLEAT_CHAOS_DBHOLD")); err == nil {
		return d
	}
	return 5 * time.Second
}()

func scriptInput(run string, sc scenario) string {
	// A Go module exports one function per entry point; a Python component exports exactly one, `run`, which dispatches inside the guest
	// (cleat#2937), so the worker is told to call `run`.
	entry := "script"
	if guestLanguage() == "python" {
		entry = "run"
	}
	b, _ := json.Marshal(map[string]any{"__entry_point": entry, "run": run, "ops": sc.Ops})
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
	if sc.Drive != nil {
		go sc.Drive(env.db, run)
	}

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
			if f.Then == "signal" {
				deliverSignal(t, env.db, run, "go")
			}
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

// runFault runs one fault at one boundary of a scenario and reports every violation of the oracle, with what is needed to reproduce it.
func runFault(t *testing.T, e *env, sc scenario, g goldenRecord, f *fault, b request, verify func(scenario, goldenRecord, *fault, request, outcome) []string, repro string) {
	t.Helper()
	o := execute(t, e, sc, f)
	viol := verify(sc, g, f, b, o)
	t.Logf("applied=%v events-at-kill=%d events-after-hold=%d status=%s requests=%s err=%.160s", o.Applied, o.Events, o.EventsAfterHold, o.Status, seqOf(o.Requests), o.ErrMsg)
	if len(viol) == 0 {
		return
	}
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
	t.Errorf("scenario=%s fault=%s\n  %s\n  requests in order: %v\n  result: %s\n  reproduce: %s\n  --- worker log (tail) ---\n%s",
		sc.Name, f, strings.Join(viol, "\n  "), seq, o.Result, repro, tail(o.Log, 2500))
}

func goldenOf(t *testing.T, e *env, sc scenario) goldenRecord {
	t.Helper()
	gold := execute(t, e, sc, nil)
	if gold.Status != "done" && gold.Status != "completed" {
		t.Fatalf("the FAULT-FREE run ended %q (%s): the golden record is not a record\n--- worker log (tail) ---\n%s", gold.Status, gold.ErrMsg, tail(gold.Log, 3000))
	}
	// A fault-free run performs every effect exactly once. Not a fault finding but the precondition of every comparison below: when it
	// fails, the engine repeated work with NO fault injected, and a golden record built from it would bless the repetition.
	succ, _, _, _ := tally(gold.Requests)
	var repeated []string
	for name, n := range succ {
		if n != 1 {
			repeated = append(repeated, fmt.Sprintf("%s x%d", name, n))
		}
	}
	sort.Strings(repeated)
	if len(repeated) > 0 {
		seq := seqOf(gold.Requests)
		if len(seq) > 400 {
			seq = seq[:400] + " ..."
		}
		t.Fatalf("the FAULT-FREE run of %s performed effects other than once: %s (%d requests in all). No fault was injected.\n  requests: %s", sc.Name, strings.Join(repeated, ", "), len(gold.Requests), seq)
	}
	g := goldenRecord{Reqs: gold.Requests, Status: gold.Status}
	t.Logf("golden: %d requests status=%s result=%s", len(g.Reqs), g.Status, gold.Result)
	if seq := seqOf(g.Reqs); len(seq) > 300 {
		t.Logf("golden requests (first 300 chars): %s ...", seq[:300])
	} else {
		t.Logf("golden requests: %s", seq)
	}
	return g
}

func runBoundaries(t *testing.T, then string, verify func(scenario, goldenRecord, *fault, request, outcome) []string, flags ...string) {
	only := os.Getenv("CLEAT_CHAOS_SCENARIO")
	if only != "" {
		found := false
		for _, sc := range scenarios {
			found = found || sc.Name == only
		}
		if !found {
			// A misspelt name must not select nothing and pass: `go test` is silent about a selection that matched nothing.
			t.Fatalf("CLEAT_CHAOS_SCENARIO=%q names no scenario", only)
		}
	}
	e := newEnv(t)
	for _, sc := range scenarios {
		if only != "" && only != sc.Name {
			continue
		}
		sc := sc
		t.Run(sc.Name, func(t *testing.T) {
			g := goldenOf(t, e, sc)
			for _, b := range g.boundaries() {
				for _, mode := range []string{"before", "after"} {
					if !applies(then, mode, b, g) {
						continue
					}
					f := &fault{Op: b.Op, Nth: b.Nth, Mode: mode, Then: then, Flags: flags}
					b := b
					t.Run(f.String(), func(t *testing.T) {
						runFault(t, e, sc, g, f, b, verify, fmt.Sprintf("CLEAT_CHAOS_SCENARIO=%s go test -tags chaos ./tests/chaos -run '%s/%s/%s'", sc.Name, t.Name()[:strings.Index(t.Name(), "/")], sc.Name, f))
					})
				}
			}
		})
	}
}

// applies reports whether a fault of kind `then` is meaningful at this boundary.
func applies(then, mode string, b request, g goldenRecord) bool {
	switch {
	case then == "terminate" && strings.HasPrefix(b.Op, "defer_"):
		return false // a kill during a run's own defer drain is a different scenario from terminating a body mid-flight
	case then == "signal" && mode == "before" && b == g.Reqs[0]:
		return false // before the first effect nothing of the poll's outcome has been acted on, so a signal that lands now may legitimately be seen
	case then == "dbwrite" && mode == "after":
		return false // the fault releases the held call itself
	}
	return true
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
	runBoundaries(t, "dbwrite", verifyRefusedWriteStopsTheGuest)
}

// A signal that arrives while the worker is dead must not change what the replay does: the original run was told "no signal".
func TestChaosSigkillThenSignalThenReclaimAtEveryBoundary(t *testing.T) {
	runBoundaries(t, "signal", verifyReclaim)
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

// A run that registered a cleanup and then parks (a sleep) before a body ContinueAsNew. TERMINATING it while it is parked runs its defer
// segment, which replays the body to the frontier: the body's ContinueAsNew must NOT be taken there, and the cleanup must run once (cleat#4159).
var parkedScenario = scenario{"defer_sleep_can", []op{{Op: "defer", Name: "D"}, {Op: "sleep", Ms: 4000}, {Op: "continue_as_new", Name: "POST"}}, nil}

func continuations(db *sql.DB, id string) int {
	var n int
	_ = db.QueryRow(`SELECT count(*) FROM workflow_leases WHERE continued_from = $1`, id).Scan(&n)
	return n
}

// executeParked runs the scenario, waits until `events` events are durable (the run is parked), and then terminates it. No worker is killed:
// the fault is the terminate landing on a parked run.
func executeParked(t *testing.T, env *env, sc scenario, events int, terminateIt bool) (o outcome, conts int) {
	t.Helper()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	queue, run := "chaos-q-"+suffix, "chaos-run-"+suffix
	deploy(t, env.db, env.wasm, queue)
	w := startWorker(t, env.bin, queue, env.svc.srv.URL)
	startRun(t, env.db, run, scriptInput(run, sc), queue)
	if terminateIt {
		deadline := time.Now().Add(runBudget)
		for eventCount(env.db, run) < events && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		o.Events = eventCount(env.db, run)
		o.Applied = o.Events >= events
		terminate(t, env.db, run)
	}
	wantDone := func(s string) bool { return s != "ready" && s != "running" && s != "terminating" }
	status, msg, result, ok := awaitStatus(env.db, run, runBudget, wantDone)
	o.Status, o.ErrMsg, o.Result = status, msg, result
	if !ok {
		o.Status = "NOT-TERMINAL:" + status
	}
	// give a continuation (the defect) time to be created and to run
	time.Sleep(2 * time.Second)
	o.Requests = env.svc.requestsOf(run)
	o.Log = w.output()
	return o, continuations(env.db, run)
}

func TestChaosTerminateWhileParkedBeforeABodyContinueAsNew(t *testing.T) {
	e := newEnv(t)
	sc := parkedScenario

	// CONTROL: left alone, the run continues, the cleanup runs once and the continuation's call happens once.
	ctl, ctlConts := executeParked(t, e, sc, 0, false)
	cs, _, _, _ := tally(ctl.Requests)
	if cs["POST"] != 1 || cs["defer_D"] != 1 || ctlConts != 1 {
		t.Fatalf("CONTROL: the undisturbed run did POST=%d defer_D=%d continuations=%d, want 1/1/1 (status %s: %s)\n%s", cs["POST"], cs["defer_D"], ctlConts, ctl.Status, ctl.ErrMsg, tail(ctl.Log, 2000))
	}

	// FAULT: terminated while parked in the sleep (defer registered and the sleep recorded: two events).
	o, conts := executeParked(t, e, sc, 2, true)
	if !o.Applied {
		t.Fatalf("UNMEASURED: the run never reached %d durable events (%d)", 2, o.Events)
	}
	s, _, _, _ := tally(o.Requests)
	var v []string
	if o.Status != "terminated" {
		v = append(v, fmt.Sprintf("final status %q, want terminated (error: %s)", o.Status, o.ErrMsg))
	}
	if s["defer_D"] != 1 {
		v = append(v, fmt.Sprintf("the registered cleanup ran %d time(s), want 1", s["defer_D"]))
	}
	if s["POST"] != 0 || conts != 0 {
		v = append(v, fmt.Sprintf("a terminated run's defer segment took the body's ContinueAsNew: POST=%d, continuation runs=%d, want 0/0", s["POST"], conts))
	}
	if len(v) > 0 {
		t.Errorf("terminate while parked:\n  %s\n  requests: %s\n--- worker log (tail) ---\n%s", strings.Join(v, "\n  "), seqOf(o.Requests), tail(o.Log, 2500))
	}
}

// A host call that blocks for longer than the instance timeout must complete: time spent waiting on the outside world is not guest compute, and
// a workflow whose single durable call is slow must not be killed for it ("execution time limit exceeded"). The component path counted that
// time against the instance timeout; this is the class, run for every guest language. No fault is injected: the fault is the slow service.
func TestChaosASlowServiceCallIsNotKilledByTheInstanceTimeout(t *testing.T) {
	e := newEnv(t)
	sc := scenario{"slow", []op{{Op: "call", Name: "A"}, {Op: "call", Name: "SLOW", Delay: slowCallMs}, {Op: "call", Name: "B"}}, nil}
	o := execute(t, e, sc, nil)
	s, _, _, _ := tally(o.Requests)
	if o.Status != "done" && o.Status != "completed" || s["A"] != 1 || s["SLOW"] != 1 || s["B"] != 1 {
		t.Fatalf("a call the service answered after %d ms: status %q (%s), effects A=%d SLOW=%d B=%d, want done and each once\n--- worker log (tail) ---\n%s",
			slowCallMs, o.Status, o.ErrMsg, s["A"], s["SLOW"], s["B"], tail(o.Log, 2500))
	}
}

// slowCallMs is longer than the 30 s default instance timeout.
const slowCallMs = 40000

// A scope's key is acquired once, however many times the run is woken and replayed afterwards (cleat#4177's second shape): the key is held by
// another run, released while this one is parked, and this run then sleeps through several wakes before its call. A trigger on concurrency_keys
// counts every INSERT of the key for this run; the effects at the service cannot see it.
func TestChaosAScopeIsAcquiredOnceAcrossWakes(t *testing.T) {
	e := newEnv(t)
	ownerExec(t, e.db, `CREATE TABLE IF NOT EXISTS chaos_key_log (workflow_id text, key_text text, at timestamptz DEFAULT now())`)
	ownerExec(t, e.db, `CREATE OR REPLACE FUNCTION chaos_log_key() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER AS $$
		BEGIN INSERT INTO chaos_key_log (workflow_id, key_text) VALUES (NEW.workflow_id, NEW.key_text); RETURN NEW; END $$`)
	ownerExec(t, e.db, `DROP TRIGGER IF EXISTS chaos_log_key ON concurrency_keys`)
	ownerExec(t, e.db, `CREATE TRIGGER chaos_log_key AFTER INSERT ON concurrency_keys FOR EACH ROW EXECUTE FUNCTION chaos_log_key()`)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	key, holder := "vo:chaos:k"+suffix, "chaos-holder-"+suffix
	// The holder is a run no worker serves; it holds the key for a day.
	startRun(t, e.db, holder, scriptInput(holder, scenario{Name: "holder"}), "chaos-q-nobody-"+suffix)
	ownerExec(t, e.db, fmt.Sprintf(`INSERT INTO concurrency_keys (key_hash, key_text, workflow_id, expires_at, tenant_id)
		VALUES (digest('%s','sha256'), '%s', '%s', now() + interval '1 day', '%s')`, key, key, holder, defaultTenant))

	sc := scenario{Name: "scope_wakes", Ops: []op{{Op: "set_scope", Name: "k" + suffix}, {Op: "sleep", Ms: 300}, {Op: "sleep", Ms: 300}, {Op: "sleep", Ms: 300}, {Op: "sleep", Ms: 300}, {Op: "call", Name: "X"}},
		Drive: func(db *sql.DB, run string) {
			time.Sleep(3 * time.Second) // let the run meet the held key and suspend on it
			_, _ = db.Exec(`DELETE FROM concurrency_keys WHERE workflow_id = $1`, holder)
		}}
	o := execute(t, e, sc, nil)
	var n int
	_ = e.db.QueryRow(`SELECT count(*) FROM chaos_key_log WHERE key_text = $1 AND workflow_id <> $2`, key, holder).Scan(&n)
	s, _, _, _ := tally(o.Requests)
	if o.Status != "done" && o.Status != "completed" || s["X"] != 1 {
		t.Fatalf("the run ended %q (%s), X performed %d time(s), want done and once; the scope key was acquired %d time(s) meanwhile\n--- worker log (tail) ---\n%s", o.Status, o.ErrMsg, s["X"], n, tail(o.Log, 1500))
	}
	if n != 1 {
		t.Errorf("the scope key was acquired %d times by the run across its wakes, want 1", n)
	}
}
