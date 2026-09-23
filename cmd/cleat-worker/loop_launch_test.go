package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"testing"
	"time"
)

// Every background loop that gets a context must also be started.
//
// # The defect this exists for
//
// `compactionLoop` was defined, tested, given a `--compaction-threshold` flag
// and a `--compaction-interval` flag, plumbed through main.go, given a health
// tracker interval, and given `initLoopCtx("compaction")` -- and never
// launched. History compaction had NEVER run in any deployment (cleat#877).
//
// Everything around it was present, which is what made it invisible. The
// missing piece was one line, and the piece most likely to be missing is always
// the one that STARTS something: every other piece of a loop is referenced by
// the thing above it, so an unreferenced loop body looks exactly like a
// referenced one from anywhere except the launch site.
//
// # Why initLoopCtx is the right anchor
//
// It is the one call that says "this loop is meant to exist" without also
// starting it. A loop with a context and no launch is a loop somebody intended
// and did not wire; a loop with neither is not a loop. Anchoring on the
// function definition instead would report every helper named `*Loop`, and
// anchoring on the flag would report configuration that legitimately has no
// loop.
//
// The stale direction matters as much: `initLoopCtx("update_dispatch")`
// outlived the loop it prepared when the 5s update ticker was deleted
// (IMPROVEMENT-PLAN 3.239), and nothing noticed. This test reports that too.
func TestEveryPreparedLoopIsLaunched(t *testing.T) {
	prepared, launched, registered := loopNamesIn(t, "setup.go")

	if len(prepared) < 5 {
		t.Fatalf("found only %d initLoopCtx calls; there were 9 on 2026-09-07. "+
			"A scan that finds almost nothing passes vacuously.", len(prepared))
	}

	var neverLaunched, neverPrepared []string
	for name := range prepared {
		if !launched[name] {
			neverLaunched = append(neverLaunched, name)
		}
	}
	for name := range launched {
		if !prepared[name] {
			neverPrepared = append(neverPrepared, name)
		}
	}
	sort.Strings(neverLaunched)
	sort.Strings(neverPrepared)

	for _, name := range neverLaunched {
		t.Errorf("loop %q has initLoopCtx but no launchLoop: it is prepared and never started.\n\n"+
			"Either add the launchLoop call, or drop the initLoopCtx if the loop is gone. "+
			"This is the defect cleat#877 was -- a complete subsystem behind a missing "+
			"start, where every other piece being present is what stopped anyone looking.",
			name)
	}
	for _, name := range neverPrepared {
		t.Errorf("loop %q is launched but has no initLoopCtx, so getLoopCtx(%q) has no entry "+
			"and the loop cannot be cancelled or restarted by the watchdog.", name, name)
	}

	// A launched loop should also be registered, or the watchdog cannot restart
	// it after a panic. Reported separately: it is a weaker failure than never
	// starting, and conflating them would let a fix for one look like a fix for
	// both.
	for name := range launched {
		if !registered[name] {
			t.Errorf("loop %q is launched but never passed to registerLoopFunc, so the "+
				"watchdog has no function to restart it with.", name)
		}
	}

	t.Logf("%d loops prepared, %d launched, %d registered", len(prepared), len(launched), len(registered))
}

// TestHealthzStaysHealthyPastMaxAgeOnADefaultWorker is cleat#2004's
// regression test. On a default worker (no --version-gc-interval, no
// tenant pools) /healthz turned 503 about two minutes after start, because
// version_gc and tenant_pool_reaper were registered with the health
// tracker at startup and never recorded a run -- they are permanently
// off by default, so nothing ever would. Kubernetes read the 503 as a
// liveness failure and restarted an otherwise-healthy worker in a loop.
//
// This exercises the SAME code Run() runs for those two loops -- not a
// hand-rolled substitute -- so a future regression that reintroduces
// unconditional health-tracker registration is caught here rather than
// two minutes into a real deployment.
func TestHealthzStaysHealthyPastMaxAgeOnADefaultWorker(t *testing.T) {
	ms := &mockStore{}
	api := newTestAPIServer(ms)
	w := api.worker

	fakeNow := time.Now()
	w.healthTracker.now = func() time.Time { return fakeNow }

	// Mirrors Run(): version_gc always gets a loop context, so launchLoop
	// can start its goroutine, but the health tracker only learns about it
	// if the loop decides to keep running.
	w.initLoopCtxUnmonitored("version_gc")
	w.versionGCInterval = 0 // the documented flag default
	w.wg.Add(1)
	w.versionGCLoop() // returns immediately: interval <= 0, never registers

	// tenant_pool_reaper's context and registration are both gated in Run()
	// on hasReapablePools(); a default test worker has neither tenantPools
	// nor a TenantPoolReaper-capable store factory, so Run() would not
	// call initLoopCtx("tenant_pool_reaper") either.
	if w.hasReapablePools() {
		t.Fatal("test worker unexpectedly reports reapable pools; this test no longer models a default worker")
	}

	// Positive control: a loop that IS running must still read healthy, so
	// the assertion below is not vacuously true from an empty tracker.
	w.healthTracker.registerLoop("heartbeat")
	w.healthTracker.setInterval("heartbeat", time.Second)

	// Advance the fake clock well past the 120s default maxAge, with no
	// real sleep, then record the heartbeat's run at the new time -- the
	// loop is still alive and ticking, only the clock moved.
	fakeNow = fakeNow.Add(3 * time.Minute)
	w.healthTracker.recordRun("heartbeat")

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	api.handleHealthz(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz = %d on a default worker 3 minutes after start, want 200: %s", rec.Code, rec.Body.String())
	}
}

// loopNamesIn collects the string-literal argument of each initLoopCtx,
// launchLoop and registerLoopFunc call.
//
// From the AST, so a loop name inside a comment or a log message is not
// mistaken for a call -- the trap this repo keeps paying for. An *ast.CallExpr
// is neither.
func loopNamesIn(t *testing.T, file string) (prepared, launched, registered map[string]bool) {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing %s: %v", file, err)
	}

	prepared, launched, registered = map[string]bool{}, map[string]bool{}, map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		var fn string
		switch v := call.Fun.(type) {
		case *ast.Ident:
			fn = v.Name // initLoopCtx is a local closure in Run
		case *ast.SelectorExpr:
			fn = v.Sel.Name
		default:
			return true
		}

		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			// A non-literal name cannot be checked statically. Not silently
			// ignored: see the vacuity floor on the caller, which is what
			// notices if this branch starts swallowing everything.
			return true
		}
		name, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}

		switch fn {
		case "initLoopCtx", "initLoopCtxUnmonitored":
			// Both give launchLoop the loopCtxMap entry it needs to start the
			// goroutine and the watchdog needs to cancel it; the only
			// difference is whether the loop is registered with the health
			// tracker immediately or registers itself once it knows it will
			// actually keep running (cleat#2004, versionGCLoop). This test's
			// "prepared" is about the context existing, not about health
			// tracking, so the two are the same answer to the question it asks.
			prepared[name] = true
		case "launchLoop":
			launched[name] = true
		case "registerLoopFunc":
			registered[name] = true
		}
		return true
	})
	return prepared, launched, registered
}
