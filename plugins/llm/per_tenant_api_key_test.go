package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/llm/providers"
)

// cleat#1988. chatRequest had no api_key field, so a per-tenant key passed as
// ${secret:NAME} was resolved, decoded into the request struct, and silently
// discarded there -- every tenant used the operator's configured key
// regardless of what a workflow asked for.
//
// WHAT THESE TESTS DO NOT COVER, AND WHY: the acceptance criterion for #1988
// reads "with two tenants and two different keys held as tenant secrets, a
// fake provider receives each tenant's own key". These tests supply
// req.APIKey directly rather than going through a real SecretStore and
// ${secret:NAME} substitution -- that pairing (which tenant's secret store
// row produces which string) is engine.ResolveSecretRefs's contract, already
// covered in engine/tenant_secrets_test.go, and the wiring that calls it
// before a plugin function runs is cmd/cleat-worker's withSecrets /
// withSecretsStream, covered in cmd/cleat-worker/secret_substitution_test.go
// (cleat#1987). What is llm-specific, and therefore what belongs here, is
// narrower: does chat/chatStream actually USE req.APIKey once it arrives?
// Before this fix the answer was no, because the field did not exist.

// recordingLLMServer is an OpenAI-compatible httptest server that records
// every request's Authorization header rather than checking it against one
// fixed value -- fakeLLMServer (host_functions_test.go) rejects any key but
// "sk-test", which cannot tell two different accepted keys apart.
type recordingLLMServer struct {
	*httptest.Server
	mu   sync.Mutex
	auth []string
}

func newRecordingLLMServer(t *testing.T) *recordingLLMServer {
	t.Helper()
	r := &recordingLLMServer{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.auth = append(r.auth, req.Header.Get("Authorization"))
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]string{"role": "assistant", "content": "ok"},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
			"model": "gpt-4o",
		})
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *recordingLLMServer) lastAuth() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.auth) == 0 {
		return ""
	}
	return r.auth[len(r.auth)-1]
}

func (r *recordingLLMServer) requestCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.auth)
}

// recordingLLMStreamServer is the SSE-streaming counterpart, mirroring
// fakeOpenAIStreamServer (llm_behavioral_test.go) but recording the
// Authorization header instead of rejecting anything but one fixed value.
type recordingLLMStreamServer struct {
	*httptest.Server
	mu   sync.Mutex
	auth []string
}

func newRecordingLLMStreamServer(t *testing.T) *recordingLLMStreamServer {
	t.Helper()
	r := &recordingLLMStreamServer{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.auth = append(r.auth, req.Header.Get("Authorization"))
		r.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		flusher.Flush()
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *recordingLLMStreamServer) lastAuth() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.auth) == 0 {
		return ""
	}
	return r.auth[len(r.auth)-1]
}

func (r *recordingLLMStreamServer) requestCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.auth)
}

// drainStream reads every event off ch, discarding content, so the goroutine
// in chatStream that closes it can be joined before the test proceeds.
func drainStream(ch <-chan plugin.StreamEvent) {
	for range ch {
	}
}

// ===========================================================================
// chat -- per-tenant api_key
// ===========================================================================

func TestChatUsesRequestAPIKeyOverConfigured(t *testing.T) {
	srv := newRecordingLLMServer(t)
	p := setupPlugin(t, srv.URL, "openai")

	tenants := []struct {
		tenant string
		key    string
	}{
		{"11111111-1111-1111-1111-111111111111", "tenant-1-key"},
		{"22222222-2222-2222-2222-222222222222", "tenant-2-key"},
	}

	for _, tc := range tenants {
		req := chatRequest{
			Provider: "openai",
			Model:    "gpt-4o",
			Messages: []providers.Message{{Role: "user", Content: "hello"}},
			APIKey:   tc.key,
		}
		reqJSON, _ := json.Marshal(req)
		ctx := ctxForTenant(tc.tenant)
		if _, err := p.chat(ctx, string(reqJSON)); err != nil {
			t.Fatalf("chat() for tenant %s: %v", tc.tenant, err)
		}
		want := "Bearer " + tc.key
		if got := srv.lastAuth(); got != want {
			t.Errorf("tenant %s: provider saw Authorization %q, want %q", tc.tenant, got, want)
		}
	}

	if srv.requestCount() != len(tenants) {
		t.Fatalf("expected %d requests, saw %d", len(tenants), srv.requestCount())
	}
}

func TestChatFallsBackToConfiguredKeyWhenNoRequestAPIKey(t *testing.T) {
	srv := newRecordingLLMServer(t)
	// setupPlugin (host_functions_test.go) configures the "openai" provider
	// with APIKey "sk-test".
	p := setupPlugin(t, srv.URL, "openai")

	req := chatRequest{
		Provider: "openai",
		Model:    "gpt-4o",
		Messages: []providers.Message{{Role: "user", Content: "hello"}},
	}
	reqJSON, _ := json.Marshal(req)
	if _, err := p.chat(tenantCtx(), string(reqJSON)); err != nil {
		t.Fatalf("chat(): %v", err)
	}
	if got, want := srv.lastAuth(), "Bearer sk-test"; got != want {
		t.Errorf("provider saw Authorization %q, want %q (the configured key)", got, want)
	}
}

