package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// GET /api/workflows/{id}/stream -- server-sent events carrying a run's plugin
// stream chunks. cleat#1572.
//
// THE DURABLE RECORD IS THE TRUTH AND THE LIVE TAIL IS A PREVIEW. That is the
// owner's decision on #1572 and it is what makes this endpoint's reconnection
// story honest rather than best-effort: every chunk is written to
// event_history as it arrives (measured on #1572, with a negative control), so
// a client that reconnects is served the part it missed FROM THE DATABASE and
// only then joins the live tail.
//
// The consequence worth stating plainly, because the issue was filed on the
// opposite premise: a reader is never asked to trust the preview. Anything it
// saw live it can see again from history under the same (step, index). The
// live tail exists for latency, not for content.
//
// WHAT IS WORKER-LOCAL AND WHAT IS NOT. The live tail is in-memory on the
// worker executing the run. A request landing on any other worker cannot see
// it -- and behind a load balancer with N workers that is roughly (N-1)/N of
// requests, which is cleat#1639.
//
// So a reader that this worker is not executing follows the run FROM THE
// DURABLE RECORD instead, reading event_history past its cursor on an interval.
// Every worker can do that, because every worker can read the table. It costs
// poll latency rather than push latency and it is not a second source of truth:
// event_history is the one both paths serve, which is what makes them
// interchangeable mid-stream.
//
// THE READER IS TOLD WHICH IT GOT. `attached.transport` is "live" or "poll",
// and `attached.live` keeps its original meaning -- "you are attached to the
// in-memory tail on the executing worker". A client that wants to refuse the
// slower one asks for ?mode=live and gets an end event instead.
//
// ROUTING WAS PRICED AND IS NOT AVAILABLE. Sending the reader to the worker
// that holds the run needs that worker to have an address, and cleat has no
// concept of one: admin.workers (migration 076) records worker_id, hostname and
// pid with no port and no scheme, deliberately -- "membership, and nothing
// else" -- and the only address a worker knows about itself is --api-addr,
// which is a BIND address and defaults to empty, so the default worker serves
// no HTTP at all while still holding live tails. Proxying instead of
// redirecting would make this cleat's first worker-to-worker link and would
// replace an honest degraded stream with a hard failure whenever the owning
// worker is unreachable. See cleat#1639 for the full measurement.
//
// Event ids are `<generation>:<step>:<index>`, which makes the SSE `id:` field
// a complete cursor and lets a browser's automatic Last-Event-ID reconnection
// work with no client code at all.

// streamCursor is a reader's position: the last chunk it is known to have seen.
//
// ONE NUMBER, NOT TWO, and that is a measurement rather than a simplification.
// Every chunk is recorded at its own step and step increases for every event in
// a run, so it is already a complete, globally unique, strictly increasing
// cursor. The chunk's index is not: it restarts at 0 for each streaming call,
// so a cursor carrying it would resume in the wrong answer.
//
// The generation rides along so a reader can see that the run changed hands. It
// does not invalidate anything -- history is append-only, and a reclaimed run
// replays rather than rewriting its chunks -- so it is reported, not enforced.
type streamCursor struct {
	generation int64
	step       int
	present    bool
}

// parseStreamCursor reads a cursor from an SSE event id.
//
// A malformed id is NOT an error and NOT silently ignored: it yields no cursor,
// so the reader is served the whole stream from the beginning. Refusing would
// strand a client whose only way to recover is the thing being refused, and
// resuming from a position that was not asked for is how a transcript loses a
// paragraph nobody notices.
func parseStreamCursor(raw string) (streamCursor, bool) {
	parts := strings.Split(strings.TrimSpace(raw), ":")
	if len(parts) != 2 {
		return streamCursor{}, false
	}
	gen, err1 := strconv.ParseInt(parts[0], 10, 64)
	step, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || step < 0 {
		return streamCursor{}, false
	}
	return streamCursor{generation: gen, step: step, present: true}, true
}

// after reports whether a chunk is past the cursor and so still owed.
func (c streamCursor) after(step int) bool {
	if !c.present {
		return true
	}
	return step > c.step
}

