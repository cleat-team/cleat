//go:build cgo

package engine

import (
	"context"
	"testing"
	"time"
)

// The execution fence (--wasm-instance-timeout) restarts when a held wait wakes (restartFence, backend_wasmtime.go). The row that shows that on a real guest is
// timing-dependent and lives behind the `timing` build tag (a_held_wait_restarts_the_fence_on_real_wasmtime_timing_test.go), run by a non-gating job. THIS is
// the part of it that needs no clock: restartFence is arithmetic over the configured timeout, the context's deadline and the tick floor, and each of those is
// pinned here with a deadline far enough away that a loaded machine cannot change the answer.
//
// What these cannot see, and the timing row can: that leaveHost re-arms the epoch deadline from what restartFence set. The structural test in
// execution_limits_test.go covers the other half, that every builder of the budget registers this function at all.

func restartFenceBackend(execTimeout time.Duration, remaining time.Duration) *wasmtimeBackend {
	b := &wasmtimeBackend{}
	b.limits.executionTimeout = execTimeout
	// A budget with no store: restart only sets the remaining time, and nothing here arms a deadline.
	b.budget = &hostBudget{remaining: remaining}
	return b
}

func TestRestartFenceGivesTheGuestItsFullBudgetBackWhenNothingBindsFirst(t *testing.T) {
	// The guest had used almost everything in its first segment.
	b := restartFenceBackend(30*time.Second, time.Millisecond)
	b.epochBoundByDeadline = true // stale state from before, which a restart must clear

	b.restartFence(context.Background())

	if got := b.budget.guestRemaining(); got != 30*time.Second {
		t.Errorf("remaining after the restart = %s, want the full 30s the execution fence is configured to", got)
	}
	if b.epochBoundByDeadline {
		t.Errorf("epochBoundByDeadline stayed true: a timeout would be blamed on the workflow's deadline when it was the fence")
	}
}

func TestRestartFenceUsesTheDefaultWhenNoTimeoutIsConfigured(t *testing.T) {
	b := restartFenceBackend(0, time.Millisecond)

	b.restartFence(context.Background())

	if got := b.budget.guestRemaining(); got != DefaultWasmtimeExecutionTimeout {
		t.Errorf("remaining = %s, want the default %s, the value configureStore applies when none is set", got, DefaultWasmtimeExecutionTimeout)
	}
}

func TestRestartFenceIsBoundByTheWorkflowDeadlineWhenThatIsSooner(t *testing.T) {
	b := restartFenceBackend(30*time.Second, time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	b.restartFence(ctx)

	got := b.budget.guestRemaining()
	// 10s less whatever the test itself took: never more than the deadline, and nowhere near the 30s fence.
	if got <= 0 || got > 10*time.Second {
		t.Fatalf("remaining = %s, want at most the 10s the workflow deadline has left", got)
	}
	if got < 5*time.Second {
		t.Fatalf("remaining = %s: the restart lost more than half of a 10s deadline to the arithmetic", got)
	}
	if !b.epochBoundByDeadline {
		t.Errorf("epochBoundByDeadline = false: a timeout in this segment would point at --wasm-instance-timeout when the workflow's deadline was the bound")
	}
}

func TestRestartFenceIsNotBoundByAWorkflowDeadlineThatIsFartherThanTheFence(t *testing.T) {
	b := restartFenceBackend(5*time.Second, time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()

	b.restartFence(ctx)

	if got := b.budget.guestRemaining(); got != 5*time.Second {
		t.Errorf("remaining = %s, want the fence's own 5s: an hour of workflow deadline is not the bound", got)
	}
	if b.epochBoundByDeadline {
		t.Errorf("epochBoundByDeadline = true for a deadline an hour away")
	}
}

// A deadline that has already passed still gets one tick, the floor configureStore and setDeadline apply: an exhausted guest stops at the next tick instead of
// being given a zero or negative budget the epoch arithmetic cannot represent.
func TestRestartFenceFloorsAnAlreadyPassedDeadlineAtOneTick(t *testing.T) {
	b := restartFenceBackend(30*time.Second, time.Millisecond)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
	defer cancel()

	b.restartFence(ctx)

	if got := b.budget.guestRemaining(); got != epochTickInterval {
		t.Errorf("remaining = %s, want exactly one tick (%s)", got, epochTickInterval)
	}
	if !b.epochBoundByDeadline {
		t.Errorf("epochBoundByDeadline = false for a deadline that had passed")
	}
}

func TestRestartFenceWithNoBudgetDoesNothing(t *testing.T) {
	// The fence disabled (newHostBudget returns nil): every method tolerates a nil budget, and so must the restart.
	b := &wasmtimeBackend{}
	b.restartFence(context.Background()) // must not panic
	if b.budget != nil {
		t.Errorf("a restart created a budget where the fence is disabled")
	}
}
