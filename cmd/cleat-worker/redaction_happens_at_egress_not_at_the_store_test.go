package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// cleat#3581, owner decision D49-1a: workflow data is stored and delivered as
// sent, and redacted only where it leaves the system.
//
// Every redaction assertion here is paired with a control that engine.Redact
// REWRITES the body in use. The first measurement of this behaviour used a body
// redaction leaves alone and "proved" nothing, so a body that stops being
// redactable fails the test instead of passing it.

// egressSecret carries a secret under a key Redact always rewrites, beside two
// values it must keep (a version and a token COUNT: the cases #3581 measured).
const egressSecret = `{"api_key":"sk-live-SECRET123","version":"1.2.3","max_tokens":5}`

func requireRedactable(t *testing.T, body string) {
	t.Helper()
	if got := engine.Redact(body); got == body || strings.Contains(got, "sk-live-SECRET123") {
		t.Fatalf("CONTROL: engine.Redact does not rewrite the secret in %s (got %s), so a test that uses it says nothing about redaction", body, got)
	}
}

// assertRedactedKeepingData is the shape of every observability assertion.
func assertRedactedKeepingData(t *testing.T, what, body string) {
	t.Helper()
	if strings.Contains(body, "sk-live-SECRET123") {
		t.Errorf("%s leaks the secret:\n%s", what, body)
	}
	if !strings.Contains(body, "[REDACTED]") {
		t.Errorf("%s shows no [REDACTED] marker, so nothing was redacted:\n%s", what, body)
	}
	// Redaction removes the secret and nothing else: the version and the count
	// that the old matchers rewrote must come through.
	if !strings.Contains(body, `1.2.3`) || !strings.Contains(body, `max_tokens`) {
		t.Errorf("%s lost data redaction should have kept:\n%s", what, body)
	}
}

// ---------------------------------------------------------------------------
// Delivery: a workflow is handed what the caller sent.
// ---------------------------------------------------------------------------

