package engine

import (
	"context"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

// cleat#4117. A plugin stream that sends exactly one chunk, marked finished, records the same row replayPluginCallStreaming used to read as
// recordStreamError's synthetic failure chunk. So a successful one-chunk stream succeeded fresh and FAILED on every replay of its own history,
// with the chunk's text as the error message. The two are told apart by EventRecord.StreamData, which the fresh path sets on plugin output.
//
// The probe runs one history through the real payload encoder and decoder (a field the payload does not carry would replay as the failure) and
// reads what the guest would be handed from a byte buffer, not from a packed length: a length that is merely non-zero says nothing about
// whether it is the stream or an error message.

// streamCallOutput runs one PluginCallStreaming on s and returns the packed result's error status and the bytes the guest would read.
func streamCallOutput(t *testing.T, s *execSession) (errCode uint32, out string) {
	t.Helper()
	buf := make([]byte, 4096)
	ctx := context.WithValue(context.Background(), wasmMemBufKey{}, buf)
	result := s.PluginCallStreaming(ctx, nil, "test-plugin", "Echo", `{}`, 0, uint32(len(buf)))
	length, _, errCode := decodeDurableCallResult(result)
	return errCode, string(buf[:length])
}

func registerOneChunkStream(s *execSession, chunks ...plugin.StreamEvent) {
	s.engine.pluginRegistry = NewPluginRegistry()
	s.engine.pluginStreamRegistry = NewPluginStreamRegistry()
	_ = s.engine.pluginStreamRegistry.Register("test-plugin", "Echo",
		func(ctx context.Context, in string) (<-chan plugin.StreamEvent, error) {
			ch := make(chan plugin.StreamEvent, len(chunks))
			for _, c := range chunks {
				ch <- c
			}
			close(ch)
			return ch, nil
		})
}

func TestAStreamOfOneFinishedChunkReplaysAsTheSuccessItWas(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chunks []plugin.StreamEvent
	}{
		{"one finished chunk", []plugin.StreamEvent{{Index: 0, Content: "only", Finish: true}}},
		{"two chunks, the second finished (control: never took the single-chunk path)",
			[]plugin.StreamEvent{{Index: 0, Content: "a"}, {Index: 1, Content: "b", Finish: true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fresh := newTestExecSession()
			registerOneChunkStream(fresh, tc.chunks...)
			freshErr, freshOut := streamCallOutput(t, fresh)
			if freshErr != 0 {
				t.Fatalf("PRECONDITION FAILED: the fresh run itself reported error status %d (output %q)", freshErr, freshOut)
			}
			if len(fresh.history) != len(tc.chunks) {
				t.Fatalf("PRECONDITION FAILED: the fresh run recorded %d event(s), want %d", len(fresh.history), len(tc.chunks))
			}

			replayed := newTestExecSession()
			registerOneChunkStream(replayed, tc.chunks...)
			replayed.isReplay = true
			for _, rec := range fresh.history {
				replayed.history = append(replayed.history, roundTripThroughPayload(t, rec))
			}
			replayErr, replayOut := streamCallOutput(t, replayed)

			if replayErr != 0 {
				t.Errorf("the replay of a successful stream reported error status %d with message %q; fresh reported success", replayErr, replayOut)
			}
			if replayOut != freshOut {
				t.Errorf("fresh and replay disagree:\n fresh : %s\n replay: %s", freshOut, replayOut)
			}
		})
	}
}

// The other side of the discriminator: a stream-level FAILURE is still a failure on replay, with the message and the code it was recorded with.
// Without this a fix that marks everything as data would pass the test above.
func TestAStreamFailureStillReplaysAsAFailureNextToThatChange(t *testing.T) {
	fresh := newTestExecSession()
	fresh.engine.pluginStreamRegistry = NewPluginStreamRegistry() // the function is not registered: a stream-level failure
	freshErr, freshOut := streamCallOutput(t, fresh)
	if freshErr == 0 || freshOut == "" || len(fresh.history) != 1 {
		t.Fatalf("PRECONDITION FAILED: the fresh failure gave status %d, output %q, %d event(s)", freshErr, freshOut, len(fresh.history))
	}
	if fresh.history[0].StreamData {
		t.Fatal("the synthetic failure chunk is marked as plugin output, so replay cannot tell it from a success")
	}

	replayed := newTestExecSession()
	replayed.engine.pluginStreamRegistry = NewPluginStreamRegistry()
	replayed.isReplay = true
	replayed.history = []EventRecord{roundTripThroughPayload(t, fresh.history[0])}
	replayErr, replayOut := streamCallOutput(t, replayed)
	if replayErr != freshErr || replayOut != freshOut {
		t.Errorf("a stream failure replayed as status %d %q, fresh gave %d %q", replayErr, replayOut, freshErr, freshOut)
	}
}

// A one-chunk success recorded BEFORE StreamData existed has no marker and is indistinguishable from a failure, so it replays as it always did.
// This pins the era rule rather than hiding it: a record already in event_history is not reinterpreted.
func TestAOneChunkRecordWithNoMarkerReplaysAsItAlwaysDid(t *testing.T) {
	old := EventRecord{
		Step: 0, EventType: EventTypePluginCallStreamChunk,
		PluginName: "test-plugin", PluginFunc: "Echo", PluginInput: `{}`,
		PluginOutput: "only", StreamChunkIndex: 0, StreamFinish: true,
	}
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{roundTripThroughPayload(t, old)}
	if errCode, out := streamCallOutput(t, s); errCode == 0 || out != "only" {
		t.Errorf("an unmarked one-chunk record replayed as status %d %q; a record already stored must keep replaying as the failure it was read as", errCode, out)
	}
}
