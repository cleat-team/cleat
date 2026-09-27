package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

// IMPROVEMENT-PLAN 2.35, plugin half.
//
// Four conditions make a streaming plugin call fail, and every one of them
// reported callErrorUnknown -- non-retryable -- because all four are recorded
// as one synthetic chunk and came back through a single replay site that could
// not tell them apart. Three of the four are conditions the NON-streaming path
// reports as callFailureCode, which is retryable. So the same deployment gap
// or the same plugin error was retryable to a workflow calling a plugin and
// permanently fatal to one streaming from it.
//
// The asymmetry is asserted here against the other path rather than against a
// constant. A test that pins the streaming code to a literal passes just as
// well after someone changes the non-streaming one, which is how the two got
// out of step in the first place.

// callCodeOf runs f and returns the call error code the guest would decode.
func callCodeOf(t *testing.T, result int64) uint32 {
	t.Helper()
	_, callErr, errCode := decodeCallResultGuest(result)
	if errCode != 1 {
		t.Fatalf("expected a failure result (errCode=1), got errCode=%d callErr=%d", errCode, callErr)
	}
	return callErr
}

// The three conditions both paths can hit, run through both, compared to each
// other. sessionFor builds a session already configured for the condition.
func TestAStreamingPluginFailureIsClassifiedLikeTheNonStreamingOne(t *testing.T) {
	boom := fmt.Errorf("upstream refused the connection")

	for _, tc := range []struct {
		name  string
		setUp func(s *execSession)
	}{{
		name: "the function is not registered",
		setUp: func(s *execSession) {
			// Both registries exist and neither has the function.
			s.engine.pluginRegistry = NewPluginRegistry()
			s.engine.pluginStreamRegistry = NewPluginStreamRegistry()
		},
	}, {
		name: "the call guard rejects the caller",
		setUp: func(s *execSession) {
			s.engine.pluginRegistry = NewPluginRegistry()
			s.engine.pluginStreamRegistry = NewPluginStreamRegistry()
			_ = s.engine.pluginRegistry.Register("test-plugin", "Echo",
				func(ctx context.Context, in string) (string, error) { return "{}", nil })
			_ = s.engine.pluginStreamRegistry.Register("test-plugin", "Echo",
				func(ctx context.Context, in string) (<-chan plugin.StreamEvent, error) {
					ch := make(chan plugin.StreamEvent)
					close(ch)
					return ch, nil
				})
			s.callerPluginName = "caller-plugin"
			guard := NewPluginCallGuard()
			// Present in the guard with an empty target set, which is how a
			// caller is denied: absent means unrestricted.
			guard.Allow("caller-plugin", nil)
			s.engine.pluginCallGuard = guard
		},
	}, {
		name: "the plugin function itself returns an error",
		setUp: func(s *execSession) {
			s.engine.pluginRegistry = NewPluginRegistry()
			s.engine.pluginStreamRegistry = NewPluginStreamRegistry()
			_ = s.engine.pluginRegistry.Register("test-plugin", "Echo",
				func(ctx context.Context, in string) (string, error) { return "", boom })
			_ = s.engine.pluginStreamRegistry.Register("test-plugin", "Echo",
				func(ctx context.Context, in string) (<-chan plugin.StreamEvent, error) {
					return nil, boom
				})
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			plain := newTestExecSession()
			tc.setUp(plain)
			plainCode := callCodeOf(t, plain.PluginCall(
				context.Background(), nil, "test-plugin", "Echo", `{}`, 0, 0))

			streaming := newTestExecSession()
			tc.setUp(streaming)
			streamCode := callCodeOf(t, streaming.PluginCallStreaming(
				context.Background(), nil, "test-plugin", "Echo", `{}`, 0, 0))

			if streamCode != plainCode {
				t.Errorf("PluginCallStreaming reported call error %d where PluginCall reported %d "+
					"for the same condition -- so the identical failure is retryable to a workflow "+
					"calling this plugin and not to one streaming from it", streamCode, plainCode)
			}
		})
	}
}

