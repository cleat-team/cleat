// Package backendkit provides shared HTTP client, middleware, response helpers,
// and config loading used by all Cleat app backends.
package backendkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client communicates with the Cleat worker REST API.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
	TenantID   string // optional tenant_id; sent to worker for namespace isolation
}

// WorkflowSummary is a summary of a workflow instance returned by ListWorkflows.
type WorkflowSummary struct {
	ID        string          `json:"id"`
	Status    string          `json:"status"`
	Input     json.RawMessage `json:"input"`
	CreatedAt time.Time       `json:"created_at"`
}

// WorkflowDetail is the full detail of a workflow instance returned by GetWorkflow.
type WorkflowDetail struct {
	ID         string          `json:"id"`
	DefName    string          `json:"def_name"`
	DefVersion int             `json:"def_version"`
	Status     string          `json:"status"`
	Input      json.RawMessage `json:"input"`
	Result     string          `json:"result,omitempty"`
	Error      string          `json:"error,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at,omitempty"`
}

// HistoryEvent represents a single event in workflow execution history.
type HistoryEvent struct {
	Step      int    `json:"step"`
	Type      string `json:"type"`
	Timestamp int64  `json:"timestamp_ms"`
	Service   string `json:"service,omitempty"`
	Op        string `json:"op,omitempty"`
	Request   string `json:"request,omitempty"`
	Response  string `json:"response,omitempty"`
	Err       string `json:"err,omitempty"`
}

// The two refusals a start can get back that are ABOUT the idempotency key
// rather than about the request.
//
// Typed because a caller has to act differently on each, and the only
// alternative was matching on the text of an error built with fmt.Errorf --
// which is a contract nobody declared and every server change can break.
//
//   - ErrIdempotencyKeyInputMismatch: the key was used before with a DIFFERENT
//     payload. Retrying cannot help; the caller sent two different requests
//     under one key and must decide which it meant.
//   - ErrIdempotencyKeyDefinitionMismatch: the key already started a different
//     workflow definition. Same shape, same conclusion.
//
// Both are terminal for that key. Neither is a reason to retry, which is
// exactly why they must be distinguishable from the transport failures that
// are.
var (
	ErrIdempotencyKeyInputMismatch      = errors.New("idempotency key was used with a different payload")
	ErrIdempotencyKeyDefinitionMismatch = errors.New("idempotency key already started a different workflow definition")
)

// StartOptions carries the per-start values that are not the input.
//
// A struct rather than more parameters: StartWorkflow and StartWorkflowRaw
// already take four, and an idempotency key is the fifth thing a start can
// want rather than the last.
type StartOptions struct {
	// EntryPoint selects a non-default entry point. Empty uses the default.
	EntryPoint string

	// TenantID overrides Client.TenantID for this call. Empty uses the client's.
	TenantID string

	// IdempotencyKey makes the start safe to retry. Send the SAME key on every
	// retry of one logical request, and a different key for a different one.
	//
	// Empty means no key, which is not the same as a key equal to "": two
	// callers who both send nothing do not collide with each other.
	IdempotencyKey string
}

// StartResult is what a start answers, including the two fields that say
// whether this call was the original.
type StartResult struct {
	// ID is the workflow run. On a replay this is the ORIGINAL run's id, which
	// is the whole point: a retry names the work its first attempt created.
	ID string `json:"id"`

	// IdempotentReplay is false when this call created the run and true when an
	// earlier call under the same key did.
	//
	// It is present on both, so it can be read unconditionally. Do not infer
	// "original" from a missing field -- an old server that does not send one
	// is indistinguishable from a server saying false.
	IdempotentReplay bool `json:"idempotent_replay"`

	// Status is the run's lifecycle status, and is only meaningful on a replay
	// -- the server has nothing to report about a run it just created.
	//
	// "unknown" means the server could not read the run, NOT that it forgot to
	// say. Treat it as "ask again", never as terminal.
	Status string `json:"status,omitempty"`
}

// Terminal reports whether Status is an end state, so a caller can stop
// polling.
//
// Branch on this rather than on Status == "running". A workflow that sleeps,
// awaits a child, waits on a signal or backs off a retry is "ready" for nearly
// all of its life; "running" covers only the slices when a worker holds it. A
// poller that waits for "running" to disappear may never see it at all.
func (r StartResult) Terminal() bool {
	switch r.Status {
	case "done", "failed", "terminated", "dead_lettered":
		return true
	}
	return false
}

// New creates a new Cleat API client with the given base URL.
func New(baseURL string) *Client {
	return &Client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// doRequest performs an HTTP request and checks for a successful response.
func (c *Client) doRequest(req *http.Request) (*http.Response, error) {
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, classifyError(resp.StatusCode, body)
	}
	return resp, nil
}

