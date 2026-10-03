package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
)

// fakeCleat stands in for cleat-worker. It exists to make two things assertable
// that a real worker cannot: WHICH paths the proxy asked for, and EXACTLY what
// headers it sent.
//
// The path allowlist is the point of the first test. cleat#1982 acceptance
// clause 2 -- "a workflow in a non-exposed class never appears in tools/list" --
// is satisfied here because the proxy never asks anything that could disclose
// one: it reads the document the API already filtered. A test that only checked
// the returned list would pass just as well against a proxy that read
// workflow_defs directly, so the assertion is on the requests.
type fakeCleat struct {
	mu    sync.Mutex
	paths []string
	keys  []string // Idempotency-Key header, in order
	runs  map[string]string
	next  int

	doc map[string]any
	// startStatus, when non-zero, is returned for any start regardless of name.
	startStatus int
	// runStatus/runResult shape the GET-a-run response the task routes read.
	runStatus string
	runResult string
	// cancels counts POSTs to a /cancel path.
	cancels int
	// traceparents records the inbound-visible header on every request, so a
	// test can assert what the proxy actually SENT rather than that it calls a
	// helper.
	traceparents []string
}

func newFakeCleat() *fakeCleat {
	return &fakeCleat{
		runs: map[string]string{},
		doc: map[string]any{
			"paths": map[string]any{
				"/api/workflows/checkout/start": map[string]any{"post": map[string]any{
					"description": "Start the checkout workflow.",
					"requestBody": map[string]any{"content": map[string]any{"application/json": map[string]any{
						"schema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"entry_point": map[string]any{"type": "string", "enum": []any{"checkout"}},
								"input": map[string]any{"type": "object", "properties": map[string]any{
									"count": map[string]any{"type": "integer"},
								}},
							},
						},
					}}},
				}},
				// A definition with no emitter: the document already chose the
				// passthrough, and the proxy must not upgrade it.
				"/api/workflows/plain/start": map[string]any{"post": map[string]any{
					"requestBody": map[string]any{"content": map[string]any{"application/json": map[string]any{
						"schema": map[string]any{"type": "object"},
					}}},
				}},
				// Not a start path. Recognising start paths and nothing else is
				// what keeps this from inventing tools out of unrelated routes.
				"/api/workflows/{id}/cancel":   map[string]any{"post": map[string]any{}},
				"/api/workflows/checkout/tags": map[string]any{"post": map[string]any{}},
			},
		},
	}
}

