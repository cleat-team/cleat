package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// GET /api/workflows/{id}/stream. cleat#1572.
//
// The capability: a workflow streaming model tokens can be followed by a
// browser as it produces them. The property that makes it worth having rather
// than merely possible: a client that loses its connection can reconnect and
// be served exactly what it missed, from event_history, because every chunk is
// persisted as it arrives.
//
// THE ASSERTION THAT SEPARATES THIS FROM A POLLING LOOP is
// TestSubscribingHappensBeforeTheHistoryReadSoNoChunkFallsBetween. Everything
// else here is satisfied by a handler that reads history and returns; only that
// one fails when the two sources are joined in the wrong order.
//
// THE FIXTURES BELOW ENCODE A MEASURED SHAPE, NOT AN ASSUMED ONE, and the
// distinction cost a rewrite. The first version of this file gave every chunk
// of one streaming call the same step, because that is what "a step is a
// durable call" suggests. It is false: recordEvent increments stepCount for
// every event, so each CHUNK has its own step, and StreamChunkIndex is what
// restarts per call. Every test here passed against the wrong fixture -- they
// were measuring the belief that produced them. engine's
// TestEachChunkIsItsOwnStepAndTheIndexRestartsPerCall drives the real chunk
// loop and pins the shape, so this file cannot quietly drift back.

// sseRecorder is an http.ResponseWriter that a test can read while the handler
// is still writing to it. httptest.ResponseRecorder is not safe for that, and
// every test here reads a stream that has not ended.
type sseRecorder struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	hdr  http.Header
	code int
}

func newSSERecorder() *sseRecorder {
	return &sseRecorder{hdr: make(http.Header)}
}

func (r *sseRecorder) Header() http.Header { return r.hdr }

func (r *sseRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(p)
}

func (r *sseRecorder) WriteHeader(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.code = code
}

func (r *sseRecorder) Flush() {}

func (r *sseRecorder) body() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

func (r *sseRecorder) status() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.code
}

// sseEvent is one parsed `event:`/`data:`/`id:` block.
type sseEvent struct {
	name string
	id   string
	data map[string]any
}

// parseSSE reads whole events out of a body that may end mid-event, which is
// the normal state of a stream still being written.
func parseSSE(body string) []sseEvent {
	var out []sseEvent
	for _, block := range strings.Split(body, "\n\n") {
		if !strings.Contains(block, "data: ") {
			continue // a comment (keepalive), or a partial trailing block
		}
		var ev sseEvent
		complete := false
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "id: "):
				ev.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				raw := strings.TrimPrefix(line, "data: ")
				if err := json.Unmarshal([]byte(raw), &ev.data); err != nil {
					// A partial trailing line. Not an error: the stream is live.
					continue
				}
				complete = true
			}
		}
		if complete {
			out = append(out, ev)
		}
	}
	return out
}

