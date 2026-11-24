package engine

import (
	"context"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/monitoring/prometheus"
)

// cleat#4201. The replays of AcquireLock, ReleaseLock and SetScope used to advance the step BEFORE they checked the recorded event's type, so a
// recorded event that belongs to another call was swallowed: the call returned error code 1 with stepCount one further on, replay still on, and no
// divergence reported. Every other replay checks first and leaves the step where it was, which is what ResolvePromise does here as the control.
// Measured before the fix, on a real execSession in replay: ResolvePromise over a create_promise left stepCount at 0; each of the three left it at 1.
//
// The own-event rows are the other half. "Did not advance over a foreign event" is also true of a call that never consumes anything, so each call
// is also run over its OWN event and must consume exactly that one step.

func replaySessionOver(history []EventRecord) *execSession {
	return &execSession{
		engine:     NewEngine(nil, nil, WithWorkflowID("replay-probe")),
		workflowID: "replay-probe",
		nowMs:      time.Now().UnixMilli(),
		deferrals:  map[string]string{},
		queryState: map[string]string{},
		isReplay:   true,
		history:    history,
	}
}

// replayFailureCount reads cleat_replay_failures_total from a real Metrics, summed over its label sets, and is 0 when the counter has not been
// emitted at all (findMetricLine fails on an absent counter, which is the case the own-event rows need to read as zero).
func replayFailureCount(t *testing.T, m *prometheus.Metrics) int {
	t.Helper()
	rec := httptest.NewRecorder()
	m.ServeHTTP().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	total := 0
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "cleat_replay_failures_total{") && !strings.HasPrefix(line, "cleat_replay_failures_total ") {
			continue
		}
		f := strings.Fields(line)
		n, err := strconv.ParseFloat(f[len(f)-1], 64)
		if err != nil {
			t.Fatalf("cannot read the value of %q: %v", line, err)
		}
		total += int(n)
	}
	return total
}

func newReplayMetrics(t *testing.T) *prometheus.Metrics {
	t.Helper()
	m, err := prometheus.New(prometheus.Config{WorkerID: "test-worker"})
	if err != nil {
		t.Fatalf("prometheus.New: %v", err)
	}
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	return m
}

func TestALockOrScopeReplayOverAForeignEventReportsADivergenceAndConsumesNothing(t *testing.T) {
	ctx := context.Background()
	foreign := []EventRecord{
		{Step: 0, EventType: EventTypeCreatePromise, PromiseID: "p", PromiseName: "n"},
		{Step: 1, EventType: EventTypeCreatePromise, PromiseID: "q", PromiseName: "m"},
	}
	calls := []struct {
		name string
		call func(s *execSession) int64
	}{
		{"ResolvePromise (control: reported a divergence before the fix too)", func(s *execSession) int64 { return s.ResolvePromise(ctx, nil, "p", "v") }},
		{"ReleaseLock", func(s *execSession) int64 { return s.ReleaseLock(ctx, nil, "k") }},
		{"AcquireLock", func(s *execSession) int64 { return s.AcquireLock(ctx, nil, "k", 1000) }},
		{"SetScope", func(s *execSession) int64 { return s.SetScope(ctx, nil, "type", "inst", 0, 0) }},
	}
	for _, c := range calls {
		t.Run(c.name, func(t *testing.T) {
			s := replaySessionOver(foreign)
			m := newReplayMetrics(t)
			s.engine.Metrics = m
			if got := c.call(s); got != 1 {
				t.Errorf("result = %#x over a create_promise, want 0x1 (error code 1)", got)
			}
			if s.stepCount != 0 {
				t.Errorf("stepCount = %d, want 0: the call consumed an event that belongs to another call and replay carried on one step out of line (cleat#4201)", s.stepCount)
			}
			if !s.isReplay {
				t.Errorf("isReplay = false: a foreign event must not end replay")
			}
			// "Reports a divergence" is observable in one place at these call sites (they pass no output buffer, so there is no message): the
			// replay-failure counter. Without this row a call that returned the same code and never counted the divergence would pass.
			if n := replayFailureCount(t, m); n != 1 {
				t.Errorf("cleat_replay_failures_total = %d after one foreign-event replay, want 1: the divergence was not counted", n)
			}
		})
	}
}

func TestALockOrScopeReplayOverItsOwnEventConsumesExactlyThatStep(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name  string
		event EventRecord
		call  func(s *execSession) int64
		want  int64
	}{
		{"ReleaseLock", EventRecord{Step: 0, EventType: EventTypeReleaseLock, LockKey: "k"},
			func(s *execSession) int64 { return s.ReleaseLock(ctx, nil, "k") }, 0},
		{"ReleaseLock, recorded failure", EventRecord{Step: 0, EventType: EventTypeReleaseLock, LockKey: "k", Err: "boom"},
			func(s *execSession) int64 { return s.ReleaseLock(ctx, nil, "k") }, 1},
		{"AcquireLock", EventRecord{Step: 0, EventType: EventTypeAcquireLock, LockKey: "k", LockTTLMs: 1000, LockAcquired: 1},
			func(s *execSession) int64 { return s.AcquireLock(ctx, nil, "k", 1000) }, packAcquireLockResult(true, 0)},
		{"AcquireLock, recorded failure", EventRecord{Step: 0, EventType: EventTypeAcquireLock, LockKey: "k", LockTTLMs: 1000, Err: "boom"},
			func(s *execSession) int64 { return s.AcquireLock(ctx, nil, "k", 1000) }, packAcquireLockResult(false, 1)},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := replaySessionOver([]EventRecord{c.event})
			m := newReplayMetrics(t)
			s.engine.Metrics = m
			if got := c.call(s); got != c.want {
				t.Errorf("result = %#x, want %#x", got, c.want)
			}
			if s.stepCount != 1 {
				t.Errorf("stepCount = %d over its own event, want 1: a call that consumes nothing would also pass the foreign-event test", s.stepCount)
			}
			if n := replayFailureCount(t, m); n != 0 {
				t.Errorf("cleat_replay_failures_total = %d over the call's own event, want 0: a matching event is not a divergence", n)
			}
		})
	}
}
