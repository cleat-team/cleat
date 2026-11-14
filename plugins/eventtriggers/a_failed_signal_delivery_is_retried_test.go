package eventtriggers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/plugin"
)

// cleat#3806. A SignalWorkflow that failed was logged once and forgotten, and the
// event it announced was already stored, so nothing told the waiting workflow
// until its own unprompted re-check. These pin the retry's contract: a transient
// failure is retried and the awaiter is then unregistered like any delivered one;
// a persistent one is attempted a bounded number of times and the awaiter stays;
// and a workflow that is gone is not retried.

func withNoRetryDelay(t *testing.T) {
	t.Helper()
	old := signalRetryDelays
	signalRetryDelays = []time.Duration{0, 0}
	t.Cleanup(func() { signalRetryDelays = old })
}

func TestAFailedSignalDeliveryIsRetriedAndThenTreatedAsDelivered(t *testing.T) {
	withNoRetryDelay(t)
	db := newRecordingDB(t)
	db.awaiters = []string{"wf-blip"}
	calls := 0
	env := &plugin.Environment{
		SignalWorkflow: func(context.Context, string, string, string) error {
			calls++
			if calls < 3 {
				return errors.New("connection reset")
			}
			return nil
		},
	}

	signalAwaiters(context.Background(), db, quietLogger(), env, uuid.New(), "order.created", `{}`, "", "", "")

	if calls != 3 {
		t.Errorf("SignalWorkflow called %d times, want 3 (two failures, then the delivery)", calls)
	}
	if !execedDelete(db, "wf-blip", "order.created") {
		t.Errorf("the awaiter was not unregistered after the retried delivery succeeded.\nexecs: %v", db.execQueries)
	}
}

func TestAPersistentSignalFailureIsAttemptedABoundedNumberOfTimesAndKeepsTheAwaiter(t *testing.T) {
	withNoRetryDelay(t)
	db := newRecordingDB(t)
	db.awaiters = []string{"wf-down"}
	calls := 0
	env := &plugin.Environment{
		SignalWorkflow: func(context.Context, string, string, string) error {
			calls++
			return errors.New("database is down")
		},
	}

	signalAwaiters(context.Background(), db, quietLogger(), env, uuid.New(), "order.created", `{}`, "", "", "")

	if want := 1 + len(signalRetryDelays); calls != want {
		t.Errorf("SignalWorkflow called %d times, want %d (the first try and each retry)", calls, want)
	}
	if execedDelete(db, "wf-down", "order.created") {
		t.Error("the awaiter was unregistered after the delivery failed every time: it would never be told")
	}
}

func TestAWorkflowThatIsGoneIsNotRetried(t *testing.T) {
	withNoRetryDelay(t)
	db := newRecordingDB(t)
	db.awaiters = []string{"wf-purged"}
	calls := 0
	env := &plugin.Environment{
		SignalWorkflow: func(context.Context, string, string, string) error {
			calls++
			return plugin.ErrWorkflowNotFound
		},
	}

	signalAwaiters(context.Background(), db, quietLogger(), env, uuid.New(), "order.created", `{}`, "", "", "")

	if calls != 1 {
		t.Errorf("SignalWorkflow called %d times for a workflow that does not exist, want 1", calls)
	}
}

func TestACancelledContextStopsTheRetries(t *testing.T) {
	old := signalRetryDelays
	signalRetryDelays = []time.Duration{time.Hour, time.Hour}
	defer func() { signalRetryDelays = old }()

	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	env := &plugin.Environment{
		SignalWorkflow: func(context.Context, string, string, string) error {
			calls++
			cancel()
			return errors.New("connection reset")
		},
	}

	done := make(chan error, 1)
	go func() { done <- signalWithRetry(ctx, env, "wf", "__evt:x", "{}") }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a cancelled retry returned nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("signalWithRetry kept waiting after its context was cancelled")
	}
	if calls != 1 {
		t.Errorf("SignalWorkflow called %d times after cancellation, want 1", calls)
	}
}