// streamMode selects where a reader's tail comes from. cleat#1639.
//
// THE DEFAULT IS THE ONE THAT WORKS EVERYWHERE, and that is a deliberate
// reversal of the issue's leaning. The argument for making the durable tail
// opt-in is that it hides a real difference in latency and cost; the argument
// against is decisive, which is that cleat#1639 is a report that MOST readers
// get silence, and a fix nobody opts into fixes nobody. An existing client
// written against #1572 gets a working stream from this change with no client
// change at all.
//
// The difference is reported rather than hidden: `attached` carries `transport`
// and `live`, so a client can see exactly which tail it is on, and ?mode=live
// is there for a caller that would rather be refused than served slowly.
type streamMode string

const (
	// streamModeAuto uses the hub when this worker is executing the run and the
	// durable tail when it is not.
	streamModeAuto streamMode = ""
	// streamModeLive refuses the durable tail: if this worker does not hold the
	// run, history is served and the stream ends, which is the behaviour
	// #1572 shipped.
	streamModeLive streamMode = "live"
	// streamModePoll never touches the hub, on any worker. A caller that wants
	// one predictable latency and cost regardless of which worker it reached
	// asks for this.
	streamModePoll streamMode = "poll"
)

// parseStreamMode reads ?mode=, and REFUSES an unrecognised one.
//
// Deliberately unlike parseStreamCursor, which treats a malformed id as absent:
// a bad cursor arrives from a client trying to recover and refusing it would
// strand them, whereas a bad mode is a caller bug on a fresh request. Silently
// ignoring it would hand back exactly the silence this endpoint was changed to
// stop, and the caller would have no way to see why.
func parseStreamMode(raw string) (streamMode, bool) {
	switch streamMode(strings.TrimSpace(raw)) {
	case streamModeAuto:
		return streamModeAuto, true
	case streamModeLive:
		return streamModeLive, true
	case streamModePoll:
		return streamModePoll, true
	}
	return streamModeAuto, false
}

// defaultStreamPollInterval is the base gap between two reads of a followed
// run's chunks.
//
// 250ms is the issue's own figure and is a latency the reader SEES, unlike the
// heartbeat below. It is a base rather than a period: a tick that finds nothing
// backs off (see streamPollBackoffCap), and any chunk resets it, so an idle run
// costs a fraction of this and a producing one is read at this rate.
const defaultStreamPollInterval = 250 * time.Millisecond

// streamPollBackoffCap bounds that backoff.
//
// The worst case this sets is how long after a model's FIRST token a reader on
// another worker waits, having attached while the model was still thinking --
// the one case where backoff has had time to grow. Two seconds is the most this
// endpoint will add to a token that is already in the database.
const streamPollBackoffCap = 2 * time.Second

// streamPollPageSize bounds one read. A full page means there is more and the
// next read happens immediately rather than after an interval.
const streamPollPageSize = 500

