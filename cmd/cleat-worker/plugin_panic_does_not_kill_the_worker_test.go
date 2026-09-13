package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

// panickingBackgroundPlugin is a plugin whose background loop panics, which is
// what twelve shipped plugins could do and none guards against.
type panickingBackgroundPlugin struct {
	name    string
	err     error
	panicOn bool

	mu  sync.Mutex
	ran bool
}

func (p *panickingBackgroundPlugin) Info() plugin.PluginInfo {
	return plugin.PluginInfo{Name: p.name}
}

func (p *panickingBackgroundPlugin) Init(context.Context, *plugin.Environment) error { return nil }

func (p *panickingBackgroundPlugin) Run(ctx context.Context) error {
	p.mu.Lock()
	p.ran = true
	p.mu.Unlock()
	if p.panicOn {
		panic("plugin background loop exploded")
	}
	return p.err
}

func (p *panickingBackgroundPlugin) didRun() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ran
}

// runPluginLoops drives Worker.startPluginBackground and waits for the loops.
//
// Through the Worker rather than a free function, because that is the whole of
// cleat#1347: the loops used to be started in main() 124 lines before the
// *Worker existed, so they were outside withPanicRecovery, the health tracker
// and the background-loop metric. A test that called a free helper could not
// tell the difference.
func runPluginLoops(t *testing.T, logger *slog.Logger, plugins ...plugin.HasBackground) *Worker {
	t.Helper()
	w := newTestWorker(&mockStore{})
	w.logger = logger
	w.bgPlugins = plugins
	var wg sync.WaitGroup
	w.bgWg = &wg
	w.startPluginBackground()
	wg.Wait()
	return w
}

// TestAPluginBackgroundPanicDoesNotKillTheWorker is cleat#1304.
//
// A panic in any of the twelve plugin background goroutines terminated the
// whole worker process, taking every in-flight workflow with it. An unrecovered
// panic in a goroutine cannot be caught by the parent, so the recover has to be
// in the function the goroutine runs.
//
// THE FALSIFICATION FOR THIS TEST IS UNUSUALLY BLUNT, and worth stating because
// it is what makes the test meaningful: remove the recover from
// startPluginBackground and this test does not fail, it CRASHES THE TEST
// BINARY -- the same way it crashed the worker. That is the defect reproduced,
// and it is why the panicking case is driven through a real goroutine rather
// than called directly.
func TestAPluginBackgroundPanicDoesNotKillTheWorker(t *testing.T) {
	bg := &panickingBackgroundPlugin{name: "exploding-plugin", panicOn: true}
	runPluginLoops(t, slog.New(slog.DiscardHandler), bg)

	if !bg.didRun() {
		t.Fatal("Run was never called, so this proves nothing about recovering from it")
	}
	// Reaching here at all is the assertion: the goroutine returned instead of
	// taking the process down.
}

// TestAPluginBackgroundPanicIsLoggedWithItsStack checks the panic is reported
// rather than swallowed.
//
// Recovering silently would pass the test above and leave an operator with a
// plugin whose background work has stopped and nothing anywhere saying so --
// trading a loud failure for a silent one, which on balance is not obviously
// the better trade. The stack is what makes the log actionable.
func TestAPluginBackgroundPanicIsLoggedWithItsStack(t *testing.T) {
	var buf strings.Builder
	var mu sync.Mutex
	logger := slog.New(slog.NewTextHandler(&syncWriter{w: &buf, mu: &mu}, nil))

	runPluginLoops(t, logger, &panickingBackgroundPlugin{name: "exploding-plugin", panicOn: true})

	mu.Lock()
	out := buf.String()
	mu.Unlock()

	for _, want := range []string{
		"PANIC in plugin background worker",
		"exploding-plugin",                // which plugin
		"plugin background loop exploded", // the panic value
		"startPluginBackground",           // the stack
		"the worker continues",            // what the operator should conclude
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the panic log does not contain %q.\n\nGot:\n%s", want, out)
		}
	}
}

