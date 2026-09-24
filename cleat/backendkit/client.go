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

// classifyError turns a refusal into a typed error where the server named one,
// and keeps the old opaque form otherwise.
//
// The server answers a refused idempotency key with a machine-readable `detail`
// alongside the human `error`. Without this, both arrive as
// "unexpected status 409: {…}" and a caller wanting to tell "retrying will
// never help" from "the service is briefly unavailable" has to match on text.
//
// Errors are WRAPPED, not replaced, so the message still carries the server's
// own words and errors.Is still answers the question the caller asked.
func classifyError(status int, body []byte) error {
	generic := fmt.Errorf("unexpected status %d: %s", status, strings.TrimSpace(string(body)))
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
		return fmt.Errorf("%w: %s", ErrIdempotencyKeyInputMismatch, strings.TrimSpace(string(body)))
	case "idempotency_key_definition_mismatch":
		return fmt.Errorf("%w: %s", ErrIdempotencyKeyDefinitionMismatch, strings.TrimSpace(string(body)))
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
	u := c.BaseURL + "/api/workflows/" + url.PathEscape(id) + "/state"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := c.doRequest(req)
	if err != nil {
		return nil, fmt.Errorf("get workflow state: %w", err)
	}
	defer resp.Body.Close()

	var state map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return state, nil
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
