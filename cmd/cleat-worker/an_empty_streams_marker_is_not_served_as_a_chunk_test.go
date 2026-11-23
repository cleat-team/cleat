package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// cleat#4342. A streaming call whose stream produced no chunk records a MARKER row in event_history (engine.IsEmptyStreamMarker): a plugin_call_stream_chunk with the data
// flag and a non-zero error code. It is a row, and no chunk the plugin produced, so the SSE reader must not hand it to a client: a client would see a phantom empty chunk that
// opens an answer (index 0, stream_start true) and never finishes. Both read paths serve rows from event_history, so both are tested: the history read when a reader attaches,
// and the durable tail a reader on another worker follows.

// markerRec is the row an empty stream records.
func markerRec(step int) engine.EventRecord {
	return engine.EventRecord{
		Step:          step,
		EventType:     engine.EventTypePluginCallStreamChunk,
		PluginName:    "llm",
		PluginFunc:    "chat_stream",
		StreamData:    true,
		StreamErrCode: 2,
	}
}

func TestTheMarkerRowThisFileUsesIsTheEngineMarker(t *testing.T) {
	// The precondition for everything below: if the fixture row stopped being a marker, every test here would pass without testing anything.
	if !engine.IsEmptyStreamMarker(markerRec(0)) {
		t.Fatal("PRECONDITION FAILED: markerRec is not what engine.IsEmptyStreamMarker calls a marker")
	}
	if engine.IsEmptyStreamMarker(chunkRec(0, 0, "x", true)) {
		t.Fatal("PRECONDITION FAILED: an ordinary chunk reads as a marker")
	}
}

// The history read: a marker between two chunks is not served, and the chunks around it are, in order, with the segmentation intact (no gap reported for the marker).
func TestTheHistoryReadDoesNotServeAnEmptyStreamsMarker(t *testing.T) {
	history := []engine.EventRecord{
		markerRec(0),
		chunkRec(1, 0, "Hel", false),
		markerRec(2),
		chunkRec(3, 1, "lo", true),
	}
	f := newStreamFixture(t, runningRun(), history)
	rec, cancel, done := f.start(t, "/api/workflows/wf-1/stream", nil)
	defer func() { cancel(); <-done }()

	evs := collectEvents(rec, 3, 5*time.Second) // attached + the two real chunks
	chunks := chunkEvents(evs)
	if len(chunks) != 2 {
		t.Fatalf("the history read served %d chunks, want 2: the marker rows (steps 0 and 2) are not chunks.\n\nbody:\n%s", len(chunks), rec.body())
	}
	for _, c := range chunks {
		if step, _ := c.data["step"].(float64); step == 0 || step == 2 {
			t.Errorf("a chunk event carries step %v, which is a marker row", step)
		}
	}
	if chunks[0].data["content"] != "Hel" || chunks[1].data["content"] != "lo" {
		t.Errorf("chunks were %q then %q, want \"Hel\" then \"lo\"", chunks[0].data["content"], chunks[1].data["content"])
	}
	if _, ok := findEvent(evs, "gap"); ok {
		t.Errorf("a gap was reported around a marker: it is not part of any answer, so it must not move the segmentation.\n\nbody:\n%s", rec.body())
	}
}

// The durable tail: a marker that lands in event_history is not served either.
func TestTheDurableTailDoesNotServeAnEmptyStreamsMarker(t *testing.T) {
	f, g := newPollFixture(t, otherWorkersRun(), nil)
	rec, cancel, done := f.start(t, "/api/workflows/wf-1/stream", nil)
	defer func() { cancel(); <-done }()
	waitForEvents(t, rec, 1)

	g.append(markerRec(0), chunkRec(1, 0, "x", true))
	evs := collectEvents(rec, 2, 5*time.Second) // attached + the one real chunk
	chunks := chunkEvents(evs)
	if len(chunks) != 1 || chunks[0].data["content"] != "x" {
		t.Fatalf("the durable tail served %d chunks, want exactly the one real chunk.\n\nbody:\n%s", len(chunks), rec.body())
	}
}

// The tail reads rows AFTER the cursor, so a marker it skips must still move the cursor, or it is read again on every poll and a page of them reads as "more behind it" and
// spins. Asserted on the cursor itself, which is exact, and not on how many polls happened in a window, which is not.
func TestTheDurableTailMovesItsCursorPastAnEmptyStreamsMarker(t *testing.T) {
	f, g := newPollFixture(t, otherWorkersRun(), nil)
	var mu sync.Mutex
	var seen []int
	f.ms.loadStreamChunksAfterFn = func(ctx context.Context, id string, afterStep, limit int) ([]engine.EventRecord, error) {
		mu.Lock()
		seen = append(seen, afterStep)
		mu.Unlock()
		return g.after(ctx, id, afterStep, limit)
	}
	rec, cancel, done := f.start(t, "/api/workflows/wf-1/stream", nil)
	defer func() { cancel(); <-done }()
	waitForEvents(t, rec, 1)

	const markerStep = 7
	g.append(markerRec(markerStep))
	// Wait until the tail has been asked at least twice AFTER the marker was appended: the first returns the marker, the next must ask past it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(seen)
		past := 0
		for _, a := range seen {
			if a >= markerStep {
				past++
			}
		}
		mu.Unlock()
		if past >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the tail never asked for rows after the marker's step %d (asked after %v, %d times): its cursor did not move past a marker it skipped, so it re-reads the marker on every poll", markerStep, seen, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := chunkEvents(parseSSE(rec.body())); len(got) != 0 {
		t.Errorf("a marker was served as %d chunk event(s)", len(got))
	}
}