// collectEvents waits for at least n events and returns whatever it has when
// the wait runs out, WITHOUT failing.
//
// The distinction from waitForEvents matters for diagnosis rather than for
// correctness. A test whose subject is "how many chunks arrive" should fail on
// its own count assertion, naming the number and what it means; if the wait
// itself fails first, every such defect reports the same "timed out" and the
// reader has to reconstruct which chunk went missing from a dump of the body.
// Falsifying these tests is what showed it: two of seven mutations went red
// through the wait instead of through the assertion written for them.
func collectEvents(rec *sseRecorder, n int, wait time.Duration) []sseEvent {
	deadline := time.Now().Add(wait)
	for {
		evs := parseSSE(rec.body())
		if len(evs) >= n || time.Now().After(deadline) {
			return evs
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitForEvents is collectEvents for the cases where not reaching n is itself
// the failure -- the handler produced nothing, or stopped early.
func waitForEvents(t *testing.T, rec *sseRecorder, n int) []sseEvent {
	t.Helper()
	evs := collectEvents(rec, n, 5*time.Second)
	if len(evs) < n {
		t.Fatalf("wanted %d SSE events, saw %d after 5s.\n\nbody:\n%s",
			n, len(evs), rec.body())
	}
	return evs
}

func chunkEvents(evs []sseEvent) []sseEvent {
	var out []sseEvent
	for _, e := range evs {
		if e.name == "chunk" {
			out = append(out, e)
		}
	}
	return out
}

func findEvent(evs []sseEvent, name string) (sseEvent, bool) {
	for _, e := range evs {
		if e.name == name {
			return e, true
		}
	}
	return sseEvent{}, false
}

// chunkRec builds the event_history row the streaming chunk loop writes.
//
// step and index are SEPARATE and move independently: step is the event's
// unique position in the run, index is its position within one answer. Callers
// here pass steps that are distinct per chunk and indices that restart at 0 per
// streaming call, which is what the engine actually records.
func chunkRec(step, index int, content string, finish bool) engine.EventRecord {
	return engine.EventRecord{
		Step:             step,
		EventType:        engine.EventTypePluginCallStreamChunk,
		PluginName:       "llm",
		PluginFunc:       "chat_stream",
		PluginOutput:     content,
		StreamChunkIndex: index,
		StreamFinish:     finish,
	}
}

// streamFixture wires an apiServer over a mockStore and a real hub.
type streamFixture struct {
	api *apiServer
	ms  *mockStore
	hub *engine.StreamHub
}

func newStreamFixture(t *testing.T, wf *engine.WorkflowInstance, history []engine.EventRecord) *streamFixture {
	t.Helper()
	ms := &mockStore{
		getWorkflowByIDFn: func(ctx context.Context, id string) (*engine.WorkflowInstance, error) {
			return wf, nil
		},
		loadEventHistoryFn: func(ctx context.Context, id string) ([]engine.EventRecord, error) {
			return history, nil
		},
	}
	hub := engine.NewStreamHub(0)
	return &streamFixture{
		api: &apiServer{store: ms, worker: newTestWorker(ms), streamHub: hub},
		ms:  ms,
		hub: hub,
	}
}

// start runs the handler in the background and returns the recorder plus a
// cancel that ends the request the way a disconnecting client would.
func (f *streamFixture) start(t *testing.T, target string, headers map[string]string) (*sseRecorder, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest(http.MethodGet, target, nil).WithContext(ctx)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	rec := newSSERecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.api.handleStreamWorkflow(rec, r, "wf-1")
	}()
	return rec, cancel, done
}

func runningRun() *engine.WorkflowInstance {
	return &engine.WorkflowInstance{
		ID: "wf-1", Status: "running", AssignedTo: "test-worker", Generation: 3,
	}
}

// ---------------------------------------------------------------------------

// The capability itself: tokens reach a client while the run is still running.
func TestALiveReaderReceivesTokensBeforeTheRunFinishes(t *testing.T) {
	f := newStreamFixture(t, runningRun(), nil)
	rec, cancel, done := f.start(t, "/api/workflows/wf-1/stream", nil)
	defer func() { cancel(); <-done }()

	// Wait for the handler to attach before publishing, or the test races the
	// subscription and measures nothing.
	waitForEvents(t, rec, 1)

	for i, tok := range []string{"Hel", "lo ", "world"} {
		f.hub.Publish("wf-1", engine.LiveChunk{
			Step: i, Index: i, Content: tok, Finish: i == 2,
		})
	}

	evs := waitForEvents(t, rec, 4) // attached + 3 chunks
	if got := rec.status(); got != 200 {
		t.Errorf("status %d, want 200", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type %q, want text/event-stream", ct)
	}

	attached, ok := findEvent(evs, "attached")
	if !ok {
		t.Fatalf("no `attached` event; got %v", evs)
	}
	if attached.data["live"] != true {
		t.Errorf("attached says live=%v, but the run is assigned to this worker",
			attached.data["live"])
	}

	chunks := chunkEvents(evs)
	if len(chunks) != 3 {
		t.Fatalf("got %d chunks, want 3: %v", len(chunks), evs)
	}
	var text strings.Builder
	for i, c := range chunks {
		text.WriteString(c.data["content"].(string))
		// The id IS the cursor. A client reconnecting sends it back verbatim
		// as Last-Event-ID, so a wrong id is a silently wrong resume point.
		if want := fmt.Sprintf("3:%d", i); c.id != want {
			t.Errorf("chunk %d has id %q, want %q (generation:step)", i, c.id, want)
		}
		if c.data["replay"] != false {
			t.Errorf("chunk %d is marked replay=%v, but it arrived live", i, c.data["replay"])
		}
	}
	if text.String() != "Hello world" {
		t.Errorf("assembled %q, want %q", text.String(), "Hello world")
	}
	// Only the first chunk opens an answer.
	if chunks[0].data["stream_start"] != true {
		t.Error("the index-0 chunk is not marked stream_start, so a client cannot tell " +
			"where one answer begins")
	}
	for i := 1; i < 3; i++ {
		if chunks[i].data["stream_start"] != false {
			t.Errorf("chunk %d is marked stream_start, but it continues the same answer", i)
		}
	}
}

// The resilience claim. A client that dropped its connection is served exactly
// what it missed, from the durable record -- not from the beginning, and not
// nothing.
func TestAReconnectingReaderIsServedOnlyWhatItMissed(t *testing.T) {
	// One streaming call: five chunks at five consecutive steps, indices 0..4.
	history := []engine.EventRecord{
		chunkRec(10, 0, "alpha ", false),
		chunkRec(11, 1, "beta ", false),
		chunkRec(12, 2, "gamma ", false),
		chunkRec(13, 3, "delta ", false),
		chunkRec(14, 4, "epsilon", true),
	}
	f := newStreamFixture(t, runningRun(), history)

	// Last-Event-ID is what a browser's EventSource sends by itself.
	rec, cancel, done := f.start(t, "/api/workflows/wf-1/stream",
		map[string]string{"Last-Event-ID": "3:12"})
	defer func() { cancel(); <-done }()

	evs := collectEvents(rec, 3, 5*time.Second) // attached + 2 chunks
	chunks := chunkEvents(evs)
	if len(chunks) != 2 {
		t.Fatalf("a reader resuming from step 12 received %d chunks, want 2 "+
			"(steps 13 and 14).\n\nReceiving 5 means the cursor was ignored and the "+
			"client's transcript now repeats everything it already displayed; receiving "+
			"0 means it was never told the rest.\n\nevents: %v", len(chunks), evs)
	}
	if got := chunks[0].data["content"]; got != "delta " {
		t.Errorf("first chunk after resume is %q, want %q", got, "delta ")
	}
	if got := chunks[1].data["content"]; got != "epsilon" {
		t.Errorf("second chunk after resume is %q, want %q", got, "epsilon")
	}
	for _, c := range chunks {
		if c.data["replay"] != true {
			t.Errorf("a chunk served from event_history is marked replay=%v; a client "+
				"cannot tell a re-sent token from a new one", c.data["replay"])
		}
	}
	// And NOT reported as the start of a new answer: these continue one.
	if chunks[0].data["stream_start"] != false {
		t.Error("a mid-answer chunk is marked stream_start after a resume, which would " +
			"make the client discard the text it already has")
	}
	if attached, ok := findEvent(evs, "attached"); ok && attached.data["resumed"] != true {
		t.Errorf("attached says resumed=%v for a request carrying Last-Event-ID",
			attached.data["resumed"])
	}
	if _, ok := findEvent(evs, "gap"); ok {
		t.Error("a `gap` was reported for a contiguous resume -- the gap check must " +
			"measure against the last chunk that EXISTS, not the last one sent")
	}
}

// ?durable_only=true refuses to show a token that is not yet in
// event_history. cleat#1572.
//
// BOTH HALVES, because either alone is satisfied by a bug. A handler that
// always filters passes the first; one that never filters passes the second.
func TestDurableOnlyShowsOnlyWhatIsAlreadyInHistory(t *testing.T) {
	for _, tc := range []struct {
		name       string
		query      string
		wantChunks int
	}{
		{"durable_only skips a chunk that is not yet persisted", "?durable_only=true", 1},
		{"by default the same chunk is shown", "", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStreamFixture(t, runningRun(), nil)
			rec, cancel, done := f.start(t, "/api/workflows/wf-1/stream"+tc.query, nil)
			defer func() { cancel(); <-done }()

			waitForEvents(t, rec, 1)
			f.hub.Publish("wf-1", engine.LiveChunk{
				Step: 0, Index: 0, Content: "persisted", Durable: true})
			f.hub.Publish("wf-1", engine.LiveChunk{
				Step: 1, Index: 1, Content: "not yet", Durable: false, Finish: true})

			evs := collectEvents(rec, 1+tc.wantChunks, 2*time.Second)
			chunks := chunkEvents(evs)
			if len(chunks) != tc.wantChunks {
				t.Fatalf("got %d chunks, want %d.\n\nevents: %v", len(chunks), tc.wantChunks, evs)
			}
			if chunks[0].data["durable"] != true {
				t.Errorf("the persisted chunk is marked durable=%v", chunks[0].data["durable"])
			}
			if tc.wantChunks == 2 && chunks[1].data["durable"] != false {
				t.Errorf("the unpersisted chunk is marked durable=%v -- a client that "+
					"wants to render only settled tokens cannot tell", chunks[1].data["durable"])
			}
		})
	}
}

// Anything served from event_history is durable by construction: it was just
// read from there. A client filtering on the flag must not lose replayed
// chunks.
func TestChunksServedFromHistoryAreMarkedDurable(t *testing.T) {
	f := newStreamFixture(t, runningRun(), []engine.EventRecord{
		chunkRec(10, 0, "one", false),
		chunkRec(11, 1, "two", true),
	})
	rec, cancel, done := f.start(t, "/api/workflows/wf-1/stream?durable_only=true", nil)
	defer func() { cancel(); <-done }()

	evs := collectEvents(rec, 3, 5*time.Second)
	chunks := chunkEvents(evs)
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks from history under durable_only, want 2: %v", len(chunks), evs)
	}
	for i, c := range chunks {
		if c.data["durable"] != true {
			t.Errorf("chunk %d came from event_history and is marked durable=%v", i, c.data["durable"])
		}
	}
	if attached, ok := findEvent(evs, "attached"); ok && attached.data["durable_only"] != true {
		t.Errorf("attached says durable_only=%v", attached.data["durable_only"])
	}
}

// A hole that opens AFTER the cursor is still reported. cleat#1572.
//
// WRITTEN BECAUSE A FALSIFICATION CAME BACK GREEN. Removing the line that
// advances the gap baseline over chunks the cursor suppresses changed nothing,
// which has two readings -- the line is dead, or the case is unwritten. It is
// the second, and this is the case.
//
// A resuming reader is sent nothing for the chunks it already has, so the
// baseline for "what index did I expect next" cannot come from what was SENT.
// It has to come from what EXISTS. Otherwise the first chunk a resumed reader
// is owed has no predecessor to be measured against, and a genuine compaction
// hole immediately after the cursor is served as though the transcript were
// continuous -- which is the exact failure the gap event exists to prevent,
// arriving through the one path that looks like it does not need it.
func TestAHoleAfterTheResumePointIsStillReported(t *testing.T) {
	f := newStreamFixture(t, runningRun(), []engine.EventRecord{
		chunkRec(10, 0, "one", false),
		chunkRec(11, 1, "two", false),
		chunkRec(12, 2, "three", false),
		// The reader resumes here. Indices 3..6 were compacted away.
		chunkRec(30, 7, "eight", true),
	})
	rec, cancel, done := f.start(t, "/api/workflows/wf-1/stream",
		map[string]string{"Last-Event-ID": "3:12"})
	defer func() { cancel(); <-done }()

	evs := collectEvents(rec, 3, 5*time.Second) // attached + gap + chunk
	chunks := chunkEvents(evs)
	if len(chunks) != 1 {
		t.Fatalf("UNMEASURED: got %d chunks, want 1 -- the resume itself is wrong, so "+
			"nothing below is about gap reporting.\n\nevents: %v", len(chunks), evs)
	}
	gap, ok := findEvent(evs, "gap")
	if !ok {
		t.Fatalf("a resumed reader was served index 7 straight after index 2 with no "+
			"`gap`.\n\nThe baseline for the gap check must be the last chunk that EXISTS "+
			"in history, not the last one this reader was sent -- a resuming reader is "+
			"sent none of them.\n\nevents: %v", evs)
	}
	if gap.data["from"] != float64(3) || gap.data["to"] != float64(6) {
		t.Errorf("gap reports from=%v to=%v, want 3..6", gap.data["from"], gap.data["to"])
	}
}

// A run's NEXT answer is marked as a new one. The index restarts at 0 per
// streaming call, and a client that cannot see the boundary renders two
// answers as one paragraph.
func TestASecondAnswerIsMarkedAsANewStream(t *testing.T) {
	f := newStreamFixture(t, runningRun(), []engine.EventRecord{
		chunkRec(10, 0, "first answer", true),
	})
	rec, cancel, done := f.start(t, "/api/workflows/wf-1/stream", nil)
	defer func() { cancel(); <-done }()

	waitForEvents(t, rec, 2) // attached + the history chunk

	// A second streaming call: a new step, and the index back to 0.
	f.hub.Publish("wf-1", engine.LiveChunk{Step: 17, Index: 0, Content: "second answer", Finish: true})

	evs := collectEvents(rec, 3, 5*time.Second)
	chunks := chunkEvents(evs)
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2: %v", len(chunks), evs)
	}
	if chunks[1].data["stream_start"] != true {
		t.Error("the second answer's opening chunk is not marked stream_start")
	}
	if _, ok := findEvent(evs, "gap"); ok {
		t.Error("a `gap` was reported at an answer boundary. The index RESTARTS at 0 " +
			"for each streaming call, so index 0 after index 0 is the normal case and " +
			"must not read as missing rows.")
	}
}