// TestAPluginBackgroundPanicReachesTheHealthTrackerAndTheMetric is cleat#1347,
// and it is the assertion the log-only tests above cannot make.
//
// The loops were started before the *Worker existed, so a panic reached
// neither healthTracker.recordPanic nor Metrics.RecordBackgroundLoop. An
// operator learned about a dead plugin loop by reading logs -- there was no
// health signal and no metric, which is silent cessation of a feature dressed
// as a running worker.
func TestAPluginBackgroundPanicReachesTheHealthTrackerAndTheMetric(t *testing.T) {
	w := runPluginLoops(t, slog.New(slog.DiscardHandler),
		&panickingBackgroundPlugin{name: "exploding-plugin", panicOn: true})

	const name = "plugin:exploding-plugin"

	_, panicked, _ := w.healthTracker.snapshot()
	if !panicked[name] {
		t.Errorf("healthTracker recorded no panic for %q; /healthz cannot report a plugin loop "+
			"that has stopped.\n\nrecorded: %v", name, panicked)
	}

	// Scraped, not asserted on a call count: what matters is that a panic is
	// visible to whatever is watching the metrics endpoint, under a label an
	// operator can alert on.
	rec := httptest.NewRecorder()
	w.Metrics.ServeHTTP().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `loop_name="`+name+`"`) || !strings.Contains(body, `status="panic"`) {
		t.Errorf("the background-loop metric has no panic for %q.\n\nWanted loop_name=%q with "+
			"status=\"panic\" in the scrape.", name, name)
	}

	// The name is prefixed so a plugin cannot collide with a worker loop.
	// "dispatch" is taken, and a plugin called that would otherwise overwrite
	// the dispatch loop's health entry -- which is worse than no entry.
	if strings.Contains(body, `loop_name="exploding-plugin"`) {
		t.Errorf("the metric carries the bare plugin name, so a plugin named after a worker loop " +
			"would collide with it")
	}
}

// TestAHealthyPluginRecordsNoPanic is the control for the test above.
//
// Without it, a health tracker that marked EVERY plugin loop as panicked would
// pass every assertion there.
func TestAHealthyPluginRecordsNoPanic(t *testing.T) {
	bg := &panickingBackgroundPlugin{name: "tidy-plugin"}
	w := runPluginLoops(t, slog.New(slog.DiscardHandler), bg)

	if !bg.didRun() {
		t.Fatal("Run was never called, so recording no panic proves nothing")
	}
	_, panicked, _ := w.healthTracker.snapshot()
	if panicked["plugin:tidy-plugin"] {
		t.Error("a plugin that returned cleanly was recorded as having panicked")
	}
}

// TestAnOrdinaryBackgroundErrorStillReachesTheLog is the control.
//
// Without it, "the goroutine returns" is satisfied by a startPluginBackground
// that returns immediately and never calls Run at all -- and every assertion
// above would still pass.
func TestAnOrdinaryBackgroundErrorStillReachesTheLog(t *testing.T) {
	var buf strings.Builder
	var mu sync.Mutex
	logger := slog.New(slog.NewTextHandler(&syncWriter{w: &buf, mu: &mu}, nil))

	bg := &panickingBackgroundPlugin{name: "tidy-plugin", err: errors.New("shutting down")}
	runPluginLoops(t, logger, bg)

	if !bg.didRun() {
		t.Fatal("Run was never called")
	}
	mu.Lock()
	out := buf.String()
	mu.Unlock()
	if !strings.Contains(out, "plugin background worker exited") || !strings.Contains(out, "shutting down") {
		t.Errorf("an ordinary Run error was not logged.\n\nGot:\n%s", out)
	}
	if strings.Contains(out, "PANIC") {
		t.Errorf("an ordinary Run error was reported as a panic.\n\nGot:\n%s", out)
	}
}

// syncWriter serialises writes to a strings.Builder, which is not safe for
// concurrent use and is written to from the plugin goroutine.
type syncWriter struct {
	w  *strings.Builder
	mu *sync.Mutex
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
