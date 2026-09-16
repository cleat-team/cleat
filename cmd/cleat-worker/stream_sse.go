package main

import (
	"encoding/json"
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
// worker executing the run, so a request landing on another worker gets the
// durable part and then waits, receiving nothing live. It is not wrong -- the
// history it served is complete up to the moment it read -- but it is a
// degraded stream, and the response says so rather than letting a client
// conclude the model stopped producing tokens. Routing a reader to the right
// worker is out of scope for v1.
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

	// SUBSCRIBE BEFORE READING HISTORY, and this order is the whole of the
	// no-gap guarantee. A chunk published between the history read and the
	// subscription would be in neither -- it is past the rows just read and
	// before the subscription existed -- and nothing downstream could detect
	// it, because the reader's cursor would advance right over it. Subscribing
	// first makes the two sources OVERLAP instead, and an overlap is removed
	// by the cursor comparison below.
	var sub *engine.StreamSub
	hub := s.streamHub
	if hub != nil {
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

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Nagle's algorithm's HTTP cousin: a buffering reverse proxy holds an SSE
	// body until it has enough of it, which for a token stream is forever.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	out := sseWriter{w: w, flusher: flusher}

	// The opening event tells the client what it is attached to. `live` is
	// false when this worker is not the one executing the run, in which case
	// history is served and nothing will follow it.
	live := hub != nil && wf.AssignedTo != "" && s.workerID() != "" && wf.AssignedTo == s.workerID()
	if err := out.event("attached", "", map[string]any{
		"workflow_id":  id,
		"generation":   wf.Generation,
		"status":       wf.Status,
		"live":         live,
		"resumed":      cursor.present,
		"durable_only": durableOnly,
	}); err != nil {
		return
	}

	// ---- the durable part ----
	history, err := st.LoadEventHistory(r.Context(), id)
	if err != nil {
		out.event("error", "", map[string]any{"message": err.Error()})
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
	if sub == nil {
		out.event("end", "", map[string]any{
			"reason": "no live tail on this worker; the durable history above is complete as of now",
		})
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
