package engine

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/cleat-team/cleat/plugin"
)

// fenceLostStore is a WorkflowStore whose Heartbeat reports that this worker no
// longer holds the run -- what flushEvent consults before writing.
//
// It implements perStepEventFlusher so flushEvent takes the fenced branch,
// which answers ErrFenceLost from the Heartbeat alone and never reaches the
// database. That is what lets this test assert the refusal without a live
// connection, and flushEventForStep failing loudly if it is ever called is the
// guard that the fence really is what refused.
type fenceLostStore struct {
	WorkflowStore
}

func (fenceLostStore) Heartbeat(ctx context.Context, workflowID, workerID string, generation int64) (bool, error) {
	return false, nil
}

func (fenceLostStore) flushEventForStep(ctx context.Context, workflowID string, rec EventRecord) error {
	panic("flushEventForStep was reached, so the fence did NOT refuse the write and " +
		"this test is measuring something else")
}

// The live tail must never be able to hold up the workflow that feeds it.
// cleat#1572.
//
// The publisher is the plugin call path inside a running workflow. Its reader
// is a browser, on a link the worker does not control, which may stop reading
// at any moment and give no indication that it has. If a slow reader can block
// a publish, a single wedged client stalls a workflow -- and since the chunk
// loop holds the guest, it stalls guest execution on that worker's slot.
//
// So these assert the two halves of the bargain: the publisher is never
// blocked, and the reader that pays for that is TOLD it was dropped. The
// second matters as much as the first, because a stream that silently stops is
// indistinguishable from a model that finished, and a client shown a truncated
// transcript it believes is complete is precisely the failure the preview
// decision on #1572 set out to avoid.

func TestPublishingNeverBlocksOnAReaderThatHasStoppedReading(t *testing.T) {
	hub := NewStreamHub(0)
	sub, err := hub.Subscribe("wf-1")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	// Nothing ever reads sub.Chunks(). Publish far past the buffer.
	done := make(chan struct{})
	go func() {
		for i := 0; i < streamSubBuffer*4; i++ {
			hub.Publish("wf-1", LiveChunk{Step: 0, Index: i, Content: "tok"})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a reader that is not consuming.\n\n" +
			"The publisher is a workflow executing a plugin call. A reader that " +
			"can block it can stall guest execution on this worker, which is why " +
			"delivery is a non-blocking send and an over-full reader is dropped.")
	}
}

func TestAReaderThatFallsBehindIsDroppedAndTold(t *testing.T) {
	hub := NewStreamHub(0)
	sub, err := hub.Subscribe("wf-1")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	for i := 0; i < streamSubBuffer*2; i++ {
		hub.Publish("wf-1", LiveChunk{Step: 0, Index: i, Content: "tok"})
	}

	// Drain whatever was buffered; the channel must then be CLOSED rather than
	// merely empty.
	drained := 0
	for range sub.Chunks() {
		drained++
	}
	if drained == 0 {
		t.Fatalf("UNMEASURED: the reader received nothing at all, so this says " +
			"nothing about what happens when one falls behind")
	}
	if !sub.Lagged() {
		t.Errorf("the reader was dropped after receiving %d of %d chunks and Lagged() is false.\n\n"+
			"A closed channel alone cannot distinguish \"the stream ended\" from \"you were "+
			"too slow\", and a client shown the first when the second happened renders a "+
			"truncated transcript as a complete one.", drained, streamSubBuffer*2)
	}

	// And it must be unregistered, not merely closed: a hub that keeps dropped
	// readers in its map leaks one entry per disconnect forever.
	if got := hub.Readers(); got != 0 {
		t.Errorf("hub still holds %d reader(s) after dropping one", got)
	}
}

// A subscription that ends normally is NOT lagged. Without this the assertion
// above is satisfied by a Lagged() that returns true unconditionally.
func TestAReaderThatClosesCleanlyIsNotReportedAsLagged(t *testing.T) {
	hub := NewStreamHub(0)
	sub, err := hub.Subscribe("wf-1")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	hub.Publish("wf-1", LiveChunk{Step: 0, Index: 0, Content: "tok"})
	<-sub.Chunks()
	sub.Close()
	if sub.Lagged() {
		t.Error("a reader that closed cleanly reports Lagged()")
	}
	if got := hub.Readers(); got != 0 {
		t.Errorf("hub still holds %d reader(s) after a clean Close", got)
	}
}

