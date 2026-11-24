package engine

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sync"
	"testing"
	"time"
)

// The restartable limits of one execution (execution_limits.go). Every row is one of the ways a run that is HELD can get this wrong: a limit that expires
// during the hold, one that is not restarted when it ends, one that is paused but ignores a cancel, a pause that leaves a limit half paused, and one that is
// invisible to the pause because a WithValue sits between it and the registry.

type ctxKeyForTest struct{}

func newLimitedCtx(t *testing.T, parent context.Context) (context.Context, *executionLimits) {
	t.Helper()
	reg := &executionLimits{}
	return withExecutionLimits(parent, reg), reg
}

func limitDone(t *testing.T, ctx context.Context, within time.Duration) bool {
	t.Helper()
	select {
	case <-ctx.Done():
		return true
	case <-time.After(within):
		return false
	}
}

func TestARestartableLimitWithNoRegistryIsContextWithTimeout(t *testing.T) {
	ctx, cancel := withRestartableTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, ok := ctx.(*restartableDeadline); ok {
		t.Fatal("a limit with no registry in its context became a restartable one: nothing could ever pause it, and it would pretend to")
	}
	if !limitDone(t, ctx, 2*time.Second) || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("the plain limit did not expire as DeadlineExceeded: %v", ctx.Err())
	}
}

// context.WithTimeout ends a deadline that is already past before it returns, and the executor reads Err straight after to report a spent deadline as a timeout.
// The first version of the restartable limit armed only an asynchronous timer, so a 1ns workflow timeout read as "no error yet" and a run that had no time at
// all was reported as the guest failing (found by the whole-package run, not by any test written for the limit).
func TestARestartableLimitThatIsAlreadyPastIsEndedBeforeItIsReturned(t *testing.T) {
	for _, d := range []time.Duration{time.Nanosecond, 0, -time.Second} {
		base, _ := newLimitedCtx(t, context.Background())
		ctx, cancel := withRestartableTimeout(base, d)
		if _, ok := ctx.(*restartableDeadline); !ok {
			t.Fatal("PRECONDITION FAILED: the limit is not restartable")
		}
		if ctx.Err() != context.DeadlineExceeded {
			t.Errorf("a %s limit: Err = %v straight after creation, want DeadlineExceeded as context.WithTimeout gives", d, ctx.Err())
		}
		select {
		case <-ctx.Done():
		default:
			t.Errorf("a %s limit: Done is not closed straight after creation", d)
		}
		cancel()
	}
}

func TestARestartableLimitExpiresAsDeadlineExceededWhenNotPaused(t *testing.T) {
	base, _ := newLimitedCtx(t, context.Background())
	ctx, cancel := withRestartableTimeout(base, 30*time.Millisecond)
	defer cancel()
	if _, ok := ctx.(*restartableDeadline); !ok {
		t.Fatal("PRECONDITION FAILED: the limit is not restartable, so this test measures the standard library")
	}
	if !limitDone(t, ctx, 2*time.Second) {
		t.Fatal("a limit that was never paused did not expire")
	}
	// The executor compares exactly this (executor.go): not errors.Is, a value comparison.
	if ctx.Err() != context.DeadlineExceeded {
		t.Errorf("Err = %v, want context.DeadlineExceeded itself", ctx.Err())
	}
	if dl, ok := ctx.Deadline(); !ok || dl.IsZero() {
		t.Errorf("an expired limit reports no deadline: %v %v", dl, ok)
	}
}

func TestAPausedLimitDoesNotExpireAndARestartedOneHasItsFullDuration(t *testing.T) {
	base, _ := newLimitedCtx(t, context.Background())
	ctx, cancel := withRestartableTimeout(base, 120*time.Millisecond)
	defer cancel()

	time.Sleep(40 * time.Millisecond) // part of the limit is used
	resume, ok := pauseExecutionLimits(ctx)
	if !ok {
		t.Fatal("a running limit could not be paused")
	}
	// A hold three times the limit's whole duration: a limit that kept running would have ended it.
	if limitDone(t, ctx, 400*time.Millisecond) {
		t.Fatalf("the limit expired DURING the pause: %v", ctx.Err())
	}
	resume(true) // restart
	start := time.Now()
	// Restarted at its FULL duration, not at the 80ms it had left: at 100ms it must still be running, and it must end by about 120ms.
	if limitDone(t, ctx, 100*time.Millisecond) {
		t.Fatalf("the limit ended after %s, so it was restarted with what it had left (80ms), not its full 120ms", time.Since(start))
	}
	if !limitDone(t, ctx, 2*time.Second) {
		t.Fatal("a restarted limit never expired")
	}
}

