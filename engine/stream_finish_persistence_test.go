package engine

import (
	"context"
	"strings"
	"testing"
)

// IMPROVEMENT-PLAN 3.96. A stream-level plugin failure is recorded by
// recordStreamError as a single chunk with StreamFinish set, and
// replayPluginCallStreaming recognises it by exactly that flag:
//
//	if len(collected) == 1 && collected[0].Finish { ...return the error... }
//
// StreamFinish was written by neither eventRecordToPayload nor any column, so
// it survived only in memory. Replay a workflow on a worker that loaded its
// history from the database and the flag is false, the error branch cannot
// fire, and the guest is handed SUCCESS with the error text as chunk content.
//
// engine/host_test.go's TestPluginCallStreamingReplay_StreamError covers the
// same branch and passes either way, because it assigns s.history directly --
// a history the store cannot produce. These tests differ from it in exactly
// one respect, which is the whole point: the history goes through the real
// encoder and decoder first.

// roundTripThroughPayload puts a record through the same encode/decode pair the
// store uses, which is what makes these tests able to fail. Building the
// history by hand tests the in-memory struct and nothing else.
func roundTripThroughPayload(t *testing.T, rec EventRecord) EventRecord {
	t.Helper()
	payload, err := eventRecordToPayload(rec)
	if err != nil {
		t.Fatalf("eventRecordToPayload: %v", err)
	}
	back := EventRecord{Step: rec.Step, EventType: rec.EventType}
	populateFromPayload(&back, payload)
	return back
}

func TestAStreamErrorIsStillAnErrorAfterItsHistoryIsReloaded(t *testing.T) {
	recorded := EventRecord{
		Step:             0,
		EventType:        EventTypePluginCallStreamChunk,
		PluginName:       "test-plugin",
		PluginFunc:       "Echo",
		PluginInput:      `{}`,
		PluginOutput:     "plugin_call_streaming: boom",
		StreamChunkIndex: 0,
		StreamFinish:     true,
	}

	// Vacuity guard: the fresh path really does mark a stream error this way,
	// so this record is the shape recordStreamError produces rather than one
	// invented for the test. If recordStreamError stops setting StreamFinish
	// this test should stop claiming to cover it.
	probe := newTestExecSession()
	probe.recordStreamError("test-plugin", "Echo", `{}`, "plugin_call_streaming: boom")
	if len(probe.history) != 1 {
		t.Fatalf("recordStreamError recorded %d events, want 1", len(probe.history))
	}
	if !probe.history[0].StreamFinish {
		t.Fatal("recordStreamError no longer sets StreamFinish; this test's premise is gone")
	}

	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{roundTripThroughPayload(t, recorded)}

	result := s.PluginCallStreaming(context.Background(), nil, "test-plugin", "Echo", `{}`, 0, 0)
	if errCode := uint32(result & 0xFFFFFFFF); errCode != 1 {
		t.Errorf("a recorded stream error replayed with errCode=%d, want 1 -- "+
			"the guest was told the call SUCCEEDED and handed the error text as content", errCode)
	}
}

func TestAFinishedStreamStillReadsAsFinishedAfterItsHistoryIsReloaded(t *testing.T) {
	back := roundTripThroughPayload(t, EventRecord{
		Step:             1,
		EventType:        EventTypePluginCallStreamChunk,
		PluginName:       "test-plugin",
		PluginFunc:       "Echo",
		PluginOutput:     "chunk2",
		StreamChunkIndex: 1,
		StreamFinish:     true,
	})
	if !back.StreamFinish {
		t.Error("StreamFinish did not survive the payload round trip")
	}
	if back.StreamChunkIndex != 1 {
		t.Errorf("StreamChunkIndex = %d after the round trip, want 1", back.StreamChunkIndex)
	}
}

// The compatibility half, and the reason both new keys are written only when
// set. An event recorded before 3.96 has neither key; it must decode to the
// values it was recorded under, and -- because the checksum is a hash of this
// exact payload -- its bytes must not move at all.
func TestAStreamChunkRecordedBeforeTheFixKeepsItsPayloadAndItsBehaviour(t *testing.T) {
	ordinary := EventRecord{
		Step:         0,
		EventType:    EventTypePluginCallStreamChunk,
		PluginName:   "test-plugin",
		PluginFunc:   "Echo",
		PluginOutput: "chunk1",
		// index 0, not finished: exactly what a first chunk looks like, and
		// what every pre-3.96 chunk decodes to.
	}
	payload, err := eventRecordToPayload(ordinary)
	if err != nil {
		t.Fatalf("eventRecordToPayload: %v", err)
	}
	for _, key := range []string{"stream_finish", "stream_chunk_index"} {
		if strings.Contains(string(payload), key) {
			t.Errorf("payload for an ordinary chunk contains %q: %s\n"+
				"writing either key unconditionally changes the bytes of every "+
				"stream chunk ever written, and the stored checksum is a hash of these bytes",
				key, payload)
		}
	}

	back := EventRecord{Step: 0, EventType: EventTypePluginCallStreamChunk}
	populateFromPayload(&back, payload)
	if back.StreamFinish {
		t.Error("a chunk with no stream_finish key decoded as finished")
	}
}

// The checksum claim above, made directly rather than inferred from the bytes:
// a record that predates 3.96 must still verify against the checksum stored
// for it. Computing it before and after the same encode is the closest this
// package can get without a database, and it is what the compatibility
// argument actually rests on.
func TestAPre396StreamChunkStillMatchesItsStoredChecksum(t *testing.T) {
	rec := EventRecord{
		Step:         4,
		EventType:    EventTypePluginCallStreamChunk,
		PluginName:   "test-plugin",
		PluginFunc:   "Echo",
		PluginOutput: "chunk1",
	}
	// The literal is the checksum this record produced before 3.96 added the
	// two keys, pinned so a later change to the encoder cannot quietly move it.
	const storedBefore = "9fb4a7f590925cab"
	if got := computeEventChecksum(rec, ""); got != storedBefore {
		t.Errorf("checksum of a pre-3.96 stream chunk = %s, want %s -- "+
			"every stream chunk already in event_history now fails verification", got, storedBefore)
	}
}

// The encoder tests above prove the mechanism; this one proves there is no
// column quietly carrying the flag behind it, on every dialect that is
// configured. It is the version of the check that first showed the loss:
// before 3.96 it failed on PostgreSQL and MySQL alike, which is what ruled
// out "the payload drops it but a column saves it".
func TestAStreamErrorsFinishFlagSurvivesTheStore(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		wfID := newIntentWorkflow(t, ctx, store, "stream-finish")

		if err := store.AppendEventHistory(ctx, wfID, EventRecord{
			Step:         0,
			EventType:    EventTypePluginCallStreamChunk,
			PluginName:   "test-plugin",
			PluginFunc:   "Echo",
			PluginInput:  `{}`,
			PluginOutput: "plugin_call_streaming: boom",
			StreamFinish: true,
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
		if !hist[0].StreamFinish {
			t.Error("StreamFinish did not survive the store; a recorded stream error " +
				"is indistinguishable from an ordinary chunk once history is reloaded")
		}
	})
}
