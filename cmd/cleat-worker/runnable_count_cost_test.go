package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// TestTheRunnableCountIsAskedOnlyAtFullBackoff pins the cost claim that made
// this feature acceptable.
//
// A zero claim is ambiguous -- ClaimWorkflows uses FOR UPDATE SKIP LOCKED, so a
// locked row is removed from the result set rather than blocking, and "nothing
// to run" and "everything was locked" arrive identically (IMPROVEMENT-PLAN
// 3.249). Telling them apart needs a count, and the count needs a query.
//
// The whole reason it is affordable is WHERE it is paid: only once idleTicks
// reaches maxIdleTicks. A count on every idle poll would put a query on this
// loop's most common path, which is the objection that ruled out the
// behavioural fix in the first place -- so a regression that moved it there
// would quietly reintroduce the cost this design exists to avoid, while every
// other test still passed.
//
// idleTicks resets only on a parent wake or a NOTIFY, and neither fires here,
// so `idleTicks == maxIdleTicks` is true EXACTLY ONCE per idle streak. Over a
// run of many empty polls the count must therefore be asked once -- not once
// per poll, and not once per six polls.
func TestTheRunnableCountIsAskedOnlyAtFullBackoff(t *testing.T) {
	var claims, counts int64

	ms := &mockStore{}
	ms.claimStickyWorkflowsFn = func(context.Context, string, int) ([]*engine.WorkflowInstance, error) {
		atomic.AddInt64(&claims, 1)
		return nil, nil
	}
	ms.claimWorkflowsFn = func(context.Context, string, int) ([]*engine.WorkflowInstance, error) {
		return nil, nil
	}
	ms.countRunnableWorkflowsFn = func(context.Context) (int, error) {
		atomic.AddInt64(&counts, 1)
		return 7, nil // non-zero, so the warning path is the one exercised
	}

	w := newTestWorker(ms)
	w.pollInterval = 5 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	w.ctx = ctx
	w.cancel = cancel

	done := make(chan struct{})
	w.wg.Add(1)
	go func() {
		w.dispatchLoop()
		close(done)
	}()
	<-done

	gotClaims := atomic.LoadInt64(&claims)
	gotCounts := atomic.LoadInt64(&counts)
	t.Logf("claim cycles=%d  runnable-count queries=%d", gotClaims, gotCounts)

	// The control: without enough idle cycles to reach the cap, this test
	// cannot distinguish "asked once" from "never asked", and a count of 0
	// would pass an assertion written as `counts <= 1`.
	if gotClaims < maxIdleTicks+2 {
		t.Fatalf("only %d claim cycles in the window, which is not enough to reach maxIdleTicks=%d; "+
			"this run measured nothing", gotClaims, maxIdleTicks)
	}
	if gotCounts == 0 {
		t.Errorf("the runnable count was never asked across %d idle cycles.\n\n"+
			"It must be asked when the worker reaches full backoff, or a worker starved by a "+
			"long-held lock stays silent -- which is the case this feature exists to surface.",
			gotClaims)
	}
	if gotCounts > 1 {
		t.Errorf("the runnable count was asked %d times across %d idle cycles, want exactly 1.\n\n"+
			"idleTicks resets only on a parent wake or a NOTIFY, neither of which fires here, so "+
			"`idleTicks == maxIdleTicks` is true once per idle streak. More than one means the "+
			"query moved onto a hotter path -- most likely `>=` in place of `==` -- which puts a "+
			"query on this loop's most common path and reintroduces the cost that ruled out the "+
			"behavioural fix.", gotCounts, gotClaims)
	}
}
