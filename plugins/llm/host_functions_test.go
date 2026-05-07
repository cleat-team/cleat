package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/rcownie/cleat/internal/plugin"
	"github.com/rcownie/cleat/plugins/llm/providers"
)

// fakeLLMServer returns an httptest server that mimics an OpenAI-compatible API.
func fakeLLMServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		auth := r.Header.Get("Authorization")
		if auth != "Bearer sk-test" {
			http.Error(w, `{"error":{"message":"invalid api key"}}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "embeddings") {
			json.NewEncoder(w).Encode(map[string]any{
				"data":  []map[string]any{{"embedding": []float64{0.1, 0.2, 0.3}, "index": 0}},
				"usage": map[string]int{"prompt_tokens": 10, "total_tokens": 10},
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]string{"role": "assistant", "content": "Hello from mock LLM"},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
			"model": "gpt-4o",
		})
	}))
}

// fakeAnthropicServer returns an httptest server that mimics the Anthropic Messages API.
func fakeAnthropicServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "sk-ant-test" {
			http.Error(w, `{"error":{"message":"invalid api key"}}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{{
				"type": "text",
				"text": "Hello from mock Anthropic",
			}},
			"usage": map[string]int{"input_tokens": 10, "output_tokens": 5},
			"model": "claude-sonnet-4-6",
		})
	}))
}

// fakeOllamaServer returns an httptest server that mimics Ollama.
func fakeOllamaServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"message":          map[string]string{"role": "assistant", "content": "Hello from mock Ollama"},
			"done":             true,
			"prompt_eval_count": 5,
			"eval_count":        3,
		})
	}))
}

func setupPlugin(t *testing.T, serverURL, provider string) *Plugin {
	t.Helper()
	p := &Plugin{}
	cfg := Config{
		Providers: map[string]ProviderConfig{
			provider: {APIKey: "sk-test", BaseURL: serverURL, Enabled: true, DefaultModel: "test-model"},
		},
	}
	cfgJSON, _ := json.Marshal(cfg)
	env := &plugin.Environment{Config: cfgJSON}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init() failed: %v", err)
	}
	return p
}

func tenantCtx() context.Context {
	return plugin.WithCallContext(context.Background(), &plugin.CallContext{
		TenantID:   uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		WorkflowID: "test-workflow",
	})
}

func TestChatOpenAI(t *testing.T) {
	srv := fakeLLMServer(t)
	defer srv.Close()

	p := setupPlugin(t, srv.URL, "openai")
	req := chatRequest{
		Provider: "openai",
		Model:    "gpt-4o",
		Messages: []providers.Message{{Role: "user", Content: "hello"}},
	}
	reqJSON, _ := json.Marshal(req)

	out, err := p.chat(tenantCtx(), string(reqJSON))
	if err != nil {
		t.Fatalf("chat() returned error: %v", err)
	}

	var resp providers.ChatOutput
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if len(resp.Choices) == 0 {
		t.Fatal("expected at least one choice")
	}
	if resp.Choices[0].Message.Content != "Hello from mock LLM" {
		t.Errorf("unexpected content: %q", resp.Choices[0].Message.Content)
	}
	if resp.Usage.TotalTokens != 8 {
		t.Errorf("expected 8 total tokens, got %d", resp.Usage.TotalTokens)
	}
	if resp.Cost <= 0 {
		t.Error("expected non-zero cost")
	}
}

func TestChatAnthropic(t *testing.T) {
	srv := fakeAnthropicServer(t)
	defer srv.Close()

	p := setupPlugin(t, srv.URL, "anthropic")
	// Anthropic uses x-api-key, so override the key.
	p.config.Providers["anthropic"] = ProviderConfig{
		APIKey: "sk-ant-test", BaseURL: srv.URL, Enabled: true, DefaultModel: "claude-sonnet-4-6",
	}

	req := chatRequest{
		Provider: "anthropic",
		Model:    "claude-sonnet-4-6",
		Messages: []providers.Message{{Role: "user", Content: "hello"}},
	}
	reqJSON, _ := json.Marshal(req)

	out, err := p.chat(tenantCtx(), string(reqJSON))
	if err != nil {
		t.Fatalf("chat() returned error: %v", err)
	}

	var resp providers.ChatOutput
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if len(resp.Choices) == 0 {
		t.Fatal("expected at least one choice")
	}
	if resp.Choices[0].Message.Content != "Hello from mock Anthropic" {
		t.Errorf("unexpected content: %q", resp.Choices[0].Message.Content)
	}
}