// THE ORDERING ASSERTION. A chunk published while the history read is in
// flight is in neither source unless the subscription is opened FIRST.
//
// This is the only test here that fails when history and the live tail are
// joined in the wrong order, and the defect it catches is invisible: the
// client's cursor advances straight over the missing token, so the transcript
// is short by one word and internally consistent.
func TestSubscribingHappensBeforeTheHistoryReadSoNoChunkFallsBetween(t *testing.T) {
	wf := runningRun()
	hub := engine.NewStreamHub(0)

	reading := make(chan struct{})
	ms := &mockStore{
		getWorkflowByIDFn: func(ctx context.Context, id string) (*engine.WorkflowInstance, error) {
			return wf, nil
		},
		loadEventHistoryFn: func(ctx context.Context, id string) ([]engine.EventRecord, error) {
			// Mid-read: the window between "the rows were selected" and "the
			// live tail is attached". A chunk published now is past the rows
			// below and before any subscription opened after this returns.
			close(reading)
			hub.Publish("wf-1", engine.LiveChunk{Step: 11, Index: 1, Content: "second", Finish: true})
			// Give a wrong-ordered handler every chance to lose it.
			time.Sleep(50 * time.Millisecond)
			return []engine.EventRecord{chunkRec(10, 0, "first", false)}, nil
		},
	}
	api := &apiServer{store: ms, worker: newTestWorker(ms), streamHub: hub}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest(http.MethodGet, "/api/workflows/wf-1/stream", nil).WithContext(ctx)
	rec := newSSERecorder()
	done := make(chan struct{})
	go func() { defer close(done); api.handleStreamWorkflow(rec, r, "wf-1") }()

	<-reading
	evs := collectEvents(rec, 3, 5*time.Second) // attached + 2 chunks
	cancel()
	<-done

	chunks := chunkEvents(evs)
	if len(chunks) != 2 {
		t.Fatalf("a chunk published DURING the history read was lost: got %d chunks, want 2.\n\n"+
			"The handler must subscribe to the live tail BEFORE reading history. Reading "+
			"first leaves a window in which a chunk is past the selected rows and before "+
			"the subscription exists, and nothing downstream can detect it -- the client's "+
			"cursor advances over the hole.\n\nevents: %v", len(chunks), evs)
	}
	if chunks[0].data["content"] != "first" || chunks[1].data["content"] != "second" {
		t.Errorf("got %q then %q, want \"first\" then \"second\"",
			chunks[0].data["content"], chunks[1].data["content"])
	}
}