// The determinism half of 2.35, and the reason the code is stored rather than
// re-derived: whatever the fresh run told the guest, the replay of that
// recorded event must tell it the same thing.
func TestAStreamFailureReplaysWithTheCodeItReportedFresh(t *testing.T) {
	fresh := newTestExecSession()
	fresh.engine.pluginStreamRegistry = NewPluginStreamRegistry()
	freshCode := callCodeOf(t, fresh.PluginCallStreaming(
		context.Background(), nil, "test-plugin", "Echo", `{}`, 0, 0))

	if len(fresh.history) != 1 {
		t.Fatalf("the fresh run recorded %d events, want 1", len(fresh.history))
	}

	// Through the real encoder and decoder, not by copying the struct: a code
	// the payload does not carry would replay as callErrorUnknown, and
	// building the history by hand would hide that.
	replayed := newTestExecSession()
	replayed.isReplay = true
	replayed.history = []EventRecord{roundTripThroughPayload(t, fresh.history[0])}
	replayCode := callCodeOf(t, replayed.PluginCallStreaming(
		context.Background(), nil, "test-plugin", "Echo", `{}`, 0, 0))

	if replayCode != freshCode {
		t.Errorf("the same step reported call error %d fresh and %d on replay -- a workflow "+
			"branching on Retryable() takes a different path after a crash than before it",
			freshCode, replayCode)
	}
}

// Events recorded before this change carry no stream_err_code and must keep
// replaying as callErrorUnknown, which is what they reported when they were
// fresh. Determinism is per recorded step, not across eras -- so the fix is
// allowed to change what NEW events report and is not allowed to change what
// old ones replay as.
func TestAStreamFailureRecordedBeforeThisChangeStillReplaysAsUnknown(t *testing.T) {
	// The shape the old code wrote: finished, index 0, no code field.
	old := EventRecord{
		Step:             0,
		EventType:        EventTypePluginCallStreamChunk,
		PluginName:       "test-plugin",
		PluginFunc:       "Echo",
		PluginInput:      `{}`,
		PluginOutput:     "plugin_call_streaming: boom",
		StreamChunkIndex: 0,
		StreamFinish:     true,
	}
	payload, err := eventRecordToPayload(old)
	if err != nil {
		t.Fatalf("eventRecordToPayload: %v", err)
	}
	if strings.Contains(string(payload), "stream_err_code") {
		t.Fatal("a record with no StreamErrCode wrote a stream_err_code key, so every " +
			"stream chunk already in event_history has a changed payload and a broken checksum")
	}

	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{roundTripThroughPayload(t, old)}
	if got := callCodeOf(t, s.PluginCallStreaming(
		context.Background(), nil, "test-plugin", "Echo", `{}`, 0, 0)); got != uint32(callErrorUnknown) {
		t.Errorf("a pre-2.35 stream error replayed as call error %d, want %d (callErrorUnknown) -- "+
			"a workflow already in flight changed its retry behaviour under it",
			got, callErrorUnknown)
	}
}

// The failure the one-function-for-both design exists to prevent: a site that
// records one code and returns another. Asserted on what streamFailure
// actually did, rather than on the shape of the code.
func TestStreamFailureRecordsTheCodeItReturns(t *testing.T) {
	for _, code := range []byte{callErrorUnknown, callFailureCode, callErrorInvalidRequest} {
		s := newTestExecSession()
		returned := callCodeOf(t, s.streamFailure(context.Background(), nil,
			"test-plugin", "Echo", `{}`, "boom", code, 0, 0))
		if len(s.history) != 1 {
			t.Fatalf("code %d: recorded %d events, want 1", code, len(s.history))
		}
		if recorded := byte(s.history[0].StreamErrCode); recorded != code {
			t.Errorf("code %d: recorded %d but returned %d; a fresh run and the replay of it "+
				"would classify the same step differently", code, recorded, returned)
		}
		if returned != uint32(code) {
			t.Errorf("code %d: returned %d", code, returned)
		}
	}
}

