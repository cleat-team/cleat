package backendkit_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/cleat/backendkit"
)

// cleat#3368: the worker requires an API key and Client never sent one, so
// every example backend wrapped HTTPClient in its own Authorization
// RoundTripper. WithAPIKey makes the client send it on every request --
// typed methods, Health and the stream alike.
func TestWithAPIKeyAuthenticatesEveryRequest(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		switch {
		case strings.HasSuffix(r.URL.Path, "/stream"):
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: end\ndata: {}\n\n")
		case r.URL.Path == "/readyz":
			w.WriteHeader(http.StatusOK)
		default:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"status":"cancellation_requested","promise_id":"p1"}`)
		}
	}))
	defer srv.Close()

	c := backendkit.New(srv.URL, backendkit.WithAPIKey("cleat_sk_test"))
	ctx := context.Background()
	if err := c.SignalWorkflow(ctx, "wf-1", "go", "{}"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CancelWorkflow(ctx, "wf-1", backendkit.CancelOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Health(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.StreamWorkflow(ctx, "wf-1", backendkit.StreamOptions{}, func(backendkit.StreamEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d requests: %v", len(got), got)
	}
	for _, g := range got {
		if !strings.HasSuffix(g, " Bearer cleat_sk_test") {
			t.Errorf("request without the API key: %q", g)
		}
	}
}

// Without a key nothing is added; with one, PluginRoute -- the escape hatch
// for plugin-mounted routes -- carries it too.
func TestAPIKeyIsNotInventedAndReachesPluginRoutes(t *testing.T) {
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if _, err := backendkit.New(srv.URL).Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	if auth[0] != "" {
		t.Errorf("a client without a key sent Authorization %q", auth[0])
	}

	c := backendkit.New(srv.URL, backendkit.WithAPIKey("cleat_sk_test"))
	if err := c.PluginRoute(context.Background(), http.MethodGet, "/plug/x", nil, nil); err != nil {
		t.Fatal(err)
	}
	if auth[1] != "Bearer cleat_sk_test" {
		t.Errorf("PluginRoute sent Authorization %q", auth[1])
	}
}

func TestUpdateWorkflow(t *testing.T) {
	t.Run("sends the payload unparsed and returns the promise id", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/api/workflows/wf 1/update/set-price" {
				t.Errorf("%s %s", r.Method, r.URL.Path)
			}
			if r.URL.RawPath != "" && r.URL.RawPath != "/api/workflows/wf%201/update/set-price" {
				t.Errorf("raw path %s: the id was not escaped", r.URL.RawPath)
			}
			body, _ := io.ReadAll(r.Body)
			if string(body) != `{"price": 7}` {
				t.Errorf("body %q, want the payload byte for byte", body)
			}
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"promise_id":"upd-123"}`)
		}))
		defer srv.Close()

		id, err := backendkit.New(srv.URL).UpdateWorkflow(context.Background(), "wf 1", "set-price", []byte(`{"price": 7}`))
		if err != nil || id != "upd-123" {
			t.Fatalf("UpdateWorkflow = %q, %v", id, err)
		}
	})

	t.Run("a pending update of the same name is ErrUpdateAlreadyPending", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"error":"update already pending with name: set-price","detail":"update_already_pending"}`)
		}))
		defer srv.Close()

		_, err := backendkit.New(srv.URL).UpdateWorkflow(context.Background(), "wf-1", "set-price", nil)
		if !errors.Is(err, backendkit.ErrUpdateAlreadyPending) {
			t.Fatalf("err = %v, want ErrUpdateAlreadyPending", err)
		}
		var se *backendkit.UpstreamStatusError
		if !errors.As(err, &se) || se.Status != http.StatusConflict {
			t.Errorf("err = %v, want it to carry the 409 as an UpstreamStatusError", err)
		}
	})

	t.Run("a run that has ended is a plain 409", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"error":"workflow is done and cannot accept updates"}`)
		}))
		defer srv.Close()

		_, err := backendkit.New(srv.URL).UpdateWorkflow(context.Background(), "wf-1", "x", nil)
		var se *backendkit.UpstreamStatusError
		if errors.Is(err, backendkit.ErrUpdateAlreadyPending) || !errors.As(err, &se) || se.Status != http.StatusConflict {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestCancelWorkflow(t *testing.T) {
	for _, tc := range []struct {
		opts   backendkit.CancelOptions
		status string
	}{
		{backendkit.CancelOptions{}, "cancellation_requested"},
		{backendkit.CancelOptions{Reason: "operator", Preemptive: true}, "cancelled"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/api/workflows/wf-1/cancel" {
				t.Errorf("%s %s", r.Method, r.URL.Path)
			}
			var req struct {
				Reason     string `json:"reason"`
				Preemptive bool   `json:"preemptive"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("body: %v", err)
			}
			if req.Reason != tc.opts.Reason || req.Preemptive != tc.opts.Preemptive {
				t.Errorf("body %+v, want %+v", req, tc.opts)
			}
			status := "cancellation_requested"
			if req.Preemptive {
				status = "cancelled"
			}
			fmt.Fprintf(w, `{"status":%q}`, status)
		}))
		got, err := backendkit.New(srv.URL).CancelWorkflow(context.Background(), "wf-1", tc.opts)
		srv.Close()
		if err != nil || got != tc.status {
			t.Errorf("CancelWorkflow(%+v) = %q, %v; want %q", tc.opts, got, err, tc.status)
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"not found"}`)
	}))
	defer srv.Close()
	_, err := backendkit.New(srv.URL).CancelWorkflow(context.Background(), "nope", backendkit.CancelOptions{})
	var se *backendkit.UpstreamStatusError
	if !errors.As(err, &se) || se.Status != http.StatusNotFound {
		t.Errorf("err = %v, want a 404 UpstreamStatusError", err)
	}
}

