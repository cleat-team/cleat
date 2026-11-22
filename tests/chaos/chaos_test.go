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
	// happens the same way each time. It must only read and write through the database and must not call t. It returns "" when it did what it
	// was there to do and otherwise the reason it did not (a wait that timed out): a run whose Drive did not act measured nothing about the
	// state the Drive was to create, and a reclaim run that finished reports UNMEASURED for it, not a defect in the engine.
	Drive func(db *sql.DB, run string) string
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
	// The shape seed 4179 drew (cleat#4169): a poll, calls, then a defer. A kill at the poll's BRANCH call (HIT or MISS) lands before the defer is
	// registered, so a terminated run must not expect its cleanup. The oracle used to match the boundary only on an op's Name, never found the poll
	// for a branch call, and counted the later defer; this is the fixed case that keeps it from regressing.
	{"poll_then_defer", []op{{Op: "await0", Name: "go", Hit: "HIT", Miss: "MISS"}, {Op: "call", Name: "C"}, {Op: "side_effect", Name: "S"}, {Op: "defer", Name: "D"}}, nil},
	// The promise calls (cleat#4207): no service request of their own, so the faults land on the calls around them and the oracle reads the
	// promise rows. A kill after the three have run and before B replays them: a replay that ran any of them fresh would leave a second row.
	{"promises", []op{{Op: "call", Name: "A"}, {Op: "promise_resolve", Name: "R"}, {Op: "promise_reject", Name: "J"}, {Op: "promise_create", Name: "C"}, {Op: "call", Name: "B"}}, nil},
	// A cleanup registered BEFORE a wait. The sleep suspends the run (still alive, still waiting), so the cleanup must not run then; before the fix
	// (cleat#4194) every Python and C# suspension drained the defer table, the cleanup ran early and the resumed run diverged. The fault-free run
	// is what sees it: no fault is needed, which is why this is the golden-run precondition's own test.
	{"defer_sleep", []op{{Op: "defer", Name: "D"}, {Op: "call", Name: "A"}, {Op: "sleep", Ms: 300}, {Op: "call", Name: "B"}}, nil},
	{"retry", []op{{Op: "call", Name: "A"}, {Op: "call_retry", Name: "R", Attempts: 5, Fail: 2}, {Op: "call", Name: "B"}}, nil},
	// Effects the recording service cannot see (cleat#4241). The faults land on the calls around the ops, and the oracle reads the tables.
	// signal_workflow: one signal per send, across a kill and a reclaim. Two rows under a name is a send that ran twice.
	{"signals", []op{{Op: "call", Name: "A"}, {Op: "signal", Name: "S"}, {Op: "call", Name: "B"}, {Op: "signal", Name: "T"}, {Op: "call", Name: "C"}}, nil},
	// schedule_cron and delete_cron, with the table changed between the run and its replay (see outside_effects_test.go): C is taken away after the
	// run created it and D is put back after the run deleted it; E is left alone. The two delayed calls give the harness time to act before the run ends.
	{"crons", []op{{Op: "cron_create", Name: "C"}, {Op: "cron_create", Name: "D"}, {Op: "cron_create", Name: "E"}, {Op: "call", Name: "X", Delay: 300},
		{Op: "cron_delete", Name: "D"}, {Op: "call", Name: "Y", Delay: 1500}}, editCronsBetweenRunAndReplay},
	// list_crons: a read the engine records. A schedule is added after the run listed, so a replay that listed afresh would report another count.
	{"cron_list", []op{{Op: "cron_create", Name: "C"}, {Op: "cron_list", Name: "L"}, {Op: "call", Name: "B", Delay: 300}}, addACronAfterTheRunListed},
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
	Applied                bool                // the fault actually fired: a fault that never did measures nothing
	Promises               map[string][]string // promise name -> the status of every workflow_promises row under it, read after the run
	Signals                map[string]int      // signal name -> the workflow_signals rows the run received under it, read after the run (cleat#4241)
	Schedules              map[string]int      // cron op name -> the workflow_schedules rows of the run's cron workflow, read after the run (cleat#4241)
	ConnAtExit             string              // pg_stat_activity as it was when the worker exited early (see connSnapshot); "" when no worker did
	WorkerExit             string              // why the FIRST worker was gone before the fault's boundary was reached; "" when it was not (cleat#4291)
	DriveNote              string              // why the scenario's Drive hook did not do what it was for; "" when it did, or was not asked (a run that did not finish)
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
func terminate(t *testing.T, db *sql.DB, id string) { terminateClaimableAfter(t, db, id, 0) }

