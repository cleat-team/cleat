package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/plugin"
)

// probeOK and probeFail put the worker's reachability into a state the way production does: through
// observeDBProbe, which every bounded database call reports to.
func probeOK(w *Worker)   { w.observeDBProbe(w.dbReach.now(), 3*time.Millisecond, nil) }
func probeFail(w *Worker) { w.observeDBProbe(w.dbReach.now(), time.Second, context.DeadlineExceeded) }

func healthGet(t *testing.T, h http.HandlerFunc, path string) (int, map[string]any, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s did not return JSON: %v\n%s", path, err, rec.Body.String())
	}
	return rec.Code, body, rec.Body.String()
}

// The acceptance case of cleat#2007, as a table: what /livez and /readyz say in each state. The point
// of the split is the database row: readiness fails and liveness does not, because restarting a
// worker cannot fix a database.
func TestLivezAndReadyzAnswerDifferentlyWhenTheDatabaseIsDown(t *testing.T) {
	type row struct {
		name              string
		setup             func(w *Worker)
		livez, readyz     int
		readyReason       string
		wantDegradedCodes []string
	}
	// One clock for the reachability state and the loop tracker, as in production, so ages compare.
	var clk *fakeClock
	stale := func(w *Worker) {
		w.healthTracker.registerLoop("heartbeat")
		clk.advance(10 * time.Minute)
	}
	rows := []row{
		{"nothing probed yet", func(w *Worker) {}, 200, 503, "starting", nil},
		{"database answering", probeOK, 200, 200, "", nil},
		{"database down", func(w *Worker) { probeOK(w); probeFail(w) }, 200, 503, "database_unreachable", nil},
		{"database down from the first probe", probeFail, 200, 503, "database_unreachable", nil},
		{"database back", func(w *Worker) { probeFail(w); probeOK(w) }, 200, 200, "", nil},
		{"draining", func(w *Worker) { probeOK(w); w.draining.Store(true) }, 200, 503, "draining", nil},
		{"a loop is stuck", func(w *Worker) { probeOK(w); stale(w) }, 503, 503, "background_loop_stuck", nil},
		// The database answered AFTER the loop went quiet, and keeps being watched: not the database's doing.
		{"a loop is stuck while the database keeps answering", func(w *Worker) {
			probeOK(w)
			stale(w)
			probeOK(w)
		}, 503, 503, "background_loop_stuck", nil}, // /readyz carries the loop too: a wedged worker is not ready
		// The docker-pause finding: a loop whose call hangs in the driver goes stale, and that must not make
		// /livez 503 (a kubelet would restart every worker for a database outage). /readyz still says why.
		{"a loop is stuck BECAUSE the database is down", func(w *Worker) { probeFail(w); stale(w); probeFail(w) }, 200, 503, "database_unreachable", nil},
		// The window the first real pause found: the loop is stuck in a call, the database has not yet been
		// KNOWN unreachable (no bounded call has missed its deadline), and it has not answered since the loop
		// went quiet. The database is the only thing that can be holding it.
		{"a loop is stuck and the database has not answered since, verdict not yet unreachable", func(w *Worker) {
			probeOK(w)
			w.healthTracker.registerLoop("heartbeat")
			clk.advance(10 * time.Minute)
			w.dbReach.beginCall() // the pending call is what keeps the evidence live
		}, 200, 200, "", nil},
		{"memory pressure is report-only", func(w *Worker) {
			probeOK(w)
			w.memoryController = &MemoryController{pressure: 0.8}
		}, 200, 200, "", []string{"memory_pressure"}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			api := newTestAPIServer(&mockStore{})
			clk = healthClock()
			api.worker.dbReach.clock = clk.now
			api.worker.healthTracker.now = clk.now
			r.setup(api.worker)
			lc, lb, _ := healthGet(t, api.handleLivez, "/livez")
			rc, rb, _ := healthGet(t, api.handleReadyz, "/readyz")
			if lc != r.livez || rc != r.readyz {
				t.Fatalf("[state=%s] /livez = %d %v, /readyz = %d %v; want %d and %d", r.name, lc, lb, rc, rb, r.livez, r.readyz)
			}
			if r.readyReason != "" && rb["reason"] != r.readyReason {
				t.Errorf("/readyz reason = %v, want %s", rb["reason"], r.readyReason)
			}
			if len(r.wantDegradedCodes) > 0 && rb["degraded"] != true {
				t.Errorf("/readyz body %v: want degraded", rb)
			}
			// /healthz is /livez under its old name.
			hc, _, _ := healthGet(t, api.handleHealthz, "/healthz")
			if hc != lc {
				t.Errorf("/healthz = %d, /livez = %d: the alias drifted", hc, lc)
			}
		})
	}
}

