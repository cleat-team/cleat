package engine

import (
	"errors"
	"sync"
)

// StreamHub is the worker-local live tail of plugin stream chunks. cleat#1572.
//
// A workflow streaming tokens from a model records each chunk as an event and
// persists it as it arrives -- measured, with a negative control, on #1572. The
// durable record is therefore complete and is the truth. What it is not is
// PROMPT: a reader polling event_history sees a token some fraction of a second
// after the guest did, and an agent's output reaching a browser one poll at a
// time is the thing this exists to avoid.
//
// So the hub carries the same chunks a second way, in memory, for readers that
// want them now. It is a PREVIEW -- the owner's decision on #1572 -- and every
// property below follows from that word:
//
//   - PUBLISHING NEVER BLOCKS. A subscriber's buffer is bounded and a full one
//     is dropped, not waited on. The publisher is the plugin call path inside a
//     running workflow; a browser on a slow link must not be able to stall it.
//   - A DROPPED SUBSCRIBER IS TOLD. It is closed with Lagged set rather than
//     just ending, because a stream that stops is indistinguishable from a
//     stream that finished, and the two want opposite things from a client.
//   - THE PREVIEW IS RECOVERABLE. A lagged reader has lost nothing permanently:
//     every chunk it missed is in event_history under the same (step, index),
//     so it can reattach from its last cursor and be served from the durable
//     record. That is only true because chunks persist as they arrive; on the
//     premise this issue was filed with -- that partial output "has no durable
//     existence" -- dropping a subscriber would have been data loss.
//   - NOTHING IS RETAINED. The hub holds no history of its own. A subscriber
//     that attaches mid-stream gets what arrives after it attached, and the
//     part it missed comes from the database.
//
// The hub does no authorization. It is keyed by workflow id alone, and a
// caller must establish that the reader may see that run BEFORE subscribing --
// which on the worker means a tenant-scoped store lookup. See
// handleStreamWorkflow in cmd/cleat-worker.
type StreamHub struct {
	mu     sync.Mutex
	subs   map[string]map[*StreamSub]struct{}
	total  int
	maxSub int
}

// LiveChunk is one streamed chunk as it is published to live readers.
//
// STEP IS THE CURSOR AND INDEX IS THE SHAPE, and conflating them is the error
// this comment exists to prevent. Measured by driving freshPluginCallStreaming
// with three chunks and then calling it a second time:
//
//		step=0 index=0        step=3 index=0
//		step=1 index=1        step=4 index=1
//		step=2 index=2 finish step=5 index=2 finish
//
//	  - Step is the event's position in the run's history. It is globally unique
//	    and strictly increasing, because recordEvent increments stepCount for
//	    every event -- so it is a complete cursor on its own, and it identifies
//	    one CHUNK, never a call.
//	  - Index restarts at 0 for each streaming call. It is what marks where one
//	    answer ends and the next begins; Finish marks the last chunk of one.
//
// Both come off the recorded row, so a reader can move between the live tail
// and event_history without translating.
type LiveChunk struct {
	Step    int
	Index   int
	Content string
	Finish  bool

	// Durable reports that this chunk's row was in the database before it was
	// published. It is true on an ordinary worker and false only where the
	// deployment has opted out of per-step persistence (--no-per-step-flush)
	// or has no database at all, in which case the chunk reaches history when
	// the segment finalizes.
	//
	// A chunk whose write was REFUSED is never published, so this field never
	// distinguishes "durable" from "lost" -- only "durable now" from "durable
	// shortly". That is what makes it safe to render either way and worth
	// labelling rather than filtering.
	Durable bool
}

// ErrTooManyStreamReaders is returned by Subscribe when the hub is at its
// subscriber ceiling.
//
// A refusal rather than an unbounded accept: each subscriber costs a goroutine,
// a held HTTP connection and a buffer, and the failure mode of not having a
// ceiling is a worker that stops executing workflows because it is busy holding
// browsers open.
//
// This is a DIFFERENT budget from --connection-budget, which governs database
// POOLS: a reader HOLDS no database connection. It is not free of the database
// either -- the SSE route polls the run's status once per heartbeat to notice a
// run that ended without a final chunk -- so the ceiling also sets a floor on
// background query load. Saying "no database connection" flat, as this comment
// did, is the kind of half-true that gets sized against.
var ErrTooManyStreamReaders = errors.New("too many live stream readers on this worker")

// NewStreamHub returns a hub admitting at most maxSubscribers concurrent
// readers. A non-positive maxSubscribers means unlimited, which is intended for
// tests rather than for a worker.
func NewStreamHub(maxSubscribers int) *StreamHub {
	return &StreamHub{
		subs:   make(map[string]map[*StreamSub]struct{}),
		maxSub: maxSubscribers,
	}
}