// The other half of subscribing first: the overlap it creates is REMOVED. A
// chunk that is both in history and in the live tail must be sent once.
func TestAChunkInBothSourcesIsSentOnce(t *testing.T) {
	f := newStreamFixture(t, runningRun(), []engine.EventRecord{
		chunkRec(10, 0, "one", false),
		chunkRec(11, 1, "two", false),
	})
	rec, cancel, done := f.start(t, "/api/workflows/wf-1/stream", nil)
	defer func() { cancel(); <-done }()

	waitForEvents(t, rec, 3) // attached + the two from history

	// Republish what history already served, then something genuinely new.
	f.hub.Publish("wf-1", engine.LiveChunk{Step: 10, Index: 0, Content: "one"})
	f.hub.Publish("wf-1", engine.LiveChunk{Step: 11, Index: 1, Content: "two"})
	f.hub.Publish("wf-1", engine.LiveChunk{Step: 12, Index: 2, Content: "three", Finish: true})

	evs := collectEvents(rec, 4, 5*time.Second) // attached + 3 chunks
	chunks := chunkEvents(evs)
	if len(chunks) != 3 {
		t.Fatalf("got %d chunks, want 3 -- a chunk present in both history and the "+
			"live tail was sent twice, so the client renders it twice.\n\nevents: %v",
			len(chunks), evs)
	}
	var got []string
	for _, c := range chunks {
		got = append(got, c.data["content"].(string))
	}
	if strings.Join(got, ",") != "one,two,three" {
		t.Errorf("got %v, want [one two three]", got)
	}
}

