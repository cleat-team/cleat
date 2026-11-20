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
	return s.reportReplayDivergence(ctx, m, divergenceMessage(&s.history[s.stepCount], expected), outPtr, outMaxLen)
}

// replayDivergenceAt is replayDivergence for a record that is not the next one: a multi-record step finds its foreign record part-way through
// its run, and the message must name THAT record, not the first of the run. Nothing is consumed.
//
// It calls reportReplayDivergence directly, as replayDivergence does, and does not delegate to it: the parity test that finds where a handler
// writes its result length follows callees to a fixed depth, and one more hop between a host call and the packer puts the packer out of reach.
func (s *execSession) replayDivergenceAt(ctx context.Context, m api.Module, got *EventRecord, expected EventType, outPtr, outMaxLen uint32) int64 {
	return s.reportReplayDivergence(ctx, m, divergenceMessage(got, expected), outPtr, outMaxLen)
}

// divergenceMessage is the one text of a type divergence: the step and both event types.
func divergenceMessage(got *EventRecord, expected EventType) string {
	return fmt.Sprintf("replay divergence at step %d: expected %s, got %s.\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).",
		got.Step, expected, got.EventType)
}

// replayIdentityDivergence reports that the event at the next step is the right TYPE for this call but names something else: a different
// promise, lock, signal or child than the one the workflow is now asking for (cleat#4211). A replay that matched on type alone accepted it,
// consumed it, and answered as though the recorded call were this one, so the history and the code had disagreed and nothing said so. Same
// shape, metric and non-consumption as replayDivergence; what differs is the message, which names what was recorded and what was asked.
func (s *execSession) replayIdentityDivergence(ctx context.Context, m api.Module, step string, why string, outPtr, outMaxLen uint32) int64 {
	return s.reportReplayDivergence(ctx, m, identityDivergenceMessage(&s.history[s.stepCount], step, why), outPtr, outMaxLen)
}

// replayIdentityDivergenceAt is replayIdentityDivergence for a record part-way through a run (direct to the reporter, for the reason above).
func (s *execSession) replayIdentityDivergenceAt(ctx context.Context, m api.Module, got *EventRecord, step string, why string, outPtr, outMaxLen uint32) int64 {
	return s.reportReplayDivergence(ctx, m, identityDivergenceMessage(got, step, why), outPtr, outMaxLen)
}

// identityDivergenceMessage is the one text of an identity divergence.
func identityDivergenceMessage(got *EventRecord, step string, why string) string {
	return fmt.Sprintf("replay divergence at step %d: %s asked for something other than the %s recorded there: %s.\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).",
		got.Step, step, got.EventType, why)
}

// reportReplayDivergence is the one place a divergence is counted and written to the guest: the failure metric, then error code 1 in a simple
// result with the message in the call's own output buffer when it has one.
func (s *execSession) reportReplayDivergence(ctx context.Context, m api.Module, msg string, outPtr, outMaxLen uint32) int64 {
	if s.engine.Metrics != nil {
		s.engine.Metrics.RecordReplayFailure(ctx)
	}
	var written uint32
	if outMaxLen > 0 {
		written, _ = s.writeResult(ctx, m, outPtr, msg, outMaxLen)
	}
	return packSimpleResult(1, written)
}