func TestWorkflowDataIsStoredAndDeliveredAsSent(t *testing.T) {
	requireRedactable(t, egressSecret)

	// The first row is the one that fails if the write-time redaction returns;
	// the rest are #3581's measured table, which the matchers no longer rewrite
	// either, so they hold on either side of that.
	for _, in := range []string{
		egressSecret,
		`{"version":"1.2.3"}`,
		`{"name":"a.b.c"}`,
		`{"note":"see v1.2.3 notes","file":"report.final.pdf","host":"api.example.com"}`,
		`{"max_tokens":5}`,
		`{"token_usage":{"input_tokens":3,"output_tokens":4}}`,
	} {
		t.Run("start input "+in[:min(len(in), 24)], func(t *testing.T) {
			var stored string
			ms := &mockStore{}
			ms.listVersionsFn = func(context.Context, string) ([]int, error) { return []int{1}, nil }
			ms.startNewRunFn = func(_ context.Context, _, _ string, _ int, input json.RawMessage, _, _ string, _ int) (string, bool, error) {
				stored = string(input)
				return "wf-new-1", false, nil
			}
			api := newTestAPIServer(ms)

			req := httptest.NewRequest(http.MethodPost, "/api/workflows/my-wf/start",
				strings.NewReader(`{"input":`+in+`}`))
			rec := httptest.NewRecorder()
			api.handleStartWorkflow(rec, req, "my-wf")

			if rec.Code != 201 {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if stored != in {
				t.Errorf("the store was given a different input than the caller sent:\n sent  %s\n given %s", in, stored)
			}
		})

		t.Run("signal payload "+in[:min(len(in), 24)], func(t *testing.T) {
			var delivered string
			ms := &mockStore{}
			ms.deliverSignalFn = func(_ context.Context, _, _, payload string) error {
				delivered = payload
				return nil
			}
			api := newTestAPIServer(ms)

			body, _ := json.Marshal(map[string]string{"signal_name": "approve", "payload": in})
			req := httptest.NewRequest(http.MethodPost, "/api/workflows/wf-target/signal", strings.NewReader(string(body)))
			rec := httptest.NewRecorder()
			api.handleWorkflows(rec, req)

			if rec.Code != 200 {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if delivered != in {
				t.Errorf("the signal was delivered with a different payload than the caller sent:\n sent      %s\n delivered %s", in, delivered)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Observability: the history and event views, the stream, and the choke point.
// ---------------------------------------------------------------------------

func recordWithSecretEverywhere() engine.EventRecord {
	return engine.EventRecord{
		Step: 3, EventType: engine.EventTypeCall, Service: "billing", Op: "charge",
		Request: egressSecret, Response: egressSecret, Err: egressSecret,
		SignalPayload: egressSecret, PromiseResult: egressSecret, ChildInput: egressSecret,
		PluginOutput: egressSecret, FetchHeaders: egressSecret, FetchBody: egressSecret,
		SideEffectResult: egressSecret, UpdatePayload: egressSecret,
	}
}

func observabilityFixture() *mockStore {
	ms := &mockStore{}
	ms.getWorkflowByIDFn = func(context.Context, string) (*engine.WorkflowInstance, error) {
		return &engine.WorkflowInstance{ID: "wf-1", Status: "done"}, nil
	}
	ms.countEventHistoryFn = func(context.Context, string) (int, error) { return 1, nil }
	ms.loadEventHistoryPaginatedFn = func(context.Context, string, int, int) ([]engine.EventRecord, error) {
		return []engine.EventRecord{recordWithSecretEverywhere()}, nil
	}
	return ms
}

func TestTheHistoryAndEventsAPIsRedactWhatTheyServe(t *testing.T) {
	requireRedactable(t, egressSecret)
	api := newTestAPIServer(observabilityFixture())

	for _, tc := range []struct {
		name, path string
		serve      func(http.ResponseWriter, *http.Request)
	}{
		{"GET /api/workflows/{id}/history", "/api/workflows/wf-1/history", api.handleWorkflows},
		{"GET /api/instances/{id}/events", "/api/instances/wf-1/events", api.handleInstancesRoutes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.serve(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rec.Code != 200 {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			assertRedactedKeepingData(t, tc.name, rec.Body.String())
			// And ?redact=false cannot be used to ask for the raw record: these
			// routes do not read the flag at all.
			rec = httptest.NewRecorder()
			tc.serve(rec, httptest.NewRequest(http.MethodGet, tc.path+"?redact=false", nil))
			assertRedactedKeepingData(t, tc.name+" with ?redact=false", rec.Body.String())
		})
	}
}

// The choke point: an event record leaves through writeJSON, so redacting its
// type there covers a handler nobody has written yet. A data-plane type is not
// touched, which is what makes the line between the two a tested fact.
func TestTheResponseWriterRedactsEventRecordsAndLeavesTheDataPlaneAlone(t *testing.T) {
	requireRedactable(t, egressSecret)
	api := newTestAPIServer(&mockStore{})
	rec := recordWithSecretEverywhere()

	for name, v := range map[string]any{
		"an EventRecord":  rec,
		"a *EventRecord":  &rec,
		"a []EventRecord": []engine.EventRecord{rec},
	} {
		w := httptest.NewRecorder()
		api.writeJSON(w, 200, v)
		assertRedactedKeepingData(t, "writeJSON of "+name, w.Body.String())
	}
	// writeJSON copied: the record the caller holds is what replay would read.
	if rec.Response != egressSecret {
		t.Error("writeJSON modified the record it was given")
	}
	// A nil slice still encodes as null.
	w := httptest.NewRecorder()
	api.writeJSON(w, 200, []engine.EventRecord(nil))
	if strings.TrimSpace(w.Body.String()) != "null" {
		t.Errorf("a nil []EventRecord encoded as %q, want null", w.Body.String())
	}

	// The data plane is NOT in the switch.
	w = httptest.NewRecorder()
	api.writeJSON(w, 200, engine.WorkflowInstance{ID: "wf-1", Result: egressSecret})
	if !strings.Contains(w.Body.String(), "sk-live-SECRET123") {
		t.Errorf("writeJSON redacted a WorkflowInstance, which is data-plane: %s", w.Body.String())
	}
}

func TestTheStreamRedactsEveryChunkItSends(t *testing.T) {
	requireRedactable(t, egressSecret)

	t.Run("served from history", func(t *testing.T) {
		f := newStreamFixture(t, runningRun(), []engine.EventRecord{chunkRec(10, 0, egressSecret, true)})
		rec, cancel, done := f.start(t, "/api/workflows/wf-1/stream?durable_only=true", nil)
		defer func() { cancel(); <-done }()

		evs := collectEvents(rec, 2, 5*time.Second)
		if len(chunkEvents(evs)) != 1 {
			t.Fatalf("got %d chunks, want 1: %v", len(chunkEvents(evs)), evs)
		}
		assertRedactedKeepingData(t, "a chunk served from history", rec.body())
	})

	t.Run("relayed live", func(t *testing.T) {
		f := newStreamFixture(t, runningRun(), nil)
		rec, cancel, done := f.start(t, "/api/workflows/wf-1/stream", nil)
		defer func() { cancel(); <-done }()
		waitForEvents(t, rec, 1)

		f.hub.Publish("wf-1", engine.LiveChunk{Step: 0, Index: 0, Content: egressSecret, Finish: true})
		evs := waitForEvents(t, rec, 2)
		if len(chunkEvents(evs)) != 1 {
			t.Fatalf("got %d chunks, want 1: %v", len(chunkEvents(evs)), evs)
		}
		assertRedactedKeepingData(t, "a live chunk", rec.body())
	})
}

// ---------------------------------------------------------------------------
// The data plane: a tenant reading its own workflow's data gets it as stored,
// unless it asks for the redacted view, which can only add redaction.
// ---------------------------------------------------------------------------

func dataPlaneFixture() *mockStore {
	wf := func() *engine.WorkflowInstance {
		return &engine.WorkflowInstance{
			ID: "wf-1", Status: "done", Input: json.RawMessage(egressSecret),
			Result: egressSecret, Error: `{"password":"hunter2","why":"bad"}`,
		}
	}
	ms := &mockStore{}
	ms.getWorkflowByIDFn = func(context.Context, string) (*engine.WorkflowInstance, error) { return wf(), nil }
	ms.listWorkflowsFn = func(context.Context, engine.WorkflowFilter) ([]engine.WorkflowInstance, error) {
		return []engine.WorkflowInstance{*wf()}, nil
	}
	ms.getQueryStateFn = func(context.Context, string, string) (string, error) { return egressSecret, nil }
	ms.getTerminalRunFn = func(context.Context, string) (*engine.WorkflowInstance, error) { return wf(), nil }
	ms.listQueryStateFn = func(context.Context, string) (map[string]string, error) {
		return map[string]string{"k": egressSecret}, nil
	}
	return ms
}

func TestADataPlaneReadIsUnredactedUnlessTheCallerAsks(t *testing.T) {
	requireRedactable(t, egressSecret)
	api := newTestAPIServer(dataPlaneFixture())

	for _, tc := range []struct {
		name, path string
		serve      func(http.ResponseWriter, *http.Request)
	}{
		{"GET /api/workflows/{id}", "/api/workflows/wf-1", api.handleWorkflows},
		{"GET /api/instances/{id}", "/api/instances/wf-1", api.handleInstancesRoutes},
		{"GET /api/workflows (list)", "/api/workflows", api.handleWorkflowsList},
		{"GET /api/workflows/{id}/terminal", "/api/workflows/wf-1/terminal", api.handleWorkflows},
		{"GET /api/workflows/{id}/query?key=", "/api/workflows/wf-1/query?key=k", api.handleWorkflows},
		{"GET /api/workflows/{id}/query (every key)", "/api/workflows/wf-1/query", api.handleWorkflows},
	} {
		t.Run(tc.name, func(t *testing.T) {
			get := func(suffix string) string {
				sep := "?"
				if strings.Contains(tc.path, "?") {
					sep = "&"
				}
				rec := httptest.NewRecorder()
				tc.serve(rec, httptest.NewRequest(http.MethodGet, tc.path+suffixWith(sep, suffix), nil))
				if rec.Code != 200 {
					t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
				}
				return rec.Body.String()
			}

			// The default is the data plane: what the workflow produced.
			if body := get(""); !strings.Contains(body, "sk-live-SECRET123") {
				t.Errorf("the default read was redacted, but a service reading its workflow's result gets it as stored:\n%s", body)
			}
			// Asking adds redaction.
			assertRedactedKeepingData(t, tc.name+" with ?redact=true", get("redact=true"))
			// Nothing but the exact word "true" asks, so no value can be mistaken
			// for a request to REMOVE redaction from a redacted view.
			for _, v := range []string{"false", "", "1", "TRUE", "yes"} {
				if body := get("redact=" + v); !strings.Contains(body, "sk-live-SECRET123") {
					t.Errorf("?redact=%s redacted the read; only the exact word \"true\" does", v)
				}
			}
		})
	}
}

func suffixWith(sep, suffix string) string {
	if suffix == "" {
		return ""
	}
	return sep + suffix
}

// ---------------------------------------------------------------------------
// The census: a handler that writes JSON around the choke point is a hole.
// ---------------------------------------------------------------------------

// TestNoHandlerSerialisesJSONAroundTheResponseWriter fails when a non-test file in
// this package serialises JSON (json.NewEncoder, json.Marshal, json.MarshalIndent)
// anywhere it is not known to, or more times than it is known to. The choke point in
// writeJSON redacts event records, so a handler that serialises its own response can
// leak them without anything failing; this makes that a decision. Each exception is
// named with what it writes and why it carries no event data.
//
// The sites are COUNTED, not looked up in a set. A set cannot tell one occurrence of a
// line from two, so a second `json.NewEncoder(w).Encode(v)` pasted into server.go beside
// writeJSON's own would have passed (measured on review of cleat#3593).
//
// What this does NOT see: a response built another way (fmt.Fprintf of a struct, a bare
// w.Write of bytes from elsewhere) and any serialiser outside this package. It is a
// census of the two ways this package serialises JSON today, not a proof that no
// handler can emit an event record unredacted.
func TestNoHandlerSerialisesJSONAroundTheResponseWriter(t *testing.T) {
	type site struct {
		count int
		why   string
	}
	known := map[string]site{
		"app.go: json.NewEncoder(w).Encode(map[string]string{\"error\": \"not found\"})":       {1, "an error body"},
		"internal_holds.go: _ = json.NewEncoder(rw).Encode(internalHoldsResponse{Held: held})": {1, "concurrency-key holds, no event data"},
		"api_admin_recovery_key.go: if b, err := json.Marshal(doc); err == nil {":              {1, "noteRecoveryAudit: the audit row's metadata document ({reason, key_id|refused}), stored on the tenant's chain and hash-covered; not an HTTP response, carries no event record, and the reason was refused if it held a key-shaped string"},
		"main.go: json.NewEncoder(w).Encode(statuses)":                                         {1, "component health statuses"},
		"server.go: json.NewEncoder(w).Encode(v)":                                              {1, "writeJSON itself, the choke point"},
		"server.go: json.NewEncoder(w).Encode(map[string]any{":                                 {1, "a fixed-shape response; read it before adding data to it"},
		"setup.go: result, _ := json.Marshal(map[string]any{":                                  {1, "the durable fetch's result, handed back to the guest and recorded as the call's response; not an HTTP response"},
		"setup.go: input, _ = json.Marshal(m)":                                                 {1, "a schedule's input with its entry point added, handed to the new workflow and stored as sent"},
		"stream_sse.go: body, err := json.Marshal(payload)":                                    {1, "sseWriter.event, the SSE sink; content is redacted in emit before it reaches here"},
	}
	needles := []string{"json.NewEncoder(", "json.Marshal(", "json.MarshalIndent("}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]int{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(src), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			for _, n := range needles {
				if strings.Contains(trimmed, n) {
					found[f+": "+trimmed]++
					break
				}
			}
		}
	}
	if len(found) == 0 {
		t.Fatal("CONTROL: found no JSON serialiser call at all, so this census read nothing")
	}
	keys := make([]string, 0, len(found))
	for k := range found {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, line := range keys {
		k, ok := known[line]
		switch {
		case !ok:
			t.Errorf("a JSON serialiser outside the known sites: %s\nIf it can carry an event record, route it through writeJSON, which redacts them (cleat#3581). If it cannot, add it to this census with the reason.", line)
		case found[line] != k.count:
			t.Errorf("%s appears %d time(s), and this census knows of %d (%s).\nA second copy is a second place that serialises; decide it, do not let it ride on the first.", line, found[line], k.count, k.why)
		}
	}
	for line := range known {
		if found[line] == 0 {
			t.Errorf("this census names %q, which no longer exists: remove it, or the list stops meaning anything", line)
		}
	}
}