// Degraded reasons combine: a worker under memory pressure used to hide an unhealthy plugin
// (the memory branch returned first).
func TestDegradedReasonsCombineAndNeverFailReadiness(t *testing.T) {
	api := newTestAPIServer(&mockStore{})
	w := api.worker
	probeOK(w)
	w.memoryController = &MemoryController{pressure: 0.5}
	w.plugList = []*plugin.LoadedPlugin{{Plugin: &healthPlugin{name: "audit-log", err: errors.New("lost 3")}, Healthy: true}}
	w.refreshPluginHealth()
	code, body, _ := healthGet(t, api.handleReadyz, "/readyz")
	if code != 200 || body["degraded"] != true {
		t.Fatalf("/readyz = %d %v: degraded is report-only, so 200 with degraded", code, body)
	}
	reasons, _ := body["reasons"].([]any)
	if len(reasons) != 2 {
		t.Errorf("reasons = %v, want memory_pressure and plugin_unhealthy together", reasons)
	}
}

// Owner decision 2A on cleat#2007: the three unauthenticated bodies carry ONLY ok, degraded and
// reason codes. Everything below is something that must not appear in them, while the authenticated
// route carries it.
func TestThePublicHealthBodiesLeakNothingAndTheAdminRouteHasTheDetail(t *testing.T) {
	api := newTestAPIServer(&mockStore{})
	w := api.worker
	secretish := "dial tcp db-prod-3.internal:5432: password authentication failed for user cleat_app"
	w.observeDBProbe(w.dbReach.now(), time.Second, errors.New(secretish))
	now := time.Now()
	w.healthTracker.now = func() time.Time { return now }
	w.healthTracker.registerLoop("reaper_of_secrets")
	now = now.Add(10 * time.Minute)
	w.memoryController = &MemoryController{pressure: 0.73}
	w.plugList = []*plugin.LoadedPlugin{{Plugin: &healthPlugin{name: "pagerduty-alert", err: errors.New("no enabled configs")}, Healthy: true}}
	w.refreshPluginHealth()

	leaks := []string{"reaper_of_secrets", "pagerduty-alert", "no enabled configs", "db-prod-3", "5432", "cleat_app", "0.73", "password"}
	for _, c := range []struct {
		name string
		h    http.HandlerFunc
	}{{"/livez", api.handleLivez}, {"/readyz", api.handleReadyz}, {"/healthz", api.handleHealthz}} {
		_, body, raw := healthGet(t, c.h, c.name)
		for _, leak := range leaks {
			if strings.Contains(raw, leak) {
				t.Errorf("%s contains %q without a credential: %s", c.name, leak, raw)
			}
		}
		for k := range body {
			if k != "ok" && k != "degraded" && k != "reason" && k != "reasons" {
				t.Errorf("%s has the field %q: only ok, degraded, reason and reasons are public", c.name, k)
			}
		}
	}

	// A positive control on the leak list: the detail IS reachable, on the admin route.
	_, _, raw := healthGet(t, api.handleAdminHealth, "/api/admin/health")
	for _, want := range []string{"reaper_of_secrets", "pagerduty-alert", "no enabled configs", "db-prod-3", "0.73", `"loops_blocked_on_database":true`} {
		if !strings.Contains(raw, want) {
			t.Errorf("/api/admin/health does not show %q, so the leak check above has nothing to compare against: %s", want, raw)
		}
	}
}

