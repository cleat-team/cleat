// Package backendkit provides shared HTTP client, middleware, response helpers,
// and config loading used by all Cleat app backends.
package backendkit

import (
	"bytes"
	"context"
	"encoding/json"
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
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return resp, nil
}

// StartWorkflow starts a new workflow instance with the given name, entry point, and input.
// entryPoint can be empty string to use the default entry point.
// Returns the workflow ID.
func (c *Client) StartWorkflow(ctx context.Context, name string, entryPoint string, input interface{}) (string, error) {
	body := map[string]interface{}{
		"input": input,
	}
	if entryPoint != "" {
		body["entry_point"] = entryPoint
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
func (c *Client) StartWorkflowRaw(ctx context.Context, name string, entryPoint string, input json.RawMessage) (string, error) {
	body := map[string]interface{}{
		"input":       input,
		"entry_point": entryPoint,
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

// Health checks the cleat worker health endpoint.
func (c *Client) Health(ctx context.Context) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/healthz", nil)
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
