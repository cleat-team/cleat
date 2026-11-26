//go:build chaos

package chaos

import (
	"bytes"
	"database/sql"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// cleat#4381, the chaos row for cleat#4340: a signal sent to a run that continued as new reaches its SUCCESSOR exactly once, with a real worker, a real kill and the real await path.
// cleat#4379 tests the guarantee at the store level on three dialects (forward, carry-over, a race of 16 rounds, a keyed retry); this is the same promise through the whole stack.
//
// THE SHAPE. The script is `call A`, then `continue_as_new` into a continuation that awaits the signal `go` and then calls HIT (or MISS on a timeout) and B. The harness sends `go`, through the
// worker's own HTTP API (not a table insert, which would bypass the forwarding being tested), to the FIRST run's id, at three timings:
//
//	before   the first run is held at call A, alive, and has not continued yet; the signal is stored on it and the continue must CARRY it to the successor
//	after    the continue has committed and the successor is parked in its await; the signal reaches a run that has continued and must be FORWARDED
//	racing   the signal is sent around the moment the first run is released to continue, at an offset that sweeps from before the release to after it, over chainRaceRounds rounds
//
// and a control that sends it to the successor's own id, so a harness that could not deliver at all would not read as green.
//
// THE ORACLE READS THE CHAIN'S END, not the run the signal was addressed to: the chain is walked from the first run by workflow_leases.continued_from, and what is asserted is
//
//   - the chain is exactly two runs, the first is no longer open, the END finished `done`;
//   - HIT was performed exactly once and MISS never (the successor consumed the signal; a lost signal times the await out into MISS, a consumed-twice one cannot happen with one await);
//   - no workflow_signals row is left under ANY run of the chain (a signal stored where nothing reads it, or delivered twice, leaves one);
//   - B once, A once.
//
// ONE NAMED ALLOWANCE (hitAtLeastOnceWindow): at a kill at HIT's "after" boundary the effect was performed and its event never became durable, so a reclaim may perform HIT once more. That is
// the harness's documented at-least-once window, it applies to HIT at that boundary and nowhere else, and it is checked by chainViolations below with synthetic states so it cannot widen unnoticed.
const (
	chainSignalName = "go"
	chainAwaitMs    = 12000 // a lost signal ends as MISS inside the run budget, not after it
	chainRaceRounds = 16
	chainStepWait   = 60 * time.Second
	chainHitAfter   = "hit-after"
	chainHitBefore  = "hit-before"
)

func chainScript() scenario {
	return scenario{Name: "signal_across_continue_as_new", Ops: []op{
		{Op: "call", Name: "A"},
		{Op: "continue_as_new", Name: "POST", Next: []op{
			{Op: "await_signal", Name: chainSignalName, Ms: chainAwaitMs, Hit: "HIT", Miss: "MISS"},
			{Op: "call", Name: "B"},
		}},
	}}
}

// chainState is everything the oracle reads, gathered after the run so the verdict is a pure function of it.
type chainState struct {
	Chain       []string       // run ids from the first to the END
	EndStatus   string         // the END run's status
	FirstStatus string         // the first run's status
	Performed   map[string]int // service op -> non-failed requests under the script's run label
	Left        map[string]int // run id -> workflow_signals rows still under it (any name)
	Sent        int            // signals the harness delivered through the API
}

// chainViolations is the oracle. kill is "", chainHitBefore or chainHitAfter.
func chainViolations(s chainState, kill string) []string {
	var v []string
	if len(s.Chain) != 2 {
		v = append(v, fmt.Sprintf("the chain has %d run(s) %v, want exactly 2 (first, successor)", len(s.Chain), s.Chain))
	}
	if s.EndStatus != "done" {
		v = append(v, fmt.Sprintf("the chain's END finished %q, want done", s.EndStatus))
	}
	if s.FirstStatus == "ready" || s.FirstStatus == "running" {
		v = append(v, fmt.Sprintf("the first run is still %q, a run that continued must be closed", s.FirstStatus))
	}
	if s.Sent != 1 {
		v = append(v, fmt.Sprintf("UNMEASURED: the harness delivered %d signals, want 1", s.Sent))
	}
	wantHit, maxHit := 1, 1
	if kill == chainHitAfter {
		maxHit = 2 // hitAtLeastOnceWindow: performed, event not durable, performed once more by the reclaim
	}
	if n := s.Performed["HIT"]; n < wantHit || n > maxHit {
		v = append(v, fmt.Sprintf("HIT performed %d time(s), want %d..%d: the signal was not consumed by the successor exactly once", n, wantHit, maxHit))
	}
	if n := s.Performed["MISS"]; n != 0 {
		v = append(v, fmt.Sprintf("MISS performed %d time(s), want 0: the successor's await timed out, so the signal never reached it", n))
	}
	if n := s.Performed["A"]; n != 1 {
		v = append(v, fmt.Sprintf("A performed %d time(s), want 1", n))
	}
	if n := s.Performed["B"]; n != 1 {
		v = append(v, fmt.Sprintf("B performed %d time(s), want 1", n))
	}
	for id, n := range s.Left {
		if n != 0 {
			v = append(v, fmt.Sprintf("%d workflow_signals row(s) left under %s: a signal stored where nothing reads it, or delivered twice", n, id))
		}
	}
	return v
}

// The oracle is exercised on synthetic states: a harness whose verdict nothing could turn red measures nothing, and this runs without a worker.
func TestTheChainOracleRefusesWhatItIsMeantToRefuse(t *testing.T) {
	good := func() chainState {
		return chainState{Chain: []string{"a", "b"}, EndStatus: "done", FirstStatus: "done", Sent: 1,
			Performed: map[string]int{"A": 1, "HIT": 1, "B": 1}, Left: map[string]int{"a": 0, "b": 0}}
	}
	if v := chainViolations(good(), ""); len(v) != 0 {
		t.Fatalf("the CONTROL state is refused: %v", v)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*chainState)
		kill   string
		want   string
	}{
		{"a lost signal (MISS taken)", func(s *chainState) { s.Performed["HIT"] = 0; s.Performed["MISS"] = 1 }, "", "MISS performed 1"},
		{"HIT twice with no kill", func(s *chainState) { s.Performed["HIT"] = 2 }, "", "HIT performed 2"},
		{"HIT twice at a kill BEFORE the effect", func(s *chainState) { s.Performed["HIT"] = 2 }, chainHitBefore, "HIT performed 2"},
		{"a signal left under the first run", func(s *chainState) { s.Left["a"] = 1 }, "", "left under a"},
		{"a duplicate row under the successor", func(s *chainState) { s.Left["b"] = 1 }, "", "left under b"},
		{"the chain never continued", func(s *chainState) { s.Chain = []string{"a"} }, "", "has 1 run"},
		{"the first run is still open", func(s *chainState) { s.FirstStatus = "ready" }, "", "still \"ready\""},
		{"the end did not finish", func(s *chainState) { s.EndStatus = "running" }, "", "finished \"running\""},
		{"B twice", func(s *chainState) { s.Performed["B"] = 2 }, chainHitAfter, "B performed 2"},
		{"A twice", func(s *chainState) { s.Performed["A"] = 2 }, "", "A performed 2"},
		{"two signals sent (the harness, not the engine)", func(s *chainState) { s.Sent = 2 }, "", "UNMEASURED"},
	} {
		s := good()
		tc.mutate(&s)
		v := strings.Join(chainViolations(s, tc.kill), "; ")
		if !strings.Contains(v, tc.want) {
			t.Errorf("%s: violations = %q, want one containing %q", tc.name, v, tc.want)
		}
	}
	// The named allowance, and only it.
	s := good()
	s.Performed["HIT"] = 2
	if v := chainViolations(s, chainHitAfter); len(v) != 0 {
		t.Errorf("HIT twice at a kill AFTER the effect is the documented window and was refused: %v", v)
	}
}