// Missing rows are reported, not papered over. History compaction folds old
// events into a blob and deletes the rows; a client handed 0,1,5 as though it
// were contiguous renders a transcript with a hole that reads as prose.
func TestAGapInHistoryIsReportedRatherThanPaperedOver(t *testing.T) {
	f := newStreamFixture(t, runningRun(), []engine.EventRecord{
		chunkRec(10, 0, "one", false),
		chunkRec(11, 1, "two", false),
		// Indices 2..4 were compacted away; this is the same answer resuming
		// at 5, which is what a hole in event_history looks like from here.
		chunkRec(15, 5, "six", true),
	})
	rec, cancel, done := f.start(t, "/api/workflows/wf-1/stream", nil)
	defer func() { cancel(); <-done }()

	evs := collectEvents(rec, 5, 5*time.Second) // attached + 2 chunks + gap + chunk
	gap, ok := findEvent(evs, "gap")
	if !ok {
		t.Fatalf("history jumped from index 1 to index 5 and no `gap` event was sent.\n\n"+
			"events: %v", evs)
	}
	if gap.data["from"] != float64(2) || gap.data["to"] != float64(4) {
		t.Errorf("gap reports from=%v to=%v, want 2..4", gap.data["from"], gap.data["to"])
	}
	if n := len(chunkEvents(evs)); n != 3 {
		t.Errorf("got %d chunks, want 3 -- the gap must be reported ALONGSIDE the "+
			"chunks that survive, not instead of them", n)
	}
}

