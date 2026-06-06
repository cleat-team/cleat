package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/llm/providers"
)

// ---------------------------------------------------------------------------
// Fake streaming HTTP servers
// ---------------------------------------------------------------------------

// fakeOpenAIStreamServer returns an HTTP server that streams OpenAI-compatible SSE chunks.
func fakeOpenAIStreamServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}

		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"},\"finish_reason\":null}]}\n\n")
		flusher.Flush()
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\" world\"},\"finish_reason\":null}]}\n\n")
		flusher.Flush()
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"\"},\"finish_reason\":\"stop\"}]}\n\n")
		flusher.Flush()
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
}

// fakeOpenAIStreamErrorServer returns a server that returns an HTTP error for streaming.
func fakeOpenAIStreamErrorServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
	}))
}

// fakeAnthropicStreamServer returns an HTTP server that streams Anthropic-compatible SSE events.
func fakeAnthropicStreamServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "sk-ant-test" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}

		fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"content_block\":{\"type\":\"text\",\"text\":\"Hello\"}}\n\n")
		flusher.Flush()
		fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\" from Anthropic\"}}\n\n")
		flusher.Flush()
		fmt.Fprintf(w, "data: {\"type\":\"message_stop\"}\n\n")
		flusher.Flush()
	}))
}

// fakeOllamaStreamServer returns an HTTP server that streams Ollama NDJSON chunks.
func fakeOllamaStreamServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}

		fmt.Fprintf(w, "{\"message\":{\"role\":\"assistant\",\"content\":\"Hello\"},\"done\":false}\n")
		flusher.Flush()
		fmt.Fprintf(w, "{\"message\":{\"role\":\"assistant\",\"content\":\" from Ollama\"},\"done\":false}\n")
		flusher.Flush()
		fmt.Fprintf(w, "{\"message\":{\"role\":\"assistant\",\"content\":\"\"},\"done\":true}\n")
		flusher.Flush()
	}))
}

// ---------------------------------------------------------------------------
// StreamFuncRegistry implementation for testing
// ---------------------------------------------------------------------------

type testStreamRegistry struct {
	funcs       map[string]plugin.FuncOptions
	streamFuncs map[string]bool
}

func newTestStreamRegistry() *testStreamRegistry {
	return &testStreamRegistry{
		funcs:       make(map[string]plugin.FuncOptions),
		streamFuncs: make(map[string]bool),
	}
}

func (r *testStreamRegistry) Register(opts plugin.FuncOptions, fn plugin.PluginFunc) error {
	r.funcs[opts.Name] = opts
	return nil
}

func (r *testStreamRegistry) RegisterStream(opts plugin.FuncOptions, fn plugin.PluginStreamFunc) error {
	r.funcs[opts.Name] = opts
	r.streamFuncs[opts.Name] = true
	return nil
}

// ---------------------------------------------------------------------------
// Helper: setup a plugin with a fake streaming server
// ---------------------------------------------------------------------------

func setupStreamPlugin(t *testing.T, serverURL, provider string, apiKey string) *Plugin {
	t.Helper()
	p := &Plugin{}
	if apiKey == "" {
		apiKey = "sk-test"
	}
	cfg := Config{
		Providers: map[string]ProviderConfig{
			provider: {APIKey: apiKey, BaseURL: serverURL, Enabled: true, DefaultModel: "test-model"},
		},
	}
	cfgJSON, _ := json.Marshal(cfg)
	env := &plugin.Environment{Config: cfgJSON}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init() failed: %v", err)
	}
	return p
}

func setupCustomPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()
	p := &Plugin{}
	cfgJSON, _ := json.Marshal(cfg)
	env := &plugin.Environment{Config: cfgJSON}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init() failed: %v", err)
	}
	return p
}

func streamTenantCtx() context.Context {
	return plugin.WithCallContext(context.Background(), &plugin.CallContext{
		TenantID:   uuid.MustParse("00000000-0000-0000-0000-000000000001").String(),
		WorkflowID: "test-workflow",
	})
}

