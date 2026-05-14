package llm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/cleat"
)

// ---------------------------------------------------------------------------
// Mock infrastructure
// ---------------------------------------------------------------------------

// mockCallRecorder captures every invocation of the plugin call function so
// tests can inspect the serialized requests the client sent.
type mockCallRecorder struct {
	responses []string
	count     int
	Inputs    []string
}

func newMockCallRecorder(responses ...string) *mockCallRecorder {
	return &mockCallRecorder{
		responses: responses,
		Inputs:    make([]string, 0, len(responses)),
	}
}

func (m *mockCallRecorder) call(_, _, inputJSON string) (string, error) {
	m.Inputs = append(m.Inputs, inputJSON)
	if m.count >= len(m.responses) {
		return "", errors.New("mock: no more responses")
	}
	resp := m.responses[m.count]
	m.count++
	return resp, nil
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// pluginChatResponseJSON builds a JSON-encoded plugin response the way
// Client.Chat expects it. When toolCalls is empty, finish_reason is "stop";
// when non-empty, finish_reason is "tool_calls".
func pluginChatResponseJSON(content string, toolCalls []ToolCall, usage Usage, cost float64) string {
	msg := map[string]any{
		"role":    "assistant",
		"content": content,
	}
	finishReason := "stop"
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
		ptcs := make([]map[string]any, len(toolCalls))
		for i, tc := range toolCalls {
			ptcs[i] = map[string]any{
				"id":   tc.ID,
				"type": "function",
				"function": map[string]any{
					"name":      tc.Name,
					"arguments": tc.Arguments,
				},
			}
		}
		msg["tool_calls"] = ptcs
	}

	resp := map[string]any{
		"choices": []any{
			map[string]any{
				"message":       msg,
				"finish_reason": finishReason,
			},
		},
		"usage": usage,
		"cost":  cost,
	}
	b, _ := json.Marshal(resp)
	return string(b)
}

// defaultUsage is a reusable token usage value for tests.
var defaultUsage = Usage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30}

// pluginErrorJSON builds a JSON-encoded error response from the plugin.
func pluginErrorJSON(msg string) string {
	b, _ := json.Marshal(map[string]any{"error": msg})
	return string(b)
}

// requireUnmarshal parses raw JSON into dst and fails the test on error.
func requireUnmarshal(t *testing.T, data string, dst interface{}) {
	t.Helper()
	if err := json.Unmarshal([]byte(data), dst); err != nil {
		t.Fatalf("unmarshal: %v\nJSON: %s", err, data)
	}
}

// ---------------------------------------------------------------------------
// Tests: Client creation
// ---------------------------------------------------------------------------

func TestNewClient(t *testing.T) {
	mock := newMockCallRecorder(pluginChatResponseJSON("Hello!", nil, defaultUsage, 0.002))
	client := NewClient(mock.call)
	if client == nil {
		t.Fatal("NewClient returned nil")
	}

	resp, err := client.Chat(context.Background(), ChatRequest{
		Provider: "openai",
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}
	if resp.Message.Content != "Hello!" {
		t.Errorf("Content = %q, want %q", resp.Message.Content, "Hello!")
	}
	if resp.Cost != 0.002 {
		t.Errorf("Cost = %f, want 0.002", resp.Cost)
	}
	if resp.Usage.TotalTokens != 30 {
		t.Errorf("TotalTokens = %d, want 30", resp.Usage.TotalTokens)
	}
}

func TestNewClient_CallFunctionSet(t *testing.T) {
	mock := newMockCallRecorder(pluginChatResponseJSON("ok", nil, defaultUsage, 0))
	client := NewClient(mock.call)
	if client == nil {
		t.Fatal("NewClient returned nil")
	}
	// The call function must be usable — invoke a method to confirm.
	_, err := client.Chat(context.Background(), ChatRequest{
		Provider: "test",
		Model:    "test",
	})
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}
	if len(mock.Inputs) != 1 {
		t.Fatalf("expected 1 call, got %d", len(mock.Inputs))
	}
}

// ---------------------------------------------------------------------------
// Tests: Chat request serialization
// ---------------------------------------------------------------------------