// sseWriter is the SSE framing. Small enough to inline and separate enough to
// test without a workflow.
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func (s sseWriter) event(name, id string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if id != "" {
		if _, err := fmt.Fprintf(s.w, "id: %s\n", id); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, body); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

func (s sseWriter) comment(text string) error {
	if _, err := fmt.Fprintf(s.w, ": %s\n\n", text); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

// streamHeartbeat is how often a comment is written on an idle stream.
//
// Not decoration: proxies and load balancers close a connection that has been
// silent, and a model can think for longer than that before its first token. A
// comment line is the SSE-native keepalive and a client's event handler never
// sees it.
const streamHeartbeat = 15 * time.Second

// handleStreamWorkflow serves one run's stream chunks as server-sent events.
func (s *apiServer) handleStreamWorkflow(w http.ResponseWriter, r *http.Request, id string) {
	// AUTHORIZE BEFORE SUBSCRIBING. The hub is keyed by workflow id alone and
	// enforces nothing, so this lookup is the whole of the tenant check: a
	// scoped store answers nil for a run belonging to someone else, and
	// runExists turns that into the same 404 an unknown id gets. Subscribing
	// first and checking after would publish another tenant's tokens for as
	// long as the check took.
	st, ok := s.scopedStore(w, r)
	if !ok {
		return
	}
	wf, err := st.GetWorkflowByID(r.Context(), id)
	if err != nil {
		s.writeError(w, 500, err.Error())
		return
	}
	if wf == nil {
		s.writeError(w, 404, "workflow not found")
		return
	}

	// ?durable_only=true refuses to show a token that is not already in
	// event_history. cleat#1572.
	//
	// ON AN ORDINARY WORKER THIS CHANGES NOTHING, and saying so is the point:
	// chunks are persisted as they arrive and a refused write is never
	// published, so every live chunk is already durable. The option exists for
	// the deployment where that is not true -- --no-per-step-flush, where
	// events reach the database at segment end -- and for the caller who would
	// rather state the requirement than depend on a default staying put.
	durableOnly := r.URL.Query().Get("durable_only") == "true"

	mode, modeOK := parseStreamMode(r.URL.Query().Get("mode"))
	if !modeOK {
		s.writeError(w, http.StatusBadRequest,
			`mode must be "live", "poll", or absent (absent = the live tail when this `+
				`worker is executing the run, the durable tail when it is not)`)
		return
	}

	cursor, _ := parseStreamCursor(r.Header.Get("Last-Event-ID"))
	if !cursor.present {
		// Query-parameter spelling, for clients that are not an EventSource --
		// curl, a server-side consumer, a mobile client with its own retry.
		cursor, _ = parseStreamCursor(r.URL.Query().Get("last_event_id"))
	}

	flusher, isFlusher := w.(http.Flusher)
	if !isFlusher {
		s.writeError(w, 500, "streaming not supported by this server")
		return
	}

	// WHICH TAIL THIS READER GETS. The hub only ever has chunks for a run this
	// worker is executing, so ownership -- not merely the hub existing -- is
	// what makes a subscription worth taking.
	//
	// That is a change: #1572 subscribed whenever a hub was configured, which
	// on a non-executing worker took a reader slot for a subscription that
	// could never deliver a chunk, and then blocked in the live loop rather
	// than ending. The `end` event this endpoint's docs quote for that case is
	// reached only when no hub is configured at all.
	hub := s.streamHub
	owns := wf.AssignedTo != "" && s.workerID() != "" && wf.AssignedTo == s.workerID()
	tail, canTail := st.(engine.StreamChunkTailReader)

	useLive := hub != nil && owns && mode != streamModePoll
	usePoll := !useLive && canTail && mode != streamModeLive

	// SUBSCRIBE BEFORE READING HISTORY, and this order is the whole of the
	// no-gap guarantee. A chunk published between the history read and the
	// subscription would be in neither -- it is past the rows just read and
	// before the subscription existed -- and nothing downstream could detect
	// it, because the reader's cursor would advance right over it. Subscribing
	// first makes the two sources OVERLAP instead, and an overlap is removed
	// by the cursor comparison below.
	//
	// The durable tail needs no equivalent, and for a stronger reason than
	// ordering: it reads the same rows the history read did, from a cursor, so
	// a chunk written between the two reads is simply picked up by the next
	// one. There is no window to close because there is only one source.
	var sub *engine.StreamSub
	if useLive {
		sub, err = hub.Subscribe(id)
		if err != nil {
			// The ceiling, not a failure of this run. 503 with Retry-After is
			// the honest answer: the request is well-formed and may succeed
			// later.
			w.Header().Set("Retry-After", "5")
			s.writeError(w, http.StatusServiceUnavailable, fmt.Sprintf(
				"%v (%d attached now; raise --max-stream-readers)", err, hub.Readers()))
			return
		}
		defer sub.Close()
	}

	// The poll ceiling is taken BEFORE any body is written, for the same reason
	// the hub's is: once the 200 and the first event are out, the only way left
	// to refuse is to end the stream, which a client cannot tell from a run
	// that finished.
	if usePoll {
		if n := s.streamPollReaders.Add(1); s.maxStreamPollReaders > 0 && int(n) > s.maxStreamPollReaders {
			s.streamPollReaders.Add(-1)
			w.Header().Set("Retry-After", "5")
			// BOTH numbers, and the attached one is n-1 rather than n: n counts
			// this request, which is being refused. An operator who has just
			// been refused needs the count they were refused against, and a
			// ceiling on its own is a sentence they cannot act on -- the same
			// reason StreamHub.Readers exists for the message above.
			s.writeError(w, http.StatusServiceUnavailable, fmt.Sprintf(
				"too many readers following runs from event_history on this worker "+
					"(%d attached now, ceiling %d; raise --max-stream-poll-readers)",
				n-1, s.maxStreamPollReaders))
			return
		}
		defer s.streamPollReaders.Add(-1)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Nagle's algorithm's HTTP cousin: a buffering reverse proxy holds an SSE
	// body until it has enough of it, which for a token stream is forever.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	out := sseWriter{w: w, flusher: flusher}

	// The opening event tells the client what it is attached to.
	//
	// `live` keeps exactly the meaning #1572 gave it -- attached to the
	// in-memory tail on the executing worker -- so a client written against
	// that release still reads it correctly. What changed underneath is that
	// `live: false` no longer implies silence, and `transport` is the field
	// that says which tail is actually feeding this stream.
	transport := "none"
	switch {
	case useLive:
		transport = "live"
	case usePoll:
		transport = "poll"
	}
	if err := out.event("attached", "", map[string]any{
		"workflow_id":  id,
		"generation":   wf.Generation,
		"status":       wf.Status,
		"live":         useLive,
		"transport":    transport,
		"resumed":      cursor.present,
		"durable_only": durableOnly,
	}); err != nil {
		return
	}

	// ---- the durable part ----
	history, err := st.LoadEventHistory(r.Context(), id)
	if err != nil {
		msg := err.Error()
		if errors.Is(err, engine.ErrPayloadDecryption) {
			// This is the strict load, so an unreadable history is an error
			// rather than a transcript of "[DECRYPTION_FAILED]" chunks that
			// reads as an answer. The client gets a sentence it can act on,
			// not the driver's text (cleat#2311).
			msg = "this worker cannot read this workflow's history: it does not hold the payload encryption key that sealed it"
		}
		out.event("error", "", map[string]any{"message": msg})
		return
	}

	// SEGMENTATION IS THE READER'S JOB AND THE DATA CARRIES IT.
	//
	// lastStep is the cursor and moves monotonically. expectIndex is the only
	// per-answer state: chunk indices restart at 0 for each streaming call, so
	// an index of 0 opens a new answer and anything else must be exactly one
	// past the previous chunk of the same answer.
	lastStep := -1
	expectIndex := 0
	sawAny := false

	emit := func(step, index int, content string, finish, replay, durable bool, plug, fn string) bool {
		// GAP DETECTION, on the index rather than the step. Steps are not
		// contiguous across a run -- any other event takes one -- so a jump in
		// step means nothing. A jump in index within one answer means rows are
		// missing: history compaction folds old events into a compacted blob
		// and deletes the rows it folded. The client is told, rather than
		// handed a transcript with a hole in it that reads as continuous prose.
		//
		// Derived from the data, so it does not depend on this handler knowing
		// WHY rows are absent.
		if index > 0 && sawAny && index > expectIndex {
			out.event("gap", "", map[string]any{
				"from":    expectIndex,
				"to":      index - 1,
				"message": "chunks in this range are no longer in event_history and cannot be replayed",
			})
		}
		payload := map[string]any{
			"step":    step,
			"index":   index,
			"content": content,
			"finish":  finish,
			"replay":  replay,
			// An explicit boundary, so a client does not have to know that
			// index 0 means "a new answer starts here".
			"stream_start": index == 0,
			// Whether this chunk's row was in event_history when it was sent.
			// Always true for anything served FROM history, by construction.
			"durable": durable,
		}
		if plug != "" {
			payload["plugin"] = plug
			payload["func"] = fn
		}
		if err := out.event("chunk", streamEventID(wf.Generation, step), payload); err != nil {
			return false
		}
		lastStep = step
		expectIndex = index + 1
		sawAny = true
		return true
	}

	for _, rec := range history {
		if rec.EventType != engine.EventTypePluginCallStreamChunk {
			continue
		}
		if !cursor.after(rec.Step) {
			// Not owed, but it still advances the segmentation state: a gap
			// must be measured against the last chunk that EXISTS, not the
			// last one this reader happened to be sent.
			lastStep = rec.Step
			expectIndex = rec.StreamChunkIndex + 1
			sawAny = true
			continue
		}
		// durable=true unconditionally: this chunk was just READ from
		// event_history, so its presence there is not an inference.
		if !emit(rec.Step, rec.StreamChunkIndex, rec.PluginOutput, rec.StreamFinish, true, true,
			rec.PluginName, rec.PluginFunc) {
			return
		}
	}

	if isTerminalStatus(wf.Status) {
		out.event("end", "", map[string]any{"reason": "run is " + wf.Status})
		return
	}
	if usePoll {
		s.followFromHistory(r.Context(), out, st, tail, id, &lastStep, emit)
		return
	}

	if sub == nil {
		reason := "no live tail on this worker; the durable history above is complete as of now"
		switch {
		case mode == streamModeLive && !owns:
			// Asked for the live tail by name, on a worker that does not hold
			// the run. Named separately from the case below because the remedy
			// differs: this one is served by dropping ?mode=live, and the next
			// one is not served by anything the client can do.
			reason = "?mode=live was requested and this worker is not executing the run; " +
				"the durable history above is complete as of now. Retry without ?mode=live " +
				"to follow the run from event_history instead."
		case !canTail:
			reason = "no live tail on this worker and this store cannot follow a run from " +
				"event_history; the durable history above is complete as of now"
		}
		out.event("end", "", map[string]any{"reason": reason})
		return
	}

	// ---- the live part ----
	ticker := time.NewTicker(streamHeartbeat)
	defer ticker.Stop()
	ctx := r.Context()

	for {
		select {
		case <-ctx.Done():
			return

		case c, open := <-sub.Chunks():
			if !open {
				// Closed without the caller asking. The only producer of that
				// is the hub dropping a reader that fell behind, and it is
				// reported rather than ended quietly: a client that thinks the
				// model stopped and a client that knows to reconnect do
				// opposite things, and only one of them is right.
				if sub.Lagged() {
					out.event("lagged", "", map[string]any{
						"message": "this reader fell behind and was dropped; reconnect with " +
							"Last-Event-ID to be served the missing chunks from event_history",
					})
				}
				out.event("end", "", map[string]any{"reason": "live tail closed"})
				return
			}
			// Drop the overlap that subscribing before the history read
			// deliberately created. Step is monotonic, so this is one compare.
			if c.Step <= lastStep || !cursor.after(c.Step) {
				continue
			}
			if durableOnly && !c.Durable {
				// The reader asked for durable tokens only and this
				// deployment does not persist per step. Skipping rather than
				// waiting: the chunk WILL reach history at segment end, and a
				// reconnect serves it from there.
				continue
			}
			if !emit(c.Step, c.Index, c.Content, c.Finish, false, c.Durable, "", "") {
				return
			}

		case <-ticker.C:
			// The heartbeat doubles as the terminal-status poll. A run that
			// finishes without producing a final chunk -- it failed, it was
			// cancelled, its last streaming call was not the last thing it did
			// -- would otherwise leave the reader waiting on a stream nothing
			// will ever write to.
			cur, statusErr := st.GetWorkflowByID(ctx, id)
			if statusErr == nil && cur != nil && isTerminalStatus(cur.Status) {
				out.event("end", "", map[string]any{"reason": "run is " + cur.Status})
				return
			}
			if err := out.comment("keepalive"); err != nil {
				return
			}
		}
	}
}

// followFromHistory serves a run's remaining chunks by reading event_history
// past the reader's cursor, which every worker can do. cleat#1639.
//
// # The ordering that makes an ending run safe
//
// A run's last chunks are written before it finalizes, so a status read that
// observes a terminal state is followed by ONE more chunk read and that read
// sees everything. Doing it the other way round -- chunks, then status -- loses
// any chunk written between the two, and loses it silently, because the reader
// is gone before the next tick.
//
// # Why the status read is not on the poll interval
//
// It answers a different question and is needed far less often. A run that ends
// having produced a final chunk announces itself in the data: finish=true
// arrives on the poll interval like any other chunk. The status read exists for
// the run that ends WITHOUT one -- it failed, it was cancelled, its last
// streaming call was not the last thing it did -- and putting it on the poll
// interval would double this endpoint's query rate to shorten that case alone.
// It is on streamHeartbeat, which is what the live path already pays.
func (s *apiServer) followFromHistory(
	ctx context.Context,
	out sseWriter,
	st engine.WorkflowStore,
	tail engine.StreamChunkTailReader,
	id string,
	lastStep *int,
	emit func(step, index int, content string, finish, replay, durable bool, plug, fn string) bool,
) {
	base := s.streamPollInterval
	if base <= 0 {
		base = defaultStreamPollInterval
	}

	statusEvery := s.streamStatusInterval
	if statusEvery <= 0 {
		statusEvery = streamHeartbeat
	}

	interval := base
	lastStatusAt := time.Now()
	lastWriteAt := time.Now()

	timer := time.NewTimer(interval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		// Status first -- see the ordering note above.
		terminal := false
		var terminalStatus string
		if time.Since(lastStatusAt) >= statusEvery {
			lastStatusAt = time.Now()
			cur, statusErr := st.GetWorkflowByID(ctx, id)
			if statusErr == nil && cur != nil && isTerminalStatus(cur.Status) {
				terminal = true
				terminalStatus = cur.Status
			}
		}

		recs, err := tail.LoadStreamChunksAfter(ctx, id, *lastStep, streamPollPageSize)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			out.event("error", "", map[string]any{"message": err.Error()})
			return
		}
		for _, rec := range recs {
			// replay=true and durable=true, both unconditionally, because both
			// are facts about where the row came from: it was just READ from
			// event_history.
			//
			// replay is PROVENANCE, not novelty, and this is the case that
			// forces the distinction. The endpoint's docs gave both readings in
			// consecutive sentences, and they disagree here: a chunk on this
			// path is new to the client and came from history. The existing
			// code already resolved it towards provenance -- history past a
			// reconnecting reader's cursor is emitted replay=true and that
			// reader has never seen it -- so this follows suit rather than
			// giving the field a third meaning. A client deciding what it has
			// already rendered uses step, which is exact.
			if !emit(rec.Step, rec.StreamChunkIndex, rec.PluginOutput, rec.StreamFinish,
				true, true, rec.PluginName, rec.PluginFunc) {
				return
			}
			lastWriteAt = time.Now()
		}

		if terminal {
			out.event("end", "", map[string]any{"reason": "run is " + terminalStatus})
			return
		}

		switch {
		case len(recs) >= streamPollPageSize:
			// A full page means there is more behind it. Read again at once
			// rather than metering out a backlog one interval at a time.
			interval = 0
		case len(recs) > 0:
			interval = base
		default:
			interval *= 2
			if interval > streamPollBackoffCap {
				interval = streamPollBackoffCap
			}
		}

		// The keepalive is on elapsed time since the last WRITE, not on the
		// tick: this loop wakes far more often than the live path's ticker and
		// a comment per tick would be noise on the wire.
		if time.Since(lastWriteAt) >= streamHeartbeat {
			if err := out.comment("keepalive"); err != nil {
				return
			}
			lastWriteAt = time.Now()
		}

		timer.Reset(interval)
	}
}

func streamEventID(generation int64, step int) string {
	return fmt.Sprintf("%d:%d", generation, step)
}

// workerID names the worker this API server belongs to, or "" when it has no
// worker (which is every unit test that builds an apiServer directly).
func (s *apiServer) workerID() string {
	if s.worker == nil {
		return ""
	}
	return s.worker.id
}
