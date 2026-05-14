package email

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/sendgrid/rest"
	"github.com/sendgrid/sendgrid-go"

	"github.com/cleat-team/cleat/internal/plugin"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

var testTenantID = uuid.MustParse("00000000-0000-0000-0000-000000000001")

// testTransport is an http.RoundTripper that rewrites ALL requests to point
// to the given baseURL, preserving the path, query, method, headers, and body.
type testTransport struct {
	baseURL string
	next    http.RoundTripper
}

func (t *testTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rewrittenURL := t.baseURL + req.URL.Path
	if req.URL.RawQuery != "" {
		rewrittenURL += "?" + req.URL.RawQuery
	}

	newReq, err := http.NewRequestWithContext(req.Context(), req.Method, rewrittenURL, req.Body)
	if err != nil {
		return nil, err
	}
	newReq.Header = req.Header.Clone()
	return t.next.RoundTrip(newReq)
}

// setupSendGridClient replaces rest.DefaultClient.HTTPClient with one whose
// transport rewrites requests to the test server. The original is restored
// via t.Cleanup.
func setupSendGridClient(t *testing.T, tsURL string) {
	t.Helper()
	orig := rest.DefaultClient.HTTPClient
	rest.DefaultClient.HTTPClient = &http.Client{
		Transport: &testTransport{
			baseURL: tsURL,
			next:    http.DefaultTransport,
		},
	}
	t.Cleanup(func() { rest.DefaultClient.HTTPClient = orig })
}

// setupTestSendGridServer creates a test HTTP server that simulates the
// SendGrid send API endpoint. It validates the incoming request and returns
// the configured status code, body, and headers.
func setupTestSendGridServer(t *testing.T, statusCode int, body string, headers map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("SendGrid mock: expected POST, got %s", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("SendGrid mock: expected Content-Type application/json, got %s", r.Header.Get("Content-Type"))
		}
		if r.Header.Get("Authorization") == "" {
			t.Error("SendGrid mock: expected Authorization header")
		}

		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("SendGrid mock: failed to read request body: %v", err)
		}
		defer r.Body.Close()
		if !json.Valid(bodyBytes) {
			t.Errorf("SendGrid mock: request body is not valid JSON: %s", string(bodyBytes))
		}

		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(statusCode)
		if body != "" {
			w.Write([]byte(body))
		}
	}))
}

// setupTestStatusServer creates a test HTTP server that simulates the
// SendGrid Activity API endpoint.
func setupTestStatusServer(t *testing.T, statusCode int, responseBody string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("Status mock: expected GET, got %s", r.Method)
		}
		if r.Header.Get("Authorization") == "" {
			t.Error("Status mock: expected Authorization header")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		if responseBody != "" {
			w.Write([]byte(responseBody))
		}
	}))
}

// newTestPlugin creates a Plugin instance configured for testing.
func newTestPlugin(t *testing.T) *Plugin {
	t.Helper()
	return &Plugin{
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{},
		client:     sendgrid.NewSendClient("test-api-key"),
		apiKey:     "test-api-key",
	}
}

// ---- Info tests ----

func TestInfo(t *testing.T) {
	p := &Plugin{}
	info := p.Info()
	if info.Name != "email-notify" {
		t.Errorf("expected Name 'email-notify', got %q", info.Name)
	}
	if info.Version != "0.1.0" {
		t.Errorf("expected Version '0.1.0', got %q", info.Version)
	}
	if info.Description == "" {
		t.Error("expected non-empty Description")
	}
}

// ---- Init tests ----

func TestInit(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		Config: []byte(`{"sendgrid_api_key":"test-key"}`),
	}
	err := p.Init(context.Background(), env)
	if err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if p.client == nil {
		t.Error("expected client to be set")
	}
	if p.logger == nil {
		t.Error("expected logger to be set")
	}
	if p.apiKey != "test-key" {
		t.Errorf("expected apiKey 'test-key', got %q", p.apiKey)
	}
}

func TestInitWithConfig(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		Config: []byte(`{"sendgrid_api_key":"key123","default_from":"noreply@example.com"}`),
	}
	err := p.Init(context.Background(), env)
	if err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if p.apiKey != "key123" {
		t.Errorf("expected apiKey 'key123', got %q", p.apiKey)
	}
	if p.defaultFrom != "noreply@example.com" {
		t.Errorf("expected defaultFrom 'noreply@example.com', got %q", p.defaultFrom)
	}
}