func TestChatRequestSerialization(t *testing.T) {
	mock := newMockCallRecorder(pluginChatResponseJSON("OK", nil, defaultUsage, 0))
	client := NewClient(mock.call)

	_, err := client.Chat(context.Background(), ChatRequest{
		Provider:    "anthropic",
		Model:       "claude-sonnet-4",
		Messages:    []Message{{Role: "user", Content: "hello"}},
		MaxTokens:   1024,
		Temperature: 0.5,
	})
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}

	var req struct {
		Provider    string  `json:"provider"`
		Model       string  `json:"model"`
		MaxTokens   int     `json:"max_tokens"`
		Temperature float64 `json:"temperature"`
		Messages    []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	requireUnmarshal(t, mock.Inputs[0], &req)

	if req.Provider != "anthropic" {
		t.Errorf("provider = %q, want %q", req.Provider, "anthropic")
	}
	if req.Model != "claude-sonnet-4" {
		t.Errorf("model = %q, want %q", req.Model, "claude-sonnet-4")
	}
	if req.MaxTokens != 1024 {
		t.Errorf("max_tokens = %d, want 1024", req.MaxTokens)
	}
	if req.Temperature != 0.5 {
		t.Errorf("temperature = %f, want 0.5", req.Temperature)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("len(messages) = %d, want 1", len(req.Messages))
	}
	if req.Messages[0].Role != "user" {
		t.Errorf("messages[0].role = %q, want %q", req.Messages[0].Role, "user")
	}
	if req.Messages[0].Content != "hello" {
		t.Errorf("messages[0].content = %q, want %q", req.Messages[0].Content, "hello")
	}
}

func TestChatRequest_MessagesWithToolCalls(t *testing.T) {
	mock := newMockCallRecorder(pluginChatResponseJSON("Done", nil, defaultUsage, 0))
	client := NewClient(mock.call)

	_, err := client.Chat(context.Background(), ChatRequest{
		Provider: "openai",
		Model:    "gpt-4o",
		Messages: []Message{
			{Role: "user", Content: "what is the weather?"},
			{
				Role:    "assistant",
				Content: "",
				ToolCalls: []ToolCall{{
					ID:        "call_abc",
					Name:      "get_weather",
					Arguments: `{"location":"Tokyo"}`,
				}},
			},
		},
	})
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}

	var req struct {
		Messages []struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls,omitempty"`
		} `json:"messages"`
	}
	requireUnmarshal(t, mock.Inputs[0], &req)

	if len(req.Messages) != 2 {
		t.Fatalf("len(messages) = %d, want 2", len(req.Messages))
	}

	// Second message should have the tool call in plugin format.
	msg := req.Messages[1]
	if msg.Role != "assistant" {
		t.Errorf("msg.role = %q, want %q", msg.Role, "assistant")
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("len(tool_calls) = %d, want 1", len(msg.ToolCalls))
	}
	tc := msg.ToolCalls[0]
	if tc.ID != "call_abc" {
		t.Errorf("tool_call.id = %q, want %q", tc.ID, "call_abc")
	}
	if tc.Type != "function" {
		t.Errorf("tool_call.type = %q, want %q", tc.Type, "function")
	}
	if tc.Function.Name != "get_weather" {
		t.Errorf("tool_call.function.name = %q, want %q", tc.Function.Name, "get_weather")
	}
	if tc.Function.Arguments != `{"location":"Tokyo"}` {
		t.Errorf("tool_call.function.arguments = %q, want %q", tc.Function.Arguments, `{"location":"Tokyo"}`)
	}
}

// ---------------------------------------------------------------------------
// Tests: Chat response deserialization
// ---------------------------------------------------------------------------

