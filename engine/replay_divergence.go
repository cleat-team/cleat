package engine

import (
	"context"
	"fmt"

	"github.com/tetratelabs/wazero/api"
)

// replayDivergenceAt reports that a record at this step is not the one this call recorded, while history still continues. It is given the record, so a multi-record step
// can name one part-way through its run and not the first of the run.
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
//
// It calls reportReplayDivergence directly and does not delegate: the parity test that finds where a handler writes its result length follows callees to a fixed
// depth, and one more hop between a host call and the packer puts the packer out of reach. (Its next-record form, replayDivergence, was deleted when the last four
// calls that used it moved onto the dispatcher, cleat#4152, W1.)
func (s *execSession) replayDivergenceAt(ctx context.Context, m api.Module, got *EventRecord, expected EventType, detail string, outPtr, outMaxLen uint32) int64 {
	return s.reportReplayDivergence(ctx, m, divergenceMessageWith(got, expected, detail), outPtr, outMaxLen)
}

// divergenceMessageWith is the one text of a type divergence, the step and both event types, with a spec's detail between the step line and the advice: what the call asked and what was recorded, in the
// lines `docs/troubleshooting.md` shows (`  actual request: ...`). detail is "" or starts with a newline and indents each line by two spaces, so a spec that
// has none reads exactly as it always has (cleat#4152, G3).
func divergenceMessageWith(got *EventRecord, expected EventType, detail string) string {
	return fmt.Sprintf("replay divergence at step %d: expected %s, got %s.%s\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).",
		got.Step, expected, got.EventType, detail)
}

// replayIdentityDivergenceAt reports that a record is the right TYPE for this call but names something else: a different promise, lock, signal or child than
// the one the workflow is now asking for (cleat#4211). A replay that matched on type alone accepted it, consumed it, and answered as though the recorded call
// were this one, so the history and the code had disagreed and nothing said so. Same shape, metric and non-consumption as replayDivergence; what differs is the
// message, which names what was recorded and what was asked, followed by the spec's detail. It is given the record, so a multi-record step can name one part-way
// through its run; it goes direct to the reporter, for the reason replayDivergenceAt gives. (The next-record form of this function, replayIdentityDivergence,
// was deleted when the dispatcher's two single-record sites moved to this one so that they could pass a detail, cleat#4152.)
func (s *execSession) replayIdentityDivergenceAt(ctx context.Context, m api.Module, got *EventRecord, step string, why string, detail string, outPtr, outMaxLen uint32) int64 {
	return s.reportReplayDivergence(ctx, m, identityDivergenceMessageWith(got, step, why, detail), outPtr, outMaxLen)
}

// identityDivergenceMessageWith is the one text of an identity divergence: the step, what the call asked and what was recorded, then the spec's detail placed as
// divergenceMessageWith places it ("" for a spec with none).
func identityDivergenceMessageWith(got *EventRecord, step string, why string, detail string) string {
	return fmt.Sprintf("replay divergence at step %d: %s asked for something other than the %s recorded there: %s.%s\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).",
		got.Step, step, got.EventType, why, detail)
}

// reportReplayDivergence is the one place a divergence is counted and written to the guest: the failure metric, then error code 1 in a simple
// result with the message in the call's own output buffer when it has one.
func (s *execSession) reportReplayDivergence(ctx context.Context, m api.Module, msg string, outPtr, outMaxLen uint32) int64 {
	if s.engine != nil && s.engine.Metrics != nil {
		s.engine.Metrics.RecordReplayFailure(ctx)
	}
	var written uint32
	if outMaxLen > 0 {
		written, _ = s.writeResult(ctx, m, outPtr, msg, outMaxLen)
	}
	return packSimpleResult(1, written)
}