// Another tenant's run is not streamable, and -- the part a 404 assertion alone
// does not cover -- no subscription is opened on the way to finding that out.
func TestAnotherTenantsRunIsNotStreamableAndIsNotSubscribedTo(t *testing.T) {
	ms := &mockStore{
		// What a tenant-scoped store answers for a run belonging to someone
		// else: not an error, just nothing.
		getWorkflowByIDFn: func(ctx context.Context, id string) (*engine.WorkflowInstance, error) {
			return nil, nil
		},
		loadEventHistoryFn: func(ctx context.Context, id string) ([]engine.EventRecord, error) {
			t.Error("history was read for a run the caller cannot see")
			return nil, nil
		},
	}
	hub := engine.NewStreamHub(0)
	api := &apiServer{store: ms, worker: newTestWorker(ms), streamHub: hub}

	rec := httptest.NewRecorder()
	api.handleStreamWorkflow(rec, httptest.NewRequest(http.MethodGet, "/api/workflows/wf-1/stream", nil), "wf-1")

	if rec.Code != 404 {
		t.Errorf("status %d, want 404 for a run the caller cannot see", rec.Code)
	}
	if got := hub.Readers(); got != 0 {
		t.Errorf("%d subscription(s) were opened for a run the caller cannot see.\n\n"+
			"The hub is keyed by workflow id and enforces nothing, so authorization has "+
			"to happen BEFORE Subscribe -- otherwise another tenant's tokens are "+
			"published to this reader for as long as the check takes.", got)
	}
}