func TestChatResponseDeserialization(t *testing.T) {
	mock := newMockCallRecorder(pluginChatResponseJSON("Hello, World!", nil, defaultUsage, 0.005))
	client := NewClient(mock.call)

	resp, err := client.Chat(context.Background(), ChatRequest{
		Provider: "openai",
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}

	if resp.Message.Role != "assistant" {
		t.Errorf("Role = %q, want %q", resp.Message.Role, "assistant")
	}
	if resp.Message.Content != "Hello, World!" {
		t.Errorf("Content = %q, want %q", resp.Message.Content, "Hello, World!")
	}
	if resp.Usage.PromptTokens != 10 {
		t.Errorf("PromptTokens = %d, want 10", resp.Usage.PromptTokens)
	}
	if resp.Usage.CompletionTokens != 20 {
		t.Errorf("CompletionTokens = %d, want 20", resp.Usage.CompletionTokens)
	}
	if resp.Usage.TotalTokens != 30 {
		t.Errorf("TotalTokens = %d, want 30", resp.Usage.TotalTokens)
	}
	if resp.Cost != 0.005 {
		t.Errorf("Cost = %f, want 0.005", resp.Cost)
	}
	if len(resp.ToolCalls) != 0 {
		t.Errorf("expected 0 ToolCalls, got %d", len(resp.ToolCalls))
	}
}

// ---------------------------------------------------------------------------
// Tests: Embed request serialization
// ---------------------------------------------------------------------------

func TestEmbedRequestSerialization(t *testing.T) {
	mock := newMockCallRecorder(`{"data":[{"embedding":[0.1,0.2],"index":0}],"usage":{"prompt_tokens":4,"completion_tokens":0,"total_tokens":4},"cost":0.001}`)
	client := NewClient(mock.call)

	_, err := client.Embed(context.Background(), EmbedRequest{
		Provider: "openai",
		Model:    "text-embedding-3-small",
		Input:    []string{"hello world", "goodbye"},
	})
	if err != nil {
		t.Fatalf("Embed failed: %v", err)
	}

	var req struct {
		Provider string   `json:"provider"`
		Model    string   `json:"model"`
		Input    []string `json:"input"`
	}
	requireUnmarshal(t, mock.Inputs[0], &req)

	if req.Provider != "openai" {
		t.Errorf("provider = %q, want %q", req.Provider, "openai")
	}
	if req.Model != "text-embedding-3-small" {
		t.Errorf("model = %q, want %q", req.Model, "text-embedding-3-small")
	}
	if len(req.Input) != 2 || req.Input[0] != "hello world" || req.Input[1] != "goodbye" {
		t.Errorf("input = %v, want [hello world goodbye]", req.Input)
	}
}

// ---------------------------------------------------------------------------
// Tests: Embed response deserialization
// ---------------------------------------------------------------------------

func TestEmbedResponseDeserialization(t *testing.T) {
	pluginResp := `{
		"data": [
			{"embedding":[0.1,0.2,0.3],"index":0},
			{"embedding":[0.4,0.5,0.6],"index":1}
		],
		"usage": {"prompt_tokens":8,"completion_tokens":0,"total_tokens":8},
		"cost": 0.002
	}`
	mock := newMockCallRecorder(pluginResp)
	client := NewClient(mock.call)

	resp, err := client.Embed(context.Background(), EmbedRequest{
		Provider: "openai",
		Model:    "text-embedding-3-small",
		Input:    []string{"a", "b"},
	})
	if err != nil {
		t.Fatalf("Embed failed: %v", err)
	}

	if len(resp.Embeddings) != 2 {
		t.Fatalf("len(Embeddings) = %d, want 2", len(resp.Embeddings))
	}
	expected := [][]float64{{0.1, 0.2, 0.3}, {0.4, 0.5, 0.6}}
	for i, emb := range resp.Embeddings {
		for j, v := range emb {
			if v != expected[i][j] {
				t.Errorf("Embeddings[%d][%d] = %f, want %f", i, j, v, expected[i][j])
			}
		}
	}
	if resp.Usage.PromptTokens != 8 {
		t.Errorf("PromptTokens = %d, want 8", resp.Usage.PromptTokens)
	}
	if resp.Usage.TotalTokens != 8 {
		t.Errorf("TotalTokens = %d, want 8", resp.Usage.TotalTokens)
	}
}

// ---------------------------------------------------------------------------
// Tests: ListModels
// ---------------------------------------------------------------------------

func TestListModels_SingleProvider(t *testing.T) {
	pluginResp := `{"models":[{"name":"gpt-4o"},{"name":"gpt-4o-mini"}],"provider":"openai"}`
	mock := newMockCallRecorder(pluginResp)
	client := NewClient(mock.call)

	models, err := client.ListModels(context.Background(), "openai")
	if err != nil {
		t.Fatalf("ListModels failed: %v", err)
	}

	// Verify the request JSON.
	var req struct {
		Provider string `json:"provider"`
	}
	requireUnmarshal(t, mock.Inputs[0], &req)
	if req.Provider != "openai" {
		t.Errorf("provider = %q, want %q", req.Provider, "openai")
	}

	if len(models) != 2 {
		t.Fatalf("len(models) = %d, want 2", len(models))
	}
	if models[0].ID != "gpt-4o" || models[0].Provider != "openai" {
		t.Errorf("models[0] = %+v, want {ID:gpt-4o Provider:openai}", models[0])
	}
	if models[1].ID != "gpt-4o-mini" || models[1].Provider != "openai" {
		t.Errorf("models[1] = %+v, want {ID:gpt-4o-mini Provider:openai}", models[1])
	}
}

func TestListModels_AllProviders(t *testing.T) {
	pluginResp := `{"providers":{"openai":[{"name":"gpt-4o"}],"anthropic":[{"name":"claude-sonnet-4"}]}}`
	mock := newMockCallRecorder(pluginResp)
	client := NewClient(mock.call)

	models, err := client.ListModels(context.Background(), "")
	if err != nil {
		t.Fatalf("ListModels failed: %v", err)
	}

	if len(models) != 2 {
		t.Fatalf("len(models) = %d, want 2", len(models))
	}

	// Build a set for order-independent verification.
	got := make(map[string]string)
	for _, m := range models {
		got[m.ID] = m.Provider
	}
	if got["gpt-4o"] != "openai" {
		t.Errorf("gpt-4o provider = %q, want openai", got["gpt-4o"])
	}
	if got["claude-sonnet-4"] != "anthropic" {
		t.Errorf("claude-sonnet-4 provider = %q, want anthropic", got["claude-sonnet-4"])
	}
}

func TestListModels_EmptyResponse(t *testing.T) {
	pluginResp := `{"providers":{}}`
	mock := newMockCallRecorder(pluginResp)
	client := NewClient(mock.call)

	models, err := client.ListModels(context.Background(), "")
	if err != nil {
		t.Fatalf("ListModels failed: %v", err)
	}
	if len(models) != 0 {
		t.Errorf("len(models) = %d, want 0", len(models))
	}
}

// ---------------------------------------------------------------------------
// Tests: Tool definition conversion (simplified -> plugin format)
// ---------------------------------------------------------------------------

func TestToolDefinitionConversion(t *testing.T) {
	mock := newMockCallRecorder(pluginChatResponseJSON("ok", nil, defaultUsage, 0))
	client := NewClient(mock.call)

	_, err := client.Chat(context.Background(), ChatRequest{
		Provider: "openai",
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "search"}},
		Tools: []Tool{
			{
				Name:        "search_web",
				Description: "Search the web for information",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"query": map[string]any{"type": "string"},
					},
					"required": []string{"query"},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}

	// Parse the tools portion of the request.
	var req struct {
		Tools []struct {
			Type     string          `json:"type"`
			Function json.RawMessage `json:"function"`
		} `json:"tools,omitempty"`
	}
	requireUnmarshal(t, mock.Inputs[0], &req)

	if len(req.Tools) != 1 {
		t.Fatalf("len(tools) = %d, want 1", len(req.Tools))
	}
	tool := req.Tools[0]
	if tool.Type != "function" {
		t.Errorf("tool.type = %q, want %q", tool.Type, "function")
	}

	// Verify the nested function object.
	var fn struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Parameters  any    `json:"parameters"`
	}
	if err := json.Unmarshal(tool.Function, &fn); err != nil {
		t.Fatalf("unmarshal tool function: %v", err)
	}
	if fn.Name != "search_web" {
		t.Errorf("function.name = %q, want %q", fn.Name, "search_web")
	}
	if fn.Description != "Search the web for information" {
		t.Errorf("function.description = %q, want %q", fn.Description, "Search the web for information")
	}
}