// terminateClaimableAfter terminates the run but makes its defer segment claimable only after `after`, the way a run terminated while its worker
// was down, or while the queue was backed up, is claimed late. The default (0) claims it at once, so a body parked in a clock-based sleep is
// replayed before the sleep is served and suspends there: the body never reaches the call a refusal exists for. A late claim replays PAST the
// sleep, which is what reaches that call (cleat#4213).
func terminateClaimableAfter(t *testing.T, db *sql.DB, id string, after time.Duration) {
	t.Helper()
	if _, err := db.Exec(`UPDATE workflow_payloads SET error_msg = 'chaos terminate' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE workflow_leases
		SET status = 'terminating', pending_terminal_status = 'terminated',
		    defer_phase_deadline = now() + interval '300 seconds',
		    next_wake_at = now() + ($2 * interval '1 millisecond'), assigned_to = NULL, generation = generation + 1
		WHERE id = $1`, id, after.Milliseconds()); err != nil {
		t.Fatal(err)
	}
}

// execute runs one scenario once, with or without a fault, on a fresh task queue and run id.
func execute(t *testing.T, env *env, sc scenario, f *fault) outcome {
	t.Helper()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	queue, run := "chaos-q-"+suffix, "chaos-run-"+suffix
	deploy(t, env.db, env.wasm, queue)
	defer forgetSchedules(env.db, run)
	// What the database holds BEFORE this case starts a worker: a count that grows from case to case is a leak, and one that does not is not.
	_, before, _ := connSnapshot(env.db)
	t.Logf("%s (before the case)", before)

	first := startWorker(t, env.bin, queue, env.svc.srv.URL, flagsOf(f)...)
	startRun(t, env.db, run, scriptInput(run, sc), queue)
	var driven chan string
	if sc.Drive != nil {
		driven = make(chan string, 1)
		go func() { driven <- sc.Drive(env.db, run) }()
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
			_, atBoundary, _ := connSnapshot(env.db)
			t.Logf("%s (at the boundary, the first worker alive)", atBoundary)
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
		case <-first.exited:
			// The worker is gone and the harness did not kill it: workers run until they are killed, so it refused to start or died, and the
			// boundary can never be reached. Say so now. Waiting out runBudget twice (240 s a case, 16 minutes for four in a row) for a worker
			// that exited in a fraction of a second is what hid this behind "the run did not reach the boundary" (cleat#4291).
			out.WorkerExit = first.exitReason()
			_, out.ConnAtExit, _ = connSnapshot(env.db)
			out.Status = "WORKER-EXITED"
			out.Requests = env.svc.requestsOf(run)
			out.Log = first.output()
			return out
		case <-time.After(runBudget):
			first.kill() // the run finished or stalled without ever reaching the boundary
		}
	}
	status, msg, result, ok := awaitStatus(env.db, run, runBudget, wantTerminal)
	out.Status, out.ErrMsg, out.Result = status, msg, result
	if !ok {
		out.Status = "NOT-TERMINAL:" + status
	}
	// Only a run that finished is asked whether its Drive did: those are the runs whose tables are read (verifyReclaim, goldenOf). A terminated or
	// stalled run may never reach what its Drive waits for, and nothing reads its tables, so waiting here would only cost time.
	if driven != nil && (status == "done" || status == "completed") {
		select {
		case out.DriveNote = <-driven:
		case <-time.After(5 * time.Second):
			out.DriveNote = "the Drive hook was still waiting when the run had finished"
		}
	}
	out.Requests = env.svc.requestsOf(run)
	out.Promises = promiseRows(env.db, run)
	out.Signals = signalRows(env.db, run)
	out.Schedules = scheduleRows(env.db, run)
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
	if o.Status == "done" || o.Status == "completed" {
		v = append(v, promiseViolations(sc, o)...)
		if o.DriveNote != "" {
			// The tables below were never put in the state the scenario expects, so a mismatch there is not about the engine.
			v = append(v, "UNMEASURED: the scenario's Drive hook did not act ("+o.DriveNote+"), so the schedule and signal tables were not put in the state this run is judged against. This is a failure of the check, not a finding about the engine.")
		} else {
			v = append(v, outsideViolations(sc, o)...)
		}
	}
	gs := g.successes()
	successes, attempts, helds, first := tally(o.Requests)
	amo := atMostOnceOps(sc)
	inFlight := callsAfterSend(sc, f.Op)
	repeatedInFlight := []string{}
	for op, want := range gs {
		if amo[op] {
			// A fire-and-forget send is recorded BEFORE it is dispatched and a replay never dispatches it, so a kill in between loses it: it may
			// have happened once or not at all, and what it may never do is happen twice. There is no at-least-once window to allow for.
			if successes[op] > want {
				v = append(v, fmt.Sprintf("fire-and-forget effect %q succeeded %d time(s), at most %d (a replay must not dispatch a recorded send again)", op, successes[op], want))
			}
			continue
		}
		w := want
		if f.Op == op && f.Mode == "after" && !b.Failed {
			w = want + 1 // the documented at-least-once window: performed, never recorded, so replayed once
		}
		if inFlight[op] && successes[op] == want+1 && !b.Failed {
			// The faulted request is a fire-and-forget send, made by a goroutine while the body went on. The body may have PERFORMED the next call
			// before the kill landed, and its event was not yet durable, so the reclaim repeats it: the same documented window, for the call the
			// kill actually interrupted rather than for the send (measured by cleat-review3 under load: sigkill@after:send_S left B twice).
			// How many calls can be in that state is bounded below, not here.
			w = want + 1
			repeatedInFlight = append(repeatedInFlight, op)
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
	if len(repeatedInFlight) > 1 {
		sort.Strings(repeatedInFlight)
		v = append(v, fmt.Sprintf("calls %v were each performed twice after a kill at the send %q: the body is sequential, so at most ONE call can have been performed and not yet recorded when the kill landed", repeatedInFlight, f.Op))
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
	if strings.Join(withoutOps(first, amo), ",") != strings.Join(withoutOps(gfirst, amo), ",") {
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
		// The boundary's op is the script op NAMED b.Op, or the poll/await whose Hit or Miss branch call b.Op is: that call is made by the
		// await0/await_promise op itself and has no op of its own, so matching on Name alone never stopped, and every later defer was counted
		// as registered when the kill had landed before it (cleat#4169; seed 4179, `await0:go call:C3 side_effect:S4 defer:D5` killed at MISS2).
		if o2.Op != "defer" && (o2.Name == b.Op || (o2.Hit != "" && o2.Hit == b.Op) || (o2.Miss != "" && o2.Miss == b.Op)) {
			break
		}
		if o2.Op == "defer" {
			want["defer_"+o2.Name]++
		}
	}
	// The loop above counts every defer up to the boundary's op, and stops there. A defer scripted after it is not registered by the kill.
	successes, _, _, _ := tally(o.Requests)
	amo := atMostOnceOps(sc)
	for op, w := range want {
		if amo[op] {
			// At most the sends the body had made, and none the body had not: a terminated run's defer segment dispatches nothing new.
			if successes[op] > w {
				v = append(v, fmt.Sprintf("fire-and-forget effect %q succeeded %d time(s), at most %d (terminated body: nothing new)", op, successes[op], w))
			}
			continue
		}
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
	if !o.Applied && o.WorkerExit != "" {
		t.Errorf("scenario=%s fault=%s\n  UNMEASURED: the worker exited before the boundary was reached (%s). This is a failure of the check, not a finding about the engine, and not a boundary the script cannot reach: no run began.\n  %s\n  reproduce: %s\n  --- worker log (tail) ---\n%s",
			sc.Name, f, o.WorkerExit, o.ConnAtExit, repro, tail(o.Log, 2500))
		return
	}
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
	if pv := promiseViolations(sc, gold); len(pv) > 0 {
		t.Fatalf("the FAULT-FREE run of %s left promise rows other than the script's: %s. No fault was injected, so the op did not do what it claims and nothing below measures it.", sc.Name, strings.Join(pv, "; "))
	}
	for op := range atMostOnceOps(sc) {
		if n := tallyOnce(gold.Requests, op); n != 1 {
			t.Fatalf("the FAULT-FREE run of %s dispatched the fire-and-forget send %q %d time(s), want exactly 1. No fault was injected, so the op did not do what it claims and nothing below measures it.", sc.Name, op, n)
		}
	}
	if gold.DriveNote != "" {
		t.Fatalf("UNMEASURED: the FAULT-FREE run of %s finished but its Drive hook did not act: %s. This is a failure of the check, not a finding about the engine: the table the scenario reads was never put in the state it expects.", sc.Name, gold.DriveNote)
	}
	if ov := outsideViolations(sc, gold); len(ov) > 0 {
		t.Fatalf("the FAULT-FREE run of %s left the signal or schedule tables other than the script's: %s. No fault was injected, so the op did not do what it claims and nothing below measures it.", sc.Name, strings.Join(ov, "; "))
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
	runBoundariesOver(t, scenarios, then, verify, flags...)
}

// runBoundariesOver is runBoundaries over a given list of scenarios. The fire-and-forget scenarios have their own list, because their effect is AT MOST
// once and the generic sweeps (terminate, event-write refusal, signal) demand exactly once; see fireAndForgetScenarios.
func runBoundariesOver(t *testing.T, list []scenario, then string, verify func(scenario, goldenRecord, *fault, request, outcome) []string, flags ...string) {
	only := os.Getenv("CLEAT_CHAOS_SCENARIO")
	if only != "" {
		inThisList, inAny := false, false
		for _, l := range [][]scenario{scenarios, fireAndForgetScenarios} {
			for _, sc := range l {
				inAny = inAny || sc.Name == only
			}
		}
		for _, sc := range list {
			inThisList = inThisList || sc.Name == only
		}
		if !inAny {
			// A misspelt name must not select nothing and pass: `go test` is silent about a selection that matched nothing.
			t.Fatalf("CLEAT_CHAOS_SCENARIO=%q names no scenario", only)
		}
		if !inThisList {
			// The name is real but belongs to the other list: this test has nothing to run for it, and says so.
			t.Logf("CLEAT_CHAOS_SCENARIO=%q is not one of this test's scenarios", only)
			return
		}
	}
	e := newEnv(t)
	for _, sc := range list {
		if only != "" && only != sc.Name {
			continue
		}
		sc := sc
		t.Run(sc.Name, func(t *testing.T) {
			g := goldenOf(t, e, sc)
			for _, b := range g.boundaries() {
				for _, mode := range []string{"before", "after"} {
					if !applies(then, mode, b, sc, g) {
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

// pollNotYetDecided reports whether a fault at boundary b (in `mode`) lands before the poll's outcome has been acted on: a `before` boundary at or
// before the first call named for the poll's outcome in the golden run, or an `after` boundary strictly before it. (after:<that call> and later
// stay in scope: the outcome was acted on, so a replay must repeat it.)
func pollNotYetDecided(sc scenario, g goldenRecord, b request, mode string) bool {
	branch := map[string]bool{}
	for _, o := range sc.Ops {
		if o.Op == "await0" || o.Op == "await_promise" {
			branch[o.Hit], branch[o.Miss] = true, true
		}
	}
	bi, pi := -1, -1
	for i, r := range g.Reqs {
		if r == b && bi < 0 {
			bi = i
		}
		if branch[r.Op] && pi < 0 {
			pi = i
		}
	}
	if mode == "after" {
		return bi >= 0 && pi >= 0 && bi < pi
	}
	return bi >= 0 && pi >= 0 && bi <= pi
}

// applies reports whether a fault of kind `then` is meaningful at this boundary.
func applies(then, mode string, b request, sc scenario, g goldenRecord) bool {
	switch {
	case then == "terminate" && strings.HasPrefix(b.Op, "defer_"):
		return false // a kill during a run's own defer drain is a different scenario from terminating a body mid-flight
	case then == "signal" && pollNotYetDecided(sc, g, b, mode):
		// Until the poll's branch call has been performed nothing of the poll's outcome has been acted on, so a signal that lands while the worker
		// is dead may legitimately be SEEN by the replayed poll (a HIT is then correct). The poll can be anywhere in a generated script, so this is
		// derived from the golden order, not from the first request (cleat-review3, #4188, seed 4188).
		return false
	case then == "dbwrite" && mode == "after":
		return false // the fault releases the held call itself
	case then == "terminate" && atMostOnceOps(sc)[b.Op]:
		// A fire-and-forget send's request is made by a goroutine, CONCURRENTLY with the body, which has already moved on: with the send held at
		// this boundary the body made the next calls within milliseconds, before the kill (measured: B, send_T and C at -14, -9 and -8 ms). So what
		// the body had done at the kill cannot be read off the golden order, which is what verifyTerminate assumes of every boundary. The sends
		// before and after this one are still swept, and so is every call around them.
		return false
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

// connSnapshot says what is holding PostgreSQL's connection slots right now, in one line: the total against max_connections, then the biggest groups by
// database, application_name and state. It exists because a worker that refuses to start for want of a slot (53300) tells you nothing about WHO holds
// them (cleat#4291, after the 2026-11-21 nightly seeded step). It excludes its own connection, so a quiet database reads 0 and a database with a live
// worker reads the worker's pools (about 25), which is what the tests below rely on to know it is measuring. It returns the total as a number too, and an
// error, in which case the line says UNMEASURED and carries the error: a query that failed is not a count of zero.
func connSnapshot(db *sql.DB) (int, string, error) {
	// ONE place says "I could not look": every failure below goes through it, so a probe that reads 0 on error cannot hide in one branch while a test
	// exercises another (the first version had four copies, and a mutant in the second survived a test that failed at the first).
	unmeasured := func(err error) (int, string, error) {
		return 0, "UNMEASURED: pg connections: " + err.Error(), err
	}
	var max int
	if err := db.QueryRow(`SHOW max_connections`).Scan(&max); err != nil {
		return unmeasured(err)
	}
	rows, err := db.Query(`SELECT coalesce(datname, ''), coalesce(application_name, ''), coalesce(state, ''), count(*) FROM pg_stat_activity
		WHERE pid <> pg_backend_pid() AND backend_type = 'client backend' GROUP BY 1, 2, 3 ORDER BY 4 DESC, 1, 2, 3`)
	if err != nil {
		return unmeasured(err)
	}
	defer rows.Close()
	total := 0
	var groups []string
	for rows.Next() {
		var d, a, st string
		var n int
		if err := rows.Scan(&d, &a, &st, &n); err != nil {
			return unmeasured(err)
		}
		total += n
		if len(groups) < 6 {
			groups = append(groups, fmt.Sprintf("%s/%q/%s=%d", d, a, st, n))
		}
	}
	if err := rows.Err(); err != nil {
		return unmeasured(err)
	}
	return total, fmt.Sprintf("pg connections: %d of max_connections %d (this session excluded); biggest groups database/application_name/state: %s", total, max, strings.Join(groups, ", ")), nil
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

// parkedSleepMs is the body's sleep; the terminated run is claimed after it.
const parkedSleepMs = 4000

// A run that registered a cleanup and then parks (a sleep) before a body ContinueAsNew. TERMINATING it while it is parked runs its defer
// segment, which replays the body to the frontier: the body's ContinueAsNew must NOT be taken there, and the cleanup must run once (cleat#4159).
var parkedScenario = scenario{"defer_sleep_can", []op{{Op: "defer", Name: "D"}, {Op: "sleep", Ms: parkedSleepMs}, {Op: "continue_as_new", Name: "POST"}}, nil}

func continuations(db *sql.DB, id string) int {
	var n int
	_ = db.QueryRow(`SELECT count(*) FROM workflow_leases WHERE continued_from = $1`, id).Scan(&n)
	return n
}

// parkedNow says the run is PARKED: no worker holds it, it is ready, and its wake time is still well ahead (a sleeping run reads exactly this,
// the same definition resolveWhenSuspended uses for an await). The harness used to decide "parked" from an event count, and that cannot mean it:
// a sleep records NO event, so the second event of this scenario is the body's own continue_as_new, recorded only AFTER the sleep ends. The case
// therefore terminated the run after its body had already continued as new, and failed or passed on a race between the two (cleat#4213, measured:
// the count reached 2 at 4.9s, 5.2s and 6.0s after the start, past the 4 s sleep, with [defer, continue_as_new] as the two events).
func parkedNow(db *sql.DB, id string) bool {
	var n int
	_ = db.QueryRow(`SELECT count(*) FROM workflow_leases WHERE id = $1 AND status = 'ready' AND assigned_to IS NULL AND next_wake_at > now() + interval '2 seconds'`, id).Scan(&n)
	return n > 0
}

// executeParked runs the scenario, waits until `events` events are durable AND the run is parked (parkedNow), and then terminates it. No
// worker is killed: the fault is the terminate landing on a parked run.
func executeParked(t *testing.T, env *env, sc scenario, events int, terminateIt bool, claimAfter time.Duration) (o outcome, conts int) {
	t.Helper()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	queue, run := "chaos-q-"+suffix, "chaos-run-"+suffix
	deploy(t, env.db, env.wasm, queue)
	w := startWorker(t, env.bin, queue, env.svc.srv.URL, residencyOff...) // the run must be PARKED in the store (parkedNow); a held run is not parked
	startRun(t, env.db, run, scriptInput(run, sc), queue)
	if terminateIt {
		deadline := time.Now().Add(runBudget)
		for (eventCount(env.db, run) < events || !parkedNow(env.db, run)) && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		o.Events = eventCount(env.db, run)
		// Applied is the precondition of the whole case: the run is durable to `events` events AND parked right now. If it is not, the
		// terminate below would be the race this case used to be, so the caller reports UNMEASURED rather than a verdict.
		o.Applied = o.Events >= events && parkedNow(env.db, run)
		terminateClaimableAfter(t, env.db, run, claimAfter)
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

// WHAT THIS CASE MEASURES, AND WHAT IT CANNOT SEE (cleat#4213). It measures the end-to-end outcome of terminating a run that is genuinely
// parked in a sleep and claimed after the sleep is served: status terminated, the registered cleanup run once, no POST, no continuation. It does
// NOT detect the removal of the engine's refusal of ContinueAsNew in a defer segment: that was tried (the check deleted, three runs, the
// mutation confirmed in the tree) and the case stayed green, because cmd/cleat-worker's `if deferPhase { finishDeferPhase }` is applied before
// the continue_as_new branch, so a defer segment's outcome is applied whatever the body came back as. Two independent layers hold this outcome
// and the harness sees only their conjunction; the engine-side refusal has to be pinned by an engine test, not by this one.
//
// IT USED TO FAIL 2 RUNS IN 4 FOR A REASON THAT WAS NOT THE ENGINE: it decided the run was parked from an event count of 2, but a sleep records
// no event, so the second event was the body's own continue_as_new, recorded after the 4 s sleep ended. It terminated a run whose body had
// already continued as new and raced the two. It now waits for the lease to be parked (parkedNow) and claims the terminated run late.
func TestChaosTerminateWhileParkedBeforeABodyContinueAsNew(t *testing.T) {
	e := newEnv(t)
	sc := parkedScenario

	// CONTROL: left alone, the run continues, the cleanup runs once and the continuation's call happens once.
	ctl, ctlConts := executeParked(t, e, sc, 0, false, 0)
	cs, _, _, _ := tally(ctl.Requests)
	if cs["POST"] != 1 || cs["defer_D"] != 1 || ctlConts != 1 {
		t.Fatalf("CONTROL: the undisturbed run did POST=%d defer_D=%d continuations=%d, want 1/1/1 (status %s: %s)\n%s", cs["POST"], cs["defer_D"], ctlConts, ctl.Status, ctl.ErrMsg, tail(ctl.Log, 2000))
	}

	// FAULT: terminated while parked in the sleep, claimed AFTER the sleep deadline (parkedSleepMs plus a margin). One durable event (the defer
	// registration: the sleep records none) and the lease parked. Claimed at once, the defer segment would replay the body to the sleep and
	// suspend there, so the body's ContinueAsNew would never be reached and the refusal under test would decide nothing.
	o, conts := executeParked(t, e, sc, 1, true, (parkedSleepMs+1000)*time.Millisecond)
	if !o.Applied {
		t.Fatalf("UNMEASURED: the run never parked after %d durable event(s) (%d events recorded); a terminate now would race the wake, not test it", 1, o.Events)
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
		Drive: func(db *sql.DB, run string) string {
			time.Sleep(3 * time.Second) // let the run meet the held key and suspend on it
			_, _ = db.Exec(`DELETE FROM concurrency_keys WHERE workflow_id = $1`, holder)
			return ""
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

// promiseRows reads every workflow_promises row of the run, by promise name. The recording service cannot see a promise-store write, so this is the
// only oracle for create_promise, resolve_promise and reject_promise: a name with two rows is a create that ran twice, a status that is not the
// scripted one is a resolve or reject that did not take.
func promiseRows(db *sql.DB, run string) map[string][]string {
	m := map[string][]string{}
	rows, err := db.Query(`SELECT promise_name, status FROM workflow_promises WHERE workflow_id = $1 ORDER BY created_at, promise_id`, run)
	if err != nil {
		return m
	}
	defer rows.Close()
	for rows.Next() {
		var name, status string
		if rows.Scan(&name, &status) == nil {
			m[name] = append(m[name], status)
		}
	}
	return m
}

// promiseViolations checks the promise ops of a script against the rows a FINISHED run left: each op's promise exists exactly once, in the status
// the op scripts. UNMEASURED is not reported here: a run that did not finish has performed an unknown prefix of the script, so the caller asks only of a
// run that ended done, and the golden run asks it unconditionally (a fault-free run that did not produce the rows did not exercise the op).
func promiseViolations(sc scenario, o outcome) []string {
	var v []string
	for _, o2 := range sc.Ops {
		want := map[string]string{"promise_create": "pending", "promise_resolve": "resolved", "promise_reject": "rejected"}[o2.Op]
		if want == "" {
			continue
		}
		got := o.Promises["p-"+o2.Name]
		if len(got) != 1 || got[0] != want {
			v = append(v, fmt.Sprintf("promise %q after %s: rows %v, want exactly [%s]", "p-"+o2.Name, o2.Op, got, want))
		}
	}
	return v
}

// continue-as-new ENDS the run, so a cleanup registered before it runs exactly once, and the continuation runs: the other side of the guard
// defer_sleep pins (cleat#4194, #4197). Every effect exactly once, no fault injected. With the Python guest the script raises the suspend sentinel
// after continue_as_new as the SDK says to, which is the shape a component guest does not drain on by itself.
func TestChaosACleanupRunsExactlyOnceAtContinueAsNew(t *testing.T) {
	e := newEnv(t)
	sc := scenario{"defer_can", []op{{Op: "defer", Name: "D"}, {Op: "call", Name: "A"}, {Op: "continue_as_new", Name: "POST"}}, nil}
	o, conts := executeParked(t, e, sc, 0, false, 0)
	s, _, _, _ := tally(o.Requests)
	if s["A"] != 1 || s["defer_D"] != 1 || s["POST"] != 1 || conts != 1 {
		t.Fatalf("a run that registered a cleanup and continued as new: A=%d defer_D=%d POST=%d continuations=%d, want 1/1/1/1 (requests %s, status %s: %s)\n--- worker log (tail) ---\n%s",
			s["A"], s["defer_D"], s["POST"], conts, seqOf(o.Requests), o.Status, o.ErrMsg, tail(o.Log, 2000))
	}
}