// ===========================================================================
// chatStream — error paths
// ===========================================================================

func TestLLM_ChatStream_NoTenantContext(t *testing.T) {
	p := &Plugin{}
	_, err := p.chatStream(context.Background(), `{}`)
	if err == nil {
		t.Fatal("expected error for missing tenant context")
	}
	if !strings.Contains(err.Error(), "no tenant context") {
		t.Errorf("expected 'no tenant context' error, got: %v", err)
	}
}

func TestLLM_ChatStream_InvalidJSON(t *testing.T) {
	p := &Plugin{}
	_, err := p.chatStream(streamTenantCtx(), "not json")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "invalid input") {
		t.Errorf("expected 'invalid input' error, got: %v", err)
	}
}

func TestLLM_ChatStream_MissingProvider(t *testing.T) {
	p := &Plugin{}
	req := chatRequest{Messages: []providers.Message{{Role: "user", Content: "hello"}}}
	reqJSON, _ := json.Marshal(req)
	_, err := p.chatStream(streamTenantCtx(), string(reqJSON))
	if err == nil {
		t.Fatal("expected error for missing provider")
	}
	if !strings.Contains(err.Error(), "provider is required") {
		t.Errorf("expected 'provider is required', got: %v", err)
	}
}

func TestLLM_ChatStream_DisabledProvider(t *testing.T) {
	p := &Plugin{}
	p.config = Config{
		Providers: map[string]ProviderConfig{
			"openai": {APIKey: "sk-test", Enabled: false},
		},
	}
	req := chatRequest{Provider: "openai", Messages: []providers.Message{{Role: "user", Content: "hello"}}}
	reqJSON, _ := json.Marshal(req)
	_, err := p.chatStream(streamTenantCtx(), string(reqJSON))
	if err == nil {
		t.Fatal("expected error for disabled provider")
	}
	if !strings.Contains(err.Error(), "not configured or disabled") {
		t.Errorf("expected 'not configured or disabled', got: %v", err)
	}
}

func TestLLM_ChatStream_UnknownProvider(t *testing.T) {
	p := &Plugin{}
	p.config = Config{
		Providers: map[string]ProviderConfig{
			"openai": {APIKey: "sk-test", Enabled: true},
		},
	}
	req := chatRequest{Provider: "nonexistent", Messages: []providers.Message{{Role: "user", Content: "hello"}}}
	reqJSON, _ := json.Marshal(req)
	_, err := p.chatStream(streamTenantCtx(), string(reqJSON))
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
	// For streaming, unknown provider returns error
	if !strings.Contains(err.Error(), "not configured or disabled") {
		t.Errorf("expected 'not configured or disabled', got: %v", err)
	}
}

func TestLLM_ChatStream_MissingProviderConfig(t *testing.T) {
	p := &Plugin{}
	p.config = Config{
		Providers: map[string]ProviderConfig{},
	}
	req := chatRequest{Provider: "openai", Messages: []providers.Message{{Role: "user", Content: "hello"}}}
	reqJSON, _ := json.Marshal(req)
	_, err := p.chatStream(streamTenantCtx(), string(reqJSON))
	if err == nil {
		t.Fatal("expected error for missing provider config")
	}
}

// ===========================================================================
// chatStream — happy path with OpenAI-compatible streaming
// ===========================================================================