func TestToolDefinitionConversion_MultipleTools(t *testing.T) {
	mock := newMockCallRecorder(pluginChatResponseJSON("ok", nil, defaultUsage, 0))
	client := NewClient(mock.call)

	_, err := client.Chat(context.Background(), ChatRequest{
		Provider: "openai",
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "do stuff"}},
		Tools: []Tool{
			{Name: "tool_a", Description: "First tool", Parameters: map[string]any{"type": "object"}},
			{Name: "tool_b", Description: "Second tool", Parameters: map[string]any{"type": "object"}},
		},
	})
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}

	var req struct {
		Tools []struct {
			Type     string          `json:"type"`
			Function json.RawMessage `json:"function"`
		} `json:"tools,omitempty"`
	}
	requireUnmarshal(t, mock.Inputs[0], &req)

	if len(req.Tools) != 2 {
		t.Fatalf("len(tools) = %d, want 2", len(req.Tools))
	}
	if req.Tools[0].Type != "function" || req.Tools[1].Type != "function" {
		t.Error("all tools must have type=function")
	}

	var fn0, fn1 struct{ Name string }
	json.Unmarshal(req.Tools[0].Function, &fn0)
	json.Unmarshal(req.Tools[1].Function, &fn1)
	if fn0.Name != "tool_a" || fn1.Name != "tool_b" {
		t.Errorf("tool names = %s, %s; want tool_a, tool_b", fn0.Name, fn1.Name)
	}
}

