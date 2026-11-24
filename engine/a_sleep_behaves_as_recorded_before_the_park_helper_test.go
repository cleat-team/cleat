package engine

import (
	"context"
	"math"
	"testing"
	"time"
)

// A table recorded from DurableSleep as it was BEFORE the park-or-suspend helper existed (cleat#4152, W2b), written and run against the
// unmodified code, then run again after the sleep moved onto the helper. A row that changes after the move is a behaviour change, and the move
// is meant to have none: the helper refuses every time until the park path exists, and a refusal has to be exactly what the inline code did.
//
// Sleep records no event and decides from the clock (IMPROVEMENT-PLAN 3.67), which is why most rows are about the clock, and why the three
// rows at the end matter: a sleep is NOT refused in a defer segment, on shutdown or after a failed event write. Every other wait is refused
// there, so "the helper refuses ineligible sessions" must not be read as "sleep is refused there"; it suspends there, as it always did.
func TestASleepBehavesAsRecordedBeforeTheParkHelper(t *testing.T) {
	const T = int64(1_800_000_000_000) // the engine's clock for every row, in ms

	closedShutdown := make(chan struct{})
	close(closedShutdown)

	rows := []struct {
		name string
		opts []EngineOption
		// build prepares the session; nowMs and history are the sleep's anchors.
		build   func(s *execSession)
		sleeps  []int64 // durations, called in order on one session
		want    []sleepRow
		wantAnd func(t *testing.T, s *execSession)
	}{
		{
			name:   "a fresh sleep suspends for the whole duration",
			build:  func(s *execSession) { s.nowMs = T },
			sleeps: []int64{5000},
			want:   []sleepRow{{suspend: true, reason: "cleat_sleep(5000ms)", until: T + 5000}},
		},
		{
			name:   "a zero sleep completes at once",
			build:  func(s *execSession) { s.nowMs = T },
			sleeps: []int64{0},
			want:   []sleepRow{{suspend: false}},
		},
		{
			name: "a sleep whose deadline is behind real time has already been served",
			// replay: the last consumed event is a minute old, so anchor + 1s is long past.
			build: func(s *execSession) {
				s.nowMs = 0
				s.history = []EventRecord{{Step: 0, EventType: fakeEvent, TimestampMs: T - 60_000}}
				s.stepCount = 1
				s.isReplay = true
			},
			sleeps: []int64{1000},
			want:   []sleepRow{{suspend: false}},
		},
		{
			name: "no anchor at all uses real time, so a fresh sleep still waits",
			// nowMs unset and nothing recorded: the CLI and embedded paths. An epoch anchor would complete every sleep at once.
			build:  func(s *execSession) { s.nowMs = 0 },
			sleeps: []int64{7000},
			want:   []sleepRow{{suspend: true, reason: "cleat_sleep(7000ms)", until: T + 7000}},
		},
		{
			name:  "two sleeps in a row stack: the second anchors on the first's deadline, not on the same instant",
			build: func(s *execSession) { s.nowMs = T },
			// With assignment instead of max() the second would read the same anchor and compute T+3000, completing a wait it never performed.
			sleeps: []int64{5000, 3000},
			want: []sleepRow{
				{suspend: true, reason: "cleat_sleep(5000ms)", until: T + 5000},
				{suspend: true, reason: "cleat_sleep(3000ms)", until: T + 8000},
			},
		},
		{
			name:   "a sleep of forever is clamped, not wrapped into the past (cleat#4105)",
			build:  func(s *execSession) { s.nowMs = T },
			sleeps: []int64{math.MaxInt64},
			wantAnd: func(t *testing.T, s *execSession) {
				if s.suspendErr == nil {
					t.Fatalf("a sleep of math.MaxInt64 completed at once: the deadline wrapped negative")
				}
				if !s.suspendErr.Until.After(time.UnixMilli(T)) {
					t.Errorf("Until = %v, want a time after the clock %v", s.suspendErr.Until, time.UnixMilli(T))
				}
			},
		},
		{
			name:   "a sleep in a defer segment's body still suspends (sleep is not refused)",
			opts:   []EngineOption{WithDeferPhase()},
			build:  func(s *execSession) { s.nowMs = T },
			sleeps: []int64{5000},
			want:   []sleepRow{{suspend: true, reason: "cleat_sleep(5000ms)", until: T + 5000}},
		},
		{
			name:   "a sleep on a worker that is shutting down still suspends",
			opts:   []EngineOption{WithShutdownSignal(closedShutdown)},
			build:  func(s *execSession) { s.nowMs = T },
			sleeps: []int64{5000},
			want:   []sleepRow{{suspend: true, reason: "cleat_sleep(5000ms)", until: T + 5000}},
		},
		{
			name:   "a sleep after a recorded event could not be written still suspends",
			build:  func(s *execSession) { s.nowMs = T; s.eventWriteFailure = context.DeadlineExceeded },
			sleeps: []int64{5000},
			want:   []sleepRow{{suspend: true, reason: "cleat_sleep(5000ms)", until: T + 5000}},
		},
	}

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			s := stepSession(append([]EngineOption{WithClock(func() int64 { return T })}, r.opts...), nil)
			r.build(s)
			for i, d := range r.sleeps {
				s.suspendErr = nil
				got := s.DurableSleep(context.Background(), nil, d)
				if i < len(r.want) {
					w := r.want[i]
					wantRes := packSleepResult(sleepStatusCompleted, 0)
					if w.suspend {
						wantRes = packSleepResult(sleepStatusSuspend, d)
					}
					if got != wantRes {
						t.Errorf("sleep %d (%dms): result = %#x, want %#x", i, d, got, wantRes)
					}
					switch {
					case w.suspend && s.suspendErr == nil:
						t.Errorf("sleep %d (%dms): no suspension was set", i, d)
					case w.suspend:
						if s.suspendErr.Reason != w.reason {
							t.Errorf("sleep %d: Reason = %q, want %q", i, s.suspendErr.Reason, w.reason)
						}
						if want := time.UnixMilli(w.until); !s.suspendErr.Until.Equal(want) {
							t.Errorf("sleep %d: Until = %v, want %v", i, s.suspendErr.Until, want)
						}
					case s.suspendErr != nil:
						t.Errorf("sleep %d (%dms) completed but also set a suspension: %+v", i, d, s.suspendErr)
					}
				}
			}
			if r.wantAnd != nil {
				r.wantAnd(t, s)
			}
			// A sleep records no event and consumes no history.
			if s.eventCount != 0 || len(s.stepViolations) != 0 {
				t.Errorf("a sleep recorded %d events and %d violations; it records none", s.eventCount, len(s.stepViolations))
			}
		})
	}
}

type sleepRow struct {
	suspend bool
	reason  string
	until   int64
}