func TestChatGroq(t *testing.T) {
	srv := fakeLLMServer(t)
	defer srv.Close()

	p := setupPlugin(t, srv.URL, "groq")
	req := chatRequest{
		Provider: "groq",
		Model:    "llama-3.3-70b",
		Messages: []providers.Message{{Role: "user", Content: "hello"}},
	}
	reqJSON, _ := json.Marshal(req)

	out, err := p.chat(tenantCtx(), string(reqJSON))
	if err != nil {
		t.Fatalf("chat() returned error: %v", err)
	}
	var resp providers.ChatOutput
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if resp.Choices[0].Message.Content != "Hello from mock LLM" {
		t.Errorf("unexpected content: %q", resp.Choices[0].Message.Content)
	}
}

func TestChatOllama(t *testing.T) {
	srv := fakeOllamaServer(t)
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

	out, err := p.chat(tenantCtx(), string(reqJSON))
	if err != nil {
		t.Fatalf("chat() returned error: %v", err)
	}
	var resp providers.ChatOutput
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if resp.Choices[0].Message.Content != "Hello from mock Ollama" {
		t.Errorf("unexpected content: %q", resp.Choices[0].Message.Content)
	}
	if resp.Cost != 0 {
		t.Errorf("expected 0 cost for ollama, got %f", resp.Cost)
	}
}

func TestChatMissingProvider(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{}
	p.Init(context.Background(), env)

	req := chatRequest{Provider: "", Messages: []providers.Message{{Role: "user", Content: "hello"}}}
	reqJSON, _ := json.Marshal(req)
	_, err := p.chat(tenantCtx(), string(reqJSON))
	if err == nil {
		t.Fatal("expected error for missing provider")
	}
}

func TestChatUnknownProvider(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{}
	p.Init(context.Background(), env)

	req := chatRequest{Provider: "unknown", Messages: []providers.Message{{Role: "user", Content: "hello"}}}
	reqJSON, _ := json.Marshal(req)
	_, err := p.chat(tenantCtx(), string(reqJSON))
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestChatDisabledProvider(t *testing.T) {
	p := &Plugin{}
	p.config = Config{
		Providers: map[string]ProviderConfig{
			"openai": {APIKey: "sk-test", Enabled: false},
		},
	}
	req := chatRequest{Provider: "openai", Messages: []providers.Message{{Role: "user", Content: "hello"}}}
	reqJSON, _ := json.Marshal(req)
	_, err := p.chat(tenantCtx(), string(reqJSON))
	if err == nil {
		t.Fatal("expected error for disabled provider")
	}
}

func TestChatNoTenantContext(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{}
	p.Init(context.Background(), env)

	req := chatRequest{Provider: "openai", Messages: []providers.Message{{Role: "user", Content: "hello"}}}
	reqJSON, _ := json.Marshal(req)
	_, err := p.chat(context.Background(), string(reqJSON))
	if err == nil {
		t.Fatal("expected error for missing tenant context")
	}
}

func TestChatInvalidJSON(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{}
	p.Init(context.Background(), env)

	_, err := p.chat(tenantCtx(), "not json")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestChatProviderError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"rate limited"}}`, http.StatusTooManyRequests)
	}))
	defer srv.Close()

	p := setupPlugin(t, srv.URL, "openai")
	req := chatRequest{
		Provider: "openai",
		Model:    "gpt-4o",
		Messages: []providers.Message{{Role: "user", Content: "hello"}},
	}
	reqJSON, _ := json.Marshal(req)

	out, err := p.chat(tenantCtx(), string(reqJSON))
	// The chat function should NOT return an error — it captures provider errors in output.Error.
	if err != nil {
		t.Fatalf("chat() returned unexpected error: %v", err)
	}
	var resp providers.ChatOutput
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if resp.Error == "" {
		t.Error("expected error field to be populated on provider failure")
	}
}

func TestEmbed(t *testing.T) {
	srv := fakeLLMServer(t)
	defer srv.Close()

	p := setupPlugin(t, srv.URL, "openai")
	req := embedRequest{Provider: "openai", Model: "text-embedding-3-small", Input: []string{"hello"}}
	reqJSON, _ := json.Marshal(req)

	out, err := p.embed(tenantCtx(), string(reqJSON))
	if err != nil {
		t.Fatalf("embed() returned error: %v", err)
	}
	var resp providers.EmbedOutput
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if len(resp.Data) == 0 {
		t.Fatal("expected embedding data")
	}
	if len(resp.Data[0].Embedding) != 3 {
		t.Errorf("expected 3 embedding dimensions, got %d", len(resp.Data[0].Embedding))
	}
}

func TestEmbedInvalidJSON(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{}
	p.Init(context.Background(), env)

	_, err := p.embed(tenantCtx(), "not json")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestEmbedNoTenantContext(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{}
	p.Init(context.Background(), env)

	req := embedRequest{Provider: "openai", Input: []string{"hello"}}
	reqJSON, _ := json.Marshal(req)
	_, err := p.embed(context.Background(), string(reqJSON))
	if err == nil {
		t.Fatal("expected error for missing tenant context")
	}
}

func TestListModelsAllProviders(t *testing.T) {
	p := &Plugin{}
	p.config = Config{
		Providers: map[string]ProviderConfig{
			"openai":    {Enabled: true},
			"anthropic": {Enabled: true},
		},
	}

	out, err := p.listModels(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("listModels() returned error: %v", err)
	}

	var result struct {
		Providers map[string][]struct {
			Name   string  `json:"name"`
			Cost1K float64 `json:"cost_per_1k_tokens"`
		} `json:"providers"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if len(result.Providers) != 2 {
		t.Errorf("expected 2 providers, got %d", len(result.Providers))
	}
}