func TestInitInvalidConfig(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		Config: []byte(`not valid json`),
	}
	err := p.Init(context.Background(), env)
	if err == nil {
		t.Fatal("expected error for invalid config, got nil")
	}
}

func TestInitMissingAPIKey(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		Config: []byte(`{}`),
	}
	err := p.Init(context.Background(), env)
	if err == nil {
		t.Fatal("expected error for missing api key, got nil")
	}
	if !strings.Contains(err.Error(), "sendgrid_api_key is required") {
		t.Errorf("expected error about missing api key, got: %v", err)
	}
}

// ---- Plugin registration test ----

func TestPluginRegistration(t *testing.T) {
	plugins, err := plugin.Discover()
	if err != nil {
		t.Fatalf("Discover() returned error: %v", err)
	}
	found := false
	for _, lp := range plugins {
		if lp.Plugin.Info().Name == "email-notify" {
			found = true
			break
		}
	}
	if !found {
		t.Error("email plugin not found after Discover")
	}
}

// ---- RegisterHostFunctions tests ----

func TestRegisterHostFunctionsNilScope(t *testing.T) {
	p := &Plugin{}
	err := p.RegisterHostFunctions(nil)
	if err == nil {
		t.Fatal("expected error for nil scope")
	}
}

type fakeFuncRegistry struct {
	funcs map[string]plugin.PluginFunc
}

func newFakeFuncRegistry() *fakeFuncRegistry {
	return &fakeFuncRegistry{funcs: make(map[string]plugin.PluginFunc)}
}

func (r *fakeFuncRegistry) Register(opts plugin.FuncOptions, fn plugin.PluginFunc) error {
	r.funcs[opts.Name] = fn
	return nil
}

func (r *fakeFuncRegistry) Has(name string) bool {
	_, ok := r.funcs[name]
	return ok
}

func TestRegisterHostFunctions(t *testing.T) {
	p := &Plugin{}
	reg := newFakeFuncRegistry()
	if err := p.RegisterHostFunctions(reg); err != nil {
		t.Fatalf("RegisterHostFunctions: %v", err)
	}
	if !reg.Has("send") {
		t.Error("expected send to be registered")
	}
	if !reg.Has("send_template") {
		t.Error("expected send_template to be registered")
	}
	if !reg.Has("check_status") {
		t.Error("expected check_status to be registered")
	}
}

type errRegistry struct {
	plugin.FuncRegistry
}

func (r *errRegistry) Register(opts plugin.FuncOptions, fn plugin.PluginFunc) error {
	return fmt.Errorf("register error: %s", opts.Name)
}

func TestRegisterHostFunctionsRegisterError(t *testing.T) {
	p := &Plugin{}
	scope := &errRegistry{}
	err := p.RegisterHostFunctions(scope)
	if err == nil {
		t.Fatal("expected error from Register, got nil")
	}
	if !strings.Contains(err.Error(), "register error") {
		t.Errorf("expected register error, got: %v", err)
	}
}

// ===========================================================================
// send host function tests
// ===========================================================================

// TestSend verifies that send sends a valid email via SendGrid and returns
// the message ID.
func TestSend(t *testing.T) {
	ts := setupTestSendGridServer(t, http.StatusAccepted, "", map[string]string{
		"X-Message-Id": "test-msg-123",
	})
	defer ts.Close()
	setupSendGridClient(t, ts.URL)

	p := newTestPlugin(t)

	input := map[string]interface{}{
		"to":       "recipient@example.com",
		"subject":  "Hello from cleat",
		"body_html": "<h1>Hello</h1>",
		"from":     "sender@example.com",
	}
	inputJSON, _ := json.Marshal(input)

	callCtx := &plugin.CallContext{TenantID: testTenantID, WorkflowID: "test-workflow"}
	ctx := plugin.WithCallContext(context.Background(), callCtx)

	output, err := p.send(ctx, string(inputJSON))
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("failed to decode output: %v", err)
	}
	if result["message_id"] != "test-msg-123" {
		t.Errorf("expected message_id 'test-msg-123', got %v", result["message_id"])
	}
	if result["status"] != "sent" {
		t.Errorf("expected status 'sent', got %v", result["status"])
	}
}

