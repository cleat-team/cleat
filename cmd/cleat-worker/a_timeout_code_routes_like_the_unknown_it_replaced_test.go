package main

// cleat#3752. A workflow whose deadline expired used to be recorded with
// error_code "unknown"; it is now "timeout". The code is a label on the failure
// and must not move where the failure goes: it is failed -- not retried, not
// reclaimed, not dead-lettered -- exactly as it was.
//
// Pinned through recordTerminalFailureWithHistory with the same histories under
// both codes, so a rule that read the code to decide the route (a dead-letter
// for "unknown", say, or a release for "timeout") shows as a difference between
// the two rather than as a count in either.

import (
	"context"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

func TestATimeoutCodeRoutesLikeTheUnknownItReplaced(t *testing.T) {
	ordinaryCall := engine.EventRecord{
		EventType: engine.EventTypeCall, Service: "svc", Op: "op",
		Response: `{"ok":true}`,
	}
	// The call the deadline interrupted: failed, but not by exhausting its retries.
	interruptedCall := engine.EventRecord{
		EventType: engine.EventTypeCall, Service: "svc", Op: "op",
		Err: "context deadline exceeded", RetriesExhausted: false,
	}

	type route struct {
		dlq, fail, released bool
		code                string
	}
	run := func(code string, history []engine.EventRecord) route {
		var r route
		ms := &mockStore{}
		ms.moveToDeadLetterQueueFn = func(ctx context.Context, workflowID, workerID string, generation int64, errorMsg, errorCode, errorOp string, queryState map[string]string) error {
			r.dlq, r.code = true, errorCode
			return nil
		}
		ms.failWorkflowFn = func(ctx context.Context, workflowID, workerID string, generation int64, errorMsg, errorCode, errorOp string, queryState map[string]string) error {
			r.fail, r.code = true, errorCode
			return nil
		}
		w := newTestWorker(ms)
		wf := testInstance("deadline-expiry-wf")
		w.recordTerminalFailureWithHistory(wf, time.Now(), "host: workflow "+wf.ID+": execution timed out", code, "", history, nil)
		return r
	}

	for _, tc := range []struct {
		name    string
		history []engine.EventRecord
	}{
		{"expired before the guest started", nil},
		{"expired mid-guest, after a completed call", []engine.EventRecord{ordinaryCall}},
		{"expired during a call", []engine.EventRecord{interruptedCall}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			was := run(engine.ErrUnknown.String(), tc.history)
			now := run(engine.ErrTimeout.String(), tc.history)

			// The outcome: failed, once, and nowhere else.
			if !now.fail || now.dlq {
				t.Errorf("a deadline expiry must be failed and not dead-lettered; got fail=%v dead-letter=%v", now.fail, now.dlq)
			}
			if now.fail != was.fail || now.dlq != was.dlq {
				t.Errorf("the code moved the route: unknown -> fail=%v dlq=%v, timeout -> fail=%v dlq=%v",
					was.fail, was.dlq, now.fail, now.dlq)
			}
			// The label: what is stored is what the engine said, untouched.
			if now.code != engine.ErrTimeout.String() {
				t.Errorf("stored error_code = %q, want %q", now.code, engine.ErrTimeout.String())
			}
			if was.code != engine.ErrUnknown.String() {
				t.Errorf("control: an unclassified failure stored %q, want %q", was.code, engine.ErrUnknown.String())
			}
		})
	}
}

// The one history where the code does reach the record: the run's last durable
// act was a call that exhausted its retries, and then the deadline expired. It
// is dead-lettered by the exhaustion (the history, not the code), and its code
// is the one develop wrote for it, retries_exhausted, whichever label the
// engine put on the expiry. cleat#3752.
func TestAnExhaustedRunWhoseDeadlineThenExpiredKeepsRetriesExhausted(t *testing.T) {
	exhausted := engine.EventRecord{
		EventType: engine.EventTypeCall, Service: "svc", Op: "op",
		Err: "retries exhausted: connection refused", RetriesExhausted: true,
	}
	for _, code := range []string{engine.ErrUnknown.String(), engine.ErrTimeout.String()} {
		t.Run("engine label "+code, func(t *testing.T) {
			var sawDLQ, sawFail bool
			var stored string
			ms := &mockStore{}
			ms.moveToDeadLetterQueueFn = func(ctx context.Context, workflowID, workerID string, generation int64, errorMsg, errorCode, errorOp string, queryState map[string]string) error {
				sawDLQ, stored = true, errorCode
				return nil
			}
			ms.failWorkflowFn = func(ctx context.Context, workflowID, workerID string, generation int64, errorMsg, errorCode, errorOp string, queryState map[string]string) error {
				sawFail = true
				return nil
			}
			w := newTestWorker(ms)
			wf := testInstance("exhausted-then-expired-wf")
			w.recordTerminalFailureWithHistory(wf, time.Now(), "host: workflow "+wf.ID+": execution timed out", code, "", []engine.EventRecord{exhausted}, nil)

			if !sawDLQ || sawFail {
				t.Fatalf("dead-letter=%v fail=%v, want dead-letter only", sawDLQ, sawFail)
			}
			if stored != engine.ErrRetriesExhausted.String() {
				t.Errorf("stored error_code = %q, want %q: the dead-letter was decided by the exhaustion", stored, engine.ErrRetriesExhausted.String())
			}
		})
	}
}
