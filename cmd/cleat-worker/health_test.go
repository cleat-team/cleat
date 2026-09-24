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
func probeOK(w *Worker)   { w.observeDBProbe(3*time.Millisecond, nil) }
func probeFail(w *Worker) { w.observeDBProbe(time.Second, context.DeadlineExceeded) }

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
	stale := func(w *Worker) {
		now := time.Now()
		w.healthTracker.now = func() time.Time { return now }
		w.healthTracker.registerLoop("heartbeat")
		now = now.Add(10 * time.Minute)
	}
	rows := []row{
		{"nothing probed yet", func(w *Worker) {}, 200, 503, "starting", nil},
		{"database answering", probeOK, 200, 200, "", nil},
		{"database down", func(w *Worker) { probeOK(w); probeFail(w) }, 200, 503, "database_unreachable", nil},
		{"database down from the first probe", probeFail, 200, 503, "database_unreachable", nil},
		{"database back", func(w *Worker) { probeFail(w); probeOK(w) }, 200, 200, "", nil},
		{"draining", func(w *Worker) { probeOK(w); w.draining.Store(true) }, 200, 503, "draining", nil},
		{"a loop is stuck", func(w *Worker) { probeOK(w); stale(w) }, 503, 503, "background_loop_stuck", nil},
		// The docker-pause finding: a loop whose call hangs in the driver goes stale, and that must not make
		// /livez 503 (a kubelet would restart every worker for a database outage). /readyz still says why.
		{"a loop is stuck BECAUSE the database is down", func(w *Worker) { probeFail(w); stale(w) }, 200, 503, "database_unreachable", nil},
		{"memory pressure is report-only", func(w *Worker) {
			probeOK(w)
			w.memoryController = &MemoryController{pressure: 0.8}
		}, 200, 200, "", []string{"memory_pressure"}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			api := newTestAPIServer(&mockStore{})
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
	w.observeDBProbe(time.Second, errors.New(secretish))
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