// Over the ceiling the route refuses in a way a client can act on, rather than
// accepting a connection the worker cannot afford.
func TestOverTheReaderCeilingTheRouteRefuses(t *testing.T) {
	wf := runningRun()
	ms := &mockStore{
		getWorkflowByIDFn: func(ctx context.Context, id string) (*engine.WorkflowInstance, error) {
			return wf, nil
		},
	}
	hub := engine.NewStreamHub(1)
	if _, err := hub.Subscribe("someone-else"); err != nil {
		t.Fatalf("UNMEASURED: could not fill the single slot: %v", err)
	}
	api := &apiServer{store: ms, worker: newTestWorker(ms), streamHub: hub}

	rec := httptest.NewRecorder()
	api.handleStreamWorkflow(rec, httptest.NewRequest(http.MethodGet, "/api/workflows/wf-1/stream", nil), "wf-1")

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503 when the worker is at its reader ceiling", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("a 503 for a temporary ceiling carries no Retry-After, so a client " +
			"cannot tell it from a permanent refusal")
	}
}

// A run that has already finished is served its history and closed, rather
// than holding a connection open on a stream nothing will write to again.
func TestATerminalRunIsServedItsHistoryAndClosed(t *testing.T) {
	wf := &engine.WorkflowInstance{ID: "wf-1", Status: "done", Generation: 3}
	f := newStreamFixture(t, wf, []engine.EventRecord{
		chunkRec(10, 0, "all ", false),
		chunkRec(11, 1, "done", true),
	})
	rec, cancel, done := f.start(t, "/api/workflows/wf-1/stream", nil)
	defer cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler held the connection open on a run that is already done")
	}

	evs := parseSSE(rec.body())
	if n := len(chunkEvents(evs)); n != 2 {
		t.Errorf("got %d chunks, want 2 from a completed run's history", n)
	}
	end, ok := findEvent(evs, "end")
	if !ok {
		t.Fatalf("no `end` event for a completed run; got %v", evs)
	}
	if msg, _ := end.data["reason"].(string); !strings.Contains(msg, "done") {
		t.Errorf("end reason %q does not say the run is done", msg)
	}
}

// A client that reconnects having already seen everything is owed no chunks,
// and STAYS ATTACHED, because a running agent may answer again.
//
// This replaces a test that asserted the opposite. That one was correct while
// the route filtered to a single streaming call -- a finished call can produce
// nothing more, so holding the connection was pointless. Once the filter went
// (a step identifies a chunk, never a call, so filtering on one selected a
// single token) the same behaviour became a bug: it would hang up on a client
// midway through an agent run, right after its first answer.
func TestAReaderThatAlreadySawEverythingWaitsForTheNextAnswer(t *testing.T) {
	f := newStreamFixture(t, runningRun(), []engine.EventRecord{
		chunkRec(10, 0, "one", false),
		chunkRec(11, 1, "two", false),
		chunkRec(12, 2, "three", true),
	})
	rec, cancel, done := f.start(t, "/api/workflows/wf-1/stream",
		map[string]string{"Last-Event-ID": "3:12"})
	defer func() { cancel(); <-done }()

	evs := collectEvents(rec, 2, 500*time.Millisecond)
	if n := len(chunkEvents(evs)); n != 0 {
		t.Errorf("got %d chunks for a reader resuming past the last chunk, want 0", n)
	}
	if _, ok := findEvent(evs, "end"); ok {
		t.Error("the stream ended for a reader attached to a RUNNING agent. The run can " +
			"still make another streaming call, and hanging up here drops the client " +
			"between one answer and the next.")
	}

	// And it is genuinely attached: the next answer reaches it.
	f.hub.Publish("wf-1", engine.LiveChunk{Step: 20, Index: 0, Content: "next answer", Finish: true})
	evs = collectEvents(rec, 2, 5*time.Second)
	chunks := chunkEvents(evs)
	if len(chunks) != 1 {
		t.Fatalf("the next answer did not reach a resumed reader: got %d chunks. "+
			"Without this the assertion above is satisfied by a handler that returned "+
			"and wrote nothing.\n\nevents: %v", len(chunks), evs)
	}
	if chunks[0].data["content"] != "next answer" {
		t.Errorf("got %q", chunks[0].data["content"])
	}
}