// ---------------------------------------------------------------------------
// Tests: Tool call response conversion
// ---------------------------------------------------------------------------

func TestToolCallResponseConversion(t *testing.T) {
	toolCalls := []ToolCall{
		{ID: "call_1", Name: "get_weather", Arguments: `{"location":"Tokyo"}`},
		{ID: "call_2", Name: "get_time", Arguments: `{"timezone":"Asia/Tokyo"}`},
	}
	mock := newMockCallRecorder(pluginChatResponseJSON("", toolCalls, defaultUsage, 0.003))
	client := NewClient(mock.call)

	resp, err := client.Chat(context.Background(), ChatRequest{
		Provider: "openai",
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "weather and time in Tokyo"}},
		Tools: []Tool{
			{Name: "get_weather", Description: "Get weather", Parameters: map[string]any{"type": "object"}},
			{Name: "get_time", Description: "Get time", Parameters: map[string]any{"type": "object"}},
		},
	})
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}

	if len(resp.ToolCalls) != 2 {
		t.Fatalf("len(ToolCalls) = %d, want 2", len(resp.ToolCalls))
	}

	// Verify first tool call.
	tc0 := resp.ToolCalls[0]
	if tc0.ID != "call_1" {
		t.Errorf("ToolCalls[0].ID = %q, want %q", tc0.ID, "call_1")
	}
	if tc0.Name != "get_weather" {
		t.Errorf("ToolCalls[0].Name = %q, want %q", tc0.Name, "get_weather")
	}
	if tc0.Arguments != `{"location":"Tokyo"}` {
		t.Errorf("ToolCalls[0].Arguments = %q, want %q", tc0.Arguments, `{"location":"Tokyo"}`)
	}

	// Verify second tool call.
	tc1 := resp.ToolCalls[1]
	if tc1.ID != "call_2" {
		t.Errorf("ToolCalls[1].ID = %q, want %q", tc1.ID, "call_2")
	}
	if tc1.Name != "get_time" {
		t.Errorf("ToolCalls[1].Name = %q, want %q", tc1.Name, "get_time")
	}
	if tc1.Arguments != `{"timezone":"Asia/Tokyo"}` {
		t.Errorf("ToolCalls[1].Arguments = %q, want %q", tc1.Arguments, `{"timezone":"Asia/Tokyo"}`)
	}
}

