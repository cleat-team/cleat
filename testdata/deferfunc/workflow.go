// Package main is a fixture for defer execution.
//
// Each entry point registers defer bodies whose only observable effect is a
// DurableCall, because a DurableCall is what the host can see. A defer that
// did not run leaves its call absent from the recorded history; a defer that
// ran leaves it present, in a position that also pins the ORDER.
package main

import (
	"time"

	"github.com/cleat-team/cleat/cleat"
)

// DeferOrder registers two defer bodies and then makes one call of its own.
// A correct run records: body, then the two defers in LIFO order --
// "body", "second", "first".
func DeferOrder(h cleat.HostCalls, input string) (string, error) {
	if _, err := h.DurableDeferFunc(func() {
		h.DurableCall("cleanup", "first", `{}`)
	}); err != nil {
		return "", err
	}
	if _, err := h.DurableDeferFunc(func() {
		h.DurableCall("cleanup", "second", `{}`)
	}); err != nil {
		return "", err
	}

	if _, err := h.DurableCall("work", "body", `{}`); err != nil {
		return "", err
	}
	return `{"status":"ok"}`, nil
}

// DeferOnError is the case a defer exists for: the entry point fails after
// registering cleanup. The cleanup must still run.
func DeferOnError(h cleat.HostCalls, input string) (string, error) {
	if _, err := h.DurableDeferFunc(func() {
		h.DurableCall("cleanup", "on_error", `{}`)
	}); err != nil {
		return "", err
	}
	return "", errFailed
}

// DeferSurvivesSuspension registers cleanup and then sleeps. A suspension is
// not workflow exit, so the first segment must leave the cleanup unrun; the
// segment that finally completes must run it.
func DeferSurvivesSuspension(h cleat.HostCalls, input string) (string, error) {
	if _, err := h.DurableDeferFunc(func() {
		h.DurableCall("cleanup", "after_sleep", `{}`)
	}); err != nil {
		return "", err
	}

	h.DurableSleepMs(300000)

	if _, err := h.DurableCall("work", "body", `{}`); err != nil {
		return "", err
	}
	return `{"status":"ok"}`, nil
}

// DeferRegistration checks the registration contract rather than the body: the
// ID handed back is the host's, and it is what the body is keyed by.
func DeferRegistration(h cleat.HostCalls, input string) (string, error) {
	id, err := h.DurableDeferFunc(func() {})
	if err != nil {
		return "", err
	}
	return `{"defer_id":"` + id + `"}`, nil
}

var errFailed = &workflowError{"the workflow failed"}

type workflowError struct{ msg string }

func (e *workflowError) Error() string { return e.msg }

// DeferOnPanic registers cleanup and then panics.
//
// A panic is not a trap: the generated dispatcher recovers it and reports the
// failure through cleat_complete, so the guest still leaves through its own
// wrapper and its defers still have somewhere to run.
func DeferOnPanic(h cleat.HostCalls, input string) (string, error) {
	if _, err := h.DurableDeferFunc(func() {
		h.DurableCall("cleanup", "on_panic", `{}`)
	}); err != nil {
		return "", err
	}
	panic("the workflow panicked")
}

// DeferRegistersDefer has a defer body that tries to register another defer.
//
// IMPROVEMENT-PLAN 3.35 phase 4. Measured before the guard: the host minted an
// ID and wrote a durable defer event for the inner registration, and nothing
// ever ran it -- _cleatRunDeferred drains the table before the first body
// starts, so the new entry landed in a table nobody walks again. A completed
// workflow's history carried a pending defer that could not be executed.
//
// "refused" is the observable: the inner DurableDeferFunc must return an
// error, and no second defer event may appear in the history.
func DeferRegistersDefer(h cleat.HostCalls, input string) (string, error) {
	if _, err := h.DurableDeferFunc(func() {
		h.DurableCall("cleanup", "outer_defer_ran", `{}`)
		if _, err := h.DurableDeferFunc(func() {
			h.DurableCall("cleanup", "inner_defer_body", `{}`)
		}); err != nil {
			h.DurableCall("cleanup", "inner_defer_refused", `{}`)
		}
	}); err != nil {
		return "", err
	}
	h.DurableCall("cleanup", "body", `{}`)
	return `{"ok":true}`, nil
}