func TestLLM_ChatStream_OpenAI(t *testing.T) {
	srv := fakeOpenAIStreamServer(t)
	defer srv.Close()

	p := setupStreamPlugin(t, srv.URL, "openai", "sk-test")
	req := chatRequest{
		Provider: "openai",
		Model:    "gpt-4o",
		Messages: []providers.Message{{Role: "user", Content: "hello"}},
	}
	reqJSON, _ := json.Marshal(req)

	ch, err := p.chatStream(streamTenantCtx(), string(reqJSON))
	if err != nil {
		t.Fatalf("chatStream() returned error: %v", err)
	}

	var events []plugin.StreamEvent
	for e := range ch {
		events = append(events, e)
	}

	if len(events) != 3 {
		t.Fatalf("expected 3 stream events, got %d", len(events))
	}
	if events[0].Content != "Hello" {
		t.Errorf("expected first chunk 'Hello', got %q", events[0].Content)
	}
	if events[1].Content != " world" {
		t.Errorf("expected second chunk ' world', got %q", events[1].Content)
	}
	if !events[2].Finish {
		t.Error("expected last chunk to have Finish=true")
	}
	if events[0].Index != 0 {
		t.Errorf("expected first chunk index 0, got %d", events[0].Index)
	}
}

// ===========================================================================
// chatStream — OpenAI streaming with error response from server
// ===========================================================================

func TestLLM_ChatStream_OpenAI_HTTPError(t *testing.T) {
	srv := fakeOpenAIStreamErrorServer(t)
	defer srv.Close()

	p := setupStreamPlugin(t, srv.URL, "openai", "sk-test")
	req := chatRequest{
		Provider: "openai",
		Model:    "gpt-4o",
		Messages: []providers.Message{{Role: "user", Content: "hello"}},
	}
	reqJSON, _ := json.Marshal(req)

	ch, err := p.chatStream(streamTenantCtx(), string(reqJSON))
	if err != nil {
		t.Fatalf("chatStream() returned unexpected error: %v", err)
	}

	// The channel should simply close with no events
	events := 0
	for range ch {
		events++
	}
	if events != 0 {
		t.Errorf("expected 0 events from HTTP 429, got %d", events)
	}
}

// ===========================================================================
// chatStream — Anthropic streaming
// ===========================================================================

func TestLLM_ChatStream_Anthropic(t *testing.T) {
	srv := fakeAnthropicStreamServer(t)
	defer srv.Close()

	p := &Plugin{}
	cfg := Config{
		Providers: map[string]ProviderConfig{
			"anthropic": {APIKey: "sk-ant-test", BaseURL: srv.URL, Enabled: true, DefaultModel: "claude-sonnet-4-6"},
		},
	}
	cfgJSON, _ := json.Marshal(cfg)
	env := &plugin.Environment{Config: cfgJSON}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init() failed: %v", err)
	}

	req := chatRequest{
		Provider: "anthropic",
		Model:    "claude-sonnet-4-6",
		Messages: []providers.Message{{Role: "user", Content: "hello"}},
	}
	reqJSON, _ := json.Marshal(req)

	ch, err := p.chatStream(streamTenantCtx(), string(reqJSON))
	if err != nil {
		t.Fatalf("chatStream() returned error: %v", err)
	}

	var events []plugin.StreamEvent
	for e := range ch {
		events = append(events, e)
	}

	if len(events) != 3 {
		t.Fatalf("expected 3 stream events, got %d", len(events))
	}
	if events[0].Content != "Hello" {
		t.Errorf("expected 'Hello', got %q", events[0].Content)
	}
	if events[1].Content != " from Anthropic" {
		t.Errorf("expected ' from Anthropic', got %q", events[1].Content)
	}
	if !events[2].Finish {
		t.Error("expected last chunk to have Finish=true")
	}
}

// ===========================================================================
// chatStream — Ollama streaming
// ===========================================================================