// No column carries this either, so the payload is the whole of it. Before the
// change the fresh path had no code to lose; after it, losing the code in the
// store would silently put every replayed failure back to callErrorUnknown --
// which is the shape of 3.96, one section earlier, and the reason this is
// checked against a real database rather than against the encoder alone.
func TestAStreamFailuresCodeSurvivesTheStore(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		wfID := newIntentWorkflow(t, ctx, store, "stream-err-code")

		if err := store.AppendEventHistory(ctx, wfID, EventRecord{
			Step:          0,
			EventType:     EventTypePluginCallStreamChunk,
			PluginName:    "test-plugin",
			PluginFunc:    "Echo",
			PluginInput:   `{}`,
			PluginOutput:  "plugin_call_streaming: boom",
			StreamFinish:  true,
			StreamErrCode: int(callFailureCode),
		}); err != nil {
			t.Fatalf("AppendEventHistory: %v", err)
		}
		hist, err := store.LoadEventHistory(ctx, wfID)
		if err != nil {
			t.Fatalf("LoadEventHistory: %v", err)
		}
		if len(hist) != 1 {
			t.Fatalf("history has %d events, want 1", len(hist))
		}
		if hist[0].StreamErrCode != int(callFailureCode) {
			t.Errorf("StreamErrCode = %d, want %d; every recorded stream failure replays as "+
				"callErrorUnknown once its history comes back from the database",
				hist[0].StreamErrCode, callFailureCode)
		}
	})
}

// The compatibility claim, made directly: a chunk written before this change
// still verifies against the checksum stored for it. The literal was measured
// against the encoder WITHOUT this change, not copied from what the new code
// prints.
func TestAPre235StreamChunkStillMatchesItsStoredChecksum(t *testing.T) {
	rec := EventRecord{
		Step:         4,
		EventType:    EventTypePluginCallStreamChunk,
		PluginName:   "test-plugin",
		PluginFunc:   "Echo",
		PluginOutput: "chunk1",
	}
	const storedBefore = "9fb4a7f590925cab"
	if got := computeEventChecksum(rec, ""); got != storedBefore {
		t.Errorf("checksum of a pre-2.35 stream chunk = %s, want %s -- every stream chunk "+
			"already in event_history now fails verification", got, storedBefore)
	}
}

// And the other direction, which an only-when-set guard can get wrong
// invisibly: setting the code must move the checksum, or the key is not inside
// the payload the checksum covers.
func TestSettingTheStreamErrorCodeChangesTheChecksum(t *testing.T) {
	base := EventRecord{
		Step:         4,
		EventType:    EventTypePluginCallStreamChunk,
		PluginName:   "test-plugin",
		PluginFunc:   "Echo",
		PluginOutput: "plugin_call_streaming: boom",
		StreamFinish: true,
	}
	withCode := base
	withCode.StreamErrCode = int(callFailureCode)
	if a, b := computeEventChecksum(base, ""), computeEventChecksum(withCode, ""); a == b {
		t.Errorf("checksum is %s with and without StreamErrCode set, so the key is not inside "+
			"the payload the checksum covers", a)
	}
}

// Guards the premise of every test above: that a stream-level failure is still
// recorded as exactly one finished chunk, which is what the replay site keys
// on. If recordStreamError stops doing that, these tests should stop claiming
// to cover it rather than quietly testing nothing.
func TestAStreamFailureIsStillRecordedAsOneFinishedChunk(t *testing.T) {
	s := newTestExecSession()
	s.engine.pluginStreamRegistry = NewPluginStreamRegistry()
	_ = s.PluginCallStreaming(context.Background(), nil, "test-plugin", "Echo", `{}`, 0, 0)
	if len(s.history) != 1 {
		t.Fatalf("a stream failure recorded %d events, want 1", len(s.history))
	}
	rec := s.history[0]
	if !rec.StreamFinish || rec.StreamChunkIndex != 0 {
		t.Fatalf("a stream failure recorded index=%d finish=%v, want 0/true; the replay site "+
			"keys on exactly that shape", rec.StreamChunkIndex, rec.StreamFinish)
	}
	if !strings.Contains(rec.PluginOutput, "test-plugin/Echo") {
		t.Fatalf("the recorded message %q does not name the function, so a reader of the "+
			"history cannot tell which call failed", rec.PluginOutput)
	}
}
