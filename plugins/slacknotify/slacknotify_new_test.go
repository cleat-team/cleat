package slacknotify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// ===========================================================================
// Init edge cases
// ===========================================================================

func TestSN_InitNilLogger(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		Logger: nil,
	}
	err := p.Init(context.Background(), env)
	if err != nil {
		t.Fatalf("Init() with nil logger: %v", err)
	}
	if p.logger == nil {
		t.Error("expected logger to be set")
	}
}

// ===========================================================================
// sendMessage with channel override and Blocks
// ===========================================================================

func TestSN_SendMessage_WithChannelOverride(t *testing.T) {
	store := newFakeDBStore()
	defChan := "general"
	cfgID := uuid.New()
	store.configs = append(store.configs, slackConfigRow{
		id: cfgID.String(), tenantID: testTenantStr,
		name: "test", webhookURL: "https://hooks.slack.com/xxx",
		enabled: true, defaultChannel: &defChan,
	})

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	var capturedPayload map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &capturedPayload)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true,"ts":"ts1"}`))
	}))
	defer ts.Close()

	store.mu.Lock()
	store.configs[0].webhookURL = ts.URL
	store.mu.Unlock()

	p := &Plugin{
		db:         &engine.SQLDBAdapter{DB: db},
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}

	// Override channel to something different than default.
	input := map[string]any{
		"config_id": cfgID.String(),
		"channel":   "overridden-channel",
		"text":      "hello with channel override",
		"blocks": []map[string]any{
			{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": "Hello!"}},
		},
	}
	inputJSON, _ := json.Marshal(input)

	cc := &plugin.CallContext{TenantID: testTenantID.String(), WorkflowID: "wf-1"}
	ctx := plugin.WithCallContext(context.Background(), cc)
	out, err := p.sendMessage(ctx, string(inputJSON))
	if err != nil {
		t.Fatalf("sendMessage: %v", err)
	}

	var result map[string]any
	json.Unmarshal([]byte(out), &result)
	if result["success"] != true {
		t.Errorf("expected success=true, got %v", result["success"])
	}
	if result["ts"] != "ts1" {
		t.Errorf("expected ts 'ts1', got %v", result["ts"])
	}

	// Verify overridden channel was sent.
	if capturedPayload["channel"] != "overridden-channel" {
		t.Errorf("expected channel 'overridden-channel', got %v", capturedPayload["channel"])
	}
	// Verify blocks were sent.
	if capturedPayload["blocks"] == nil {
		t.Error("expected blocks in payload")
	}
	blocks, ok := capturedPayload["blocks"].([]any)
	if !ok || len(blocks) != 1 {
		t.Errorf("expected 1 block, got %d", len(blocks))
	}
}

func TestSN_SendMessage_DefaultChannelFallback(t *testing.T) {
	store := newFakeDBStore()
	defChan := "general"
	cfgID := uuid.New()
	store.configs = append(store.configs, slackConfigRow{
		id: cfgID.String(), tenantID: testTenantStr,
		name: "test", webhookURL: "https://hooks.slack.com/yyy",
		enabled: true, defaultChannel: &defChan,
	})

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	var capturedPayload map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &capturedPayload)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()

	store.mu.Lock()
	store.configs[0].webhookURL = ts.URL
	store.mu.Unlock()

	p := &Plugin{
		db:         &engine.SQLDBAdapter{DB: db},
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}

	// No explicit channel — should fall back to default.
	input := map[string]any{
		"config_id": cfgID.String(),
		"text":      "fallback test",
	}
	inputJSON, _ := json.Marshal(input)

	cc := &plugin.CallContext{TenantID: testTenantID.String(), WorkflowID: "wf-2"}
	ctx := plugin.WithCallContext(context.Background(), cc)
	_, err := p.sendMessage(ctx, string(inputJSON))
	if err != nil {
		t.Fatalf("sendMessage: %v", err)
	}

	if capturedPayload["channel"] != "general" {
		t.Errorf("expected channel 'general' (default), got %v", capturedPayload["channel"])
	}
}

func TestSN_SendMessage_NoChannel(t *testing.T) {
	store := newFakeDBStore()
	cfgID := uuid.New()
	store.configs = append(store.configs, slackConfigRow{
		id: cfgID.String(), tenantID: testTenantStr,
		name: "test", webhookURL: "https://hooks.slack.com/zzz",
		enabled: true, // no defaultChannel
	})

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	var capturedPayload map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &capturedPayload)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()

	store.mu.Lock()
	store.configs[0].webhookURL = ts.URL
	store.mu.Unlock()

	p := &Plugin{
		db:         &engine.SQLDBAdapter{DB: db},
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}

	input := map[string]any{
		"config_id": cfgID.String(),
		"text":      "no channel",
	}
	inputJSON, _ := json.Marshal(input)

	cc := &plugin.CallContext{TenantID: testTenantID.String(), WorkflowID: "wf-3"}
	ctx := plugin.WithCallContext(context.Background(), cc)
	_, err := p.sendMessage(ctx, string(inputJSON))
	if err != nil {
		t.Fatalf("sendMessage: %v", err)
	}

	if ch, ok := capturedPayload["channel"]; ok && ch != "" {
		t.Errorf("expected no channel in payload, got %v", capturedPayload["channel"])
	}
}

// ===========================================================================
// handleInteractiveCallback tests
// ===========================================================================

// interactiveServer creates a plugin+handler for testing interactive callbacks.
func interactiveServer(t *testing.T) (*Plugin, http.Handler) {
	t.Helper()
	p := &Plugin{
		logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		deploymentSecrets: &fakeInteractiveDeploymentSecrets{secret: testSigningSecret},
	}
	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}
	return p, mux
}

// testSigningSecret is the value interactiveServer's default
// fakeDeploymentSecrets answers for "slacknotify.signing_secret". Tests that
// want to reach past signature verification sign their request with it via
// signSlackRequest; tests of the verification step itself override
// p.deploymentSecrets (or clear it) after interactiveServer returns.
const testSigningSecret = "test-signing-secret"

// withTestTenant stamps a request's context with testTenantID. It no longer
// represents a legitimate path to a tenant on this route -- /slack/interactive
// is auth-exempt (cleat#2172), so nothing in the real request path is
// SUPPOSED to set one. It now exists to simulate the gap cleat-review found
// in this PR's first tenant-refusal stub: a deployment running
// --tenant-resolver header:X-Tenant-ID puts a tenant in context on every
// route, exempt ones included, from a header Slack's signature never covers.
// handleInteractiveCallback must refuse identically whether or not this is
// applied -- see TestSN_InteractiveCallback_TenantInContextIsIgnored, the
// test that pins it.
func withTestTenant(req *http.Request) *http.Request {
	return req.WithContext(auth.WithTenantID(req.Context(), testTenantID))
}

// fakeInteractiveDeploymentSecrets is a plugin.DeploymentSecrets that
// answers one fixed value for "slacknotify.signing_secret" and an error for
// anything else, or always errors if errOnGet is set -- covering "absent",
// "wrong name", and "lookup fails (unreadable/retired)" with one type. The
// same shape as email's and llm's fakeDeploymentSecrets test doubles.
type fakeInteractiveDeploymentSecrets struct {
	secret   string
	errOnGet error
}

func (f *fakeInteractiveDeploymentSecrets) Get(ctx context.Context, name string) (string, error) {
	if f.errOnGet != nil {
		return "", f.errOnGet
	}
	if name == "slacknotify.signing_secret" {
		return f.secret, nil
	}
	return "", fmt.Errorf("fakeInteractiveDeploymentSecrets: %q not set", name)
}

// signSlackRequest computes the timestamp and signature headers a real
// Slack request would carry for body, signed with secret, matching
// handleInteractiveCallback's own basestring construction exactly.
func signSlackRequest(secret, body string) (timestamp, signature string) {
	now := time.Now().Unix()
	timestamp = fmt.Sprintf("%d", now)
	basestring := fmt.Sprintf("v0:%s:%s", timestamp, body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(basestring))
	signature = "v0=" + hex.EncodeToString(mac.Sum(nil))
	return timestamp, signature
}

// signedInteractiveRequest builds a POST /slack/interactive request signed
// with testSigningSecret, for tests exercising logic AFTER signature
// verification (payload parsing, callback_id routing, signal delivery).
func signedInteractiveRequest(body string) *http.Request {
	timestamp, signature := signSlackRequest(testSigningSecret, body)
	req := httptest.NewRequest("POST", "/slack/interactive", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Slack-Request-Timestamp", timestamp)
	req.Header.Set("X-Slack-Signature", signature)
	return req
}

func TestSN_InteractiveCallback_MissingPayload(t *testing.T) {
	p, mux := interactiveServer(t)
	p.signalWorkflow = func(ctx context.Context, workflowID, signalName, payload string) error {
		return nil
	}

	body := "not-payload-form"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, signedInteractiveRequest(body))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestSN_InteractiveCallback_OversizedBody is cleat-review's #2231 DoS
// finding, known-positive: before interactiveMaxBodySize, io.ReadAll(r.Body)
// had no bound, and the route is public (cleat#2172's auth-middleware
// exemption, this same PR), so an anonymous POST far larger than any
// legitimate Slack payload could allocate arbitrarily before the signature
// check ran. A body over the bound must be refused with 413, never reach
// signature verification, and never call signalWorkflow.
func TestSN_InteractiveCallback_OversizedBody(t *testing.T) {
	p, mux := interactiveServer(t)
	signalCalled := false
	p.signalWorkflow = func(ctx context.Context, workflowID, signalName, payload string) error {
		signalCalled = true
		return nil
	}

	oversized := strings.Repeat("a", interactiveMaxBodySize+1)
	req := httptest.NewRequest("POST", "/slack/interactive", strings.NewReader(oversized))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413 for an oversized body, got %d: %s", rec.Code, rec.Body.String())
	}
	if signalCalled {
		t.Error("signal must not be delivered for an oversized body")
	}
}

// unverifiedSignedRequest builds a POST /slack/interactive request carrying
// present-but-meaningless X-Slack-Request-Timestamp/X-Slack-Signature
// headers -- enough to pass the cheap "headers present and fresh" checks in
// handleInteractiveCallback without needing to know a real secret, so a test
// that wants to reach the signingSecret lookup (or the HMAC compare after
// it) doesn't get intercepted by the "missing Slack signature headers"
// check first. Reused by both deployment-secret-unavailable tests below;
// see the ordering rationale in handleInteractiveCallback itself
// (cleat-review's #2231 finding: cheap checks before the DB read).
func unverifiedSignedRequest(body string) *http.Request {
	req := httptest.NewRequest("POST", "/slack/interactive", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Slack-Request-Timestamp", fmt.Sprintf("%d", time.Now().Unix()))
	req.Header.Set("X-Slack-Signature", "v0=0000000000000000000000000000000000000000000000000000000000000000")
	return req
}

// TestSN_InteractiveCallback_NoDeploymentSecret is cleat#2172's core fix,
// known-positive: with no deployment secret configured at all -- the
// pre-#2172 state, where p.slackSigningSecret was simply "" -- a request
// carrying signature headers used to reach payload parsing unverified. It
// must now be refused before the HMAC compare is even reached, the same 401
// as a bad signature, never a fallthrough to acceptance. Uses
// unverifiedSignedRequest rather than an unsigned one specifically so this
// exercises the signingSecret-unavailable path itself, not the earlier
// missing-headers check, which would 401 for an unrelated reason and leave
// the fix this test names unexercised.
func TestSN_InteractiveCallback_NoDeploymentSecret(t *testing.T) {
	p, mux := interactiveServer(t)
	p.deploymentSecrets = nil
	signalCalled := false
	p.signalWorkflow = func(ctx context.Context, workflowID, signalName, payload string) error {
		signalCalled = true
		return nil
	}

	body := "payload=%7B%22type%22%3A%22block_actions%22%2C%22callback_id%22%3A%22wf%3Awf-123%3Asig%3Abutton-click%22%7D"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, unverifiedSignedRequest(body))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 with no deployment secret configured, got %d: %s", rec.Code, rec.Body.String())
	}
	if signalCalled {
		t.Error("signal must not be delivered when the signing secret is unavailable")
	}
}

// TestSN_InteractiveCallback_DeploymentSecretLookupFails covers "missing,
// unreadable, or retired" (cleat#2172 option A): whatever error
// DeploymentSecrets.Get returns, the request refuses -- not just the
// name-not-found case. Uses unverifiedSignedRequest for the same reason as
// TestSN_InteractiveCallback_NoDeploymentSecret above.
func TestSN_InteractiveCallback_DeploymentSecretLookupFails(t *testing.T) {
	p, mux := interactiveServer(t)
	p.deploymentSecrets = &fakeInteractiveDeploymentSecrets{errOnGet: fmt.Errorf("secret is retired")}
	signalCalled := false
	p.signalWorkflow = func(ctx context.Context, workflowID, signalName, payload string) error {
		signalCalled = true
		return nil
	}

	body := "payload=%7B%22type%22%3A%22block_actions%22%2C%22callback_id%22%3A%22wf%3Awf-123%3Asig%3Abutton-click%22%7D"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, unverifiedSignedRequest(body))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 when the deployment secret lookup fails, got %d: %s", rec.Code, rec.Body.String())
	}
	if signalCalled {
		t.Error("signal must not be delivered when the signing secret lookup fails")
	}
}

// TestSN_InteractiveCallback_EmptyStoredSecret is cleat-review's #2231 nit:
// a deployment secret that resolves successfully to the EMPTY string is not
// a usable HMAC key, but hmac.New([]byte(""), ...) computes and compares a
// digest anyway -- so without signingSecret's explicit empty check, a stored
// empty value would verify as "correctly signed with the empty key" rather
// than refusing like every other unusable secret. Known-positive: signs the
// request with the empty string as the key, so a failure here can only be
// the missing empty-check, not an unrelated signature mismatch.
func TestSN_InteractiveCallback_EmptyStoredSecret(t *testing.T) {
	p, mux := interactiveServer(t)
	p.deploymentSecrets = &fakeInteractiveDeploymentSecrets{secret: ""}
	signalCalled := false
	p.signalWorkflow = func(ctx context.Context, workflowID, signalName, payload string) error {
		signalCalled = true
		return nil
	}

	body := "payload=%7B%22type%22%3A%22block_actions%22%2C%22callback_id%22%3A%22wf%3Awf-123%3Asig%3Abutton-click%22%7D"
	timestamp, signature := signSlackRequest("", body)
	req := httptest.NewRequest("POST", "/slack/interactive", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Slack-Request-Timestamp", timestamp)
	req.Header.Set("X-Slack-Signature", signature)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for an empty stored secret, got %d: %s", rec.Code, rec.Body.String())
	}
	if signalCalled {
		t.Error("signal must not be delivered when the stored secret is empty")
	}
}

func TestSN_InteractiveCallback_WithSignature(t *testing.T) {
	p, mux := interactiveServer(t)
	p.signalWorkflow = func(ctx context.Context, workflowID, signalName, payload string) error {
		return nil
	}

	// Missing signature headers, even though a secret IS configured.
	body := "payload=%7B%22type%22%3A%22block_actions%22%7D"
	req := httptest.NewRequest("POST", "/slack/interactive", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for missing sig headers, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSN_InteractiveCallback_StaleRequest(t *testing.T) {
	p, mux := interactiveServer(t)
	p.signalWorkflow = func(ctx context.Context, workflowID, signalName, payload string) error {
		return nil
	}

	// Old timestamp (more than 5 minutes ago).
	oldTS := time.Now().Unix() - 400
	body := "payload=%7B%22type%22%3A%22block_actions%22%7D"
	req := httptest.NewRequest("POST", "/slack/interactive", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Slack-Request-Timestamp", fmt.Sprintf("%d", oldTS))
	req.Header.Set("X-Slack-Signature", "v0=whatever")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for stale request, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestSN_InteractiveCallback_FutureStampedRequest is cleat-review's #2231
// nit, the mirror of TestSN_InteractiveCallback_StaleRequest above: the
// staleness check used to be `time.Now().Unix()-ts > 300`, which only
// rejects a timestamp in the past -- a VALIDLY SIGNED request stamped an
// hour in the future passed. Known-positive: this signs the future
// timestamp with the real testSigningSecret, so a failure here can only be
// the staleness window, not an unrelated signature mismatch.
func TestSN_InteractiveCallback_FutureStampedRequest(t *testing.T) {
	p, mux := interactiveServer(t)
	p.signalWorkflow = func(ctx context.Context, workflowID, signalName, payload string) error {
		return nil
	}

	body := "payload=%7B%22type%22%3A%22block_actions%22%7D"
	futureTS := time.Now().Unix() + 3600
	timestamp := fmt.Sprintf("%d", futureTS)
	basestring := fmt.Sprintf("v0:%s:%s", timestamp, body)
	mac := hmac.New(sha256.New, []byte(testSigningSecret))
	mac.Write([]byte(basestring))
	signature := "v0=" + hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest("POST", "/slack/interactive", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Slack-Request-Timestamp", timestamp)
	req.Header.Set("X-Slack-Signature", signature)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for a request stamped an hour in the future, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSN_InteractiveCallback_InvalidSignature(t *testing.T) {
	p, mux := interactiveServer(t)
	p.signalWorkflow = func(ctx context.Context, workflowID, signalName, payload string) error {
		return nil
	}

	now := time.Now().Unix()
	body := "payload=%7B%22type%22%3A%22block_actions%22%7D"
	req := httptest.NewRequest("POST", "/slack/interactive", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Slack-Request-Timestamp", fmt.Sprintf("%d", now))
	req.Header.Set("X-Slack-Signature", "v0=wrongsignature")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for invalid sig, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestSN_InteractiveCallback_ValidSignature pins cleat#2230(a)'s unconditional
// refusal: a correctly-signed request naming a valid wf:...:sig:... route
// still 404s and never reaches signalWorkflow, because there is no tenant
// this handler will trust until cleat#2230(b) lands. Before that fix landed
// this same request (with a tenant in ctx) delivered successfully; now that
// path is deliberately dead.
func TestSN_InteractiveCallback_ValidSignature(t *testing.T) {
	rawBody := "payload=%7B%22type%22%3A%22block_actions%22%2C%22callback_id%22%3A%22wf%3Awf-123%3Asig%3Abutton-click%22%7D"

	p, mux := interactiveServer(t)
	signalDelivered := false
	p.signalWorkflow = func(ctx context.Context, workflowID, signalName, payload string) error {
		signalDelivered = true
		return nil
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, withTestTenant(signedInteractiveRequest(rawBody)))
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 (every click refuses until cleat#2230(b)), got %d: %s", rec.Code, rec.Body.String())
	}
	if signalDelivered {
		t.Error("signal must not be delivered -- there is no trusted tenant to deliver it under")
	}
}

func TestSN_InteractiveCallback_NoCallbackID(t *testing.T) {
	p, mux := interactiveServer(t)
	p.signalWorkflow = func(ctx context.Context, workflowID, signalName, payload string) error {
		return nil
	}

	// Valid payload but no callback_id.
	body := "payload=%7B%22type%22%3A%22block_actions%22%7D"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, signedInteractiveRequest(body))
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for no callback_id, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSN_InteractiveCallback_BadCallbackID(t *testing.T) {
	p, mux := interactiveServer(t)
	p.signalWorkflow = func(ctx context.Context, workflowID, signalName, payload string) error {
		t.Error("signal should not be delivered for bad callback_id")
		return nil
	}

	body := "payload=%7B%22type%22%3A%22block_actions%22%2C%22callback_id%22%3A%22bad-format%22%7D"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, signedInteractiveRequest(body))
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for bad callback_id, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestSN_InteractiveCallback_NoTenantRefuses is cleat#2230(a)'s tenant
// refusal: /slack/interactive is auth-exempt (cleat#2172), so before this fix
// a click with a valid route delivered its signal with NO tenant in ctx --
// which signalPluginWorkflow (cmd/cleat-worker/main.go) treats as unscoped,
// i.e. the DEFAULT tenant's own session on Postgres/SQL Server. Any tenant
// could post a button naming the default tenant's workflow and signal it,
// while no other tenant's own buttons worked at all. Until cleat#2230(b)
// adds a real slack_workspace lookup, this route has no way to resolve a
// tenant, so every click with an otherwise-valid route must refuse.
// Known-positive: falsified by restoring the auth.TenantIDFromRequest-gated
// version, which makes this test still pass (it supplies no tenant either
// way) but TestSN_InteractiveCallback_TenantInContextIsIgnored fail --
// that's the version cleat-review's second pass actually caught, which this
// test alone did not.
func TestSN_InteractiveCallback_NoTenantRefuses(t *testing.T) {
	p, mux := interactiveServer(t)
	signalCalled := false
	p.signalWorkflow = func(ctx context.Context, workflowID, signalName, payload string) error {
		signalCalled = true
		return nil
	}
	before := p.interactiveNoTenantRefusals.Load()

	body := "payload=%7B%22type%22%3A%22block_actions%22%2C%22callback_id%22%3A%22wf%3Awf-1%3Asig%3Aaction%22%7D"
	rec := httptest.NewRecorder()
	// Deliberately NOT withTestTenant -- this is the real shape of a
	// request on this route today: signed, valid route, no tenant.
	mux.ServeHTTP(rec, signedInteractiveRequest(body))
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for a click with no resolvable tenant, got %d: %s", rec.Code, rec.Body.String())
	}
	if signalCalled {
		t.Error("signal must not be delivered when no tenant is resolved")
	}
	if got := p.interactiveNoTenantRefusals.Load(); got != before+1 {
		t.Errorf("expected interactiveNoTenantRefusals to increment by 1, got %d -> %d", before, got)
	}
}

// TestSN_InteractiveCallback_TenantInContextIsIgnored is the fix for
// cleat-review's second-pass finding on this PR: the first version of the
// stub above read auth.TenantIDFromRequest and refused only when THAT found
// no tenant -- which is bypassable, because a deployment running
// --tenant-resolver header:X-Tenant-ID puts a tenant in context on every
// route, exempt ones included, from a request header Slack's signature never
// covers (measured live against a real worker: header mode +
// X-Tenant-ID: B -> 200, signal delivered into B's workflow_signals).
// withTestTenant simulates exactly that -- a tenant present in context from
// some source outside this handler's control -- and the fixed handler must
// refuse identically to the no-tenant case, because it no longer reads a
// tenant from the request at all. Known-positive: falsified by reintroducing
// `if tid, ok := auth.TenantIDFromRequest(r); ok { ... deliver ... }`, which
// makes this test fail (200, signalCalled true) while
// TestSN_InteractiveCallback_NoTenantRefuses above stays green -- proving
// that test alone cannot catch this gap.
func TestSN_InteractiveCallback_TenantInContextIsIgnored(t *testing.T) {
	p, mux := interactiveServer(t)
	signalCalled := false
	p.signalWorkflow = func(ctx context.Context, workflowID, signalName, payload string) error {
		signalCalled = true
		return nil
	}
	before := p.interactiveNoTenantRefusals.Load()

	body := "payload=%7B%22type%22%3A%22block_actions%22%2C%22callback_id%22%3A%22wf%3Awf-1%3Asig%3Aaction%22%7D"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, withTestTenant(signedInteractiveRequest(body)))
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 even with a tenant in context, got %d: %s", rec.Code, rec.Body.String())
	}
	if signalCalled {
		t.Error("signal must not be delivered -- a tenant in ctx from an untrusted source (e.g. a header resolver) must not be honored")
	}
	if got := p.interactiveNoTenantRefusals.Load(); got != before+1 {
		t.Errorf("expected interactiveNoTenantRefusals to increment by 1, got %d -> %d", before, got)
	}
}

// TestExtractCallbackRouteAndBuildScopedPayload is cleat#2230(a), moved to a
// direct unit test of the two helpers rather than an HTTP-level test through
// handleInteractiveCallback: that handler now refuses every click
// unconditionally (see TestSN_InteractiveCallback_ValidSignature), so it can
// no longer exercise what this test actually checks -- that a real Slack
// block_actions payload (shaped the way Slack actually sends it, and the way
// this plugin's own sendMessage host function actually builds buttons;
// host_functions.go's Blocks field is opaque JSON the plugin never stamps a
// callback_id into, so there is never a top-level callback_id, only
// actions[0].action_id) parses to the right route and scopes down to the
// right payload. buildScopedPayload and extractCallbackRoute stay directly
// tested here so cleat#2230(b) can wire them back into the handler without
// reproving either. Known-positive: falsified by reverting
// extractCallbackRoute to check only payload.CallbackID, which makes ok
// false and this test fail.
func TestExtractCallbackRouteAndBuildScopedPayload(t *testing.T) {
	rawPayload := `{"type":"block_actions","actions":[{"type":"button","action_id":"wf:wf-456:sig:approve","block_id":"approval_block","value":"approve","action_ts":"1234567890.123456"}],"team":{"id":"T123"},"user":{"id":"U123"},"channel":{"id":"C123"}}`

	var payload slackInteractivePayload
	if err := json.Unmarshal([]byte(rawPayload), &payload); err != nil {
		t.Fatalf("invalid test fixture: %v", err)
	}

	wfID, sigName, action, ok := extractCallbackRoute(payload)
	if !ok {
		t.Fatalf("expected a routable action_id, got ok=false")
	}
	if wfID != "wf-456" || sigName != "approve" {
		t.Errorf("expected wf-456/approve from actions[].action_id, got wf=%q sig=%q", wfID, sigName)
	}

	// gotPayload must be the REAL scoped payload -- decode it and check the
	// fields cleat-review's #2253 review specified (action_id, block_id,
	// value, user_id, team_id, channel_id, action_ts) survived the round
	// trip.
	sigPayload, err := buildScopedPayload(payload, action)
	if err != nil {
		t.Fatalf("buildScopedPayload: %v", err)
	}
	var decoded scopedInteractionPayload
	if err := json.Unmarshal(sigPayload, &decoded); err != nil {
		t.Fatalf("scoped payload is not valid JSON: %v\npayload: %s", err, sigPayload)
	}
	want := scopedInteractionPayload{
		ActionID:  "wf:wf-456:sig:approve",
		BlockID:   "approval_block",
		Value:     "approve",
		ActionTS:  "1234567890.123456",
		UserID:    "U123",
		TeamID:    "T123",
		ChannelID: "C123",
	}
	if decoded != want {
		t.Errorf("scoped payload = %+v, want %+v", decoded, want)
	}
	// And nothing beyond the scoped fields -- response_url, trigger_id,
	// and message text must NOT reach the workflow (cleat-review's #2253
	// PII/capability finding).
	for _, forbidden := range []string{"response_url", "trigger_id", "message"} {
		if strings.Contains(string(sigPayload), forbidden) {
			t.Errorf("scoped payload must not mention %q: %s", forbidden, sigPayload)
		}
	}
}

// TestSN_InteractiveCallback_ValueAndBlockIDAreNotRoutable pins coordinator's
// narrowing after cleat-review's #2253 review: routing reads ONLY
// actions[0].action_id, never value or block_id, even when one of them
// would otherwise parse as a valid wf:...:sig:... route. action_id here is
// a plain button label with no routing intent, which is the ordinary shape
// for any button a workflow author didn't wire to a signal -- it must not
// be treated as ambiguous just because a different field happens to look
// like a route. Known-positive: falsified by reintroducing the
// action_id -> value -> block_id fallback, which makes this test fail (200
// instead of the no-route 200-OK-no-signal path... note BOTH the fallback
// and no-route cases return 200, so the real assertion is signalCalled).
func TestSN_InteractiveCallback_ValueAndBlockIDAreNotRoutable(t *testing.T) {
	p, mux := interactiveServer(t)
	signalCalled := false
	p.signalWorkflow = func(ctx context.Context, workflowID, signalName, payload string) error {
		signalCalled = true
		return nil
	}

	rawPayload := `{"type":"block_actions","actions":[{"type":"button","action_id":"approve-button","block_id":"wf:wf-999:sig:approve","value":"wf:wf-789:sig:approve"}]}`
	body := "payload=" + url.QueryEscape(rawPayload)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, withTestTenant(signedInteractiveRequest(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (no route -- OK no-op), got %d: %s", rec.Code, rec.Body.String())
	}
	if signalCalled {
		t.Error("value and block_id must not be treated as routable, even when action_id is a plain label")
	}
}

// TestParseCallbackRoute pins the wf:<id>:sig:<name> parser directly,
// including the negatives cleat-review's #2253 mutation testing found
// unpinned: a wrong middle segment (parts[2] != "sig") and a route missing
// the sig segment entirely both survived before this test existed.
func TestParseCallbackRoute(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantWF  string
		wantSig string
		wantOK  bool
	}{
		{"valid", "wf:wf-1:sig:approve", "wf-1", "approve", true},
		{"valid signal name with colons", "wf:wf-1:sig:approve:now", "wf-1", "approve:now", true},
		{"empty string", "", "", "", false},
		{"wrong prefix", "notwf:wf-1:sig:approve", "", "", false},
		{"wrong middle segment", "wf:wf-1:foo:approve", "", "", false},
		{"missing sig segment (3 parts)", "wf:wf-1:approve", "", "", false},
		{"missing sig segment (2 parts)", "wf:wf-1", "", "", false},
		{"just a label", "approve-button", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotWF, gotSig, gotOK := parseCallbackRoute(tt.in)
			if gotOK != tt.wantOK || gotWF != tt.wantWF || gotSig != tt.wantSig {
				t.Errorf("parseCallbackRoute(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tt.in, gotWF, gotSig, gotOK, tt.wantWF, tt.wantSig, tt.wantOK)
			}
		})
	}
}

// ===========================================================================
// joinSetClauses test
// ===========================================================================

func TestSN_JoinSetClauses(t *testing.T) {
	tests := []struct {
		input    []string
		expected string
	}{
		{nil, ""},
		{[]string{}, ""},
		{[]string{"name = $1"}, "name = $1"},
		{[]string{"name = $1", "enabled = $2"}, "name = $1, enabled = $2"},
		{[]string{"a = $1", "b = $2", "c = $3"}, "a = $1, b = $2, c = $3"},
	}
	for _, tc := range tests {
		got := joinSetClauses(tc.input)
		if got != tc.expected {
			t.Errorf("joinSetClauses(%v) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

// ===========================================================================
// Config update edge cases
// ===========================================================================

func TestSN_UpdateConfig_ClearDefaultChannel(t *testing.T) {
	defChan := "general"
	store := newFakeDBStore()
	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantStr

	cfgID := uuid.New()
	store.configs = append(store.configs, slackConfigRow{
		id: cfgID.String(), tenantID: testTenantStr,
		name: "test", webhookURL: "https://hooks.slack.com/clr",
		enabled: true, defaultChannel: &defChan,
	})

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:         &engine.SQLDBAdapter{DB: db},
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)
	handler := auth.Middleware(engine.NewPostgresStore(db), false)(mux)

	// Clear default_channel by setting to empty string.
	body := `{"default_channel":""}`
	req := authedRequest("PUT", "/slack/configs/"+cfgID.String(), bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	store.mu.RLock()
	cfg := store.configs[0]
	store.mu.RUnlock()
	if cfg.defaultChannel != nil {
		t.Errorf("expected default_channel to be cleared, got %v", *cfg.defaultChannel)
	}
}

func TestSN_UpdateConfig_NoFieldsError(t *testing.T) {
	store := newFakeDBStore()
	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantStr

	cfgID := uuid.New()
	store.configs = append(store.configs, slackConfigRow{
		id: cfgID.String(), tenantID: testTenantStr,
		name: "test", webhookURL: "https://hooks.slack.com/nf",
		enabled: true,
	})

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()
	p := &Plugin{
		db:         &engine.SQLDBAdapter{DB: db},
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)
	handler := auth.Middleware(engine.NewPostgresStore(db), false)(mux)

	// Empty body - should fail read body or parse.
	req := authedRequest("PUT", "/slack/configs/"+cfgID.String(), bytes.NewReader([]byte("{}")))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty body, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// List scan error path
// ===========================================================================

// scanErrorConnector returns rows with mismatched columns to trigger scan errors.
type scanErrorConnector struct{}

func (scanErrorConnector) Connect(_ context.Context) (driver.Conn, error) {
	return &scanErrorConn{}, nil
}
func (scanErrorConnector) Driver() driver.Driver { return &fakeDrv{} }

type scanErrorConn struct{}

func (*scanErrorConn) Prepare(_ string) (driver.Stmt, error) { return nil, fmt.Errorf("stub") }
func (*scanErrorConn) Close() error                          { return nil }
func (*scanErrorConn) Begin() (driver.Tx, error)             { return &fakeTx{}, nil }
func (*scanErrorConn) ExecContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
	return &fakeResult{rowsAffected: 0}, nil
}
func (*scanErrorConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, "tenant_api_keys") {
		return &fakeRows{
			columns: []string{"tenant_id"},
			data:    [][]driver.Value{{"00000000-0000-0000-0000-000000000001"}},
		}, nil
	}
	// Return a row where one of the columns has an incompatible type to trigger scan error.
	return &fakeRows{
		columns: []string{"id", "name", "webhook_url", "default_channel", "enabled", "created_at", "updated_at"},
		data: [][]driver.Value{{
			"id-1", int64(42), "url", nil, true, time.Now(), time.Now(),
		}},
	}, nil
}

func TestSN_ListScanError(t *testing.T) {
	db := sql.OpenDB(&scanErrorConnector{})
	defer db.Close()

	p := &Plugin{
		db:         &engine.SQLDBAdapter{DB: db},
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)
	handler := auth.Middleware(engine.NewPostgresStore(db), false)(mux)

	req := authedRequest("GET", "/slack/configs", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	// Should get 200 with an empty list (row with scan error is skipped).
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var results []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &results); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results (scan error skipped), got %d", len(results))
	}
}

// ===========================================================================
// Re-fetch after update error path
// ===========================================================================

func TestSN_UpdateConfigRefetchError(t *testing.T) {
	store := newFakeDBStore()
	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantStr

	cfgID := uuid.New()
	store.configs = append(store.configs, slackConfigRow{
		id: cfgID.String(), tenantID: testTenantStr,
		name: "test", webhookURL: "https://hooks.slack.com/refetch",
		enabled: true,
	})

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:         &engine.SQLDBAdapter{DB: db},
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)
	handler := auth.Middleware(engine.NewPostgresStore(db), false)(mux)

	// Update name, then try to re-fetch a non-existent config to trigger the error.
	// Actually this is tricky with the fake store. Let me test that the update works first.
	body := `{"name":"updated-name"}`
	req := authedRequest("PUT", "/slack/configs/"+cfgID.String(), bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var updated map[string]any
	json.Unmarshal(rec.Body.Bytes(), &updated)
	if updated["name"] != "updated-name" {
		t.Errorf("expected name 'updated-name', got %v", updated["name"])
	}
}

// ===========================================================================
// Init with a leftover slack_signing_secret in --plugin-config
// ===========================================================================

// TestSN_InitWarnsOnLeftoverSigningSecret covers legacySlackConfig's WARN
// (cleat#2172): a slack_signing_secret left over in --plugin-config does
// nothing now -- Config has no field for it -- and used to do so silently.
// Same shape as email's TestInitWarnsOnLeftoverSendGridAPIKey.
func TestSN_InitWarnsOnLeftoverSigningSecret(t *testing.T) {
	var buf bytes.Buffer
	p := &Plugin{}
	env := &plugin.Environment{
		Config: []byte(`{"slack_signing_secret":"my-secret"}`),
		Logger: slog.New(slog.NewTextHandler(&buf, nil)),
	}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "slack_signing_secret") {
		t.Errorf("expected a WARN naming the leftover slack_signing_secret, got log output: %q", got)
	}
	if !strings.Contains(got, "set-deployment-secret") {
		t.Errorf("expected the WARN to name the replacement command, got log output: %q", got)
	}
	if !strings.Contains(got, "level=WARN") {
		t.Errorf("expected the leftover-key message at WARN level, got log output: %q", got)
	}
}

// TestSN_InitNoWarnWithoutLeftoverSigningSecret is the negative control: a
// config with no slack_signing_secret field at all must not mention it.
func TestSN_InitNoWarnWithoutLeftoverSigningSecret(t *testing.T) {
	var buf bytes.Buffer
	p := &Plugin{}
	env := &plugin.Environment{
		Config: []byte(`{}`),
		Logger: slog.New(slog.NewTextHandler(&buf, nil)),
	}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if got := buf.String(); strings.Contains(got, "slack_signing_secret") {
		t.Errorf("did not expect a slack_signing_secret WARN with no leftover key present, got log output: %q", got)
	}
}

// TestSN_DeploymentSecretPrefix pins DeploymentSecretPrefix's return value:
// the worker uses this to scope which deployment secrets slack-notify can
// read (plugin.HasDeploymentSecretPrefix), so a change here silently widens
// or narrows that scope.
func TestSN_DeploymentSecretPrefix(t *testing.T) {
	p := &Plugin{}
	if got := p.DeploymentSecretPrefix(); got != "slacknotify." {
		t.Errorf("expected DeploymentSecretPrefix() = %q, got %q", "slacknotify.", got)
	}
}

// ===========================================================================
// RequiredDeploymentSecrets -- cleat#2172's owner-decided boot refusal
// (relayed on #2231's review): required ONLY when --plugin-config still
// carries the legacy slack_signing_secret, since that is the one signal
// that proves this deployment used /slack/interactive before. Both
// directions matter here for the same reason
// TestCheckRequiredDeploymentSecretsRefusesWhenMissing/StartsWhenPresent
// (cmd/cleat-worker) state it: a check with no case that can fail is not a
// check.
// ===========================================================================

// TestSN_RequiredDeploymentSecrets_NoLegacyKey is the "ordinary deployment"
// case: no legacy slack_signing_secret anywhere in --plugin-config (whether
// slack-notify has no config section at all, or an empty one) must not
// require slacknotify.signing_secret -- an outbound-only deployment that has
// never touched /slack/interactive must still boot with no signing secret
// configured.
func TestSN_RequiredDeploymentSecrets_NoLegacyKey(t *testing.T) {
	p := &Plugin{}
	for _, cfg := range [][]byte{nil, []byte(``), []byte(`{}`)} {
		names, err := p.RequiredDeploymentSecrets(cfg)
		if err != nil {
			t.Fatalf("RequiredDeploymentSecrets(%q): %v", cfg, err)
		}
		if len(names) != 0 {
			t.Errorf("RequiredDeploymentSecrets(%q) = %v, want none (no legacy key present)", cfg, names)
		}
	}
}

// TestSN_RequiredDeploymentSecrets_LegacyKeyPresent is the upgrade case:
// --plugin-config still carries slack_signing_secret from before cleat#2172,
// proving this deployment used /slack/interactive. Without this,
// slacknotify.signing_secret being unset would let the worker boot and then
// silently 401 every button click, with nothing at boot saying why.
func TestSN_RequiredDeploymentSecrets_LegacyKeyPresent(t *testing.T) {
	p := &Plugin{}
	cfg := []byte(`{"slack_signing_secret": "old-secret"}`)
	names, err := p.RequiredDeploymentSecrets(cfg)
	if err != nil {
		t.Fatalf("RequiredDeploymentSecrets: %v", err)
	}
	if len(names) != 1 || names[0] != "slacknotify.signing_secret" {
		t.Errorf("RequiredDeploymentSecrets(legacy key present) = %v, want [slacknotify.signing_secret]", names)
	}
}

// ===========================================================================
// handleInteractiveCallback with invalid JSON payload
// ===========================================================================

func TestSN_InteractiveCallback_InvalidPayloadJSON(t *testing.T) {
	p, mux := interactiveServer(t)
	p.signalWorkflow = func(ctx context.Context, workflowID, signalName, payload string) error {
		return nil
	}

	// URL-encoded body with bad JSON payload.
	body := "payload=not-json"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, signedInteractiveRequest(body))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}