func TestChatRefusesUnresolvedSecretReference(t *testing.T) {
	srv := newRecordingLLMServer(t)
	p := setupPlugin(t, srv.URL, "openai")

	req := chatRequest{
		Provider: "openai",
		Model:    "gpt-4o",
		Messages: []providers.Message{{Role: "user", Content: "hello"}},
		APIKey:   "${secret:openai}",
	}
	reqJSON, _ := json.Marshal(req)
	_, err := p.chat(tenantCtx(), string(reqJSON))
	if err == nil {
		t.Fatal("expected an error for an unresolved secret reference in api_key")
	}
	if !strings.Contains(err.Error(), "unresolved secret reference") {
		t.Errorf("error does not name the problem: %v", err)
	}
	if srv.requestCount() != 0 {
		t.Errorf("provider was called with an unresolved reference as the key; requests=%d", srv.requestCount())
	}
}

// ===========================================================================
// chatStream -- per-tenant api_key
// ===========================================================================

func TestChatStreamUsesRequestAPIKeyOverConfigured(t *testing.T) {
	srv := newRecordingLLMStreamServer(t)
	p := setupStreamPlugin(t, srv.URL, "openai", "sk-test")

	tenants := []struct {
		tenant string
		key    string
	}{
		{"11111111-1111-1111-1111-111111111111", "tenant-1-stream-key"},
		{"22222222-2222-2222-2222-222222222222", "tenant-2-stream-key"},
	}

	for _, tc := range tenants {
		req := chatRequest{
			Provider: "openai",
			Model:    "test-model",
			Messages: []providers.Message{{Role: "user", Content: "hello"}},
			APIKey:   tc.key,
		}
		reqJSON, _ := json.Marshal(req)
		ctx := ctxForTenant(tc.tenant)
		ch, err := p.chatStream(ctx, string(reqJSON))
		if err != nil {
			t.Fatalf("chatStream() for tenant %s: %v", tc.tenant, err)
		}
		drainStream(ch)
		want := "Bearer " + tc.key
		if got := srv.lastAuth(); got != want {
			t.Errorf("tenant %s: provider saw Authorization %q, want %q", tc.tenant, got, want)
		}
	}

	if srv.requestCount() != len(tenants) {
		t.Fatalf("expected %d requests, saw %d", len(tenants), srv.requestCount())
	}
}

func TestChatStreamFallsBackToConfiguredKeyWhenNoRequestAPIKey(t *testing.T) {
	srv := newRecordingLLMStreamServer(t)
	p := setupStreamPlugin(t, srv.URL, "openai", "sk-test")

	req := chatRequest{
		Provider: "openai",
		Model:    "test-model",
		Messages: []providers.Message{{Role: "user", Content: "hello"}},
	}
	reqJSON, _ := json.Marshal(req)
	ch, err := p.chatStream(streamTenantCtx(), string(reqJSON))
	if err != nil {
		t.Fatalf("chatStream(): %v", err)
	}
	drainStream(ch)
	if got, want := srv.lastAuth(), "Bearer sk-test"; got != want {
		t.Errorf("provider saw Authorization %q, want %q (the configured key)", got, want)
	}
}

func TestChatStreamRefusesUnresolvedSecretReference(t *testing.T) {
	srv := newRecordingLLMStreamServer(t)
	p := setupStreamPlugin(t, srv.URL, "openai", "sk-test")

	req := chatRequest{
		Provider: "openai",
		Model:    "test-model",
		Messages: []providers.Message{{Role: "user", Content: "hello"}},
		APIKey:   "${secret:openai}",
	}
	reqJSON, _ := json.Marshal(req)
	_, err := p.chatStream(streamTenantCtx(), string(reqJSON))
	if err == nil {
		t.Fatal("expected an error for an unresolved secret reference in api_key")
	}
	if !strings.Contains(err.Error(), "unresolved secret reference") {
		t.Errorf("error does not name the problem: %v", err)
	}
	if srv.requestCount() != 0 {
		t.Errorf("provider was called with an unresolved reference as the key; requests=%d", srv.requestCount())
	}
}

// ===========================================================================
// effectiveAPIKey -- direct unit tests for the decision table itself
// ===========================================================================

func TestEffectiveAPIKey(t *testing.T) {
	cfg := ProviderConfig{APIKey: "configured-key"}

	cases := []struct {
		name       string
		requestKey string
		want       string
		wantErr    bool
	}{
		{"no request key falls back to configured", "", "configured-key", false},
		{"request key overrides configured", "override-key", "override-key", false},
		{"unresolved reference is refused", "${secret:openai}", "", true},
		{"unresolved reference mid-string is refused", "prefix-${secret:openai}-suffix", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := effectiveAPIKey(tc.requestKey, cfg)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got key %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// ctxForTenant mirrors tenantCtx/streamTenantCtx (host_functions_test.go,
// llm_behavioral_test.go) but takes the tenant id as a parameter, so two
// tenants in the same test get two distinct, real-looking call contexts
// rather than sharing the fixed one those helpers hardcode.
func ctxForTenant(tenantID string) context.Context {
	return plugin.WithCallContext(context.Background(), &plugin.CallContext{
		TenantID:   tenantID,
		WorkflowID: "test-workflow",
	})
}