// StreamSub is one reader's view of a run's live chunks.
type StreamSub struct {
	hub        *StreamHub
	workflowID string

	ch chan LiveChunk

	mu     sync.Mutex
	closed bool
	lagged bool
}

// streamSubBuffer is how many chunks a reader may fall behind before it is
// dropped.
//
// Sized for a reader that is merely slow rather than stuck: an SSE handler
// writing to a live socket keeps up with a model's token rate comfortably, and
// a buffer this size absorbs a scheduling hiccup or a brief stall. What it
// deliberately does NOT absorb is a client that has stopped reading, which is
// the case that must not be allowed to accumulate.
const streamSubBuffer = 256

// Subscribe attaches a reader to a run's live chunks. The returned
// subscription must be closed by the caller.
//
// THERE IS NO PER-CALL FILTER, and the reason is measured rather than chosen.
// Each chunk is recorded at its OWN step -- recordEvent increments stepCount
// and the chunk loop reads it fresh on every iteration -- so a step identifies one
// chunk and never a call. A filter on it would select a single token.
//
// What separates one answer from the next is StreamChunkIndex, which restarts
// at 0 for every streaming call. That boundary travels with the chunk, so a
// reader segments the stream itself and needs nothing from the hub.
func (h *StreamHub) Subscribe(workflowID string) (*StreamSub, error) {
	if h == nil {
		return nil, errors.New("no live stream hub configured on this worker")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.maxSub > 0 && h.total >= h.maxSub {
		return nil, ErrTooManyStreamReaders
	}
	s := &StreamSub{
		hub:        h,
		workflowID: workflowID,
		ch:         make(chan LiveChunk, streamSubBuffer),
	}
	if h.subs[workflowID] == nil {
		h.subs[workflowID] = make(map[*StreamSub]struct{})
	}
	h.subs[workflowID][s] = struct{}{}
	h.total++
	return s, nil
}

// Publish delivers a chunk to every reader attached to this run.
//
// It never blocks and never returns an error, because its caller is a workflow
// executing a plugin call and has nothing useful to do about a reader that
// cannot keep up. A reader whose buffer is full is dropped, flagged as lagged
// and closed; it recovers by reattaching against event_history.
func (h *StreamHub) Publish(workflowID string, c LiveChunk) {
	if h == nil {
		return
	}
	h.mu.Lock()
	subs := h.subs[workflowID]
	if len(subs) == 0 {
		h.mu.Unlock()
		return
	}
	// Copy under the lock and deliver outside it. Delivery is a non-blocking
	// send, so holding the lock across it would be safe -- but dropping a
	// lagged subscriber calls back into the hub to unregister, and doing that
	// while iterating the map it is removing itself from is the kind of thing
	// that works until someone changes the drop path.
	targets := make([]*StreamSub, 0, len(subs))
	for s := range subs {
		targets = append(targets, s)
	}
	h.mu.Unlock()

	for _, s := range targets {
		s.deliver(c)
	}
}

// deliver makes one non-blocking attempt to hand a chunk to this reader.
func (s *StreamSub) deliver(c LiveChunk) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	select {
	case s.ch <- c:
		s.mu.Unlock()
		return
	default:
		// Full. Drop the reader rather than the chunk: dropping the chunk
		// would leave a reader believing it had seen a contiguous stream when
		// it had not, and the whole point of the cursor is that a client can
		// tell. Closing under the lock is what makes the send above safe --
		// nothing can send on a channel this method has already closed.
		s.lagged = true
		s.closed = true
		close(s.ch)
		s.mu.Unlock()
	}
	s.hub.remove(s)
}

// Chunks is the channel a reader consumes. It is closed when the subscription
// ends, whether because the caller closed it or because the reader lagged --
// call Lagged to tell those apart.
func (s *StreamSub) Chunks() <-chan LiveChunk { return s.ch }

// Lagged reports whether this subscription was dropped for falling behind.
//
// Read it after Chunks closes. A closed channel alone cannot distinguish "the
// stream ended" from "you were too slow", and a client shown the first when
// the second happened has a truncated transcript it believes is complete.
func (s *StreamSub) Lagged() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lagged
}

// Close ends the subscription. Safe to call more than once.
func (s *StreamSub) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.ch)
	s.mu.Unlock()
	s.hub.remove(s)
}

func (h *StreamHub) remove(s *StreamSub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	subs := h.subs[s.workflowID]
	if _, ok := subs[s]; !ok {
		return
	}
	delete(subs, s)
	h.total--
	if len(subs) == 0 {
		delete(h.subs, s.workflowID)
	}
}

// Readers reports how many subscriptions are currently attached, across all
// runs.
//
// The SSE route puts it in the body of the 503 it returns at the ceiling: an
// operator who has just been refused needs the number they are being refused
// against, and "too many readers" without it is a sentence they cannot act on.
// Tests also use it to assert a subscription was actually released, which a
// closed channel does not show.
func (h *StreamHub) Readers() int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.total
}