func TestLLM_ChatStream_Ollama(t *testing.T) {
	srv := fakeOllamaStreamServer(t)
	defer srv.Close()

	p := &Plugin{}
	cfg := Config{
		Providers: map[string]ProviderConfig{
			"ollama": {BaseURL: srv.URL, Enabled: true, DefaultModel: "llama3.2"},
		},
	}
	cfgJSON, _ := json.Marshal(cfg)
	env := &plugin.Environment{Config: cfgJSON}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init() failed: %v", err)
	}

	req := chatRequest{
		Provider: "ollama",
		Model:    "llama3.2",
		Messages: []providers.Message{{Role: "user", Content: "hello"}},
	}
	reqJSON, _ := json.Marshal(req)

	ch, err := p.chatStream(streamTenantCtx(), string(reqJSON))
	if err != nil {
		t.Fatalf("chatStream() returned error: %v", err)
	}

	var events []plugin.StreamEvent
	for e := range ch {
		events = append(events, e)
	}

	if len(events) != 3 {
		t.Fatalf("expected 3 stream events, got %d", len(events))
	}
	if events[0].Content != "Hello" {
		t.Errorf("expected 'Hello', got %q", events[0].Content)
	}
	if events[1].Content != " from Ollama" {
		t.Errorf("expected ' from Ollama', got %q", events[1].Content)
	}
	if !events[2].Finish {
		t.Error("expected last chunk to have Finish=true")
	}
}

// ===========================================================================
// chatStream — Groq (OpenAI-compatible) streaming
// ===========================================================================

func TestLLM_ChatStream_Groq(t *testing.T) {
	srv := fakeOpenAIStreamServer(t)
	defer srv.Close()

	p := setupStreamPlugin(t, srv.URL, "groq", "gsk-test")
	p.config.Providers["groq"] = ProviderConfig{
		APIKey: "sk-test", BaseURL: srv.URL, Enabled: true, DefaultModel: "llama-3.3-70b",
	}

	req := chatRequest{
		Provider: "groq",
		Model:    "llama-3.3-70b",
		Messages: []providers.Message{{Role: "user", Content: "hello"}},
	}
	reqJSON, _ := json.Marshal(req)

	ch, err := p.chatStream(streamTenantCtx(), string(reqJSON))
	if err != nil {
		t.Fatalf("chatStream() returned error: %v", err)
	}

	var events []plugin.StreamEvent
	for e := range ch {
		events = append(events, e)
	}

	if len(events) != 3 {
		t.Fatalf("expected 3 stream events, got %d", len(events))
	}
	if events[0].Content != "Hello" {
		t.Errorf("expected 'Hello', got %q", events[0].Content)
	}
}

// ===========================================================================
// chatStream — model defaults to provider DefaultModel when empty
// ===========================================================================

func TestLLM_ChatStream_DefaultModel(t *testing.T) {
	srv := fakeOpenAIStreamServer(t)
	defer srv.Close()

	p := setupStreamPlugin(t, srv.URL, "openai", "sk-test")
	p.config.Providers["openai"] = ProviderConfig{
		APIKey: "sk-test", BaseURL: srv.URL, Enabled: true, DefaultModel: "gpt-4o",
	}

	// Empty model should fall back to DefaultModel
	req := chatRequest{
		Provider: "openai",
		Model:    "",
		Messages: []providers.Message{{Role: "user", Content: "hello"}},
	}
	reqJSON, _ := json.Marshal(req)

	ch, err := p.chatStream(streamTenantCtx(), string(reqJSON))
	if err != nil {
		t.Fatalf("chatStream() with empty model returned error: %v", err)
	}

	events := 0
	for range ch {
		events++
	}
	if events == 0 {
		t.Error("expected events from streaming with default model")
	}
}

// ===========================================================================
// RegisterHostFunctions — verifies both regular and stream registrations
// ===========================================================================

func TestLLM_RegisterHostFunctions_StreamRegistration(t *testing.T) {
	p := &Plugin{}
	reg := newTestStreamRegistry()
	if err := p.RegisterHostFunctions(reg); err != nil {
		t.Fatalf("RegisterHostFunctions: %v", err)
	}

	// Check regular functions
	for _, name := range []string{"chat", "embed", "list_models"} {
		if _, ok := reg.funcs[name]; !ok {
			t.Errorf("expected function %q to be registered", name)
		}
	}

	// Check stream function
	if !reg.streamFuncs["chat_stream"] {
		t.Error("expected chat_stream to be registered as stream function")
	}
}