func (f *fakeCleat) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths = append(f.paths, r.URL.Path)
	f.traceparents = append(f.traceparents, r.Header.Get("traceparent"))

	switch {
	case r.URL.Path == "/api/openapi.json":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(f.doc)
	case strings.HasSuffix(r.URL.Path, "/start") && r.Method == http.MethodPost:
		key := r.Header.Get("Idempotency-Key")
		f.keys = append(f.keys, key)
		if f.startStatus != 0 {
			w.WriteHeader(f.startStatus)
			_, _ = w.Write([]byte(`{"error":"refused by the API"}`))
			return
		}
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/workflows/"), "/start")
		if name != "checkout" && name != "plain" {
			// The API's own refusal, which is what an internal or absent
			// definition gets. The proxy must relay it, not pre-empt it.
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"workflow not found"}`))
			return
		}
		id, ok := f.runs[key]
		if !ok {
			f.next++
			id = fmt.Sprintf("run-%d", f.next)
			f.runs[key] = id
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(`{"id":%q}`, id)))
	case strings.HasSuffix(r.URL.Path, "/cancel") && r.Method == http.MethodPost:
		f.cancels++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"run-1","status":"cancelled"}`))
	case strings.HasPrefix(r.URL.Path, "/api/workflows/") && r.Method == http.MethodGet:
		status := f.runStatus
		if status == "" {
			status = "done"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(`{"id":"run-1","status":%q,"result":%q}`,
			status, f.runResult)))
	default:
		// Any other path is the proxy reaching somewhere it should not. Answer
		// 404 AND fail the test, so an added data source cannot pass quietly.
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"unexpected path"}`))
	}
}

func (f *fakeCleat) requestedPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...)
}

func (f *fakeCleat) startKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.keys...)
}

// newProxyPair wires a proxy to a fake and returns both, plus the proxy's server.
func newProxyPair(t *testing.T) (*proxy, *fakeCleat) {
	t.Helper()
	f := newFakeCleat()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &proxy{api: srv.URL, http: srv.Client()}, f
}

func call(t *testing.T, p *proxy, method string, params any, headers map[string]string) map[string]any {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(body)))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %v\nbody: %s", err, rec.Body.String())
	}
	return out
}

func toolNames(t *testing.T, resp map[string]any) []string {
	t.Helper()
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", resp)
	}
	raw, _ := result["tools"].([]any)
	names := make([]string, 0, len(raw))
	for _, r := range raw {
		tm, _ := r.(map[string]any)
		n, _ := tm["name"].(string)
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ---- acceptance clause 2: the exposure decision is the API's -------------

// TestTheToolListIsExactlyTheDocumentsStartPaths pins the invariant that makes
// clause 2 hold by construction: the tool list is a projection of
// /api/openapi.json and nothing else, so an `internal` definition is absent for
// the same reason it is absent from that document.
func TestTheToolListIsExactlyTheDocumentsStartPaths(t *testing.T) {
	p, f := newProxyPair(t)

	resp := call(t, p, "tools/list", map[string]any{}, nil)
	got := toolNames(t, resp)
	want := []string{"checkout", "plain"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("tools = %v, want %v", got, want)
	}

	// The load-bearing half: the proxy asked ONLY for the document. If it ever
	// gained a second source -- reading workflow_defs, listing defs over HTTP,
	// anything that could see an internal definition -- that request appears
	// here and this fails.
	for _, path := range f.requestedPaths() {
		if path != "/api/openapi.json" {
			t.Errorf("tools/list requested %q; the exposure decision must come from the document alone", path)
		}
	}
}

// TestACallToANameTheDocumentOmittedIsRefusedByTheAPI covers the second half of
// clause 2: calling a non-exposed workflow BY NAME fails. The refusal must be
// the API's 404 relayed as a tool execution error -- not a proxy-local rule,
// which would be a second implementation of the class decision.
func TestACallToANameTheDocumentOmittedIsRefusedByTheAPI(t *testing.T) {
	p, _ := newProxyPair(t)

	resp := call(t, p, "tools/call", map[string]any{
		"name":      "an-internal-workflow",
		"arguments": map[string]any{"input": map[string]any{}},
	}, nil)

	result, _ := resp["result"].(map[string]any)
	if result == nil {
		t.Fatalf("expected a result carrying isError, got %v", resp)
	}
	if isErr, _ := result["isError"].(bool); !isErr {
		t.Fatalf("a refused start must be a tool execution error; result = %v", result)
	}
	text := firstText(t, result)
	if !strings.Contains(text, "workflow not found") {
		t.Errorf("relayed message = %q, want the API's own refusal", text)
	}
	if !strings.Contains(text, "404") {
		t.Errorf("relayed message = %q, want the API's status", text)
	}
}

func firstText(t *testing.T, result map[string]any) string {
	t.Helper()
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("no content in %v", result)
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	return text
}

// ---- acceptance clause 3: a retry starts exactly one run ----------------

func TestARetriedCallStartsExactlyOneRun(t *testing.T) {
	p, f := newProxyPair(t)

	args := map[string]any{"input": map[string]any{"count": 3}}
	first := call(t, p, "tools/call", map[string]any{"name": "checkout", "arguments": args}, nil)
	second := call(t, p, "tools/call", map[string]any{"name": "checkout", "arguments": args}, nil)

	idOf := func(resp map[string]any) string {
		result, _ := resp["result"].(map[string]any)
		sc, _ := result["structuredContent"].(map[string]any)
		id, _ := sc["run_id"].(string)
		return id
	}
	if a, b := idOf(first), idOf(second); a == "" || a != b {
		t.Fatalf("retry returned run %q, first returned %q; a retry must return the SAME run", b, a)
	}

	keys := f.startKeys()
	if len(keys) != 2 {
		t.Fatalf("expected 2 start calls, got %d", len(keys))
	}
	if keys[0] == "" || keys[0] != keys[1] {
		t.Fatalf("derived keys differ across a retry: %q vs %q", keys[0], keys[1])
	}
}

// TestTheDerivedKeyIsStableAcrossArgumentOrder is the reason canonicalJSON
// exists. Same arguments, different field order, must be the SAME call -- a
// client that reorders keys is not making a new request.
func TestTheDerivedKeyIsStableAcrossArgumentOrder(t *testing.T) {
	p, f := newProxyPair(t)

	call(t, p, "tools/call", map[string]any{"name": "checkout",
		"arguments": map[string]any{"a": 1, "b": 2}}, nil)
	call(t, p, "tools/call", map[string]any{"name": "checkout",
		"arguments": map[string]any{"b": 2, "a": 1}}, nil)

	keys := f.startKeys()
	if len(keys) != 2 || keys[0] != keys[1] {
		t.Fatalf("key depends on field order: %v", keys)
	}
}

// TestTheDerivedKeyIsScopedToTheCaller guards a disclosure, not a duplicate.
// Without the caller in the hash, two tenants calling the same tool with the
// same arguments compute the same key -- and cleat's dedupe would hand the
// second caller the FIRST caller's run id.
func TestTheDerivedKeyIsScopedToTheCaller(t *testing.T) {
	p, f := newProxyPair(t)

	args := map[string]any{"input": map[string]any{"count": 3}}
	call(t, p, "tools/call", map[string]any{"name": "checkout", "arguments": args},
		map[string]string{"Authorization": "Bearer tenant-a"})
	call(t, p, "tools/call", map[string]any{"name": "checkout", "arguments": args},
		map[string]string{"Authorization": "Bearer tenant-b"})

	keys := f.startKeys()
	if len(keys) != 2 {
		t.Fatalf("expected 2 starts, got %d", len(keys))
	}
	if keys[0] == keys[1] {
		t.Fatalf("two callers derived the SAME key %q; one tenant would receive the other's run id", keys[0])
	}
}

// TestAnExplicitKeyOverridesTheDerivationAndNeverEntersTheBody: the override is
// what lets a caller start a SECOND run on purpose. It must reach cleat as the
// header and must NOT be forwarded inside `input`, where it would become
// workflow data.
func TestAnExplicitKeyOverridesTheDerivationAndNeverEntersTheBody(t *testing.T) {
	f := newFakeCleat()
	var gotBody string
	var mu sync.Mutex
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/openapi.json" {
			f.ServeHTTP(w, r)
			return
		}
		buf, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = string(buf)
		mu.Unlock()
		f.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	p := &proxy{api: srv.URL, http: srv.Client()}

	call(t, p, "tools/call", map[string]any{"name": "checkout",
		"arguments": map[string]any{"idempotency_key": "explicit-123",
			"input": map[string]any{"count": 1}}}, nil)

	keys := f.startKeys()
	if len(keys) != 1 || keys[0] != "explicit-123" {
		t.Fatalf("Idempotency-Key = %v, want the explicit key", keys)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(gotBody, "explicit-123") {
		t.Fatalf("the idempotency key was forwarded inside the start body: %s", gotBody)
	}
}

// ---- the annotation direction guard -------------------------------------

// TestNoWorkflowToolAdvertisesReadOnly is the guard on the ONE direction that
// would be harmful. `readOnlyHint: true` tells a client it need not confirm, so
// on the surface the owner called a prompt-injection path it is the annotation
// that would remove the human. Everything else in the annotation block is a
// tuning choice; this is a failure.
func TestNoWorkflowToolAdvertisesReadOnly(t *testing.T) {
	p, _ := newProxyPair(t)
	resp := call(t, p, "tools/list", map[string]any{}, nil)
	result, _ := resp["result"].(map[string]any)
	raw, _ := result["tools"].([]any)
	if len(raw) == 0 {
		t.Fatal("no tools to check; the assertion below would be vacuous")
	}
	for _, r := range raw {
		tm, _ := r.(map[string]any)
		ann, _ := tm["annotations"].(map[string]any)
		if ann == nil {
			t.Fatalf("tool %v carries no annotations", tm["name"])
		}
		if ro, _ := ann["readOnlyHint"].(bool); ro {
			t.Errorf("tool %v advertises readOnlyHint:true, which suppresses the client's confirmation prompt", tm["name"])
		}
		if idem, ok := ann["idempotentHint"].(bool); !ok || !idem {
			t.Errorf("tool %v does not advertise idempotentHint:true, which the argument-derived key makes true", tm["name"])
		}
	}
}

// ---- shaping ------------------------------------------------------------

// TestTheInputSchemaIsPassedThroughUnaltered: a passthrough stays a passthrough.
// Upgrading it here would make a workflow with no schema look typed, which is
// the same false claim #1980's narrow-scope note was written to avoid.
func TestTheInputSchemaIsPassedThroughUnaltered(t *testing.T) {
	p, _ := newProxyPair(t)
	resp := call(t, p, "tools/list", map[string]any{}, nil)
	result, _ := resp["result"].(map[string]any)
	raw, _ := result["tools"].([]any)
	for _, r := range raw {
		tm, _ := r.(map[string]any)
		if tm["name"] != "plain" {
			continue
		}
		schema, _ := tm["inputSchema"].(map[string]any)
		if len(schema) != 1 || schema["type"] != "object" {
			t.Fatalf("plain's schema was altered: %v", schema)
		}
		return
	}
	t.Fatal("plain was not listed")
}

// TestAnUnrepresentableWorkflowNameIsSanitisedNotDropped: the spec restricts
// tool names to [A-Za-z0-9_.-]. Dropping a workflow whose name breaks that
// would be a workflow that exists and cannot be called.
func TestAnUnrepresentableWorkflowNameIsSanitisedNotDropped(t *testing.T) {
	f := newFakeCleat()
	f.doc = map[string]any{"paths": map[string]any{
		"/api/workflows/odd name/start": map[string]any{"post": map[string]any{
			"requestBody": map[string]any{"content": map[string]any{"application/json": map[string]any{
				"schema": map[string]any{"type": "object"},
			}}},
		}},
	}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	p := &proxy{api: srv.URL, http: srv.Client()}

	got := toolNames(t, call(t, p, "tools/list", map[string]any{}, nil))
	if len(got) != 1 {
		t.Fatalf("tools = %v, want exactly one (sanitised)", got)
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_.-]+$`).MatchString(got[0]) {
		t.Fatalf("tool name %q is not legal for MCP", got[0])
	}
}

