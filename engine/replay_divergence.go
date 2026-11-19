package engine

import (
	"context"
	"fmt"

	"github.com/tetratelabs/wazero/api"
)

// replayDivergence reports that the event at the next step is not the one this call recorded, while history still continues.
//
// cleat#4157. A host call that ALWAYS records an event, when it finds some other event at its step with history still unread, has not met
// the frontier: the original run reached a LATER event without recording this call, so the workflow's code is not the code that produced
// this history. These calls used to call exitReplay() there and carry on fresh, and that made the rest of the replay run FRESH: a durable
// call already recorded after the step executed again (a duplicate side effect), and recordEvent appended at a step that already had an
// event. Measured on v0.5.0 for await_signals (cleat#4153) and, by the probe in
// a_host_call_never_leaves_replay_with_history_unread_test.go, for nine more.
//
// It reports in the shape the other replayed calls use (replayCall, the cron calls, Fetch): the failure metric, a message naming the step
// and both event types, and error code 1 in a simple result with the message in the call's own output buffer when it has one.
//
// It does NOT consume the event. replayCall does, and for a call that records nothing on some path that is wrong, because the event
// belongs to the call that follows; leaving it means that call still reads its own record. A true divergence keeps diverging on each
// later call, which is the honest answer.
func (s *execSession) replayDivergence(ctx context.Context, m api.Module, expected EventType, outPtr, outMaxLen uint32) int64 {
	got := s.history[s.stepCount]
	if s.engine.Metrics != nil {
		s.engine.Metrics.RecordReplayFailure(ctx)
	}
	msg := fmt.Sprintf("replay divergence at step %d: expected %s, got %s.\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).",
		got.Step, expected, got.EventType)
	var written uint32
	if outMaxLen > 0 {
		written, _ = s.writeResult(ctx, m, outPtr, msg, outMaxLen)
	}
	return packSimpleResult(1, written)
}