func TestLLM_RegisterHostFunctions_NilRegistry(t *testing.T) {
	p := &Plugin{}
	err := p.RegisterHostFunctions(nil)
	if err == nil {
		t.Fatal("expected error for nil registry")
	}
	if !strings.Contains(err.Error(), "nil function registry") {
		t.Errorf("expected 'nil function registry', got: %v", err)
	}
}

// ===========================================================================
// Health endpoint
// ===========================================================================

func TestLLM_HealthEndpoint(t *testing.T) {
	p := &Plugin{}
	p.config = Config{
		Providers: map[string]ProviderConfig{
			"openai":    {APIKey: "sk-test", Enabled: true, DefaultModel: "gpt-4o"},
			"anthropic": {APIKey: "sk-ant-test", Enabled: false, DefaultModel: "claude-sonnet-4-6"},
		},
	}

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/llm/health", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("health: want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var result map[string]interface{}
	if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
		t.Fatalf("decode health response: %v", err)
	}

	if result["status"] != "ok" {
		t.Errorf("expected status 'ok', got %v", result["status"])
	}
	if result["plugin"] != "llm" {
		t.Errorf("expected plugin 'llm', got %v", result["plugin"])
	}

	providers, ok := result["providers"].(map[string]interface{})
	if !ok {
		t.Fatal("expected providers map in health response")
	}

	openaiInfo, ok := providers["openai"].(map[string]interface{})
	if !ok {
		t.Fatal("expected openai provider info")
	}
	if openaiInfo["enabled"] != true {
		t.Error("expected openai to be enabled")
	}
	if openaiInfo["has_api_key"] != true {
		t.Error("expected openai to have api key")
	}

	anthropicInfo, ok := providers["anthropic"].(map[string]interface{})
	if !ok {
		t.Fatal("expected anthropic provider info")
	}
	if anthropicInfo["enabled"] != false {
		t.Error("expected anthropic to be disabled")
	}
}

func TestLLM_HealthEndpoint_NoProviders(t *testing.T) {
	p := &Plugin{}

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/llm/health", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("health: want 200, got %d", rec.Code)
	}

	var result map[string]interface{}
	json.NewDecoder(rec.Body).Decode(&result)
	if result["status"] != "ok" {
		t.Errorf("expected status 'ok', got %v", result["status"])
	}
}

// ===========================================================================
// Models endpoint
// ===========================================================================

func TestLLM_ModelsEndpoint_AllProviders(t *testing.T) {
	p := &Plugin{}
	p.config = Config{
		Providers: map[string]ProviderConfig{
			"openai":    {Enabled: true},
			"anthropic": {Enabled: true},
		},
	}

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/llm/models", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("models: want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var result map[string]interface{}
	json.NewDecoder(rec.Body).Decode(&result)
	providers, ok := result["providers"].(map[string]interface{})
	if !ok {
		t.Fatal("expected providers map in models response")
	}
	if len(providers) != 2 {
		t.Errorf("expected 2 providers, got %d", len(providers))
	}
}

func TestLLM_ModelsEndpoint_SpecificProvider(t *testing.T) {
	p := &Plugin{}
	p.config = Config{
		Providers: map[string]ProviderConfig{
			"openai": {Enabled: true},
		},
	}

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/llm/models?provider=openai", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("models: want 200, got %d", rec.Code)
	}

	var result map[string]interface{}
	json.NewDecoder(rec.Body).Decode(&result)
	if result["provider"] != "openai" {
		t.Errorf("expected provider 'openai', got %v", result["provider"])
	}
	models, ok := result["models"].([]interface{})
	if !ok {
		t.Fatal("expected models array")
	}
	if len(models) == 0 {
		t.Error("expected non-empty models list")
	}
}

// ===========================================================================
// Embed — missing provider
// ===========================================================================

func TestLLM_Embed_MissingProvider(t *testing.T) {
	p := &Plugin{}
	_, err := p.embed(streamTenantCtx(), `{"input":["hello"]}`)
	if err == nil {
		t.Fatal("expected error for missing provider")
	}
	if !strings.Contains(err.Error(), "provider is required") {
		t.Errorf("expected 'provider is required', got: %v", err)
	}
}