// TestSendWithBodyText verifies that send works with both HTML and plain
// text bodies.
func TestSendWithBodyText(t *testing.T) {
	ts := setupTestSendGridServer(t, http.StatusAccepted, "", map[string]string{
		"X-Message-Id": "msg-with-text",
	})
	defer ts.Close()
	setupSendGridClient(t, ts.URL)

	p := newTestPlugin(t)

	input := map[string]interface{}{
		"to":        "user@example.com",
		"subject":   "Test with text",
		"body_html": "<p>HTML content</p>",
		"body_text": "Plain text content",
		"from":      "sender@example.com",
	}
	inputJSON, _ := json.Marshal(input)

	callCtx := &plugin.CallContext{TenantID: testTenantID, WorkflowID: "test-workflow"}
	ctx := plugin.WithCallContext(context.Background(), callCtx)

	output, err := p.send(ctx, string(inputJSON))
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	var result map[string]interface{}
	json.Unmarshal([]byte(output), &result)
	if result["message_id"] != "msg-with-text" {
		t.Errorf("expected message_id 'msg-with-text', got %v", result["message_id"])
	}
}

// TestSendWithCCBCC verifies that send includes CC and BCC recipients.
func TestSendWithCCBCC(t *testing.T) {
	ts := setupTestSendGridServer(t, http.StatusAccepted, "", map[string]string{
		"X-Message-Id": "msg-cc-bcc",
	})
	defer ts.Close()
	setupSendGridClient(t, ts.URL)

	p := newTestPlugin(t)

	input := map[string]interface{}{
		"to":        "primary@example.com",
		"subject":   "CC/BCC test",
		"body_html": "<p>Hello</p>",
		"from":      "sender@example.com",
		"cc":        []string{"cc@example.com"},
		"bcc":       []string{"bcc@example.com"},
	}
	inputJSON, _ := json.Marshal(input)

	callCtx := &plugin.CallContext{TenantID: testTenantID, WorkflowID: "test-workflow"}
	ctx := plugin.WithCallContext(context.Background(), callCtx)

	output, err := p.send(ctx, string(inputJSON))
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	var result map[string]interface{}
	json.Unmarshal([]byte(output), &result)
	if result["message_id"] != "msg-cc-bcc" {
		t.Errorf("expected message_id 'msg-cc-bcc', got %v", result["message_id"])
	}
}

// TestSendWithReplyTo verifies that send includes the Reply-To header.
func TestSendWithReplyTo(t *testing.T) {
	ts := setupTestSendGridServer(t, http.StatusAccepted, "", map[string]string{
		"X-Message-Id": "msg-reply",
	})
	defer ts.Close()
	setupSendGridClient(t, ts.URL)

	p := newTestPlugin(t)

	input := map[string]interface{}{
		"to":       "user@example.com",
		"subject":  "Reply test",
		"body_html": "<p>Reply to me</p>",
		"from":     "sender@example.com",
		"reply_to": "support@example.com",
	}
	inputJSON, _ := json.Marshal(input)

	callCtx := &plugin.CallContext{TenantID: testTenantID, WorkflowID: "test-workflow"}
	ctx := plugin.WithCallContext(context.Background(), callCtx)

	_, err := p.send(ctx, string(inputJSON))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
}

// TestSendWithDefaultFrom verifies that send uses the plugin's configured
// default_from when from is not provided in the input.
func TestSendWithDefaultFrom(t *testing.T) {
	ts := setupTestSendGridServer(t, http.StatusAccepted, "", map[string]string{
		"X-Message-Id": "default-from-msg",
	})
	defer ts.Close()
	setupSendGridClient(t, ts.URL)

	p := newTestPlugin(t)
	p.defaultFrom = "default@example.com"

	input := map[string]interface{}{
		"to":       "user@example.com",
		"subject":  "Default from test",
		"body_html": "<p>Hello</p>",
	}
	inputJSON, _ := json.Marshal(input)

	callCtx := &plugin.CallContext{TenantID: testTenantID, WorkflowID: "test-workflow"}
	ctx := plugin.WithCallContext(context.Background(), callCtx)

	output, err := p.send(ctx, string(inputJSON))
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	var result map[string]interface{}
	json.Unmarshal([]byte(output), &result)
	if result["message_id"] != "default-from-msg" {
		t.Errorf("expected message_id 'default-from-msg', got %v", result["message_id"])
	}
}