// A bounded call fails when it errors OR when it returns success after its deadline: #2005's rule,
// which is the only thing that catches a database that delays instead of refusing.
func TestALateSuccessCountsAsUnreachable(t *testing.T) {
	api := newTestAPIServer(&mockStore{})
	w := api.worker
	oldFloor := dbCallDeadlineFloor
	dbCallDeadlineFloor = 20 * time.Millisecond // the floor is 2s in production
	defer func() { dbCallDeadlineFloor = oldFloor }()
	w.heartbeatInterval = 40 * time.Millisecond // dbCallDeadline is half of it: 20ms
	if err := w.probeBoundedCall(func(ctx context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if s := w.dbReach.snapshot(); !s.Known || !s.Reachable {
		t.Fatalf("a prompt success left %+v", s)
	}
	err := w.probeBoundedCall(func(ctx context.Context) error { time.Sleep(60 * time.Millisecond); return nil })
	if err == nil {
		t.Fatal("a call that returned success past its deadline was trusted")
	}
	if s := w.dbReach.snapshot(); s.Reachable || s.Failures != 1 {
		t.Errorf("a late success left %+v, want unreachable with 1 failure", s)
	}
}

// The two lines an operator greps for, once each per transition and not per probe.
func TestTheTransitionsAreLoggedOnceEachWithTheOutageLength(t *testing.T) {
	api := newTestAPIServer(&mockStore{})
	w := api.worker
	var mu sync.Mutex
	var buf bytes.Buffer
	w.logger = slog.New(slog.NewTextHandler(&lockedWriter{mu: &mu, w: &buf}, &slog.HandlerOptions{Level: slog.LevelDebug}))

	probeOK(w) // unknown -> reachable is not a recovery
	probeFail(w)
	probeFail(w)
	probeFail(w)
	time.Sleep(1100 * time.Millisecond)
	probeOK(w)
	probeOK(w)

	mu.Lock()
	out := buf.String()
	mu.Unlock()
	if n := strings.Count(out, "database unreachable (deadline exceeded)"); n != 1 {
		t.Errorf("the unreachable line appeared %d times over 3 failed probes, want 1:\n%s", n, out)
	}
	if n := strings.Count(out, "database reachable again after "); n != 1 {
		t.Errorf("the recovery line appeared %d times, want 1:\n%s", n, out)
	}
	if !strings.Contains(out, "reachable again after 1s") {
		t.Errorf("the recovery line does not carry the outage length (~1s):\n%s", out)
	}
	if s := w.dbReach.snapshot(); s.Failures != 0 || !s.Reachable {
		t.Errorf("after recovery: %+v", s)
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// The metrics carry the state, and are fed by production code (the feeder guard in
// monitoring/prometheus checks the latter; this checks the scrape).
func TestTheReachabilityMetricsFollowTheProbes(t *testing.T) {
	old := globalWorker
	t.Cleanup(func() { globalWorker = old })
	api := newTestAPIServer(&mockStore{})
	w := api.worker
	w.Metrics = newTestPrometheus()
	w.dbDialect = "postgres"
	globalWorker = w

	scrape := func() string {
		rec := httptest.NewRecorder()
		handleMetrics(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		return rec.Body.String()
	}
	line := func(text, prefix string) string {
		for _, l := range strings.Split(text, "\n") {
			if strings.HasPrefix(l, prefix) {
				return l
			}
		}
		return ""
	}
	probeOK(w)
	text := scrape()
	if l := line(text, "cleat_db_reachable{"); !strings.HasSuffix(l, " 1") || !strings.Contains(l, `dialect="postgres"`) {
		t.Errorf("after a good probe: %q", l)
	}
	if line(text, "cleat_db_last_success_timestamp_seconds{") == "" || line(text, "cleat_db_probe_duration_seconds_count{") == "" {
		t.Errorf("last-success or probe-duration series missing:\n%s", text)
	}
	probeFail(w)
	probeFail(w)
	text = scrape()
	if l := line(text, "cleat_db_reachable{"); !strings.HasSuffix(l, " 0") {
		t.Errorf("after two failed probes: %q", l)
	}
	if l := line(text, "cleat_db_consecutive_failures{"); !strings.HasSuffix(l, " 2") {
		t.Errorf("consecutive failures: %q, want 2", l)
	}
}

// Wiring, read from source: an observer that nothing calls reads as done (the shape of cleat#2168's
// hook). probeBoundedCall must report to observeDBProbe, and Run must probe once before its loops.
func TestTheProbeIsWiredIntoTheBoundedCallAndRunProbesAtStartup(t *testing.T) {
	src, err := os.ReadFile("setup.go")
	if err != nil {
		t.Fatal(err)
	}
	f, err := parser.ParseFile(token.NewFileSet(), "setup.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls := func(fn, callee string) bool {
		found := false
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Name.Name != fn {
				continue
			}
			ast.Inspect(fd, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok {
					if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == callee {
						found = true
					}
				}
				return true
			})
		}
		return found
	}
	if !calls("probeBoundedCall", "observeDBProbe") {
		t.Error("probeBoundedCall does not call observeDBProbe: every bounded call would leave /readyz and the metrics blind")
	}
	if !calls("Run", "probeBoundedCall") {
		t.Error("Worker.Run never probes the database before its loops: /readyz would stay `starting` for a full heartbeat interval, and a database that is down at boot would be silent")
	}
}

// healthClock is a clock that moves only when told to (fakeClock is connection_share_test.go's), so the
// tests below are about ORDER and AGE rather than about sleeping.
func healthClock() *fakeClock { return &fakeClock{t: time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)} }

// sharedClock puts the reachability state and the loop tracker on one fake clock, as time.Now does in
// production, so a loop's age and an observation's age can be compared.
func sharedClock(w *Worker) *fakeClock {
	clk := healthClock()
	w.dbReach.clock = clk.now
	w.healthTracker.now = clk.now
	return clk
}

// cleat-review, #2263 point 4b. The first version excused a stale loop for as long as the last verdict was
// "unreachable", so when every loop that could observe the database was wedged on something that was NOT
// the database, the verdict never refreshed and /livez stayed 200 forever.
func TestAnUnreachableVerdictExpiresWhenNothingIsWatchingTheDatabase(t *testing.T) {
	api := newTestAPIServer(&mockStore{})
	w := api.worker
	clk := sharedClock(w)
	w.heartbeatInterval = 5 * time.Second
	probeOK(w)
	clk.advance(time.Second)
	probeFail(w)
	w.healthTracker.registerLoop("dispatch")
	clk.advance(2 * time.Minute) // the loop is now stale (20s interval x 6 = 2m); the last observation is 2m old
	w.dbReach.beginCall()        // ...but a call is still waiting on the database

	livez := func() (int, map[string]any) { c, b, _ := healthGet(t, api.handleLivez, "/livez"); return c, b }

	// A call in flight is evidence however long it has been there: a paused database holds it.
	clk.advance(10 * time.Minute)
	if c, b := livez(); c != 200 {
		t.Fatalf("[last observation 12m old, call in flight] /livez = %d %v, want 200: a call still waiting on the database is the database", c, b)
	}
	w.dbReach.endCall()
	// Expired: nothing has observed the database for twelve minutes and no call is waiting on it.
	if c, b := livez(); c != 503 || b["reason"] != "background_loop_stuck" {
		t.Fatalf("[last observation 12m old, no call in flight] /livez = %d %v, want 503 background_loop_stuck: a verdict nobody refreshed must stop excusing a wedge", c, b)
	}
	// And while it IS being watched, the same stale loop is the outage's doing.
	probeFail(w)
	if c, b := livez(); c != 200 {
		t.Fatalf("[a fresh failing observation] /livez = %d %v, want 200", c, b)
	}
}

// cleat-review, #2263 point 4a. /livez was 503 for 0.5 to 4.5s after an outage ended, because the excuse
// ended at the first good probe and the loops it had been holding had not ticked yet.
func TestStaleLoopsAreStillExcusedForAGraceAfterTheDatabaseComesBack(t *testing.T) {
	api := newTestAPIServer(&mockStore{})
	w := api.worker
	clk := sharedClock(w)
	probeOK(w)
	clk.advance(time.Second)
	probeFail(w)
	w.healthTracker.registerLoop("dispatch")
	clk.advance(3 * time.Minute) // stale: 20s interval x 6 = 2m
	w.dbReach.beginCall()
	probeOK(w) // the database answers again, after the loop went quiet
	w.dbReach.endCall()

	livez := func() int { c, _, _ := healthGet(t, api.handleLivez, "/livez"); return c }
	if c := livez(); c != 200 {
		t.Fatalf("[recovered 0s ago, loop still stale] /livez = %d, want 200 inside the grace", c)
	}
	clk.advance(dbRecoveryGrace - time.Second)
	if c := livez(); c != 200 {
		t.Fatalf("[recovered %v ago] /livez = %d, want 200 just inside the grace", dbRecoveryGrace-time.Second, c)
	}
	probeOK(w) // still watched, still answering
	clk.advance(2 * time.Second)
	if c := livez(); c != 503 {
		t.Fatalf("[recovered %v ago, loop STILL stale] /livez = %d, want 503: a loop that has not resumed after the grace is wedged", dbRecoveryGrace+time.Second, c)
	}
}

// The window the second real pause found (cleat#2007): at pause +5s /livez was 503 although the database
// was the cause. The loop was stuck in a call, but no BOUNDED call had yet missed its deadline, so the
// database was not yet KNOWN to be unreachable. What is known is that it has not answered since the loop
// went quiet, and that is what the decision now rests on.
func TestALoopThatWentQuietBeforeTheDatabaseLastAnsweredIsWedgedAndOneAfterIsNot(t *testing.T) {
	api := newTestAPIServer(&mockStore{})
	w := api.worker
	clk := sharedClock(w)
	w.heartbeatInterval = 5 * time.Second
	w.healthTracker.setInterval("dispatch", time.Second)

	w.healthTracker.registerLoop("dispatch")
	w.healthTracker.recordRun("dispatch") // its last tick
	clk.advance(500 * time.Millisecond)
	probeOK(w) // the database answered within an interval of that tick: cannot tell them apart
	clk.advance(7 * time.Second)
	w.dbReach.beginCall() // the next heartbeat call is pending on the paused database
	if c, b, _ := healthGet(t, api.handleLivez, "/livez"); c != 200 {
		t.Fatalf("[loop silent 7.5s, database last answered 0.5s after its tick, call pending] /livez = %d %v, want 200", c, b)
	}
	w.dbReach.endCall()

	// Same loop, but the database ANSWERED, a full interval or more after the tick, while the loop stayed
	// silent: it is not the database that is holding it.
	clk.advance(time.Second)
	probeOK(w)
	if c, b, _ := healthGet(t, api.handleLivez, "/livez"); c != 503 || b["reason"] != "background_loop_stuck" {
		t.Fatalf("[the database answered after the loop had been silent 8s] /livez = %d %v, want 503 background_loop_stuck", c, b)
	}
}

// cleat-review, #2263 point 4c. Call A hangs; call B succeeds; A's deadline timer fires later. The timer's
// failure is older than B's success and must not make a database that answered read as unreachable.
func TestAnOlderCallsDeadlineDoesNotOverrideANewerSuccess(t *testing.T) {
	api := newTestAPIServer(&mockStore{})
	w := api.worker
	oldFloor := dbCallDeadlineFloor
	dbCallDeadlineFloor = 150 * time.Millisecond
	defer func() { dbCallDeadlineFloor = oldFloor }()
	w.heartbeatInterval = 100 * time.Millisecond // dbCallDeadline() = the floor

	release := make(chan struct{})
	aDone := make(chan error, 1)
	go func() {
		aDone <- w.probeBoundedCall(func(ctx context.Context) error { <-release; return nil }) // hangs, as a paused driver does
	}()
	// Make sure A has started before B does: B's start must be later than A's.
	for w.dbReach.snapshot().InFlight == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(5 * time.Millisecond)
	if err := w.probeBoundedCall(func(ctx context.Context) error { return nil }); err != nil {
		t.Fatalf("B: %v", err)
	}
	// Wait until A's timer has fired and been dropped, so the assertion below is about the state AFTER it.
	deadline := time.Now().Add(5 * time.Second)
	for w.dbReach.snapshot().Superseded == 0 {
		if time.Now().After(deadline) {
			close(release)
			t.Fatalf("A's deadline never fired or was not classified as superseded: %+v", w.dbReach.snapshot())
		}
		time.Sleep(5 * time.Millisecond)
	}
	s := w.dbReach.snapshot()
	close(release)
	<-aDone
	if !s.Reachable || s.Failures != 0 {
		t.Fatalf("after B succeeded and A's older deadline fired: %+v, want reachable with 0 failures", s)
	}
	if c, b, _ := healthGet(t, api.handleReadyz, "/readyz"); c != 200 {
		t.Fatalf("/readyz = %d %v while the database answers", c, b)
	}
	// The ordering rule is not "ignore failures": a call that STARTED after the success and fails counts.
	failed := w.probeBoundedCall(func(ctx context.Context) error { return errors.New("connection refused") })
	if failed == nil || w.dbReach.snapshot().Reachable {
		t.Fatalf("a failure from a call that started after the success was dropped: %+v", w.dbReach.snapshot())
	}
}

// A call that hung past its deadline was reported failed by its timer. When it finally returns, the
// verdict must not look abandoned: that call was the evidence that kept a stale loop excused, and the next
// probe is up to a heartbeat interval away. Measured against a real `docker pause`: /livez was 503 half a
// second after the database came back.
func TestAHungCallsReturnRefreshesTheVerdictWithoutChangingIt(t *testing.T) {
	api := newTestAPIServer(&mockStore{})
	w := api.worker
	oldFloor := dbCallDeadlineFloor
	dbCallDeadlineFloor = 20 * time.Millisecond
	defer func() { dbCallDeadlineFloor = oldFloor }()
	w.heartbeatInterval = 40 * time.Millisecond

	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- w.probeBoundedCall(func(ctx context.Context) error { <-release; return nil })
	}()
	var atTimer dbSnapshot
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		if atTimer = w.dbReach.snapshot(); atTimer.Failures == 1 {
			break
		}
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("the deadline timer never reported the hung call")
		}
	}
	time.Sleep(15 * time.Millisecond)
	close(release)
	if err := <-done; err == nil {
		t.Fatal("a call that returned success after its deadline was trusted")
	}
	after := w.dbReach.snapshot()
	if !after.LastObserved.After(atTimer.LastObserved) {
		t.Errorf("the hung call's return did not refresh the verdict: lastObserved %v then %v", atTimer.LastObserved, after.LastObserved)
	}
	if after.Reachable || after.Failures != 1 {
		t.Errorf("the return changed the verdict: %+v, want still unreachable with 1 failure", after)
	}
	if after.InFlight != 0 {
		t.Errorf("a returned call is still counted in flight: %d", after.InFlight)
	}
}