// ===========================================================================
// Embed — disabled provider
// ===========================================================================

func TestLLM_Embed_DisabledProvider(t *testing.T) {
	p := &Plugin{}
	p.config = Config{
		Providers: map[string]ProviderConfig{
			"openai": {APIKey: "sk-test", Enabled: false},
		},
	}
	_, err := p.embed(streamTenantCtx(), `{"provider":"openai","model":"text-embedding-3-small","input":["hello"]}`)
	if err == nil {
		t.Fatal("expected error for disabled provider")
	}
	if !strings.Contains(err.Error(), "not configured or disabled") {
		t.Errorf("expected 'not configured or disabled', got: %v", err)
	}
}

// ===========================================================================
// Embed — provider error captured in output
// ===========================================================================

func TestLLM_Embed_ProviderError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
	}))
	defer srv.Close()

	p := setupStreamPlugin(t, srv.URL, "openai", "sk-test")
	req := embedRequest{
		Provider: "openai",
		Model:    "text-embedding-3-small",
		Input:    []string{"hello"},
	}
	reqJSON, _ := json.Marshal(req)

	out, err := p.embed(streamTenantCtx(), string(reqJSON))
	// Embed should NOT return an error — it captures provider errors in output.Error
	if err != nil {
		t.Fatalf("embed() returned unexpected error: %v", err)
	}
	var resp providers.EmbedOutput
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if resp.Error == "" {
		t.Error("expected error field to be populated on provider failure")
	}
}

// ===========================================================================
// Embed — unknown provider (uses OpenAI-compatible path)
// ===========================================================================

func TestLLM_Embed_UnknownProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"data":  []map[string]any{{"embedding": []float64{0.1, 0.2}, "index": 0}},
			"usage": map[string]int{"prompt_tokens": 1, "total_tokens": 1},
		})
	}))
	defer srv.Close()

	p := setupCustomPlugin(t, Config{
		Providers: map[string]ProviderConfig{
			"custom": {APIKey: "sk-test", BaseURL: srv.URL, Enabled: true, DefaultModel: "text-embedding-3-small"},
		},
	})

	req := embedRequest{
		Provider: "custom",
		Model:    "text-embedding-3-small",
		Input:    []string{"hello"},
	}
	reqJSON, _ := json.Marshal(req)

	out, err := p.embed(streamTenantCtx(), string(reqJSON))
	if err != nil {
		t.Fatalf("embed() returned error: %v", err)
	}
	var resp providers.EmbedOutput
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if len(resp.Data) == 0 {
		t.Fatal("expected embedding data from custom provider")
	}
}

// ===========================================================================
// Chat — empty model defaults to provider DefaultModel
// ===========================================================================

func TestLLM_Chat_DefaultModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]string{"role": "assistant", "content": "Hello"},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
			"model": "gpt-4o",
		})
	}))
	defer srv.Close()

	p := setupStreamPlugin(t, srv.URL, "openai", "sk-test")
	p.config.Providers["openai"] = ProviderConfig{
		APIKey: "sk-test", BaseURL: srv.URL, Enabled: true, DefaultModel: "gpt-4o",
	}

	// Empty model should use DefaultModel
	req := chatRequest{
		Provider: "openai",
		Model:    "",
		Messages: []providers.Message{{Role: "user", Content: "hello"}},
	}
	reqJSON, _ := json.Marshal(req)

	out, err := p.chat(streamTenantCtx(), string(reqJSON))
	if err != nil {
		t.Fatalf("chat() with empty model returned error: %v", err)
	}
	var resp providers.ChatOutput
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if len(resp.Choices) == 0 {
		t.Fatal("expected at least one choice")
	}
}

// ===========================================================================
// Chat — with tools
// ===========================================================================

