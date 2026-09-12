package main

import (
	"context"
	"errors"
	"log/slog"
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
	ran     bool
}

func (p *panickingBackgroundPlugin) Info() plugin.PluginInfo {
	return plugin.PluginInfo{Name: p.name}
}

func (p *panickingBackgroundPlugin) Init(context.Context, *plugin.Environment) error { return nil }

func (p *panickingBackgroundPlugin) Run(ctx context.Context) error {
	p.ran = true
	if p.panicOn {
		panic("plugin background loop exploded")
	}
	return p.err
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
// runPluginBackground and this test does not fail, it CRASHES THE TEST BINARY --
// the same way it crashed the worker. That is the defect reproduced, and it is
// why the panicking case is driven through a real goroutine rather than called
// directly.
func TestAPluginBackgroundPanicDoesNotKillTheWorker(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)

	bg := &panickingBackgroundPlugin{name: "exploding-plugin", panicOn: true}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		runPluginBackground(context.Background(), logger, "worker-1", bg)
	}()
	wg.Wait()

	if !bg.ran {
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

	bg := &panickingBackgroundPlugin{name: "exploding-plugin", panicOn: true}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		runPluginBackground(context.Background(), logger, "worker-1", bg)
	}()
	wg.Wait()

	mu.Lock()
	out := buf.String()
	mu.Unlock()

	for _, want := range []string{
		"PANIC in plugin background worker",
		"exploding-plugin",                // which plugin
		"plugin background loop exploded", // the panic value
		"runPluginBackground",             // the stack
		"the worker continues",            // what the operator should conclude
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the panic log does not contain %q.\n\nGot:\n%s", want, out)
		}
	}
}

// TestAnOrdinaryBackgroundErrorStillReachesTheLog is the control.
//
// Without it, "the goroutine returns" is satisfied by a runPluginBackground
// that returns immediately and never calls Run at all -- and every assertion
// above would still pass.
func TestAnOrdinaryBackgroundErrorStillReachesTheLog(t *testing.T) {
	var buf strings.Builder
	var mu sync.Mutex
	logger := slog.New(slog.NewTextHandler(&syncWriter{w: &buf, mu: &mu}, nil))

	bg := &panickingBackgroundPlugin{name: "tidy-plugin", err: errors.New("shutting down")}
	runPluginBackground(context.Background(), logger, "worker-1", bg)

	if !bg.ran {
		t.Fatal("Run was never called")
	}
	mu.Lock()
	out := buf.String()
	mu.Unlock()
	if !strings.Contains(out, "plugin background worker exited") || !strings.Contains(out, "shutting down") {
		t.Errorf("an ordinary Run error was not logged.\n\nGot:\n%s", out)
	}
	if strings.Contains(out, "PANIC") {
		t.Errorf("an ordinary error was reported as a panic.\n\nGot:\n%s", out)
	}
}

type syncWriter struct {
	w  *strings.Builder
	mu *sync.Mutex
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