// UpstreamStatusError carries the HTTP status the Cleat worker actually
// returned, recoverable with errors.As.
//
// cleat#2718: classifyError used to return a plain error built with
// fmt.Errorf, which has no field a caller can read the status back out of.
// Every example backend that maps a Client error to an HTTP response had no
// way to ask "was this a 401?" and so mapped everything to 502 (bad gateway)
// -- including a genuine 401, which sent an operator or a retry policy to the
// wrong layer (the gateway looked broken; authentication had refused).
//
// classifyError attaches this to every non-2xx response, including the ones
// that also carry a more specific sentinel like ErrIdempotencyKeyInputMismatch
// -- Go's multi-%w wrapping (1.20+) lets errors.Is and errors.As both walk
// past it, so a caller checking for a specific sentinel is unaffected and a
// caller that only wants the status still gets it.
type UpstreamStatusError struct {
	// Status is the HTTP status code the worker returned.
	Status int
	// Body is the raw response body, truncated to 4096 bytes by doRequest.
	Body []byte
}

func (e *UpstreamStatusError) Error() string {
	return fmt.Sprintf("unexpected status %d: %s", e.Status, strings.TrimSpace(string(e.Body)))
}

// classifyError turns a refusal into a typed error where the server named one,
// and keeps the old opaque form otherwise. Every path returns an error that
// satisfies errors.As(err, &(*UpstreamStatusError)(nil)) -- see
// UpstreamStatusError's own doc comment for why that matters.
//
// The server answers a refused idempotency key with a machine-readable `detail`
// alongside the human `error`. Without this, both arrive as
// "unexpected status 409: {…}" and a caller wanting to tell "retrying will
// never help" from "the service is briefly unavailable" has to match on text.
//
// Errors are WRAPPED, not replaced, so the message still carries the server's
// own words and errors.Is still answers the question the caller asked.
func classifyError(status int, body []byte) error {
	generic := &UpstreamStatusError{Status: status, Body: body}
	if status != http.StatusConflict {
		return generic
	}
	var detail struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(body, &detail); err != nil {
		return generic
	}
	switch detail.Detail {
	case "idempotency_key_input_mismatch":
		return fmt.Errorf("%w: %w", ErrIdempotencyKeyInputMismatch, generic)
	case "idempotency_key_definition_mismatch":
		return fmt.Errorf("%w: %w", ErrIdempotencyKeyDefinitionMismatch, generic)
	}
	return generic
}