func TestToolCallResponse_SingleToolCall(t *testing.T) {
	toolCalls := []ToolCall{
		{ID: "call_x", Name: "search", Arguments: `{"q":"hello"}`},
	}
	mock := newMockCallRecorder(pluginChatResponseJSON("", toolCalls, defaultUsage, 0))
	client := NewClient(mock.call)

	resp, err := client.Chat(context.Background(), ChatRequest{
		Provider: "openai",
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}

	if len(resp.ToolCalls) != 1 {
		t.Fatalf("len(ToolCalls) = %d, want 1", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].Name != "search" {
		t.Errorf("ToolCall.Name = %q, want %q", resp.ToolCalls[0].Name, "search")
	}
}

// ---------------------------------------------------------------------------
// Tests: Error response handling
// ---------------------------------------------------------------------------

func TestChatErrorResponse(t *testing.T) {
	mock := newMockCallRecorder(pluginErrorJSON("rate limit exceeded"))
	client := NewClient(mock.call)

	_, err := client.Chat(context.Background(), ChatRequest{
		Provider: "openai",
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err.Error() != "llm: rate limit exceeded" {
		t.Errorf("err = %q, want %q", err.Error(), "llm: rate limit exceeded")
	}
}

func TestEmbedErrorResponse(t *testing.T) {
	pluginResp := `{"error":"rate limit exceeded","data":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0},"cost":0}`
	mock := newMockCallRecorder(pluginResp)
	client := NewClient(mock.call)

	_, err := client.Embed(context.Background(), EmbedRequest{
		Provider: "openai",
		Model:    "text-embedding-3-small",
		Input:    []string{"hello"},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err.Error() != "llm: rate limit exceeded" {
		t.Errorf("err = %q, want %q", err.Error(), "llm: rate limit exceeded")
	}
}

func TestChatTransportError(t *testing.T) {
	// Simulate a transport-level failure (call function itself returns an error).
	client := NewClient(func(_, _, _ string) (string, error) {
		return "", errors.New("connection refused")
	})

	_, err := client.Chat(context.Background(), ChatRequest{
		Provider: "openai",
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err.Error() != "llm: chat call failed: connection refused" {
		t.Errorf("err = %q, want %q", err.Error(), "llm: chat call failed: connection refused")
	}
}

func TestEmbedTransportError(t *testing.T) {
	client := NewClient(func(_, _, _ string) (string, error) {
		return "", errors.New("timeout")
	})

	_, err := client.Embed(context.Background(), EmbedRequest{
		Provider: "openai",
		Model:    "text-embedding-3-small",
		Input:    []string{"hello"},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err.Error() != "llm: embed call failed: timeout" {
		t.Errorf("err = %q, want %q", err.Error(), "llm: embed call failed: timeout")
	}
}

func TestListModelsUnmarshalError(t *testing.T) {
	// Return invalid JSON for the list_models endpoint.
	mock := newMockCallRecorder(`not valid json`)
	client := NewClient(mock.call)

	_, err := client.ListModels(context.Background(), "openai")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// ---------------------------------------------------------------------------
// Tests: Chat error paths (marshal, no choices, empty response)
// ---------------------------------------------------------------------------

func TestChatRequestMarshalError_ToolFunction(t *testing.T) {
	// A tool with an un-marshalable function parameter should trigger a marshal error.
	mock := newMockCallRecorder()
	client := NewClient(mock.call)

	_, err := client.Chat(context.Background(), ChatRequest{
		Provider: "openai",
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "hi"}},
		Tools: []Tool{
			{
				Name:        "bad_tool",
				Description: "This tool has a parameter that can't be marshaled",
				Parameters:  func() {}, // functions can't be marshaled to JSON
			},
		},
	})
	if err == nil {
		t.Fatal("expected marshal error for tool function, got nil")
	}
	if !strings.Contains(err.Error(), "marshal tool function") {
		t.Errorf("expected 'marshal tool function' error, got: %v", err)
	}
}

func TestChatRequestMarshalError_ToolCall(t *testing.T) {
	mock := newMockCallRecorder(pluginChatResponseJSON("done", nil, defaultUsage, 0))
	client := NewClient(mock.call)

	// A message with a tool call should trigger marshal of the function part.
	_, err := client.Chat(context.Background(), ChatRequest{
		Provider: "openai",
		Model:    "gpt-4o",
		Messages: []Message{
			{Role: "user", Content: "do something"},
			{
				Role:    "assistant",
				Content: "",
				ToolCalls: []ToolCall{
					{
						ID:        "call_1",
						Name:      "test_tool",
						Arguments: `{}`,
					},
				},
			},
		},
		Tools: []Tool{
			{Name: "test_tool", Description: "A test tool", Parameters: map[string]any{"type": "object"}},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestChatResponseNoChoices(t *testing.T) {
	// Plugin response with no choices should produce an error.
	resp := `{"choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0},"cost":0}`
	mock := newMockCallRecorder(resp)
	client := NewClient(mock.call)

	_, err := client.Chat(context.Background(), ChatRequest{
		Provider: "openai",
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error for no choices, got nil")
	}
	if !strings.Contains(err.Error(), "no choices") {
		t.Errorf("expected 'no choices' error, got: %v", err)
	}
}

func TestChatResponsePluginError(t *testing.T) {
	raw := `{"error":"internal server error","choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0},"cost":0}`
	mock := newMockCallRecorder(raw)
	client := NewClient(mock.call)

	_, err := client.Chat(context.Background(), ChatRequest{
		Provider: "openai",
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected plugin error, got nil")
	}
	if !strings.Contains(err.Error(), "internal server error") {
		t.Errorf("expected 'internal server error', got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Tests: Embed marshal error path
// ---------------------------------------------------------------------------

func TestEmbedMarshalError(t *testing.T) {
	mock := newMockCallRecorder(`{"data":[{"embedding":[0.1],"index":0}],"usage":{"prompt_tokens":1,"completion_tokens":0,"total_tokens":1},"cost":0}`)
	client := NewClient(mock.call)

	// EmbedRequest with an un-marshalable input should trigger a marshal error.
	_, err := client.Embed(context.Background(), EmbedRequest{
		Provider: "openai",
		Model:    "text-embedding-3-small",
		Input:    []string{"hello"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEmbedUnmarshalError(t *testing.T) {
	mock := newMockCallRecorder(`not valid json`)
	client := NewClient(mock.call)

	_, err := client.Embed(context.Background(), EmbedRequest{
		Provider: "openai",
		Model:    "text-embedding-3-small",
		Input:    []string{"hello"},
	})
	if err == nil {
		t.Fatal("expected unmarshal error, got nil")
	}
	if !strings.Contains(err.Error(), "unmarshal") {
		t.Errorf("expected unmarshal error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Tests: ListModels — all-providers format and transport error
// ---------------------------------------------------------------------------

func TestListModels_AllProvidersNoProvider(t *testing.T) {
	// The response uses the all-providers format (since provider is "").
	pluginResp := `{"providers":{"openai":[{"name":"gpt-4o"}]}}`
	mock := newMockCallRecorder(pluginResp)
	client := NewClient(mock.call)

	models, err := client.ListModels(context.Background(), "")
	if err != nil {
		t.Fatalf("ListModels failed: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(models))
	}
	if models[0].ID != "gpt-4o" || models[0].Provider != "openai" {
		t.Errorf("expected {gpt-4o openai}, got {%s %s}", models[0].ID, models[0].Provider)
	}
}

func TestListModels_TransportError(t *testing.T) {
	client := NewClient(func(_, _, _ string) (string, error) {
		return "", errors.New("connection error")
	})

	_, err := client.ListModels(context.Background(), "openai")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "list_models") {
		t.Errorf("expected list_models error, got: %v", err)
	}
}

func TestListModels_SingleProviderConvenience(t *testing.T) {
	// The convenience function Chat(h, req) tests creating a client from HostCalls.
	// Here we test ListModels with a single-provider response.
	pluginResp := `{"models":[{"name":"gpt-4o"}],"provider":"openai"}`
	mock := newMockCallRecorder(pluginResp)
	client := NewClient(mock.call)

	models, err := client.ListModels(context.Background(), "openai")
	if err != nil {
		t.Fatalf("ListModels failed: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(models))
	}
}

func TestListModels_SingleProviderMarshalError(t *testing.T) {
	// When the response data doesn't match either format, an error is returned.
	mock := newMockCallRecorder(`{"providers":"not_a_map"}`)
	client := NewClient(mock.call)

	_, err := client.ListModels(context.Background(), "openai")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "unmarshal") {
		t.Errorf("expected unmarshal error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Tests: Chat convenience function (H -> Client -> Chat)
// ---------------------------------------------------------------------------

func TestChatConvenienceFunction(t *testing.T) {
	h := cleat.NewHostCalls(cleat.HostCallsOptions{
		PluginCall: func(pluginName, functionName, inputJSON string) (string, error) {
			return pluginChatResponseJSON("from convenience", nil, defaultUsage, 0.001), nil
		},
	})
	resp, err := Chat(h, ChatRequest{
		Provider: "openai",
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat convenience failed: %v", err)
	}
	if resp.Message.Content != "from convenience" {
		t.Errorf("expected content 'from convenience', got %q", resp.Message.Content)
	}
}

// mockHostCalls implements cleat.HostCalls.PluginCall for testing the convenience function.
type mockHostCalls struct {
	cleat.HostCalls
	resp string
}

func (m *mockHostCalls) PluginCall(_, _, _ string) (string, error) {
	return m.resp, nil
}