func TestLLM_Chat_WithTools(t *testing.T) {
	containedTools := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Capture request to verify tools are included
		var bodyMap map[string]interface{}
		json.NewDecoder(r.Body).Decode(&bodyMap)
		if _, ok := bodyMap["tools"]; ok {
			containedTools = true
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]string{
					"role":    "assistant",
					"content": "I'll calculate that for you.",
				},
				"finish_reason": "tool_calls",
			}},
			"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
			"model": "gpt-4o",
		})
	}))
	defer srv.Close()

	p := setupStreamPlugin(t, srv.URL, "openai", "sk-test")
	req := chatRequest{
		Provider: "openai",
		Model:    "gpt-4o",
		Messages: []providers.Message{{Role: "user", Content: "what is 2+2?"}},
		Tools: []providers.Tool{{
			Type: "function",
			Function: providers.ToolFunction{
				Name:        "calculator",
				Description: "Performs arithmetic",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"expr": map[string]interface{}{"type": "string"},
					},
				},
			},
		}},
	}
	reqJSON, _ := json.Marshal(req)

	out, err := p.chat(streamTenantCtx(), string(reqJSON))
	if err != nil {
		t.Fatalf("chat() with tools returned error: %v", err)
	}
	var resp providers.ChatOutput
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if len(resp.Choices) == 0 {
		t.Fatal("expected at least one choice")
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("expected finish_reason 'tool_calls', got %q", resp.Choices[0].FinishReason)
	}
	if !containedTools {
		t.Error("expected tools to be included in request body")
	}
}

// ===========================================================================
// Chat — with system prompt
// ===========================================================================

func TestLLM_Chat_WithSystemPrompt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]string{"role": "assistant", "content": "You are helpful."},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
			"model": "gpt-4o",
		})
	}))
	defer srv.Close()

	p := setupStreamPlugin(t, srv.URL, "openai", "sk-test")
	req := chatRequest{
		Provider: "openai",
		Model:    "gpt-4o",
		Messages: []providers.Message{{Role: "user", Content: "hello"}},
		System:   "You are a helpful assistant.",
	}
	reqJSON, _ := json.Marshal(req)

	out, err := p.chat(streamTenantCtx(), string(reqJSON))
	if err != nil {
		t.Fatalf("chat() with system prompt returned error: %v", err)
	}
	var resp providers.ChatOutput
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if len(resp.Choices) == 0 {
		t.Fatal("expected at least one choice")
	}
}

// ===========================================================================
// listModels — invalid input JSON
// ===========================================================================

func TestLLM_ListModels_InvalidJSON(t *testing.T) {
	p := &Plugin{}
	_, err := p.listModels(context.Background(), "not json")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "invalid input") {
		t.Errorf("expected 'invalid input', got: %v", err)
	}
}

// ===========================================================================
// RegisterRoutes — routes are registered and return expected content types
// ===========================================================================

func TestLLM_RegisterRoutes_ReturnsNil(t *testing.T) {
	p := &Plugin{}
	p.config = Config{
		Providers: map[string]ProviderConfig{
			"openai": {Enabled: true},
		},
	}
	mux := http.NewServeMux()
	err := p.RegisterRoutes(mux)
	if err != nil {
		t.Fatalf("RegisterRoutes returned unexpected error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/llm/health", nil)
	mux.ServeHTTP(rec, req)
	if rec.Code == 404 {
		t.Error("expected /api/llm/health to be registered")
	}
}

// ===========================================================================
// Routes interaction test — models endpoint with no providers configured
// ===========================================================================

func TestLLM_ModelsEndpoint_NoProviders(t *testing.T) {
	p := &Plugin{}
	p.config = Config{
		Providers: map[string]ProviderConfig{},
	}

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/llm/models", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("models: want 200, got %d", rec.Code)
	}

	var result map[string]interface{}
	json.NewDecoder(rec.Body).Decode(&result)
	providers, ok := result["providers"].(map[string]interface{})
	if !ok {
		t.Fatal("expected providers map")
	}
	if len(providers) != 0 {
		t.Errorf("expected 0 providers, got %d", len(providers))
	}
}