func TestAResumeThatDoesNotRestartCarriesOnWithTheTimeItHadLeft(t *testing.T) {
	base, _ := newLimitedCtx(t, context.Background())
	ctx, cancel := withRestartableTimeout(base, 150*time.Millisecond)
	defer cancel()
	time.Sleep(60 * time.Millisecond) // 90ms left
	resume, ok := pauseExecutionLimits(ctx)
	if !ok {
		t.Fatal("could not pause")
	}
	time.Sleep(300 * time.Millisecond) // a pause that was NOT a hold
	resume(false)
	start := time.Now()
	if !limitDone(t, ctx, 2*time.Second) {
		t.Fatal("never expired")
	}
	if got := time.Since(start); got > 140*time.Millisecond {
		t.Errorf("a limit resumed without a restart ran for %s more, want about the 90ms it had left (it was handed a fresh 150ms)", got)
	}
}

func TestACancelReachesAPausedLimitAtOnce(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	base, _ := newLimitedCtx(t, parent)
	ctx, cancel := withRestartableTimeout(base, time.Hour)
	defer cancel()
	if _, ok := pauseExecutionLimits(ctx); !ok {
		t.Fatal("could not pause")
	}
	cancelParent() // a terminate while the run is held
	if !limitDone(t, ctx, time.Second) {
		t.Fatal("a cancel did not reach a paused limit: a terminate would wait for the hold")
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Errorf("Err = %v, want Canceled (it must not read as a timeout)", ctx.Err())
	}
}

func TestALimitThatHasAlreadyEndedCannotBePausedAndLeavesNothingPaused(t *testing.T) {
	base, _ := newLimitedCtx(t, context.Background())
	long, cancelLong := withRestartableTimeout(base, time.Hour) // outer, healthy
	defer cancelLong()
	short, cancelShort := withRestartableTimeout(long, 5*time.Millisecond) // inner, about to end
	defer cancelShort()
	if !limitDone(t, short, 2*time.Second) {
		t.Fatal("PRECONDITION FAILED: the short limit did not expire")
	}
	if resume, ok := pauseExecutionLimits(short); ok || resume != nil {
		t.Fatal("a run whose limit had already ended was allowed to be held: it would end in a timeout the pause was meant to prevent")
	}
	// Nothing may be left paused by the failed attempt: the outer limit still runs and still expires on its own schedule.
	if dl, ok := long.Deadline(); !ok || time.Until(dl) < 50*time.Minute {
		t.Errorf("the healthy outer limit was left paused or changed by a failed pause: deadline %v ok=%v", dl, ok)
	}
}

func TestEveryLimitIsPausedEvenWithAWithValueBetweenThem(t *testing.T) {
	base, _ := newLimitedCtx(t, context.Background())
	outer, c1 := withRestartableTimeout(base, 80*time.Millisecond)
	defer c1()
	// The workflow span is a WithValue, and a WithValue hides the method set of the limit under it: a pause found by asserting the context's type would
	// pause only the inner limit and leave this one running.
	wrapped := context.WithValue(outer, ctxKeyForTest{}, "span")
	inner, c2 := withRestartableTimeout(wrapped, 80*time.Millisecond)
	defer c2()
	resume, ok := pauseExecutionLimits(inner)
	if !ok {
		t.Fatal("could not pause")
	}
	if limitDone(t, outer, 300*time.Millisecond) || limitDone(t, inner, 10*time.Millisecond) {
		t.Fatalf("a limit expired during the pause: outer %v, inner %v (the OUTER one is the one a type assertion would miss)", outer.Err(), inner.Err())
	}
	resume(true)
	if !limitDone(t, inner, 2*time.Second) || !limitDone(t, outer, 2*time.Second) {
		t.Fatal("the restarted limits never expired")
	}
}

