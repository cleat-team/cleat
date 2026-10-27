package backendkit

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// StreamEvent is one server-sent event from a run's stream.
//
// The worker's events (cmd/cleat-worker/stream_sse.go) are "attached",
// "chunk", "gap", "lagged", "error" and "end", each with a JSON object as its
// data. ID is set on chunks: "<generation>:<step>:<index>", a complete cursor
// to resume from with StreamOptions.LastEventID.
type StreamEvent struct {
	ID    string
	Event string
	Data  string
}

// StreamOptions are StreamWorkflow's optional settings; the zero value
// streams the run from its beginning, live or polled.
type StreamOptions struct {
	// LastEventID resumes after that event: the worker serves what came after
	// it from the durable record, then joins the tail.
	LastEventID string
	// Mode "live" refuses the polled fallback (an end event instead); ""
	// takes whichever the worker can serve.
	Mode string
	// DurableOnly serves only what is already in the durable record.
	DurableOnly bool
}

// ErrStopStream may be returned by a StreamWorkflow callback to end the
// stream early; StreamWorkflow then returns nil.
var ErrStopStream = errors.New("stop stream")

// maxStreamLine bounds one SSE line. A chunk's data is a single JSON line,
// so this bounds the largest chunk a reader accepts.
const maxStreamLine = 4 << 20

// StreamWorkflow reads the run's server-sent event stream
// (GET /api/workflows/{id}/stream) and calls fn with each event, in order. It
// returns nil when the worker ends the stream or fn returns ErrStopStream,
// ctx's error when ctx ends, and otherwise the first error from the request,
// the read, or fn.
//
// The stream is not subject to HTTPClient's Timeout, which bounds a whole
// request including its body and would cut every stream off at it; ctx is
// what ends it. The transport, and the API key, are the client's own.
func (c *Client) StreamWorkflow(ctx context.Context, id string, opts StreamOptions, fn func(StreamEvent) error) error {
	q := url.Values{}
	if opts.Mode != "" {
		q.Set("mode", opts.Mode)
	}
	if opts.DurableOnly {
		q.Set("durable_only", "true")
	}
	u := c.BaseURL + "/api/workflows/" + url.PathEscape(id) + "/stream"
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	if opts.LastEventID != "" {
		req.Header.Set("Last-Event-ID", opts.LastEventID)
	}

	hc := *c.HTTPClient
	hc.Timeout = 0
	resp, err := c.send(&hc, req)
	if err != nil {
		return fmt.Errorf("stream workflow: request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("stream workflow: %w", classifyError(resp.StatusCode, body))
	}

	err = readSSE(resp.Body, fn)
	switch {
	case errors.Is(err, ErrStopStream):
		return nil
	case ctx.Err() != nil:
		return ctx.Err()
	}
	return err
}

// readSSE parses a text/event-stream (the WHATWG format: "field: value"
// lines, events ended by a blank line, data lines joined with "\n", lines
// starting with ":" ignored) and calls fn for each event that has data.
func readSSE(r io.Reader, fn func(StreamEvent) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxStreamLine)
	var ev StreamEvent
	var data []string
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if len(data) > 0 {
				ev.Data = strings.Join(data, "\n")
				if ev.Event == "" {
					ev.Event = "message"
				}
				if err := fn(ev); err != nil {
					return err
				}
			}
			ev, data = StreamEvent{}, nil
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "id":
			ev.ID = value
		case "event":
			ev.Event = value
		case "data":
			data = append(data, value)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read stream: %w", err)
	}
	return nil
}