// THE RECORDED SHAPE THE SSE ROUTE IS BUILT ON. cleat#1572.
//
// This test exists because the route was first built on the opposite
// assumption -- that one streaming call shares a step, so a reader could be
// filtered to it -- and every test of that route passed, because the fixtures
// were written from the same wrong belief. Nothing failed. What settled it was
// driving the real chunk loop and reading what it recorded.
//
// So this pins the two properties the route depends on, against the actual
// engine path rather than against a hand-built fixture:
//
//   - step is unique per CHUNK and strictly increasing, because recordEvent
//     increments stepCount for every event. It is therefore a complete cursor
//     on its own, and useless as a call identifier.
//   - StreamChunkIndex restarts at 0 for each streaming call, and is what
//     marks where one answer ends and the next begins.
//
// If either changes, the route's cursor and its segmentation are wrong and
// this goes red -- which is the whole point of measuring it here rather than
// describing it in a comment on the handler.
func TestEachChunkIsItsOwnStepAndTheIndexRestartsPerCall(t *testing.T) {
	psr := NewPluginStreamRegistry()
	if err := psr.Register("p", "s",
		func(ctx context.Context, inputJSON string) (<-chan plugin.StreamEvent, error) {
			ch := make(chan plugin.StreamEvent, 3)
			ch <- plugin.StreamEvent{Index: 0, Content: "a"}
			ch <- plugin.StreamEvent{Index: 1, Content: "b"}
			ch <- plugin.StreamEvent{Index: 2, Content: "c", Finish: true}
			close(ch)
			return ch, nil
		}); err != nil {
		t.Fatalf("registering the streaming function: %v", err)
	}

	hub := NewStreamHub(0)
	sess := newTestExecSession()
	sess.engine.pluginStreamRegistry = psr
	sess.engine.streamHub = hub
	sess.workflowID = "wf-1"

	sub, err := hub.Subscribe("wf-1")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	buf := make([]byte, 4096)
	ctx := contextWithRawMemBuf(context.Background(), buf)
	// TWO calls, because one cannot show that the index restarts.
	sess.PluginCallStreaming(ctx, nil, "p", "s", `{}`, 0, 4095)
	sess.PluginCallStreaming(ctx, nil, "p", "s", `{}`, 0, 4095)

	var chunks []EventRecord
	for _, rec := range sess.history {
		if rec.EventType == EventTypePluginCallStreamChunk {
			chunks = append(chunks, rec)
		}
	}
	if len(chunks) != 6 {
		t.Fatalf("UNMEASURED: %d chunk events recorded, want 6 (two calls of three). "+
			"Nothing below is about the shape of a recorded stream.", len(chunks))
	}

	// Step: unique and strictly increasing across BOTH calls.
	for i := 1; i < len(chunks); i++ {
		if chunks[i].Step <= chunks[i-1].Step {
			t.Errorf("chunk %d is at step %d and chunk %d is at step %d -- step must be "+
				"strictly increasing for it to be a cursor", i-1, chunks[i-1].Step, i, chunks[i].Step)
		}
	}

	// Index: 0,1,2 then 0,1,2 -- the restart is the answer boundary.
	var gotIdx []int
	for _, c := range chunks {
		gotIdx = append(gotIdx, c.StreamChunkIndex)
	}
	want := []int{0, 1, 2, 0, 1, 2}
	for i := range want {
		if gotIdx[i] != want[i] {
			t.Fatalf("chunk indices are %v, want %v.\n\nThe SSE route segments one answer "+
				"from the next on index==0; if the index does not restart per call, a "+
				"client renders two answers as one.", gotIdx, want)
		}
	}
	if !chunks[2].StreamFinish || !chunks[5].StreamFinish {
		t.Error("the last chunk of each call does not carry StreamFinish")
	}

	// And the live tail carries exactly the recorded pair.
	sub.Close()
	var live []LiveChunk
	for c := range sub.Chunks() {
		live = append(live, c)
	}
	if len(live) != len(chunks) {
		t.Fatalf("published %d chunks live and recorded %d", len(live), len(chunks))
	}
	for i := range live {
		if live[i].Step != chunks[i].Step || live[i].Index != chunks[i].StreamChunkIndex {
			t.Errorf("live chunk %d is (step=%d,index=%d) and the recorded row is "+
				"(step=%d,index=%d).\n\nA reader that moves between the live tail and "+
				"event_history does so on this pair; if they disagree it resumes in the "+
				"wrong place.", i, live[i].Step, live[i].Index,
				chunks[i].Step, chunks[i].StreamChunkIndex)
		}
	}
}