// StartWorkflow starts a new workflow instance with the given name, entry point, and input.
// entryPoint can be empty string to use the default entry point.
// tenantID is optional; pass "" to use the worker default namespace.
// Returns the workflow ID.
func (c *Client) StartWorkflow(ctx context.Context, name string, entryPoint string, input interface{}, tenantID string) (string, error) {
	body := map[string]interface{}{
		"input": input,
	}
	if entryPoint != "" {
		body["entry_point"] = entryPoint
	}
	tid := tenantID
	if tid == "" {
		tid = c.TenantID
	}
	if tid != "" {
		body["tenant_id"] = tid
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/api/workflows/"+url.PathEscape(name)+"/start",
		bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.doRequest(req)
	if err != nil {
		return "", fmt.Errorf("start workflow: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	return result.ID, nil
}

// StartWorkflowRaw starts a new workflow instance with raw JSON input and explicit entry point.
// tenantID is optional; pass "" to use the worker default namespace.
func (c *Client) StartWorkflowRaw(ctx context.Context, name string, entryPoint string, input json.RawMessage, tenantID string) (string, error) {
	body := map[string]interface{}{
		"input":       input,
		"entry_point": entryPoint,
	}
	tid := tenantID
	if tid == "" {
		tid = c.TenantID
	}
	if tid != "" {
		body["tenant_id"] = tid
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/api/workflows/"+url.PathEscape(name)+"/start",
		bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.doRequest(req)
	if err != nil {
		return "", fmt.Errorf("start workflow: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	return result.ID, nil
}

// StartWorkflowWithOptions starts a workflow and returns everything the start
// answered, including whether this call created the run or matched an earlier
// one.
//
// THIS IS THE ONE TO USE WHEN A START MUST BE SAFE TO RETRY. StartWorkflow and
// StartWorkflowRaw cannot send an idempotency key and discard the replay flag,
// so a caller using them has no way to retry a start without risking a second
// run.
//
// WHAT A CORRECT RETRY LOOP LOOKS LIKE, because the shape is not obvious and
// getting it wrong is silent:
//
//	res, err := c.StartWorkflowWithOptions(ctx, "charge", input,
//		backendkit.StartOptions{IdempotencyKey: key})
//	// retry err on TRANSPORT failures only; the same key makes that safe.
//	// Do NOT retry ErrIdempotencyKeyInputMismatch or …DefinitionMismatch --
//	// those say the request was different, and repeating it changes nothing.
//	for {
//		wf, err := c.GetWorkflow(ctx, res.ID)
//		…            // wf.Status terminal? wf.Result is there. Otherwise wait.
//	}
//
// TWO LOOPS, NOT ONE, and the reason is that they have different budgets. "Did
// my POST land" is bounded -- seconds, and a failure means something is wrong.
// "Is the work finished" is unbounded: a durable workflow may legitimately
// sleep for days. Collapsing them makes a bounded retry budget govern an
// unbounded wait, and "gave up" then looks exactly like "failed".
//
// THE START RESPONSE NEVER CARRIES THE RESULT, on a replay or otherwise. The
// run is the only source for that, which is why the loop above ends at
// GetWorkflow rather than reading anything from res.
//
// A RETRY CAN BLOCK. If the original request is still inside its transaction
// when the retry arrives, the retry waits on the database until that
// transaction ends -- measured at three seconds against a deliberately slow
// original. It is not a fast "already exists" lookup, so the client timeout has
// to allow for it, or a retry gets cancelled precisely when it was about to
// report the truth.
func (c *Client) StartWorkflowWithOptions(ctx context.Context, name string, input json.RawMessage, opts StartOptions) (StartResult, error) {
	var res StartResult

	body := map[string]interface{}{"input": input}
	if opts.EntryPoint != "" {
		body["entry_point"] = opts.EntryPoint
	}
	tid := opts.TenantID
	if tid == "" {
		tid = c.TenantID
	}
	if tid != "" {
		body["tenant_id"] = tid
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return res, fmt.Errorf("marshal request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/api/workflows/"+url.PathEscape(name)+"/start",
		bytes.NewReader(bodyBytes))
	if err != nil {
		return res, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Only when non-empty. An empty header is a key equal to "", which would
	// make every caller that sent no key collide with every other.
	if opts.IdempotencyKey != "" {
		req.Header.Set("Idempotency-Key", opts.IdempotencyKey)
	}

	resp, err := c.doRequest(req)
	if err != nil {
		return res, fmt.Errorf("start workflow: %w", err)
	}
	defer resp.Body.Close()

	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return res, fmt.Errorf("decode response: %w", err)
	}
	return res, nil
}

// SignalWorkflow sends a signal to a running workflow.
func (c *Client) SignalWorkflow(ctx context.Context, id, signalName, payload string) error {
	body := map[string]string{
		"signal_name": signalName,
		"payload":     payload,
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/api/workflows/"+url.PathEscape(id)+"/signal",
		bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.doRequest(req)
	if err != nil {
		return fmt.Errorf("signal workflow: %w", err)
	}
	resp.Body.Close()
	return nil
}

// ListWorkflows returns a list of workflow instances, optionally filtered by status.
func (c *Client) ListWorkflows(ctx context.Context, status string, limit int) ([]WorkflowSummary, error) {
	u := c.BaseURL + "/api/workflows"
	q := url.Values{}
	if status != "" {
		q.Set("status", status)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if len(q) > 0 {
		u += "?" + q.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := c.doRequest(req)
	if err != nil {
		return nil, fmt.Errorf("list workflows: %w", err)
	}
	defer resp.Body.Close()

	var workflows []WorkflowSummary
	if err := json.NewDecoder(resp.Body).Decode(&workflows); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return workflows, nil
}

// GetWorkflow retrieves a single workflow instance by ID.
func (c *Client) GetWorkflow(ctx context.Context, id string) (*WorkflowDetail, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.BaseURL+"/api/workflows/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := c.doRequest(req)
	if err != nil {
		return nil, fmt.Errorf("get workflow: %w", err)
	}
	defer resp.Body.Close()

	var detail WorkflowDetail
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &detail, nil
}

// QueryState retrieves a single query state value from a workflow.
func (c *Client) QueryState(ctx context.Context, id, key string) (string, error) {
	u := c.BaseURL + "/api/workflows/" + url.PathEscape(id) + "/query?key=" + url.QueryEscape(key)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}

	resp, err := c.doRequest(req)
	if err != nil {
		return "", fmt.Errorf("query state: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	return result.Value, nil
}

// GetWorkflowState retrieves the full state (query state) of a workflow.
func (c *Client) GetWorkflowState(ctx context.Context, id string) (map[string]string, error) {
	// THE ROUTE IS /query WITH NO key, NOT /state, and this method called the
	// latter until 2026-09-28 -- a route that does not exist. Every call
	// answered 404, every caller treated that as "no state", and three shipped
	// example backends rendered a run's state panel as empty.
	//
	// The worker's dispatch is `cmd/cleat-worker/server.go` `handleWorkflows`:
	// the :id/:verb pairs it accepts are start, signal, cancel, retry, terminal,
	// history, stream, query, dag, promises, routing and tags. There is no
	// `state` among them -- `/api/instances/{id}/state` exists, on a DIFFERENT
	// prefix, which is what made the name look right.
	//
	// `?key=` is OPTIONAL and its ABSENCE is the list: with no key,
	// `handleGetQueryState` calls `ListQueryState` and answers
	// `{"state": {...every key the run published...}}`. With a key it answers
	// `{"key":k,"value":v}` -- which is what `QueryState` below uses. So the two
	// methods differ by the parameter's presence, not by a different verb.
	u := c.BaseURL + "/api/workflows/" + url.PathEscape(id) + "/query"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := c.doRequest(req)
	if err != nil {
		return nil, fmt.Errorf("get workflow state: %w", err)
	}
	defer resp.Body.Close()

	// The envelope is decoded, not the bare map: the route wraps the state in
	// `{"state": ...}` so that a listing and a keyed read are distinguishable at
	// the top level. Decoding straight into a map[string]string against this
	// route would fail; against the OLD route it succeeded, because the test
	// that covered it served a bare map the worker never sends.
	var out struct {
		State map[string]string `json:"state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return out.State, nil
}

// GetHistory retrieves the event history of a workflow with pagination.
func (c *Client) GetHistory(ctx context.Context, id string, offset, limit int) ([]HistoryEvent, error) {
	u := c.BaseURL + "/api/workflows/" + url.PathEscape(id) + "/history"
	q := url.Values{}
	q.Set("offset", strconv.Itoa(offset))
	q.Set("limit", strconv.Itoa(limit))
	u += "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := c.doRequest(req)
	if err != nil {
		return nil, fmt.Errorf("get history: %w", err)
	}
	defer resp.Body.Close()

	var events []HistoryEvent
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return events, nil
}

// DeleteWorkflow deletes a workflow instance by ID.
func (c *Client) DeleteWorkflow(ctx context.Context, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		c.BaseURL+"/api/workflows/"+url.PathEscape(id), nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	resp, err := c.doRequest(req)
	if err != nil {
		return fmt.Errorf("delete workflow: %w", err)
	}
	resp.Body.Close()
	return nil
}

// CallPlugin invokes a plugin function on the cleat worker.
func (c *Client) CallPlugin(ctx context.Context, pluginName, functionName, inputJSON string) (string, error) {
	u := c.BaseURL + "/api/plugins/" + url.PathEscape(pluginName) + "/" + url.PathEscape(functionName)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader([]byte(inputJSON)))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.doRequest(req)
	if err != nil {
		return "", fmt.Errorf("plugin call: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	return string(body), nil
}

// PluginRoute performs an authenticated request against a route a PLUGIN
// registered, rather than against the workflow API.
//
// WHY THIS EXISTS. Client's typed methods cover the /api/* resources, and
// CallPlugin covers the plugin HOST-FUNCTION path (/api/plugins/{plugin}/{fn}).
// Neither reaches the routes a plugin mounts itself in RegisterRoutes — which
// are on the same mux and are part of the worker's public surface.
// plugins/notifications alone registers six, and there is no host-function
// substitute for the case that found this: it registers send_webhook and
// nothing that lists deliveries, so a delivery log exists ONLY as an HTTP route
// (cleat#2550).
//
// Without this an app that fronts a plugin spells the URL itself, which means
// either hand-rolling the request — losing the API key, the timeout and the
// error classification on the way — or reaching into Client.HTTPClient and
// Client.BaseURL to rebuild what this method already does correctly.
//
// path must begin with "/". It is joined to the Client's BaseURL, and the HOST
// therefore cannot be influenced by the caller: a path is appended to a URL
// that already has an authority, so nothing a caller passes can redirect the
// request — and the API key riding this client's transport — anywhere else.
//
// body is marshalled as JSON when non-nil; out is decoded from the response
// when non-nil. A refusal is classified exactly as every other method's,
// through doRequest, so a caller gets the same typed errors.
func (c *Client) PluginRoute(ctx context.Context, method, path string, body, out any) error {
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("plugin route %q must begin with %q: it is appended to the "+
			"client's base URL, and a relative path would be resolved against the wrong "+
			"base", path, "/")
	}

	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request body: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.doRequest(req)
	if err != nil {
		return fmt.Errorf("plugin route %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if out == nil {
		// Drain, so the connection can be reused rather than torn down by the
		// client on a body nobody read.
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode %s %s response: %w", method, path, err)
	}
	return nil
}

// Health reports whether the worker is ready to serve: GET /readyz answers 200 (its database answered
// and it is not draining). A worker that is alive but not ready is reported false. It was /healthz, which
// is now /livez under its old name and says nothing about the database. cleat#2007.
func (c *Client) Health(ctx context.Context) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/readyz", nil)
	if err != nil {
		return false, fmt.Errorf("create request: %w", err)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("health check failed: %w", err)
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK, nil
}