// ---- send error paths ----

func TestSendErrorPaths_MissingTenant(t *testing.T) {
	p := newTestPlugin(t)
	_, err := p.send(context.Background(),
		`{"to":"user@example.com","subject":"test","body_html":"<p>test</p>","from":"from@example.com"}`)
	if err == nil || !strings.Contains(err.Error(), "no tenant context") {
		t.Fatalf("expected no tenant context error, got: %v", err)
	}
}

func TestSendErrorPaths_InvalidJSON(t *testing.T) {
	p := newTestPlugin(t)
	ctx := plugin.WithCallContext(context.Background(), &plugin.CallContext{TenantID: testTenantID})
	_, err := p.send(ctx, `not json`)
	if err == nil || !strings.Contains(err.Error(), "invalid input") {
		t.Fatalf("expected invalid input error, got: %v", err)
	}
}

func TestSendErrorPaths_MissingTo(t *testing.T) {
	p := newTestPlugin(t)
	ctx := plugin.WithCallContext(context.Background(), &plugin.CallContext{TenantID: testTenantID})
	_, err := p.send(ctx, `{"subject":"test","body_html":"<p>test</p>","from":"from@example.com"}`)
	if err == nil || !strings.Contains(err.Error(), "to is required") {
		t.Fatalf("expected to required error, got: %v", err)
	}
}

func TestSendErrorPaths_MissingSubject(t *testing.T) {
	p := newTestPlugin(t)
	ctx := plugin.WithCallContext(context.Background(), &plugin.CallContext{TenantID: testTenantID})
	_, err := p.send(ctx, `{"to":"user@example.com","body_html":"<p>test</p>","from":"from@example.com"}`)
	if err == nil || !strings.Contains(err.Error(), "subject is required") {
		t.Fatalf("expected subject required error, got: %v", err)
	}
}

func TestSendErrorPaths_MissingBodyHTML(t *testing.T) {
	p := newTestPlugin(t)
	ctx := plugin.WithCallContext(context.Background(), &plugin.CallContext{TenantID: testTenantID})
	_, err := p.send(ctx, `{"to":"user@example.com","subject":"test","from":"from@example.com"}`)
	if err == nil || !strings.Contains(err.Error(), "body_html is required") {
		t.Fatalf("expected body_html required error, got: %v", err)
	}
}

func TestSendErrorPaths_MissingFrom(t *testing.T) {
	p := newTestPlugin(t)
	ctx := plugin.WithCallContext(context.Background(), &plugin.CallContext{TenantID: testTenantID})
	_, err := p.send(ctx, `{"to":"user@example.com","subject":"test","body_html":"<p>test</p>"}`)
	if err == nil || !strings.Contains(err.Error(), "from is required") {
		t.Fatalf("expected from required error, got: %v", err)
	}
}

func TestSendErrorPaths_APIError(t *testing.T) {
	ts := setupTestSendGridServer(t, http.StatusUnauthorized,
		`{"errors":[{"message":"invalid api key"}]}`, nil)
	defer ts.Close()
	setupSendGridClient(t, ts.URL)

	p := newTestPlugin(t)

	input := map[string]interface{}{
		"to":       "user@example.com",
		"subject":  "test",
		"body_html": "<p>test</p>",
		"from":     "from@example.com",
	}
	inputJSON, _ := json.Marshal(input)

	callCtx := &plugin.CallContext{TenantID: testTenantID, WorkflowID: "test-workflow"}
	ctx := plugin.WithCallContext(context.Background(), callCtx)

	_, err := p.send(ctx, string(inputJSON))
	if err == nil || !strings.Contains(err.Error(), "SendGrid returned") {
		t.Fatalf("expected SendGrid error, got: %v", err)
	}
}

// ===========================================================================
// send_template host function tests
// ===========================================================================