// ---- the real thing -------------------------------------------------------------------------------------------------------------------------------------------------

type chainHarness struct {
	env    *env
	queue  string
	addr   string
	key    string
	flags  []string
	worker *worker
}

func chainAPIKey(t *testing.T, bin string) string {
	t.Helper()
	var last string
	for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
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
	t.Fatalf("UNMEASURED: no API key could be minted:\n%s", last)
	return ""
}

func (h *chainHarness) signal(run string) error {
	body := fmt.Sprintf(`{"signal_name":%q,"payload":"{}"}`, chainSignalName)
	req, err := http.NewRequest(http.MethodPost, "http://"+h.addr+"/api/workflows/"+run+"/signal", bytes.NewBufferString(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+h.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("POST signal to %s: HTTP %d", run, resp.StatusCode)
	}
	return nil
}

func newChainHarness(t *testing.T, env *env) *chainHarness {
	t.Helper()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	h := &chainHarness{env: env, queue: "chaos-chain-" + suffix, addr: freeLoopbackAddr(t)}
	deploy(t, env.db, env.wasm, h.queue)
	h.flags = []string{"--api-addr", h.addr}
	h.worker = startWorker(t, env.bin, h.queue, env.svc.srv.URL, h.flags...)
	h.key = chainAPIKey(t, env.bin)
	return h
}

func successorOf(db *sql.DB, id string) string {
	var next string
	_ = db.QueryRow(`SELECT id FROM workflow_leases WHERE continued_from = $1`, id).Scan(&next)
	return next
}

func chainOf(db *sql.DB, first string) []string {
	chain := []string{first}
	for i := 0; i < 8; i++ {
		next := successorOf(db, chain[len(chain)-1])
		if next == "" {
			break
		}
		chain = append(chain, next)
	}
	return chain
}

func awaitClaimed(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	for deadline := time.Now().Add(chainStepWait); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM workflow_leases WHERE id = $1 AND (assigned_to IS NOT NULL OR status NOT IN ('ready'))`, id).Scan(&n); err != nil {
			t.Fatalf("UNMEASURED: reading the lease of %s: %v", id, err)
		}
		if n == 1 {
			return
		}
	}
	t.Fatalf("UNMEASURED: no worker ever claimed the successor %s", id)
}

func awaitSuccessor(t *testing.T, db *sql.DB, first string) string {
	t.Helper()
	for deadline := time.Now().Add(chainStepWait); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if next := successorOf(db, first); next != "" {
			return next
		}
	}
	t.Fatalf("UNMEASURED: %s never continued", first)
	return ""
}

// chainCase runs one case: timing is "before", "after", "racing" or "to-successor", kill is "", chainHitBefore or chainHitAfter, and offset (racing only) is the signal's offset
// from the release of call A in milliseconds, negative meaning before it.
func chainCase(t *testing.T, h *chainHarness, timing, kill string, offset int) {
	t.Helper()
	env := h.env
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	run := "chaos-chain-" + suffix
	sc := chainScript()
	sent := 0
	send := func(to string) {
		if err := h.signal(to); err != nil {
			t.Errorf("delivering the signal to %s: %v", to, err)
			return
		}
		sent++
	}
	var hitGate *gate
	armHit := func() {
		if kill == "" {
			return
		}
		mode := "before"
		if kill == chainHitAfter {
			mode = "after"
		}
		hitGate = &gate{run: run, op: "HIT", nth: 1, mode: mode}
		env.svc.arm(hitGate)
	}
	defer env.svc.disarm()

	switch timing {
	case "before", "racing":
		a := &gate{run: run, op: "A", nth: 1, mode: "before"}
		env.svc.arm(a)
		startRun(t, env.db, run, scriptInput(run, sc), h.queue)
		select {
		case <-a.arrived:
		case <-time.After(chainStepWait):
			t.Fatalf("UNMEASURED: call A never arrived")
		}
		// The next gate is armed before A is released, so HIT cannot arrive unguarded; the held A request keeps its own gate.
		armHit()
		if timing == "before" {
			send(run)
			close(a.release)
		} else {
			if offset < 0 {
				send(run)
				time.Sleep(time.Duration(-offset) * time.Millisecond)
				close(a.release)
			} else {
				close(a.release)
				time.Sleep(time.Duration(offset) * time.Millisecond)
				send(run)
			}
		}
	case "after", "to-successor":
		startRun(t, env.db, run, scriptInput(run, sc), h.queue)
		next := awaitSuccessor(t, env.db, run)
		armHit()
		// The successor row exists (awaitSuccessor) and is then waited on until a worker has CLAIMED it; the fixed 300 ms after that is a grace for the guest to reach its await and
		// is NOT a guarantee that it has. A signal that lands before the await reads it is a legal outcome (the await finds it stored), so the oracle does not depend on the order.
		awaitClaimed(t, env.db, next)
		time.Sleep(300 * time.Millisecond)
		if timing == "after" {
			send(run)
		} else {
			send(next)
		}
	default:
		t.Fatalf("unknown timing %q", timing)
	}

	if hitGate != nil {
		select {
		case <-hitGate.arrived:
		case <-time.After(chainStepWait):
			t.Fatalf("UNMEASURED: HIT never reached its %s boundary, so the kill did not fire", kill)
		}
		killedWorkers(t, h)
		close(hitGate.abort)
		end := awaitSuccessor(t, env.db, run)
		ageLease(t, env.db, end)
		h.worker = startWorker(t, env.bin, h.queue, env.svc.srv.URL, h.flags...)
	}

	var end string
	deadline := time.Now().Add(runBudget)
	for time.Now().Before(deadline) {
		chain := chainOf(env.db, run)
		end = chain[len(chain)-1]
		if st, _, _, ok := awaitStatus(env.db, end, time.Second, func(s string) bool { return s == "done" || s == "failed" || s == "terminated" }); ok && st != "" && end != run {
			break
		}
	}
	// A signal consumed twice cannot be seen from a count, so give a duplicate delivery a moment to land before the rows are read.
	time.Sleep(500 * time.Millisecond)

	st := chainState{Chain: chainOf(env.db, run), Performed: map[string]int{}, Left: map[string]int{}, Sent: sent}
	st.EndStatus = statusOf(t, env.db, st.Chain[len(st.Chain)-1])
	st.FirstStatus = statusOf(t, env.db, run)
	for _, r := range env.svc.requestsOf(run) {
		if !r.Failed {
			st.Performed[r.Op]++
		}
	}
	for _, id := range st.Chain {
		var n int
		// A failed count must not read as 0 rows left, which is the passing value.
		if err := env.db.QueryRow(`SELECT count(*) FROM workflow_signals WHERE workflow_id = $1`, id).Scan(&n); err != nil {
			t.Fatalf("UNMEASURED: counting the signals left under %s: %v", id, err)
		}
		st.Left[id] = n
	}
	if v := chainViolations(st, kill); len(v) > 0 {
		t.Errorf("timing=%s kill=%q offset=%dms: %s\n  chain %v, performed %v, signals left %v", timing, kill, offset, strings.Join(v, "\n  "), st.Chain, st.Performed, st.Left)
	}
}

func statusOf(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	var s string
	if err := db.QueryRow(`SELECT status FROM workflow_leases WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatalf("UNMEASURED: reading the status of %s: %v", id, err)
	}
	return s
}