func TestListModelsSpecificProvider(t *testing.T) {
	p := &Plugin{}
	p.config = Config{
		Providers: map[string]ProviderConfig{
			"openai": {Enabled: true},
		},
	}

	reqJSON := `{"provider": "openai"}`
	out, err := p.listModels(context.Background(), reqJSON)
	if err != nil {
		t.Fatalf("listModels() returned error: %v", err)
	}

	var result struct {
		Models   []struct {
			Name   string  `json:"name"`
			Cost1K float64 `json:"cost_per_1k_tokens"`
		} `json:"models"`
		Provider string `json:"provider"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if result.Provider != "openai" {
		t.Errorf("expected provider 'openai', got %q", result.Provider)
	}
	if len(result.Models) == 0 {
		t.Error("expected models")
	}
}

func TestListModelsUnknownProvider(t *testing.T) {
	p := &Plugin{}
	out, err := p.listModels(context.Background(), `{"provider": "nonexistent"}`)
	if err != nil {
		t.Fatalf("listModels() returned error: %v", err)
	}
	// Should return empty models list, not error.
	var result struct {
		Models []any `json:"models"`
	}
	json.Unmarshal([]byte(out), &result)
	if len(result.Models) != 0 {
		t.Errorf("expected empty models, got %d", len(result.Models))
	}
}

func TestOllamaCostTracking(t *testing.T) {
	srv := fakeOllamaServer(t)
	defer srv.Close()

	p := &Plugin{}
	cfg := Config{
		Providers: map[string]ProviderConfig{
			"ollama": {BaseURL: srv.URL, Enabled: true, DefaultModel: "llama3.2"},
		},
	}
	cfgJSON, _ := json.Marshal(cfg)
	p.Init(context.Background(), &plugin.Environment{Config: cfgJSON})

	req := chatRequest{Provider: "ollama", Messages: []providers.Message{{Role: "user", Content: "hello"}}}
	reqJSON, _ := json.Marshal(req)
	out, err := p.chat(tenantCtx(), string(reqJSON))
	if err != nil {
		t.Fatalf("chat() returned error: %v", err)
	}

	var resp providers.ChatOutput
	json.Unmarshal([]byte(out), &resp)
	// Ollama cost should be zero.
	if resp.Cost != 0 {
		t.Errorf("expected 0 cost for ollama, got %f", resp.Cost)
	}
	if resp.Usage.TotalTokens != 8 {
		t.Errorf("expected 8 total tokens, got %d", resp.Usage.TotalTokens)
	}
}