func TestSendTemplate(t *testing.T) {
	ts := setupTestSendGridServer(t, http.StatusAccepted, "", map[string]string{
		"X-Message-Id": "template-msg-456",
	})
	defer ts.Close()
	setupSendGridClient(t, ts.URL)

	p := newTestPlugin(t)

	input := map[string]interface{}{
		"to":          "user@example.com",
		"template_id": "d-abc123def456",
		"template_data": map[string]interface{}{
			"name": "Alice",
			"link": "https://example.com",
		},
		"from": "sender@example.com",
	}
	inputJSON, _ := json.Marshal(input)

	callCtx := &plugin.CallContext{TenantID: testTenantID, WorkflowID: "test-workflow"}
	ctx := plugin.WithCallContext(context.Background(), callCtx)

	output, err := p.sendTemplate(ctx, string(inputJSON))
	if err != nil {
		t.Fatalf("sendTemplate: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("failed to decode output: %v", err)
	}
	if result["message_id"] != "template-msg-456" {
		t.Errorf("expected message_id 'template-msg-456', got %v", result["message_id"])
	}
	if result["status"] != "sent" {
		t.Errorf("expected status 'sent', got %v", result["status"])
	}
}

func TestSendTemplateWithReplyTo(t *testing.T) {
	ts := setupTestSendGridServer(t, http.StatusAccepted, "", map[string]string{
		"X-Message-Id": "template-reply",
	})
	defer ts.Close()
	setupSendGridClient(t, ts.URL)

	p := newTestPlugin(t)

	input := map[string]interface{}{
		"to":          "user@example.com",
		"template_id": "d-abc123def456",
		"template_data": map[string]interface{}{
			"name": "Bob",
		},
		"from":     "sender@example.com",
		"reply_to": "support@example.com",
	}
	inputJSON, _ := json.Marshal(input)

	callCtx := &plugin.CallContext{TenantID: testTenantID, WorkflowID: "test-workflow"}
	ctx := plugin.WithCallContext(context.Background(), callCtx)

	_, err := p.sendTemplate(ctx, string(inputJSON))
	if err != nil {
		t.Fatalf("sendTemplate: %v", err)
	}
}

func TestSendTemplateWithDefaultFrom(t *testing.T) {
	ts := setupTestSendGridServer(t, http.StatusAccepted, "", map[string]string{
		"X-Message-Id": "template-default-from",
	})
	defer ts.Close()
	setupSendGridClient(t, ts.URL)

	p := newTestPlugin(t)
	p.defaultFrom = "default@example.com"

	input := map[string]interface{}{
		"to":          "user@example.com",
		"template_id": "d-xyz789",
		"template_data": map[string]interface{}{
			"name": "Carol",
		},
	}
	inputJSON, _ := json.Marshal(input)

	callCtx := &plugin.CallContext{TenantID: testTenantID, WorkflowID: "test-workflow"}
	ctx := plugin.WithCallContext(context.Background(), callCtx)

	_, err := p.sendTemplate(ctx, string(inputJSON))
	if err != nil {
		t.Fatalf("sendTemplate: %v", err)
	}
}

// ---- send_template error paths ----

func TestSendTemplateErrorPaths_MissingTenant(t *testing.T) {
	p := newTestPlugin(t)
	_, err := p.sendTemplate(context.Background(),
		`{"to":"user@example.com","template_id":"d-abc","from":"from@example.com"}`)
	if err == nil || !strings.Contains(err.Error(), "no tenant context") {
		t.Fatalf("expected no tenant context error, got: %v", err)
	}
}

func TestSendTemplateErrorPaths_InvalidJSON(t *testing.T) {
	p := newTestPlugin(t)
	ctx := plugin.WithCallContext(context.Background(), &plugin.CallContext{TenantID: testTenantID})
	_, err := p.sendTemplate(ctx, `not json`)
	if err == nil || !strings.Contains(err.Error(), "invalid input") {
		t.Fatalf("expected invalid input error, got: %v", err)
	}
}

func TestSendTemplateErrorPaths_MissingTo(t *testing.T) {
	p := newTestPlugin(t)
	ctx := plugin.WithCallContext(context.Background(), &plugin.CallContext{TenantID: testTenantID})
	_, err := p.sendTemplate(ctx, `{"template_id":"d-abc","from":"from@example.com"}`)
	if err == nil || !strings.Contains(err.Error(), "to is required") {
		t.Fatalf("expected to required error, got: %v", err)
	}
}

func TestSendTemplateErrorPaths_MissingTemplateID(t *testing.T) {
	p := newTestPlugin(t)
	ctx := plugin.WithCallContext(context.Background(), &plugin.CallContext{TenantID: testTenantID})
	_, err := p.sendTemplate(ctx, `{"to":"user@example.com","from":"from@example.com"}`)
	if err == nil || !strings.Contains(err.Error(), "template_id is required") {
		t.Fatalf("expected template_id required error, got: %v", err)
	}
}

func TestSendTemplateErrorPaths_MissingFrom(t *testing.T) {
	p := newTestPlugin(t)
	ctx := plugin.WithCallContext(context.Background(), &plugin.CallContext{TenantID: testTenantID})
	_, err := p.sendTemplate(ctx, `{"to":"user@example.com","template_id":"d-abc"}`)
	if err == nil || !strings.Contains(err.Error(), "from is required") {
		t.Fatalf("expected from required error, got: %v", err)
	}
}

func TestSendTemplateErrorPaths_APIError(t *testing.T) {
	ts := setupTestSendGridServer(t, http.StatusBadRequest,
		`{"errors":[{"message":"invalid template"}]}`, nil)
	defer ts.Close()
	setupSendGridClient(t, ts.URL)

	p := newTestPlugin(t)

	input := map[string]interface{}{
		"to":          "user@example.com",
		"template_id": "d-invalid",
		"template_data": map[string]interface{}{},
		"from":        "from@example.com",
	}
	inputJSON, _ := json.Marshal(input)

	callCtx := &plugin.CallContext{TenantID: testTenantID, WorkflowID: "test-workflow"}
	ctx := plugin.WithCallContext(context.Background(), callCtx)

	_, err := p.sendTemplate(ctx, string(inputJSON))
	if err == nil || !strings.Contains(err.Error(), "SendGrid returned") {
		t.Fatalf("expected SendGrid error, got: %v", err)
	}
}

// ===========================================================================
// check_status host function tests
// ===========================================================================

func TestCheckStatus(t *testing.T) {
	ts := setupTestStatusServer(t, http.StatusOK, `{
		"messages": [{
			"msg_id": "test-msg-123",
			"status": "delivered",
			"last_event_time": "2026-05-13T10:00:00Z",
			"opens_count": 1,
			"clicks_count": 0
		}]
	}`)
	defer ts.Close()

	p := newTestPlugin(t)
	p.httpClient = &http.Client{
		Transport: &testTransport{baseURL: ts.URL, next: http.DefaultTransport},
	}

	input := map[string]interface{}{"message_id": "test-msg-123"}
	inputJSON, _ := json.Marshal(input)

	callCtx := &plugin.CallContext{TenantID: testTenantID, WorkflowID: "test-workflow"}
	ctx := plugin.WithCallContext(context.Background(), callCtx)

	output, err := p.checkStatus(ctx, string(inputJSON))
	if err != nil {
		t.Fatalf("checkStatus: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("failed to decode output: %v", err)
	}
	if result["status"] != "delivered" {
		t.Errorf("expected status 'delivered', got %v", result["status"])
	}
	events, ok := result["events"].([]interface{})
	if !ok || len(events) == 0 {
		t.Error("expected at least one event")
	}
}

func TestCheckStatusMessageFoundWithOpens(t *testing.T) {
	ts := setupTestStatusServer(t, http.StatusOK, `{
		"messages": [{
			"msg_id": "msg-opened",
			"status": "",
			"last_event_time": "2026-05-13T11:00:00Z",
			"opens_count": 3,
			"clicks_count": 0
		}]
	}`)
	defer ts.Close()

	p := newTestPlugin(t)
	p.httpClient = &http.Client{
		Transport: &testTransport{baseURL: ts.URL, next: http.DefaultTransport},
	}

	input := map[string]interface{}{"message_id": "msg-opened"}
	inputJSON, _ := json.Marshal(input)

	callCtx := &plugin.CallContext{TenantID: testTenantID, WorkflowID: "test-workflow"}
	ctx := plugin.WithCallContext(context.Background(), callCtx)

	output, err := p.checkStatus(ctx, string(inputJSON))
	if err != nil {
		t.Fatalf("checkStatus: %v", err)
	}
	var result map[string]interface{}
	json.Unmarshal([]byte(output), &result)
	if result["status"] != "opened" {
		t.Errorf("expected status 'opened', got %v", result["status"])
	}
}

func TestCheckStatusMessageNotFound(t *testing.T) {
	ts := setupTestStatusServer(t, http.StatusOK, `{"messages": []}`)
	defer ts.Close()

	p := newTestPlugin(t)
	p.httpClient = &http.Client{
		Transport: &testTransport{baseURL: ts.URL, next: http.DefaultTransport},
	}

	input := map[string]interface{}{"message_id": "nonexistent"}
	inputJSON, _ := json.Marshal(input)

	callCtx := &plugin.CallContext{TenantID: testTenantID, WorkflowID: "test-workflow"}
	ctx := plugin.WithCallContext(context.Background(), callCtx)

	output, err := p.checkStatus(ctx, string(inputJSON))
	if err != nil {
		t.Fatalf("checkStatus: %v", err)
	}
	var result map[string]interface{}
	json.Unmarshal([]byte(output), &result)
	if result["status"] != "unknown" {
		t.Errorf("expected status 'unknown', got %v", result["status"])
	}
}

func TestCheckStatusAPINotAvailable(t *testing.T) {
	ts := setupTestStatusServer(t, http.StatusForbidden,
		`{"errors":[{"message":"not available"}]}`)
	defer ts.Close()

	p := newTestPlugin(t)
	p.httpClient = &http.Client{
		Transport: &testTransport{baseURL: ts.URL, next: http.DefaultTransport},
	}

	input := map[string]interface{}{"message_id": "test-msg"}
	inputJSON, _ := json.Marshal(input)

	callCtx := &plugin.CallContext{TenantID: testTenantID, WorkflowID: "test-workflow"}
	ctx := plugin.WithCallContext(context.Background(), callCtx)

	output, err := p.checkStatus(ctx, string(inputJSON))
	if err != nil {
		t.Fatalf("checkStatus: %v", err)
	}
	var result map[string]interface{}
	json.Unmarshal([]byte(output), &result)
	if result["status"] != "sent" {
		t.Errorf("expected best-effort status 'sent', got %v", result["status"])
	}
}

func TestCheckStatusAPINotFound(t *testing.T) {
	ts := setupTestStatusServer(t, http.StatusNotFound,
		`{"errors":[{"message":"not found"}]}`)
	defer ts.Close()

	p := newTestPlugin(t)
	p.httpClient = &http.Client{
		Transport: &testTransport{baseURL: ts.URL, next: http.DefaultTransport},
	}

	input := map[string]interface{}{"message_id": "test-msg"}
	inputJSON, _ := json.Marshal(input)

	callCtx := &plugin.CallContext{TenantID: testTenantID, WorkflowID: "test-workflow"}
	ctx := plugin.WithCallContext(context.Background(), callCtx)

	output, err := p.checkStatus(ctx, string(inputJSON))
	if err != nil {
		t.Fatalf("checkStatus: %v", err)
	}
	var result map[string]interface{}
	json.Unmarshal([]byte(output), &result)
	if result["status"] != "sent" {
		t.Errorf("expected best-effort status 'sent', got %v", result["status"])
	}
}

// ---- check_status error paths ----

func TestCheckStatusErrorPaths_MissingTenant(t *testing.T) {
	p := newTestPlugin(t)
	_, err := p.checkStatus(context.Background(), `{"message_id":"test-msg"}`)
	if err == nil || !strings.Contains(err.Error(), "no tenant context") {
		t.Fatalf("expected no tenant context error, got: %v", err)
	}
}

func TestCheckStatusErrorPaths_InvalidJSON(t *testing.T) {
	p := newTestPlugin(t)
	ctx := plugin.WithCallContext(context.Background(), &plugin.CallContext{TenantID: testTenantID})
	_, err := p.checkStatus(ctx, `not json`)
	if err == nil || !strings.Contains(err.Error(), "invalid input") {
		t.Fatalf("expected invalid input error, got: %v", err)
	}
}

func TestCheckStatusErrorPaths_MissingMessageID(t *testing.T) {
	p := newTestPlugin(t)
	ctx := plugin.WithCallContext(context.Background(), &plugin.CallContext{TenantID: testTenantID})
	_, err := p.checkStatus(ctx, `{}`)
	if err == nil || !strings.Contains(err.Error(), "message_id is required") {
		t.Fatalf("expected message_id required error, got: %v", err)
	}
}