// cleat-review's probe on 9a4132fc: a loop wedged while the database is healthy, plus one missed
// deadline every ~26s, kept /livez at 200 for 17 minutes. Each miss is an "outage" of a few seconds, and
// its recovery opened a grace that excused EVERY stale loop. The grace is for the loops THAT outage held.
func TestTheRecoveryGraceExcusesOnlyLoopsThatWentQuietDuringTheOutage(t *testing.T) {
	api := newTestAPIServer(&mockStore{})
	w := api.worker
	clk := sharedClock(w)
	w.healthTracker.setInterval("held", time.Second)
	w.healthTracker.setInterval("wedged", time.Second)

	// "wedged" stops ticking now, for a reason that is not the database. Both loops are registered and tick.
	w.healthTracker.registerLoop("held")
	w.healthTracker.registerLoop("wedged")
	w.healthTracker.recordRun("held")
	w.healthTracker.recordRun("wedged")
	probeOK(w)
	clk.advance(time.Minute)
	// "held" keeps ticking until the outage begins (the database then holds its call).
	w.healthTracker.recordRun("held")
	clk.advance(200 * time.Millisecond)
	probeFail(w) // one missed deadline: the outage begins
	clk.advance(10 * time.Second)
	probeOK(w) // and ends; the grace opens
	livez := func() (int, map[string]any) { c, b, _ := healthGet(t, api.handleLivez, "/livez"); return c, b }

	// Both are stale. Only "held" went quiet during the outage, so only it is excused: "wedged" fails /livez.
	c, b := livez()
	if c != 503 || b["reason"] != "background_loop_stuck" {
		t.Fatalf("[a loop wedged a minute BEFORE a 10s outage, just after recovery] /livez = %d %v, want 503: the grace excused a loop the outage did not hold", c, b)
	}
	// Remove the wedged loop from the picture: the held one alone is excused inside the grace.
	w.healthTracker.recordRun("wedged") // ticks again, so only the held loop is stale now
	if c, b := livez(); c != 200 {
		t.Fatalf("[only the loop the outage held is stale, inside the grace] /livez = %d %v, want 200", c, b)
	}
	// And once the grace runs out, the held loop that still has not resumed is wedged as well.
	clk.advance(dbRecoveryGrace + time.Second)
	probeOK(w)
	if c, b := livez(); c != 503 {
		t.Fatalf("[grace over, the loop never resumed] /livez = %d %v, want 503", c, b)
	}
}