// killedWorkers kills the harness's current worker, the one holding the run.
func killedWorkers(t *testing.T, h *chainHarness) {
	t.Helper()
	h.worker.kill()
}

func TestASignalAcrossAContinueAsNewReachesItsSuccessorOnceAtEveryTiming(t *testing.T) {
	env := newEnv(t)
	h := newChainHarness(t, env)
	t.Run("control: the signal sent to the successor's own id", func(t *testing.T) { chainCase(t, h, "to-successor", "", 0) })
	t.Run("before the continue", func(t *testing.T) { chainCase(t, h, "before", "", 0) })
	t.Run("after the continue", func(t *testing.T) { chainCase(t, h, "after", "", 0) })
	for i := 0; i < chainRaceRounds; i++ {
		// from 12 ms before the release to 18 ms after it, so the sweep straddles the window in which the first run's own continue commits
		off := -12 + 2*i
		t.Run(fmt.Sprintf("racing round %02d offset %+dms", i, off), func(t *testing.T) { chainCase(t, h, "racing", "", off) })
	}
}

// A SIGKILL at the successor's HIT boundary, the signal having been sent at each timing. "before": HIT has arrived at the service and nothing was performed, so the reclaim owes it exactly
// once. "after": HIT was performed and its reply withheld, so the reclaim may perform it once more (the named allowance in chainViolations, and nothing else gets one). The signal's
// consumption must be durable by then or replayed to the same answer: a reclaim that loses it takes the await's MISS branch, which the oracle refuses.
func TestASignalAcrossAContinueAsNewSurvivesAKillAtTheSuccessorsHit(t *testing.T) {
	env := newEnv(t)
	for _, timing := range []string{"before", "after", "racing"} {
		for _, kill := range []string{chainHitBefore, chainHitAfter} {
			timing, kill := timing, kill
			t.Run(timing+"/"+kill, func(t *testing.T) {
				h := newChainHarness(t, env)
				off := 0
				if timing == "racing" {
					off = -2
				}
				chainCase(t, h, timing, kill, off)
			})
		}
	}
}