func TestADeadlineWhilePausedReportsOnlyTheOnesThatCannotBePaused(t *testing.T) {
	// The wake source asks ctx.Deadline() AFTER pausing, to see what is left that a hold cannot stop.
	plain, cancelPlain := context.WithTimeout(context.Background(), time.Hour) // a caller's own deadline
	defer cancelPlain()
	base, _ := newLimitedCtx(t, plain)
	limit, cancel := withRestartableTimeout(base, time.Minute)
	defer cancel()
	if dl, ok := limit.Deadline(); !ok || time.Until(dl) > 2*time.Minute {
		t.Fatalf("before the pause the deadline is the limit's own (about a minute): %v", time.Until(dl))
	}
	resume, ok := pauseExecutionLimits(limit)
	if !ok {
		t.Fatal("could not pause")
	}
	defer resume(false)
	dl, ok := limit.Deadline()
	if !ok || time.Until(dl) < 50*time.Minute {
		t.Errorf("while paused the deadline is %v (ok=%v); want the caller's own hour, which cannot be paused", time.Until(dl), ok)
	}
}

func TestRestartingTheLimitsTellsTheListenersAfterTheyHaveRestarted(t *testing.T) {
	base, _ := newLimitedCtx(t, context.Background())
	ctx, cancel := withRestartableTimeout(base, 200*time.Millisecond)
	defer cancel()
	var mu sync.Mutex
	var remainingAtNotify time.Duration
	notified := 0
	onLimitsRestart(ctx, func() {
		mu.Lock()
		defer mu.Unlock()
		notified++
		if dl, ok := ctx.Deadline(); ok {
			remainingAtNotify = time.Until(dl)
		}
	})
	resume, ok := pauseExecutionLimits(ctx)
	if !ok {
		t.Fatal("could not pause")
	}
	time.Sleep(50 * time.Millisecond)
	resume(false)
	mu.Lock()
	if notified != 0 {
		t.Errorf("a resume that did not restart notified the fence %d time(s): its budget would have been reset with no hold", notified)
	}
	mu.Unlock()

	resume, _ = pauseExecutionLimits(ctx)
	resume(true)
	mu.Lock()
	defer mu.Unlock()
	if notified != 1 {
		t.Fatalf("a restart notified the listeners %d time(s), want 1", notified)
	}
	if remainingAtNotify < 150*time.Millisecond {
		t.Errorf("the listener ran when the limit had %s left: it must run AFTER the restart, or the fence is sized from the old deadline", remainingAtNotify)
	}
}

func TestPausingUnderTheRaceDetector(t *testing.T) {
	base, _ := newLimitedCtx(t, context.Background())
	ctx, cancel := withRestartableTimeout(base, 3*time.Millisecond)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if resume, ok := pauseExecutionLimits(ctx); ok {
					resume(j%2 == 0)
				}
				_, _ = ctx.Deadline()
				_ = ctx.Err()
			}
		}()
	}
	wg.Wait()
}

// Every function that builds the fence budget must register its restart, or a held run on that path keeps a fence that counts the work before the wait and the work
// after it together. The component path (ExecuteComponentCGo) cannot be run without a Python or C# toolchain, which is why this reads the source: by FUNCTION, parsed
// and not counted as text, because a raw count of constructions is wrong the moment a construction is one that never holds a run. The two defer passes are exactly
// that: they run after the guest has stopped, and canPark is false while draining defers, so nothing is ever held there and a restart there would be meaningless.
var fenceBudgetBuildersThatNeverHold = map[string]string{
	"runGuestDefersAfterSuspend": "a post-run defer pass: canPark() is false while draining defers (park.go), so no wait in it is ever held",
	"runGuestDefersAfterKill":    "a post-run defer pass over a killed guest: the same",
}

