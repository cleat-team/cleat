package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// GET /api/workflows/{id}/stream on a worker that is NOT executing the run.
// cleat#1639.
//
// #1572 put the live tail in memory on the executing worker, which is correct
// and is also the whole problem: behind a load balancer with N workers, roughly
// (N-1)/N of readers land somewhere else and get the durable history followed
// by silence. Every test in this file is about a reader on the wrong worker.
//
// THE FIXTURE PUTS THE RUN ON ANOTHER WORKER AND THE CHUNKS IN THE STORE, which
// is the shape that separates this from #1572's file: nothing here publishes to
// a hub, because a reader on another worker cannot see one.

// otherWorkersRun is a run this worker is not executing. newTestWorker's id is
// "test-worker"; this names a different one, which is the only thing that makes
// these tests about #1639 rather than about #1572.
func otherWorkersRun() *engine.WorkflowInstance {
	return &engine.WorkflowInstance{
		ID: "wf-1", Status: "running", AssignedTo: "some-other-worker", Generation: 3,
	}
}

// growingHistory is an event_history that gains chunks while a reader follows
// it, answering LoadStreamChunksAfter from a cursor the way a real store does.
type growingHistory struct {
	mu     sync.Mutex
	chunks []engine.EventRecord
	calls  int
}

func (g *growingHistory) append(recs ...engine.EventRecord) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.chunks = append(g.chunks, recs...)
}

func (g *growingHistory) after(_ context.Context, _ string, afterStep, limit int) ([]engine.EventRecord, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	var out []engine.EventRecord
	for _, c := range g.chunks {
		if c.Step > afterStep {
			out = append(out, c)
		}
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (g *growingHistory) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

// pollFixture wires an apiServer whose worker does not hold the run, with a
// store that can tail chunks and a poll interval short enough for a test.
func newPollFixture(t *testing.T, wf *engine.WorkflowInstance, history []engine.EventRecord) (*streamFixture, *growingHistory) {
	t.Helper()
	g := &growingHistory{}
	ms := &mockStore{
		getWorkflowByIDFn: func(ctx context.Context, id string) (*engine.WorkflowInstance, error) {
			return wf, nil
		},
		loadEventHistoryFn: func(ctx context.Context, id string) ([]engine.EventRecord, error) {
			return history, nil
		},
		loadStreamChunksAfterFn: g.after,
	}
	hub := engine.NewStreamHub(0)
	f := &streamFixture{
		api: &apiServer{
			store: ms, worker: newTestWorker(ms), streamHub: hub,
			// Short enough that a test does not wait out 250ms per chunk, long
			// enough that the loop is still a poll loop rather than a spin.
			streamPollInterval: 5 * time.Millisecond,
		},
		ms:  ms,
		hub: hub,
	}
	return f, g
}

// waitDone waits for the handler to return, and FAILS rather than hanging if it
// does not.
//
// A bare <-done is the obvious spelling and it is wrong for every test whose
// subject is a REFUSAL. When the refusal is removed the handler streams
// normally and never returns, so the test hangs until `go test`'s package
// timeout and reports `panic: test timed out after 10m0s` -- a failure about
// the harness, naming no guard, six minutes after the fact.
//
// Falsifying the ceiling test is what found it: the mutation was correct, the
// test was going to catch it, and the report would have been useless. CLAUDE.md
// asks for a red that names the reason you expect; a red that names the clock
// is not one.
func waitDone(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not return within 5s.\n\nIt is still streaming, which means the "+
			"request was SERVED rather than refused -- the guard under test did not fire.",
			what)
	}
}

// ---------------------------------------------------------------------------

