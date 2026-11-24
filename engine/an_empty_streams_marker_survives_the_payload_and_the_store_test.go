package engine

import (
	"context"
	"testing"
)

// cleat#4342. The empty-stream marker is told by two persisted fields (StreamData and a non-zero StreamErrCode), and every history load and the SSE tail rebuild a record
// from the stored payload through populateFromPayload. If that stopped restoring one of them, the marker would read back as a plain chunk or as nothing, and #4342 (a replay
// serving the next stream's chunks to an empty call, and running that call again) would be back in production while every test that builds its history in memory stayed green
// (cleat-review3: a mutant that stopped restoring stream_data left 244 tests green). So the marker is written through the real encoder and decoder, and through each store.

func TestAnEmptyStreamsMarkerSurvivesThePayloadRoundTrip(t *testing.T) {
	marker := emptyStreamMarkerRecord(3, "test-plugin", "Echo", `{}`)
	if !IsEmptyStreamMarker(marker) {
		t.Fatal("PRECONDITION FAILED: the record emptyStreamMarkerRecord builds is not a marker")
	}
	if back := roundTripThroughPayload(t, marker); !IsEmptyStreamMarker(back) {
		t.Errorf("the marker did not survive the payload encoder and decoder: StreamData=%v StreamErrCode=%d", back.StreamData, back.StreamErrCode)
	}
	// The control: what the round trip must NOT turn into a marker.
	for name, rec := range map[string]EventRecord{
		"a chunk the plugin produced": {Step: 0, EventType: EventTypePluginCallStreamChunk, PluginName: "p", PluginFunc: "f", PluginOutput: "x", StreamData: true},
		"a synthetic failure chunk":   streamErrorRecord(0, "p", "f", `{}`, "boom", callFailureCode),
	} {
		if back := roundTripThroughPayload(t, rec); IsEmptyStreamMarker(back) {
			t.Errorf("%s read back as an empty-stream marker", name)
		}
	}
}

func TestAnEmptyStreamsMarkerSurvivesTheStore(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		wfID := newIntentWorkflow(t, ctx, store, "stream-marker")

		if err := store.AppendEventHistory(ctx, wfID, emptyStreamMarkerRecord(0, "test-plugin", "Echo", `{}`)); err != nil {
			t.Fatalf("AppendEventHistory: %v", err)
		}
		hist, err := store.LoadEventHistory(ctx, wfID)
		if err != nil {
			t.Fatalf("LoadEventHistory: %v", err)
		}
		if len(hist) != 1 || !IsEmptyStreamMarker(hist[0]) {
			t.Fatalf("the marker did not read back as a marker from LoadEventHistory: %+v", hist)
		}

		// The SSE tail reads the same row through its own loader and the same decoder.
		if tail, ok := store.(StreamChunkTailReader); ok {
			rows, err := tail.LoadStreamChunksAfter(ctx, wfID, -1, 10)
			if err != nil {
				t.Fatalf("LoadStreamChunksAfter: %v", err)
			}
			if len(rows) != 1 || !IsEmptyStreamMarker(rows[0]) {
				t.Errorf("the marker did not read back as a marker from LoadStreamChunksAfter: %+v", rows)
			}
		}
	})
}