// TestServerDiscoverNamesTheProtocolVersion: the spec's optional discovery RPC.
// A client that asks learns the revision rather than inferring it.
func TestServerDiscoverNamesTheProtocolVersion(t *testing.T) {
	p, _ := newProxyPair(t)
	resp := call(t, p, "server/discover", map[string]any{}, nil)
	result, _ := resp["result"].(map[string]any)
	if result == nil || result["protocolVersion"] != protocolVersion {
		t.Fatalf("discover = %v, want protocolVersion %s", resp, protocolVersion)
	}
}

// ---- tasks/get and tasks/cancel -----------------------------------------

func taskResult(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", resp)
	}
	return result
}

func TestTasksGetReportsARunningRunAsWorking(t *testing.T) {
	p, f := newProxyPair(t)
	f.runStatus = "running"

	result := taskResult(t, call(t, p, "tasks/get", map[string]any{"taskId": "run-1"}, nil))
	if result["status"] != "working" {
		t.Fatalf("status = %v, want working for a running run", result["status"])
	}
	if result["taskId"] != "run-1" {
		t.Fatalf("taskId = %v, want the run id", result["taskId"])
	}
}

func TestTasksGetReportsACompletedRunWithItsResult(t *testing.T) {
	p, f := newProxyPair(t)
	f.runStatus = "done"
	f.runResult = `{"count":3}`

	result := taskResult(t, call(t, p, "tasks/get", map[string]any{"taskId": "run-1"}, nil))
	if result["status"] != "completed" {
		t.Fatalf("status = %v, want completed for a done run", result["status"])
	}
	if _, ok := result["result"]; !ok {
		t.Fatalf("a completed task must carry its result: %v", result)
	}
}