// THE HEADLINE. A reader on a worker that is not executing the run receives
// tokens as they are produced, rather than history and then silence.
func TestAReaderOnAnotherWorkerStillReceivesTokens(t *testing.T) {
	f, g := newPollFixture(t, otherWorkersRun(), nil)
	rec, cancel, done := f.start(t, "/api/workflows/wf-1/stream", nil)
	defer func() { cancel(); <-done }()

	// Attach first, or the test races the first poll and measures nothing.
	waitForEvents(t, rec, 1)

	// Tokens land in event_history, written by the OTHER worker. Nothing is
	// published to this worker's hub, because nothing could be.
	g.append(
		chunkRec(0, 0, "Hel", false),
		chunkRec(1, 1, "lo ", false),
		chunkRec(2, 2, "world", true),
	)

	// collectEvents, NOT waitForEvents: the subject is how many chunks arrive,
	// so this test must fail on its own count assertion naming cleat#1639 --
	// not on a generic "wanted 4 events, saw 2". Falsifying it is what showed
	// the difference, exactly as the #1572 file records for its own mutations.
	evs := collectEvents(rec, 4, 5*time.Second) // attached + 3 chunks
	chunks := chunkEvents(evs)
	if len(chunks) != 3 {
		t.Fatalf("a reader on another worker saw %d chunks, want 3.\n\n"+
			"This is cleat#1639: the live tail is in memory on the executing worker, "+
			"so a reader anywhere else must follow the run from event_history. If this "+
			"reports 0, the handler served history and stopped.\n\nbody:\n%s",
			len(chunks), rec.body())
	}

	var got string
	for _, c := range chunks {
		got += c.data["content"].(string)
	}
	if got != "Hello world" {
		t.Errorf("reassembled %q, want %q", got, "Hello world")
	}

	// Provenance travels with every chunk, and both flags are facts about the
	// row rather than guesses: it was read from event_history.
	for i, c := range chunks {
		if c.data["durable"] != true {
			t.Errorf("chunk %d has durable=%v, want true -- it was read out of "+
				"event_history, so its presence there is not an inference", i, c.data["durable"])
		}
		if c.data["replay"] != true {
			t.Errorf("chunk %d has replay=%v, want true.\n\n"+
				"replay is PROVENANCE, not novelty: it says the chunk came from "+
				"event_history rather than the in-memory tail. This one did. A "+
				"client deciding what it has already rendered uses step, which is "+
				"exact, not replay, which is advisory.", i, c.data["replay"])
		}
	}

	// The reader is told which tail it is on, and told honestly.
	att, ok := findEvent(evs, "attached")
	if !ok {
		t.Fatalf("no attached event.\n\nbody:\n%s", rec.body())
	}
	if att.data["transport"] != "poll" {
		t.Errorf("attached.transport = %v, want \"poll\" -- a reader must be able to see "+
			"which tail it got, because the two have different latency", att.data["transport"])
	}
	if att.data["live"] != false {
		t.Errorf("attached.live = %v, want false. `live` keeps its #1572 meaning "+
			"(attached to the in-memory tail on the executing worker); it is "+
			"`transport` that says a durable tail is feeding this stream",
			att.data["live"])
	}
}

// A chunk already in history when the reader attaches is served once, not
// twice -- the history read and the first poll must not overlap into a
// duplicate.
func TestAChunkAlreadyInHistoryIsNotServedTwiceByTheDurableTail(t *testing.T) {
	seeded := []engine.EventRecord{chunkRec(0, 0, "Hel", false)}
	f, g := newPollFixture(t, otherWorkersRun(), seeded)
	// The same row is in the tail's view too, which is what a real store would
	// return: history and the tail read one table.
	g.append(seeded...)

	rec, cancel, done := f.start(t, "/api/workflows/wf-1/stream", nil)
	defer func() { cancel(); <-done }()

	waitForEvents(t, rec, 2) // attached + the seeded chunk
	g.append(chunkRec(1, 1, "lo", true))
	evs := collectEvents(rec, 3, 5*time.Second)

	chunks := chunkEvents(evs)
	if len(chunks) != 2 {
		t.Fatalf("saw %d chunks, want 2 (one from history, one from the tail).\n\n"+
			"More than 2 means the first poll re-served what the history read "+
			"already sent: the cursor did not carry across.\n\nbody:\n%s",
			len(chunks), rec.body())
	}
	if chunks[0].data["content"] != "Hel" || chunks[1].data["content"] != "lo" {
		t.Errorf("chunks were %q then %q, want \"Hel\" then \"lo\"",
			chunks[0].data["content"], chunks[1].data["content"])
	}
}