// fenceBudgetBuildersRegisteredByTheirCaller are builders that DO hold a run and do not register the restart themselves, because their caller does, before it dispatches to
// them. Unlike the map above this is a claim about reachability, so a bare entry would be a claim nobody could disagree with: TestTheComponentPathIsOnlyReachedThrough
// ExecuteWhichRegistersFirst is what checks it, and an entry here with no such check is a hole.
var fenceBudgetBuildersRegisteredByTheirCaller = map[string]string{
	"ExecuteComponentCGo": "reached only through Execute (backend_wasmtime.go), which registers the restart before it dispatches; its closure reads b.budget when it runs, so it restarts this builder's budget",
}

// fenceBuildersIn returns, for each function in src that assigns b.budget = newHostBudget(...), whether it also calls onLimitsRestart.
func fenceBuildersIn(t *testing.T, name, src string) map[string]bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), name, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	out := map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		builds, registers := false, false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.AssignStmt:
				for i, rhs := range x.Rhs {
					call, ok := rhs.(*ast.CallExpr)
					if !ok || i >= len(x.Lhs) {
						continue
					}
					if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "newHostBudget" {
						if sel, ok := x.Lhs[i].(*ast.SelectorExpr); ok && sel.Sel.Name == "budget" {
							builds = true
						}
					}
				}
			case *ast.CallExpr:
				if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "onLimitsRestart" {
					registers = true
				}
			}
			return true
		})
		if builds {
			out[fn.Name.Name] = registers
		}
	}
	return out
}

func TestEveryFunctionThatBuildsTheFenceBudgetRegistersItsRestartOrNamesWhyNot(t *testing.T) {
	all := map[string]bool{}
	for _, f := range []string{"backend_wasmtime.go", "component_cgo.go"} {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for fn, reg := range fenceBuildersIn(t, f, string(raw)) {
			all[fn] = reg
		}
	}
	// Vacuity, by name: the scan must SEE the two paths that hold, or it is reading the wrong thing.
	for _, want := range []string{"Execute", "ExecuteComponentCGo"} {
		if _, ok := all[want]; !ok {
			t.Fatalf("the scan did not see %s build the fence budget: it is reading the wrong thing", want)
		}
	}
	for fn, registers := range all {
		_, exempt := fenceBudgetBuildersThatNeverHold[fn]
		_, byCaller := fenceBudgetBuildersRegisteredByTheirCaller[fn]
		switch {
		case byCaller && registers:
			t.Errorf("%s is listed as registered by its caller and registers a restart itself: the entry is stale, and the restart would run twice on every wake", fn)
		case byCaller:
			// Allowed, and checked by TestTheComponentPathIsOnlyReachedThroughExecuteWhichRegistersFirst.
		case !registers && !exempt:
			t.Errorf("%s builds the fence budget and does not register its restart: a held run on that path would be killed for the work of both segments. Register it, or name it in fenceBudgetBuildersThatNeverHold with the reason it can never hold", fn)
		case registers && exempt:
			t.Errorf("%s is listed as never holding and registers a restart: the entry is stale", fn)
		}
	}
	// Negative controls: the scan must say "does not register" for a builder that does not, and "registers" for one that does.
	const synthetic = "package engine\nfunc a(b *wasmtimeBackend) { b.budget = newHostBudget(nil, 0) }\nfunc c(b *wasmtimeBackend) { b.budget = newHostBudget(nil, 0); onLimitsRestart(nil, nil) }\nfunc d(b *wasmtimeBackend) { onLimitsRestart(nil, nil) }\n"
	got := fenceBuildersIn(t, "synthetic.go", synthetic)
	if reg, ok := got["a"]; !ok || reg {
		t.Errorf("scan control: a builder with no registration was reported as %v (seen=%v)", reg, ok)
	}
	if reg, ok := got["c"]; !ok || !reg {
		t.Errorf("scan control: a builder that registers was reported as %v (seen=%v)", reg, ok)
	}
	if _, ok := got["d"]; ok {
		t.Error("scan control: a function that only registers was reported as a builder")
	}
}