// A CHUNK THE DATABASE REFUSED IS NOT SHOWN TO ANYBODY. cleat#1572.
//
// The case is a reclaim: another worker takes the run mid-stream, this
// worker's fence is lost, and flushEvent refuses the write. recordEvent logs
// it at debug and returns normally, so nothing downstream notices -- and the
// first version of the live tail published the chunk regardless, putting
// tokens on a user's screen that the run's history will never contain. It is
// also the one case a reader cannot recover from: there is no row to reconnect
// to.
//
// This drives the real chunk loop with a store whose Heartbeat reports the
// fence lost, which is what a reclaim looks like from inside flushEvent.
func TestAChunkTheDatabaseRefusedIsNeverPublished(t *testing.T) {
	psr := NewPluginStreamRegistry()
	if err := psr.Register("p", "s",
		func(ctx context.Context, inputJSON string) (<-chan plugin.StreamEvent, error) {
			ch := make(chan plugin.StreamEvent, 2)
			ch <- plugin.StreamEvent{Index: 0, Content: "ghost"}
			ch <- plugin.StreamEvent{Index: 1, Content: "token", Finish: true}
			close(ch)
			return ch, nil
		}); err != nil {
		t.Fatalf("registering the streaming function: %v", err)
	}

	hub := NewStreamHub(0)
	sess := newTestExecSession()
	sess.engine.pluginStreamRegistry = psr
	sess.engine.streamHub = hub
	sess.workflowID = "wf-1"

	// A non-nil db is what makes recordEvent attempt a write at all; the
	// fence-losing store below is what makes the attempt fail. Both are
	// needed, and with only the first this test would pass against the
	// unfixed code.
	sess.engine.db = &sql.DB{}
	sess.engine.workflowStore = fenceLostStore{}
	sess.engine.workerID = "worker-that-lost-the-run"
	sess.engine.generation = 1

	sub, err := hub.Subscribe("wf-1")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	buf := make([]byte, 4096)
	ctx := contextWithRawMemBuf(context.Background(), buf)
	sess.PluginCallStreaming(ctx, nil, "p", "s", `{}`, 0, 4095)

	sub.Close()
	var published []LiveChunk
	for c := range sub.Chunks() {
		published = append(published, c)
	}
	if len(published) != 0 {
		t.Errorf("%d chunk(s) were published to a live reader after the database "+
			"refused to write them: %q.\n\nThe fence is lost, so this worker's events "+
			"are not going into the run's history at all. A reader shown these has a "+
			"transcript the system does not believe happened, and no row to reconnect "+
			"to.", len(published), published[0].Content)
	}
}

// A run nobody is watching must not pay for the hub, and a reader on a
// DIFFERENT run must not receive these chunks.
func TestChunksReachOnlyReadersOfTheSameRun(t *testing.T) {
	hub := NewStreamHub(0)
	mine, err := hub.Subscribe("wf-mine")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer mine.Close()
	theirs, err := hub.Subscribe("wf-theirs")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer theirs.Close()

	hub.Publish("wf-mine", LiveChunk{Step: 0, Index: 0, Content: "mine"})

	select {
	case c := <-mine.Chunks():
		if c.Content != "mine" {
			t.Errorf("got %q", c.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the run's own reader received nothing")
	}
	select {
	case c := <-theirs.Chunks():
		t.Errorf("a reader of another run received %q -- the hub is keyed by "+
			"workflow id and this is the only thing separating two runs' tokens", c.Content)
	default:
	}
}

// The ceiling is a refusal, not a silent accept. A worker with no bound on
// readers stops executing workflows because it is busy holding browsers open.
func TestTheReaderCeilingRefusesRatherThanOverCommits(t *testing.T) {
	hub := NewStreamHub(2)
	a, err := hub.Subscribe("wf-1")
	if err != nil {
		t.Fatalf("first Subscribe: %v", err)
	}
	if _, err := hub.Subscribe("wf-2"); err != nil {
		t.Fatalf("second Subscribe: %v", err)
	}
	if _, err := hub.Subscribe("wf-3"); err == nil {
		t.Fatal("a third reader was admitted against a ceiling of 2")
	}

	// And a closed reader gives its slot back, or the ceiling becomes a
	// lifetime quota and the worker refuses every stream after its first
	// thousand disconnects.
	a.Close()
	if _, err := hub.Subscribe("wf-3"); err != nil {
		t.Errorf("a slot was not released by Close: %v", err)
	}
}

// An engine with no hub configured must publish nowhere without crashing --
// which is every embedded engine and every test that does not opt in.
func TestANilHubIsANoOp(t *testing.T) {
	var hub *StreamHub
	hub.Publish("wf-1", LiveChunk{Step: 0, Index: 0, Content: "tok"})
	if _, err := hub.Subscribe("wf-1"); err == nil {
		t.Error("Subscribe on a nil hub returned no error")
	}
	if hub.Readers() != 0 {
		t.Error("nil hub reports readers")
	}
}
