package main

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cleat-team/cleat/plugin"
)

// A plugin that reports its own health, and one that does not (cleat#2168).
type healthPlugin struct {
	name  string
	err   error
	calls atomic.Int64
	hang  chan struct{} // Health blocks until this is closed, when set
}

func (h *healthPlugin) Info() plugin.PluginInfo                         { return plugin.PluginInfo{Name: h.name} }
func (h *healthPlugin) Init(context.Context, *plugin.Environment) error { return nil }
func (h *healthPlugin) Health() error {
	h.calls.Add(1)
	if h.hang != nil {
		<-h.hang
	}
	return h.err
}

type silentPlugin struct{}

func (silentPlugin) Info() plugin.PluginInfo                         { return plugin.PluginInfo{Name: "silent"} }
func (silentPlugin) Init(context.Context, *plugin.Environment) error { return nil }

func healthzOf(t *testing.T, api *apiServer) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	api.handleHealthz(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("/healthz did not return JSON: %v\n%s", err, rec.Body.String())
	}
	return rec.Code, body
}

// A plugin that has lost events degrades the worker; it does not fail it, and the body says so by code only. A 503 would have an
// orchestrator restart a worker whose only fault is an audit table that stalled, which turns the
// audit log's trouble into the API's outage.
func TestHealthzReportsAnUnhealthyPluginAsDegradedAnd200(t *testing.T) {
	api := newTestAPIServer(&mockStore{})
	api.worker.plugList = []*plugin.LoadedPlugin{
		{Plugin: silentPlugin{}, Healthy: true},
		{Plugin: &healthPlugin{name: "audit-log", err: errors.New("audit-log lost 3 event(s)")}, Healthy: true},
		{Plugin: &healthPlugin{name: "fine"}, Healthy: true},
	}
	api.worker.refreshPluginHealth()
	code, body := healthzOf(t, api)
	if code != http.StatusOK {
		t.Fatalf("/healthz = %d with an unhealthy plugin, want 200 (degraded, not failed)", code)
	}
	if body["ok"] != true || body["degraded"] != true || body["reason"] != "plugin_unhealthy" {
		t.Errorf("body = %v, want ok:true degraded:true reason:plugin_unhealthy", body)
	}
	// /healthz needs no credential, so the body is the reason code and nothing that names the plugin or
	// quotes what it said (the owner's #2168 wording: a reason code, no names).
	if len(body) != 4 {
		t.Errorf("body = %v: want exactly ok, degraded, reason and reasons", body)
	}
	raw, _ := json.Marshal(body)
	for _, leak := range []string{"audit-log", "lost 3", "plugins"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("the unauthenticated /healthz body contains %q: %s", leak, raw)
		}
	}

	// Healthy, and a plugin that does not implement HasHealth, are the plain answer.
	api.worker.plugList = []*plugin.LoadedPlugin{{Plugin: silentPlugin{}, Healthy: true}, {Plugin: &healthPlugin{name: "fine"}, Healthy: true}}
	api.worker.refreshPluginHealth()
	code, body = healthzOf(t, api)
	if code != http.StatusOK || len(body) != 1 || body["ok"] != true {
		t.Errorf("/healthz with only healthy plugins = %d %v, want 200 {ok:true}", code, body)
	}
}