// TestAnUnrecognisedStatusIsNotReportedAsCompleted is the direction guard on the
// status mapping. Any status this proxy does not know must read as still in
// progress: telling a client a run has FINISHED when it has not is the one
// direction a client acts on, and cleat's vocabulary is not a closed set this
// proxy may assume.
func TestAnUnrecognisedStatusIsNotReportedAsCompleted(t *testing.T) {
	p, f := newProxyPair(t)
	f.runStatus = "terminating" // a real cleat status the extension has no name for

	result := taskResult(t, call(t, p, "tasks/get", map[string]any{"taskId": "run-1"}, nil))
	if result["status"] == "completed" {
		t.Fatalf("an unrecognised status was reported as completed: %v", result)
	}
	if result["status"] != "working" {
		t.Fatalf("status = %v, want working (the safe default)", result["status"])
	}
}

func TestTasksCancelHitsTheCancelRoute(t *testing.T) {
	p, f := newProxyPair(t)

	result := taskResult(t, call(t, p, "tasks/cancel", map[string]any{"taskId": "run-1"}, nil))
	if result["status"] != "cancelled" {
		t.Fatalf("status = %v, want cancelled", result["status"])
	}
	if f.cancels != 1 {
		t.Fatalf("cancel route called %d times, want 1", f.cancels)
	}
}

