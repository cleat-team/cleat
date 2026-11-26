package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// cleat#4311. A worker at its concurrency limit slept a whole --poll in the
// dispatch loop, which nothing could interrupt, so a slot freed (or a NOTIFY
// that would resume a run) a moment after the sleep began went unused until the
// tick: 340 to 403 ms at --poll 500ms against 69 to 99 ms with a free slot.
// These pin the wait that replaced the sleep. The end-to-end figure is a
// tests/chaos measurement; what a unit test can hold is that a freed slot ends
// the wait, that the timer is still the ceiling, and that cancellation ends it.

func newSlotWaitWorker(t *testing.T, poll time.Duration) (*Worker, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &Worker{ctx: ctx, pollInterval: poll, slotFreedCh: newSlotFreedChannel()}, cancel
}

func TestAFreedSlotEndsTheAtCapacityWaitBeforeThePollTick(t *testing.T) {
	w, _ := newSlotWaitWorker(t, 30*time.Second)
	wf := &engine.WorkflowInstance{ID: "wf-slot", Generation: 1}
	reg := &execRegistration{generation: 1}
	w.inflight.Store(wf.ID, wf)

	go func() {
		time.Sleep(50 * time.Millisecond)
		w.deregisterExecution(wf, reg)
	}()
	start := time.Now()
	if !w.waitForSlot() {
		t.Fatal("waitForSlot reported a stopped worker")
	}
	// The ceiling is 30 s; a wait that only the timer ended would take all of it.
	if took := time.Since(start); took < 40*time.Millisecond || took > 5*time.Second {
		t.Fatalf("the wait took %s: want it ended by the slot freed after ~50ms, not by the 30s tick", took)
	}
	if _, still := w.inflight.Load(wf.ID); still {
		t.Fatal("the execution is still in flight after deregisterExecution")
	}
}

func TestASlotFreedBeforeTheWaitBeginsIsNotLost(t *testing.T) {
	// The loop counts, then waits; a slot freed between the two must still end
	// the wait. One buffered token is what carries it.
	w, _ := newSlotWaitWorker(t, 30*time.Second)
	wf := &engine.WorkflowInstance{ID: "wf-early", Generation: 1}
	w.inflight.Store(wf.ID, wf)
	w.deregisterExecution(wf, &execRegistration{generation: 1})

	start := time.Now()
	if !w.waitForSlot() || time.Since(start) > 5*time.Second {
		t.Fatalf("a slot freed before the wait began was lost: waited %s", time.Since(start))
	}
}

func TestTheAtCapacityWaitStillEndsAtThePollTick(t *testing.T) {
	// A missed signal must cost what the sleep always cost, not more.
	w, _ := newSlotWaitWorker(t, 60*time.Millisecond)
	start := time.Now()
	if !w.waitForSlot() {
		t.Fatal("waitForSlot reported a stopped worker")
	}
	if took := time.Since(start); took < 50*time.Millisecond || took > 5*time.Second {
		t.Fatalf("the wait took %s, want about the 60ms poll", took)
	}
}

func TestTheAtCapacityWaitEndsWhenTheWorkerStops(t *testing.T) {
	w, cancel := newSlotWaitWorker(t, 30*time.Second)
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if w.waitForSlot() {
		t.Fatal("waitForSlot reported a live worker after its context was cancelled")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("a stopped worker waited %s", took)
	}
}

func TestDeregisterExecutionDoesNotBlockOrPanicWithoutASlotChannel(t *testing.T) {
	// Worker values built without the channel (most tests) must keep working: a
	// send on a nil channel in a select with a default is a no-op.
	w := &Worker{}
	wf := &engine.WorkflowInstance{ID: "wf-nil", Generation: 1}
	w.inflight.Store(wf.ID, wf)
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.deregisterExecution(wf, &execRegistration{generation: 1})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deregisterExecution blocked with a nil slotFreedCh")
	}
}

// cleat#4476. The tests above hold waitForSlot and the send that ends it; nothing at PR time held that the dispatch loop's at-capacity branch CALLS
// waitForSlot, so a change putting a plain time.Sleep back there (the cleat#4311 defect) kept every one of them green, and was caught only by the
// timing row in the scheduled chaos sweep. The loop is too large to drive without a database and a clock, so this reads it: the `free <= 0` branch
// of dispatchLoop must call w.waitForSlot and must not sleep on its own.
func TestTheDispatchLoopsAtCapacityBranchWaitsForASlotAndDoesNotSleep(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "setup.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var branch *ast.BlockStmt
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "dispatchLoop" || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if is, ok := n.(*ast.IfStmt); ok {
				if b, ok := is.Cond.(*ast.BinaryExpr); ok && b.Op == token.LEQ {
					if x, ok := b.X.(*ast.Ident); ok && x.Name == "free" {
						if y, ok := b.Y.(*ast.BasicLit); ok && y.Value == "0" {
							branch = is.Body
						}
					}
				}
			}
			return true
		})
	}
	if branch == nil {
		t.Fatal("found no `if free <= 0` in dispatchLoop: the test's anchor is gone, so it cannot say what the at-capacity branch does (UNMEASURED, not a pass)")
	}
	var waits, sleeps int
	ast.Inspect(branch, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			switch {
			case sel.Sel.Name == "waitForSlot":
				waits++
			case sel.Sel.Name == "Sleep":
				sleeps++
			}
		}
		return true
	})
	if waits != 1 || sleeps != 0 {
		t.Fatalf("the at-capacity branch of dispatchLoop calls waitForSlot %d times and time.Sleep %d times, want 1 and 0: a worker at its limit must wake when a slot is freed (cleat#4311)", waits, sleeps)
	}
}