// ?mode=live refuses the durable tail, and the refusal names the remedy.
func TestModeLiveRefusesTheDurableTailAndSaysWhy(t *testing.T) {
	f, g := newPollFixture(t, otherWorkersRun(), nil)
	rec, cancel, done := f.start(t, "/api/workflows/wf-1/stream?mode=live", nil)
	defer func() { cancel(); <-done }()

	evs := collectEvents(rec, 2, 5*time.Second) // attached + end
	end, ok := findEvent(evs, "end")
	if !ok {
		t.Fatalf("?mode=live on a non-executing worker did not end the stream.\n\n"+
			"It is the one mode that asks to be refused rather than served slowly.\n\nbody:\n%s",
			rec.body())
	}
	reason, _ := end.data["reason"].(string)
	if !strings.Contains(reason, "mode=live") {
		t.Errorf("end reason %q does not name ?mode=live. A client that gets an "+
			"immediate end needs to know the remedy is its own parameter", reason)
	}
	if n := g.callCount(); n != 0 {
		t.Errorf("?mode=live read the durable tail %d times, want 0 -- it exists "+
			"precisely to not do that", n)
	}
}

// ?mode=poll never touches the hub, even on the worker executing the run.
func TestModePollNeverTouchesTheHubEvenOnTheExecutingWorker(t *testing.T) {
	// runningRun() is assigned to "test-worker", which IS this worker.
	f, g := newPollFixture(t, runningRun(), nil)
	rec, cancel, done := f.start(t, "/api/workflows/wf-1/stream?mode=poll", nil)
	defer func() { cancel(); <-done }()

	waitForEvents(t, rec, 1)
	if n := f.hub.Readers(); n != 0 {
		t.Errorf("?mode=poll took %d hub subscription(s), want 0. A caller asking "+
			"for one predictable latency should not also be consuming a "+
			"--max-stream-readers slot", n)
	}

	g.append(chunkRec(0, 0, "tok", true))
	evs := collectEvents(rec, 2, 5*time.Second)
	if len(chunkEvents(evs)) != 1 {
		t.Errorf("?mode=poll on the executing worker did not serve the chunk from "+
			"event_history.\n\nbody:\n%s", rec.body())
	}
}

// The wart #1572 left, fixed as a side effect and asserted so it stays fixed:
// a reader of a run this worker is not executing took a hub slot for a
// subscription that could never deliver a chunk.
func TestANonExecutingReaderDoesNotTakeAHubSlot(t *testing.T) {
	f, _ := newPollFixture(t, otherWorkersRun(), nil)
	rec, cancel, done := f.start(t, "/api/workflows/wf-1/stream", nil)
	defer func() { cancel(); <-done }()

	waitForEvents(t, rec, 1)
	if n := f.hub.Readers(); n != 0 {
		t.Errorf("a reader of a run on another worker holds %d hub subscription(s), "+
			"want 0.\n\nThe hub only ever carries chunks for runs THIS worker "+
			"executes, so such a subscription cannot deliver anything -- it only "+
			"consumes a --max-stream-readers slot that a reader on the right "+
			"worker could have used", n)
	}
}