// TestATaskRequestWithoutAnIdIsRefused: taskId is required, and the refusal is
// a protocol error (-32602) rather than a tool execution error, because the
// request itself is malformed -- a model cannot fix it by changing arguments.
func TestATaskRequestWithoutAnIdIsRefused(t *testing.T) {
	p, _ := newProxyPair(t)
	resp := call(t, p, "tasks/get", map[string]any{}, nil)
	e, _ := resp["error"].(map[string]any)
	if e == nil {
		t.Fatalf("expected a protocol error, got %v", resp)
	}
	if code, _ := e["code"].(float64); int(code) != codeInvalidPar {
		t.Fatalf("error code = %v, want %d", e["code"], codeInvalidPar)
	}
}

// TestTheCallersTraceContinuesIntoCleat is the known-positive for the required
// check that was red on the first push. That check accepts a MENTION of
// SetTraceparent, so a call wired to nothing satisfies it while the trace still
// ends at this hop -- which is the exact shape it exists to prevent. So this
// asserts the OUTBOUND HEADER, not the call.
func TestTheCallersTraceContinuesIntoCleat(t *testing.T) {
	p, f := newProxyPair(t)
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	const callerSpan = "00f067aa0ba902b7"
	inbound := "00-" + traceID + "-" + callerSpan + "-01"

	call(t, p, "tools/call", map[string]any{"name": "checkout",
		"arguments": map[string]any{"input": map[string]any{}}},
		map[string]string{"traceparent": inbound})

	var got string
	f.mu.Lock()
	for _, tp := range f.traceparents {
		if tp != "" {
			got = tp
		}
	}
	f.mu.Unlock()

	if got == "" {
		t.Fatalf("no traceparent reached cleat: the caller's trace ended at this hop")
	}
	parts := strings.Split(got, "-")
	if len(parts) != 4 || parts[1] != traceID {
		t.Fatalf("outbound traceparent = %q, want the caller's trace-id %s", got, traceID)
	}
	if parts[2] == callerSpan {
		t.Errorf("the caller's span-id was forwarded verbatim; a proxy hop synthesises a new span for itself")
	}
}

// TestANonPostIsRefused: the 2026-07-28 Streamable HTTP transport carries
// JSON-RPC on POST.
func TestANonPostIsRefused(t *testing.T) {
	p, _ := newProxyPair(t)
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET returned %d, want 405", rec.Code)
	}
}