// A malformed cursor resumes from the beginning rather than being refused.
// A client whose only way to recover is to reconnect must not be refused for
// the state it is trying to recover from.
func TestAMalformedCursorServesTheWholeStream(t *testing.T) {
	f := newStreamFixture(t, &engine.WorkflowInstance{ID: "wf-1", Status: "done", Generation: 3},
		[]engine.EventRecord{chunkRec(10, 0, "one", false), chunkRec(11, 1, "two", true)})
	rec, cancel, done := f.start(t, "/api/workflows/wf-1/stream",
		map[string]string{"Last-Event-ID": "not-a-cursor"})
	defer cancel()
	<-done

	if n := len(chunkEvents(parseSSE(rec.body()))); n != 2 {
		t.Errorf("got %d chunks for a request with a malformed Last-Event-ID, want 2 "+
			"(the whole stream)", n)
	}
}

func TestStreamCursorParsing(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		ok   bool
		want streamCursor
	}{
		{"3:4", true, streamCursor{generation: 3, step: 4, present: true}},
		{"0:0", true, streamCursor{present: true}},
		{"", false, streamCursor{}},
		{"3", false, streamCursor{}},
		{"3:4:7", false, streamCursor{}},
		{"a:4", false, streamCursor{}},
		{"3:-1", false, streamCursor{}},
	} {
		got, ok := parseStreamCursor(tc.raw)
		if ok != tc.ok {
			t.Errorf("parseStreamCursor(%q) ok=%v, want %v", tc.raw, ok, tc.ok)
			continue
		}
		if got != tc.want {
			t.Errorf("parseStreamCursor(%q) = %+v, want %+v", tc.raw, got, tc.want)
		}
	}

	// Step is strictly increasing across the whole run, so the comparison is
	// one compare and there is no per-answer case to get wrong.
	c := streamCursor{generation: 1, step: 9, present: true}
	if c.after(9) {
		t.Error("a cursor at step 9 re-sent step 9")
	}
	if !c.after(10) {
		t.Error("a cursor at step 9 suppressed step 10")
	}
	if c.after(8) {
		t.Error("a cursor at step 9 re-sent step 8")
	}

	// An absent cursor owes everything.
	var none streamCursor
	if !none.after(0) {
		t.Error("an absent cursor suppressed step 0")
	}
}

// The 503 at the ceiling names the number the caller was refused against.
//
// Split from the status-code assertion deliberately: "too many readers" with no
// figure in it is a sentence an operator cannot act on, and a test that only
// checks the code passes against exactly that.
func TestTheCeilingRefusalNamesTheCountAndTheKnob(t *testing.T) {
	ms := &mockStore{
		getWorkflowByIDFn: func(ctx context.Context, id string) (*engine.WorkflowInstance, error) {
			return runningRun(), nil
		},
	}
	hub := engine.NewStreamHub(2)
	for _, id := range []string{"other-a", "other-b"} {
		if _, err := hub.Subscribe(id); err != nil {
			t.Fatalf("UNMEASURED: could not fill the ceiling: %v", err)
		}
	}
	api := &apiServer{store: ms, worker: newTestWorker(ms), streamHub: hub}

	rec := httptest.NewRecorder()
	api.handleStreamWorkflow(rec, httptest.NewRequest(http.MethodGet, "/api/workflows/wf-1/stream", nil), "wf-1")

	body := rec.Body.String()
	if !strings.Contains(body, "2 attached") {
		t.Errorf("the 503 body does not say how many readers are attached: %q", body)
	}
	if !strings.Contains(body, "--max-stream-readers") {
		t.Errorf("the 503 body does not name the flag that moves the ceiling: %q", body)
	}
}