// DeferContinuesAsNew has a defer body that tries to continue-as-new.
//
// Measured before the guard: a continue_as_new event was recorded AND the
// wrapper reported the workflow's already-decided result, so one history
// carried two contradictory terminal facts. The worker stores "done" because a
// result is present, and the continuation is silently never taken.
func DeferContinuesAsNew(h cleat.HostCalls, input string) (string, error) {
	if _, err := h.DurableDeferFunc(func() {
		h.DurableCall("cleanup", "defer_ran", `{}`)
		if err := h.ContinueAsNew(`{"round":2}`); err != nil {
			h.DurableCall("cleanup", "continue_as_new_refused", `{}`)
		}
	}); err != nil {
		return "", err
	}
	h.DurableCall("cleanup", "body", `{}`)
	return `{"ok":true}`, nil
}

// DeferChildWorkflow starts a CHILD WORKFLOW as its body's new work, and
// registers cleanup first.
//
// IMPROVEMENT-PLAN 3.84. The other entry points here reach the host through
// cleat_call, which is the one path 3.83's stop sentinel covered. A child
// workflow goes through cleat_child_workflow, returns a different result
// layout, and -- unlike a durable call, whose side effect is somebody else's
// problem -- leaves a workflow_instances row behind that outlives the segment.
// So it is the sharpest test of whether the stop reaches a second path: the
// assertion is that no child is started while the defers still run.
func DeferChildWorkflow(h cleat.HostCalls, input string) (string, error) {
	if _, err := h.DurableDeferFunc(func() {
		h.DurableCall("cleanup", "after_child", `{}`)
	}); err != nil {
		return "", err
	}

	if _, err := h.ChildWorkflow("some-child", `{}`); err != nil {
		return "", err
	}
	return `{"status":"ok"}`, nil
}

// DeferOnRetriesExhausted registers cleanup and then exhausts a retry policy.
//
// Two things are observable in one run, and IMPROVEMENT-PLAN 3.88 needs both:
// whether an exhausting retry stays inside one segment, and whether the terminal
// error it produces is one the worker would dead-letter.
//
// The intervals are 1ms rather than the default second because the point is the
// exhaustion, not the wait. That does NOT make the backoff cheap -- see
// engine/retry_backoff_test.go, where a 1ms backoff suspends the workflow like
// any other durable sleep whenever its deadline is ahead of the engine's clock.
//
// It does make the interval too short to decide anything on the REAL clock: 1ms
// is less than the time a loaded machine takes to get from recording the failed
// call event to evaluating the sleep deadline, so on a real clock this fixture
// suspends or exhausts depending on the load. Both tests over it pin a clock.
// Raising the interval here would hide that rather than fix it -- the tests
// choose which side of the deadline they want, and neither waits.
func DeferOnRetriesExhausted(h cleat.HostCalls, input string) (string, error) {
	if _, err := h.DurableDeferFunc(func() {
		h.DurableCall("cleanup", "after_exhaustion", `{}`)
	}); err != nil {
		return "", err
	}

	_, err := h.DurableCallWithOptions(cleat.CallOptions{
		Retry: &cleat.RetryPolicy{
			MaxAttempts:        2,
			InitialInterval:    time.Millisecond,
			BackoffCoefficient: 1.0,
			MaxInterval:        time.Millisecond,
		},
	}, "always-fails", "op", `{}`)
	return "", err
}

// DeferOnLongRetryPolicy is DeferOnRetriesExhausted with a policy too long to
// hold a worker for.
//
// IMPROVEMENT-PLAN 3.88. The SDK decides between the host-side retry loop (one
// segment, worker held) and its own DurableSleep loop (one segment per backoff)
// from the policy's worst-case total backoff, against cleat.hostRetryBudget.
// Three attempts two minutes apart is four minutes of waiting, which is not
// something to keep a worker for -- so this must take the SDK path and suspend,
// where DeferOnRetriesExhausted's 1ms policy takes the host path and does not.
//
// The pair is the test: either entry point alone would pass against a build
// that ignored the threshold and always picked one path.
func DeferOnLongRetryPolicy(h cleat.HostCalls, input string) (string, error) {
	if _, err := h.DurableDeferFunc(func() {
		h.DurableCall("cleanup", "after_long_policy", `{}`)
	}); err != nil {
		return "", err
	}

	_, err := h.DurableCallWithOptions(cleat.CallOptions{
		Retry: &cleat.RetryPolicy{
			MaxAttempts:        3,
			InitialInterval:    2 * time.Minute,
			BackoffCoefficient: 1.0,
			MaxInterval:        2 * time.Minute,
		},
	}, "always-fails", "op", `{}`)
	return "", err
}