// The hook is what turns a plugin's report into the metric an operator alerts on.
func TestThePluginEventsLostHookFeedsTheCounter(t *testing.T) {
	old := globalWorker
	t.Cleanup(func() { globalWorker = old })
	m := newTestPrometheus()
	globalWorker = &Worker{Metrics: m}

	hook := pluginEventsLostHook(m)
	hook("audit-log", "buffer_full", 3)
	hook("audit-log", "buffer_full", 2)
	hook("audit-log", "shutdown", 1)
	hook("audit-log", "insert_failed", 0) // nothing lost: no series

	rec := httptest.NewRecorder()
	handleMetrics(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body, _ := io.ReadAll(rec.Result().Body)
	text := string(body)
	for _, want := range []string{
		`cleat_plugin_events_lost_total{`,
		`plugin="audit-log"`, `reason="buffer_full"`, `reason="shutdown"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the scrape does not contain %q:\n%s", want, text)
		}
	}
	var fullLine string
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, "cleat_plugin_events_lost_total{") && strings.Contains(l, `reason="buffer_full"`) {
			fullLine = l
		}
	}
	if !strings.HasSuffix(fullLine, " 5") {
		t.Errorf("buffer_full line = %q, want a total of 5", fullLine)
	}
	if strings.Contains(text, `reason="insert_failed"`) {
		t.Error("a count of 0 created a series")
	}
	pluginEventsLostHook(nil)("audit-log", "buffer_full", 1) // no metrics: must not panic
}

// THE HOOK IS WIRED TO NOTHING UNLESS main() SETS IT. A counter with a feeder in a test and none in
// production reads as done (the shape of cleat#1673); this reads main.go and requires the
// plugin.Environment literal to set EventsLost from pluginEventsLostHook.
func TestTheWorkerSetsEventsLostOnThePluginEnvironment(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	found, wired := false, false
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if sel, ok := lit.Type.(*ast.SelectorExpr); !ok || sel.Sel.Name != "Environment" {
			return true
		}
		found = true
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "EventsLost" {
				if call, ok := kv.Value.(*ast.CallExpr); ok {
					if fn, ok := call.Fun.(*ast.Ident); ok && fn.Name == "pluginEventsLostHook" {
						wired = true
					}
				}
			}
		}
		return true
	})
	if !found {
		t.Fatal("main.go has no plugin.Environment literal, so this test measures nothing")
	}
	if !wired {
		t.Error("main.go's plugin.Environment does not set EventsLost from pluginEventsLostHook: every plugin's " +
			"lost events would be logged and never counted")
	}
}

// /healthz is unauthenticated and polled by every probe. Health() is plugin code (pagerdutyalert's runs a
// cross-tenant SELECT), so it must never run on the request: 50 probes must cost 0 Health() calls.
func TestHealthzDoesNotRunPluginHealthOnTheRequestPath(t *testing.T) {
	api := newTestAPIServer(&mockStore{})
	p := &healthPlugin{name: "counted", err: errors.New("down")}
	api.worker.plugList = []*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}
	api.worker.refreshPluginHealth()
	if p.calls.Load() != 1 {
		t.Fatalf("one refresh made %d Health() calls, want 1", p.calls.Load())
	}
	for i := 0; i < 50; i++ {
		if code, body := healthzOf(t, api); code != 200 || body["reason"] != "plugin_unhealthy" {
			t.Fatalf("probe %d: %d %v", i, code, body)
		}
	}
	if n := p.calls.Load(); n != 1 {
		t.Errorf("50 /healthz probes ran Health() %d more times: it is on the request path", n-1)
	}
}

// A plugin whose Init failed is not asked, and one that hangs neither stalls the refresh nor is asked
// again while the first call is still running.
func TestPluginHealthSkipsAFailedInitAndSurvivesAHangingHealth(t *testing.T) {
	api := newTestAPIServer(&mockStore{})
	failedInit := &healthPlugin{name: "failed-init", err: errors.New("would report unhealthy")}
	hang := make(chan struct{})
	hung := &healthPlugin{name: "hung", hang: hang}
	api.worker.plugList = []*plugin.LoadedPlugin{
		{Plugin: failedInit, Healthy: false, Error: errors.New("init failed")},
		{Plugin: hung, Healthy: true},
	}
	old := pluginHealthCallTimeoutForTest(200 * time.Millisecond)
	defer old()
	start := time.Now()
	api.worker.refreshPluginHealth()
	if failedInit.calls.Load() != 0 {
		t.Error("Health() was called on a plugin whose Init failed")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("a hanging Health() held the refresh for %s", d)
	}
	api.worker.refreshPluginHealth() // the first call is still running: not asked again
	if hung.calls.Load() != 1 {
		t.Errorf("a plugin with a Health() call still running was asked %d times, want 1", hung.calls.Load())
	}
	if code, body := healthzOf(t, api); code != 200 || len(body) != 1 {
		t.Errorf("an unanswered Health() degraded the worker: %d %v", code, body)
	}
	close(hang)
}

// The cache is filled by a loop, and a loop nobody launches leaves /healthz reporting every plugin
// healthy forever: correct-looking, and measuring nothing. This reads setup.go for the launch call.
func TestThePluginHealthLoopIsLaunched(t *testing.T) {
	src, err := os.ReadFile("setup.go")
	if err != nil {
		t.Fatal(err)
	}
	f, err := parser.ParseFile(token.NewFileSet(), "setup.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	launches := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "launchLoop" {
			return true
		}
		if lit, ok := call.Args[0].(*ast.BasicLit); ok {
			launches[strings.Trim(lit.Value, `"`)] = true
		}
		return true
	})
	if len(launches) < 5 {
		t.Fatalf("found only %d launchLoop calls: the scan is broken, not the wiring", len(launches))
	}
	if !launches["plugin_health"] {
		t.Error(`setup.go never calls launchLoop("plugin_health", ...): /healthz would never see a plugin's health`)
	}
}

// pluginHealthCallTimeoutForTest sets the timeout and returns the function that restores it.
func pluginHealthCallTimeoutForTest(d time.Duration) (restore func()) {
	old := pluginHealthCallTimeout
	pluginHealthCallTimeout = d
	return func() { pluginHealthCallTimeout = old }
}

// A Health() call that does not answer keeps the plugin's last reported state: a slow answer must neither
// clear a real problem nor invent one. (cleat-review: removing this branch survived.)
func TestPluginHealthKeepsTheLastAnswerWhenACallDoesNotAnswer(t *testing.T) {
	api := newTestAPIServer(&mockStore{})
	p := &healthPlugin{name: "audit-log", err: errors.New("audit-log lost 3 event(s)")}
	api.worker.plugList = []*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}
	api.worker.refreshPluginHealth()
	if len(api.worker.unhealthyPlugins()) != 1 {
		t.Fatal("the first refresh did not record the problem, so this measures nothing")
	}

	defer pluginHealthCallTimeoutForTest(100 * time.Millisecond)()
	hang := make(chan struct{})
	p.hang = hang
	api.worker.refreshPluginHealth() // the call does not answer within the timeout
	close(hang)
	if got := api.worker.unhealthyPlugins(); len(got) != 1 {
		t.Errorf("an unanswered Health() cleared the plugin's last reported problem: %v", got)
	}
	if code, body := healthzOf(t, api); code != 200 || body["reason"] != "plugin_unhealthy" {
		t.Errorf("/healthz after an unanswered Health(): %d %v, want it to keep saying plugin_unhealthy", code, body)
	}
}