// An unrecognised mode is refused rather than ignored.
func TestAnUnknownStreamModeIsRefused(t *testing.T) {
	f, _ := newPollFixture(t, otherWorkersRun(), nil)
	rec, cancel, done := f.start(t, "/api/workflows/wf-1/stream?mode=whatever", nil)
	defer func() { cancel(); <-done }()
	waitDone(t, done, "a request with an unrecognised ?mode")

	if got := rec.status(); got != 400 {
		t.Errorf("?mode=whatever answered %d, want 400.\n\nIgnoring an unrecognised "+
			"mode hands back exactly the silence this endpoint was changed to stop, "+
			"with no way for the caller to see why.\n\nbody:\n%s", got, rec.body())
	}
}

// The durable tail has its own ceiling, and it refuses BEFORE writing a body.
func TestTheDurableTailCeilingIsEnforcedBeforeTheStreamOpens(t *testing.T) {
	f, _ := newPollFixture(t, otherWorkersRun(), nil)
	f.api.maxStreamPollReaders = 1

	rec1, cancel1, done1 := f.start(t, "/api/workflows/wf-1/stream", nil)
	defer func() { cancel1(); <-done1 }()
	waitForEvents(t, rec1, 1)

	rec2, cancel2, done2 := f.start(t, "/api/workflows/wf-1/stream", nil)
	defer func() { cancel2(); <-done2 }()
	waitDone(t, done2, "the second reader over the ceiling")

	if got := rec2.status(); got != 503 {
		t.Fatalf("the second reader over a ceiling of 1 answered %d, want 503.\n\n"+
			"Unbounded durable-tail readers are an unbounded query rate against "+
			"event_history, which is the resource --max-stream-poll-readers "+
			"bounds.\n\nbody:\n%s", got, rec2.body())
	}
	if len(parseSSE(rec2.body())) > 0 {
		t.Errorf("the refused reader got SSE events before the refusal.\n\n"+
			"Once a 200 and an event are out, the only way left to refuse is to end "+
			"the stream, which a client cannot tell from a run that finished.\n\n"+
			"body:\n%s", rec2.body())
	}
}

// A run that ends without a final chunk still ends the reader's stream.
func TestARunThatEndsWithoutAFinalChunkEndsTheDurableTail(t *testing.T) {
	wf := otherWorkersRun()
	var mu sync.Mutex
	status := "running"

	g := &growingHistory{}
	ms := &mockStore{
		getWorkflowByIDFn: func(ctx context.Context, id string) (*engine.WorkflowInstance, error) {
			mu.Lock()
			defer mu.Unlock()
			cur := *wf
			cur.Status = status
			return &cur, nil
		},
		loadEventHistoryFn: func(ctx context.Context, id string) ([]engine.EventRecord, error) {
			return nil, nil
		},
		loadStreamChunksAfterFn: g.after,
	}
	api := &apiServer{
		store: ms, worker: newTestWorker(ms), streamHub: engine.NewStreamHub(0),
		streamPollInterval: 5 * time.Millisecond,
		// The subject is that the status read HAPPENS, not that it happens
		// every 15 seconds. Asserting it against the default constant costs 15
		// seconds of wall clock to measure a boolean.
		streamStatusInterval: 20 * time.Millisecond,
	}
	f := &streamFixture{api: api, ms: ms}

	rec, cancel, done := f.start(t, "/api/workflows/wf-1/stream", nil)
	defer func() { cancel(); <-done }()
	waitForEvents(t, rec, 1)

	mu.Lock()
	status = "failed"
	mu.Unlock()

	evs := collectEvents(rec, 2, 5*time.Second)
	end, ok := findEvent(evs, "end")
	if !ok {
		t.Fatalf("a run that reached a terminal status without a final chunk left the "+
			"reader waiting.\n\nThe status read is on the heartbeat cadence rather "+
			"than the poll cadence, so this can take up to one heartbeat -- but it "+
			"must happen.\n\nbody:\n%s", rec.body())
	}
	if reason, _ := end.data["reason"].(string); !strings.Contains(reason, "failed") {
		t.Errorf("end reason = %q, want it to name the terminal status", reason)
	}
}
