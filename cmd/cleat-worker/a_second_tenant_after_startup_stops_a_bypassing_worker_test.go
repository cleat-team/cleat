package main

// cleat#3773: a second tenant that appears AFTER startup, on a connection that
// bypasses row-level security, must stop the worker at its next dispatch tick.
//
// The negative control is the first case below and it is the important one: a
// worker whose connection IS subject to the policies serves any number of
// tenants legitimately, and a guard that fired there would take down the normal
// multi-tenant mode. It also pins the cost: that worker must not issue the
// count query at all.

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

func TestDecideRuntimeMultiTenant(t *testing.T) {
	reasons := []engine.RLSBypassReason{{Detail: "connected as a superuser"}}

	tests := []struct {
		name     string
		reasons  []engine.RLSBypassReason
		tenants  int
		wantStop bool
		why      string
	}{
		{
			name:    "a connection subject to the policies holds any number of tenants",
			reasons: nil, tenants: 3, wantStop: false,
			why: "this is the normal multi-tenant mode and the guard must never fire on it",
		},
		{
			name:    "a bypassing connection with no tenant stops for nothing",
			reasons: reasons, tenants: 0, wantStop: false,
		},
		{
			name:    "a bypassing connection holding one tenant keeps running",
			reasons: reasons, tenants: 1, wantStop: false,
			why: "one tenant is the single-tenant laptop configuration D82 leaves alone",
		},
		{
			name:    "a bypassing connection holding two tenants must stop",
			reasons: reasons, tenants: 2, wantStop: true,
		},
		{
			name:    "and three",
			reasons: reasons, tenants: 3, wantStop: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decideRuntimeMultiTenant(tt.reasons, tt.tenants); got != tt.wantStop {
				t.Fatalf("decideRuntimeMultiTenant(%d reasons, %d tenants) = %v, want %v -- %s",
					len(tt.reasons), tt.tenants, got, tt.wantStop, tt.why)
			}
		})
	}
}

// isolationProbe is the smallest Worker that checkRuntimeTenantIsolation and the
// top of dispatchLoop need. It counts how often the tenant query was issued, so
// "the policies case pays nothing" is measured rather than asserted.
type isolationProbe struct {
	*Worker
	counts int32
}

func newIsolationProbe(t *testing.T, reasons []engine.RLSBypassReason, tenants int, countErr error) (*isolationProbe, *int32) {
	t.Helper()
	var exited int32
	p := &isolationProbe{}
	w := &Worker{
		id:               "probe-worker",
		logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		ctx:              context.Background(),
		pollInterval:     time.Millisecond,
		healthTracker:    newHealthTracker(),
		rlsBypassReasons: reasons,
		rlsBypassRole:    "probe_role",
		exitFunc: func(int) {
			atomic.AddInt32(&exited, 1)
		},
		countTenantsForIsolation: func(context.Context) (int, error) {
			atomic.AddInt32(&p.counts, 1)
			return tenants, countErr
		},
	}
	p.Worker = w
	return p, &exited
}

// TestTheRuntimeGuardFiresOnlyOnABypassingConnectionHoldingTwoTenants covers the
// decision AND its cost: the policies case must not issue the count query.
func TestTheRuntimeGuardFiresOnlyOnABypassingConnectionHoldingTwoTenants(t *testing.T) {
	bypass := []engine.RLSBypassReason{{Detail: "connected as a superuser"}}

	tests := []struct {
		name      string
		reasons   []engine.RLSBypassReason
		tenants   int
		wantExit  int32
		wantCount int32
	}{
		{
			name:    "subject to the policies, three tenants: no exit, and no query at all",
			reasons: nil, tenants: 3, wantExit: 0, wantCount: 0,
		},
		{
			name:    "bypassing, one tenant held: no exit, but it does look",
			reasons: bypass, tenants: 1, wantExit: 0, wantCount: 1,
		},
		{
			// This pins the THRESHOLD only: the count arrives through the
			// injected counter. Which rows it counts -- suspended included -- is
			// pinned against a real database in
			// engine/a_suspended_tenant_is_not_served_test.go, because that is
			// the half the guard's whole premise rests on.
			name:    "bypassing, two tenants counted: exit",
			reasons: bypass, tenants: 2, wantExit: 1, wantCount: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, exited := newIsolationProbe(t, tt.reasons, tt.tenants, nil)
			fired := p.checkRuntimeTenantIsolation()
			if got := atomic.LoadInt32(exited); got != tt.wantExit {
				t.Fatalf("exit called %d times, want %d", got, tt.wantExit)
			}
			if got := atomic.LoadInt32(&p.counts); got != tt.wantCount {
				t.Fatalf("tenant query issued %d times, want %d", got, tt.wantCount)
			}
			if fired != (tt.wantExit == 1) {
				t.Fatalf("checkRuntimeTenantIsolation reported %v, want %v", fired, tt.wantExit == 1)
			}
		})
	}
}

// TestACountFailureDoesNotStopTheWorker pins the fail-open direction the startup
// check also takes: an inconclusive read must not turn a database hiccup into a
// crash loop, and the next tick retries.
func TestACountFailureDoesNotStopTheWorker(t *testing.T) {
	bypass := []engine.RLSBypassReason{{Detail: "connected as a superuser"}}
	p, exited := newIsolationProbe(t, bypass, 2, context.DeadlineExceeded)
	if p.checkRuntimeTenantIsolation() {
		t.Fatal("the guard fired on a failed count")
	}
	if atomic.LoadInt32(exited) != 0 {
		t.Fatal("the guard exited the worker on a failed count")
	}
	if atomic.LoadInt32(&p.counts) != 1 {
		t.Fatal("the count was not attempted")
	}
}

// TestTheDispatchLoopRunsTheGuard drives the REAL loop, which is what makes the
// wiring observable: the guard lives at the top of the tick rather than in a
// claim path, precisely so a saturated worker and one started with
// --claim-across-tenants=false cannot skip it.
func TestTheDispatchLoopRunsTheGuard(t *testing.T) {
	bypass := []engine.RLSBypassReason{{Detail: "connected as a superuser"}}
	p, exited := newIsolationProbe(t, bypass, 2, nil)

	done := make(chan struct{})
	panicked := make(chan any, 1)
	p.wg.Add(1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				panicked <- r
			}
			close(done)
		}()
		p.dispatchLoop()
	}()

	select {
	case r := <-panicked:
		// Reached when the guard is made unreachable: the probe has no
		// memoryController, so the tick dies on the statement AFTER the guard.
		// Reporting the panic this way names the missing guard instead of a
		// nil-pointer in a component the test never built.
		t.Fatalf("dispatchLoop panicked before the guard stopped it (%v); the guard must return before the tick does any other work", r)
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatchLoop did not return after the guard fired; a substituted exit must still stop the loop")
	}
	if atomic.LoadInt32(exited) != 1 {
		t.Fatalf("exit called %d times, want 1", atomic.LoadInt32(exited))
	}
}