func TestStreamWorkflow(t *testing.T) {
	t.Run("delivers each event with its id, in order, and ends with the stream", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/workflows/wf-1/stream" || r.URL.Query().Get("mode") != "live" ||
				r.URL.Query().Get("durable_only") != "true" || r.Header.Get("Last-Event-ID") != "1:4:0" {
				t.Errorf("request %s ?%s Last-Event-ID=%q", r.URL.Path, r.URL.RawQuery, r.Header.Get("Last-Event-ID"))
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, ": a comment\n\n")
			fmt.Fprint(w, "event: attached\ndata: {\"live\":true}\n\n")
			fmt.Fprint(w, "id: 1:5:0\nevent: chunk\ndata: {\"text\":\"a\"}\n\n")
			fmt.Fprint(w, "id: 1:5:1\nevent: chunk\ndata: line one\ndata: line two\n\n")
			fmt.Fprint(w, "event: end\ndata: {\"reason\":\"done\"}\n\n")
		}))
		defer srv.Close()

		var got []backendkit.StreamEvent
		err := backendkit.New(srv.URL).StreamWorkflow(context.Background(), "wf-1",
			backendkit.StreamOptions{LastEventID: "1:4:0", Mode: "live", DurableOnly: true},
			func(ev backendkit.StreamEvent) error { got = append(got, ev); return nil })
		if err != nil {
			t.Fatal(err)
		}
		want := []backendkit.StreamEvent{
			{Event: "attached", Data: `{"live":true}`},
			{ID: "1:5:0", Event: "chunk", Data: `{"text":"a"}`},
			{ID: "1:5:1", Event: "chunk", Data: "line one\nline two"},
			{Event: "end", Data: `{"reason":"done"}`},
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("events:\n got %+v\nwant %+v", got, want)
		}
	})

	t.Run("outlives HTTPClient's timeout", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: attached\ndata: {}\n\n")
			w.(http.Flusher).Flush()
			time.Sleep(300 * time.Millisecond)
			fmt.Fprint(w, "event: end\ndata: {}\n\n")
		}))
		defer srv.Close()

		c := backendkit.New(srv.URL)
		c.HTTPClient.Timeout = 100 * time.Millisecond
		n := 0
		err := c.StreamWorkflow(context.Background(), "wf-1", backendkit.StreamOptions{},
			func(backendkit.StreamEvent) error { n++; return nil })
		if err != nil || n != 2 {
			t.Fatalf("got %d events, err %v: a stream was cut off by the request timeout", n, err)
		}
		if c.HTTPClient.Timeout != 100*time.Millisecond {
			t.Errorf("StreamWorkflow changed the client's own timeout to %v", c.HTTPClient.Timeout)
		}
	})

	t.Run("ErrStopStream ends it early without an error; another callback error is returned", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			for i := 0; i < 5; i++ {
				fmt.Fprintf(w, "event: chunk\ndata: %d\n\n", i)
			}
		}))
		defer srv.Close()

		c := backendkit.New(srv.URL)
		n := 0
		err := c.StreamWorkflow(context.Background(), "wf-1", backendkit.StreamOptions{}, func(backendkit.StreamEvent) error {
			if n++; n == 2 {
				return backendkit.ErrStopStream
			}
			return nil
		})
		if err != nil || n != 2 {
			t.Errorf("stopped after %d events with %v", n, err)
		}
		boom := errors.New("boom")
		err = c.StreamWorkflow(context.Background(), "wf-1", backendkit.StreamOptions{},
			func(backendkit.StreamEvent) error { return boom })
		if !errors.Is(err, boom) {
			t.Errorf("err = %v, want the callback's", err)
		}
	})

	t.Run("a refusal is classified like any other request's", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"not found"}`)
		}))
		defer srv.Close()
		err := backendkit.New(srv.URL).StreamWorkflow(context.Background(), "nope", backendkit.StreamOptions{},
			func(backendkit.StreamEvent) error { return nil })
		var se *backendkit.UpstreamStatusError
		if !errors.As(err, &se) || se.Status != http.StatusNotFound {
			t.Errorf("err = %v, want a 404 UpstreamStatusError", err)
		}
	})

	t.Run("cancelling ctx ends it with ctx's error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: attached\ndata: {}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}))
		defer srv.Close()
		ctx, cancel := context.WithCancel(context.Background())
		err := backendkit.New(srv.URL).StreamWorkflow(ctx, "wf-1", backendkit.StreamOptions{},
			func(backendkit.StreamEvent) error { cancel(); return nil })
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	})
}
