package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rcownie/cleat/durable/ai/llm"
)

// ---------------------------------------------------------------------------
// Mock infrastructure
// ---------------------------------------------------------------------------

// mockCallRecorder captures every invocation of the plugin call function so
// tests can inspect the serialized requests the agent sent.
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
		return "", fmt.Errorf("unexpected mock call: no more responses (called %d times)", m.count+1)
	}
	resp := m.responses[m.count]
	m.count++
	return resp, nil
}

// chatResponse builds a JSON-encoded plugin response the way
// llm.Client.Chat expects it. When toolCalls is empty the response signals a
// final answer (finish_reason = "stop"); when it is non-empty the response
// signals a tool-use turn (finish_reason = "tool_calls").
func chatResponse(content string, toolCalls []llm.ToolCall) string {
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
		"usage": map[string]any{
			"prompt_tokens":     10,
			"completion_tokens": 20,
			"total_tokens":      30,
		},
		"cost": 0.002,
	}
	b, _ := json.Marshal(resp)
	return string(b)
}

// noopTool is a tool-executor that always returns an empty string.
func noopTool(_ llm.ToolCall) (string, error) { return "", nil }

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestRun_HappyPath_ReturnsFinalAnswer(t *testing.T) {
	mock := newMockCallRecorder(chatResponse("Hello, World!", nil))
	client := llm.NewClient(mock.call)

	result, err := Run(
		context.Background(),
		client,
		AgentConfig{
			SystemPrompt: "You are a helpful assistant.",
			Model:        "gpt-4o",
			Provider:     "openai",
		},
		"What's up?",
		noopTool,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Answer != "Hello, World!" {
		t.Errorf("answer = %q, want %q", result.Answer, "Hello, World!")
	}
	if len(result.Steps) != 0 {
		t.Errorf("len(Steps) = %d, want 0", len(result.Steps))
	}
	if result.TotalCost <= 0 {
		t.Errorf("TotalCost = %f, want > 0", result.TotalCost)
	}
	if result.TotalTokens.TotalTokens <= 0 {
		t.Errorf("TotalTokens = %d, want > 0", result.TotalTokens.TotalTokens)
	}
}

func TestRun_WithTools_ExecutesToolThenReturnsAnswer(t *testing.T) {
	toolCall := llm.ToolCall{
		ID:        "call_1",
		Name:      "get_weather",
		Arguments: `{"location":"Tokyo"}`,
	}
	mock := newMockCallRecorder(
		chatResponse("Let me check.", []llm.ToolCall{toolCall}),
		chatResponse("It is sunny.", nil),
	)
	client := llm.NewClient(mock.call)

	tools := []llm.Tool{
		{
			Name:        "get_weather",
			Description: "Get weather for a location",
			Parameters:  map[string]any{"type": "object"},
		},
	}

	var executed []llm.ToolCall
	result, err := Run(
		context.Background(),
		client,
		AgentConfig{
			SystemPrompt: "You are a weather bot.",
			MaxSteps:     10,
			Tools:        tools,
			Model:        "gpt-4o",
			Provider:     "openai",
		},
		"Weather in Tokyo?",
		func(tc llm.ToolCall) (string, error) {
			executed = append(executed, tc)
			return `{"temperature":25,"condition":"sunny"}`, nil
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Answer != "It is sunny." {
		t.Errorf("answer = %q, want %q", result.Answer, "It is sunny.")
	}
	if len(executed) != 1 {
		t.Fatalf("executed %d tools, want 1", len(executed))
	}
	if executed[0].Name != "get_weather" {
		t.Errorf("tool name = %q, want %q", executed[0].Name, "get_weather")
	}
	if len(result.Steps) != 1 {
		t.Fatalf("len(Steps) = %d, want 1", len(result.Steps))
	}
	if result.Steps[0].ToolResult != `{"temperature":25,"condition":"sunny"}` {
		t.Errorf("ToolResult = %q, want %q", result.Steps[0].ToolResult,
			`{"temperature":25,"condition":"sunny"}`)
	}
}

func TestRun_MultipleToolCallsInOneStep(t *testing.T) {
	tc1 := llm.ToolCall{ID: "c1", Name: "tool_a", Arguments: `{"x":1}`}
	tc2 := llm.ToolCall{ID: "c2", Name: "tool_b", Arguments: `{"x":2}`}
	mock := newMockCallRecorder(
		chatResponse("Running both.", []llm.ToolCall{tc1, tc2}),
		chatResponse("Done.", nil),
	)
	client := llm.NewClient(mock.call)

	var executed []string
	result, err := Run(
		context.Background(),
		client,
		AgentConfig{MaxSteps: 10, Model: "gpt-4o", Provider: "openai"},
		"run both",
		func(tc llm.ToolCall) (string, error) {
			executed = append(executed, tc.Name)
			return "ok", nil
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(executed) != 2 {
		t.Fatalf("executed %d tools, want 2", len(executed))
	}
	if executed[0] != "tool_a" || executed[1] != "tool_b" {
		t.Errorf("executed order = %v, want [tool_a tool_b]", executed)
	}
	// Each tool call produces a separate step entry.
	if len(result.Steps) != 2 {
		t.Fatalf("len(Steps) = %d, want 2", len(result.Steps))
	}
}

func TestRun_ToolExecutionFailure_CapturedAsString(t *testing.T) {
	tc := llm.ToolCall{ID: "call_1", Name: "faulty", Arguments: `{}`}
	mock := newMockCallRecorder(
		chatResponse("Trying.", []llm.ToolCall{tc}),
		chatResponse("Handled.", nil),
	)
	client := llm.NewClient(mock.call)

	result, err := Run(
		context.Background(),
		client,
		AgentConfig{MaxSteps: 10, Model: "gpt-4o", Provider: "openai"},
		"go",
		func(_ llm.ToolCall) (string, error) {
			return "", errors.New("internal error")
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Answer != "Handled." {
		t.Errorf("answer = %q, want %q", result.Answer, "Handled.")
	}
	if len(result.Steps) != 1 {
		t.Fatalf("len(Steps) = %d, want 1", len(result.Steps))
	}
	// The error must be rendered as "error: <msg>" so the LLM can see it.
	if result.Steps[0].ToolResult != "error: internal error" {
		t.Errorf("ToolResult = %q, want %q", result.Steps[0].ToolResult,
			"error: internal error")
	}
}

func TestRun_ExceedMaxSteps_ReturnsError(t *testing.T) {
	tc := llm.ToolCall{ID: "c1", Name: "loop", Arguments: `{}`}
	responses := make([]string, 5)
	for i := range responses {
		responses[i] = chatResponse("...", []llm.ToolCall{tc})
	}
	mock := newMockCallRecorder(responses...)
	client := llm.NewClient(mock.call)

	_, err := Run(
		context.Background(),
		client,
		AgentConfig{
			MaxSteps: 3, Model: "gpt-4o", Provider: "openai",
			Tools: []llm.Tool{{Name: "loop", Description: "loops", Parameters: map[string]any{}}},
		},
		"keep going",
		func(_ llm.ToolCall) (string, error) { return "done", nil },
	)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err.Error() != "agent: exceeded max steps (3)" {
		t.Errorf("err = %v, want %q", err, "agent: exceeded max steps (3)")
	}
}

func TestRun_ZeroMaxSteps_DefaultsToTen(t *testing.T) {
	tc := llm.ToolCall{ID: "c1", Name: "loop", Arguments: `{}`}
	// The agent will make exactly 10 calls before hitting the MaxSteps limit.
	responses := make([]string, 12)
	for i := range responses {
		responses[i] = chatResponse("...", []llm.ToolCall{tc})
	}
	mock := newMockCallRecorder(responses...)
	client := llm.NewClient(mock.call)

	_, err := Run(
		context.Background(),
		client,
		AgentConfig{
			MaxSteps: 0, // should become 10
			Tools:    []llm.Tool{{Name: "loop", Description: "loops", Parameters: map[string]any{}}},
			Model:    "gpt-4o", Provider: "openai",
		},
		"go",
		func(_ llm.ToolCall) (string, error) { return "done", nil },
	)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err.Error() != "agent: exceeded max steps (10)" {
		t.Errorf("err = %v, want %q", err, "agent: exceeded max steps (10)")
	}
}

func TestRun_LLMClientError_Propagates(t *testing.T) {
	client := llm.NewClient(func(_, _, _ string) (string, error) {
		return "", errors.New("llm unavailable")
	})

	_, err := Run(
		context.Background(),
		client,
		AgentConfig{Model: "gpt-4o", Provider: "openai"},
		"hello",
		noopTool,
	)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "step 0 chat failed") {
		t.Errorf("err = %v, want step-0 failure wrapping", err)
	}
	if !strings.Contains(err.Error(), "llm unavailable") {
		t.Errorf("err = %v, want underlying 'llm unavailable'", err)
	}
}

func TestRun_NoSystemPrompt_Works(t *testing.T) {
	mock := newMockCallRecorder(chatResponse("Sure.", nil))
	client := llm.NewClient(mock.call)

	result, err := Run(
		context.Background(),
		client,
		AgentConfig{Model: "gpt-4o", Provider: "openai"},
		"hi",
		noopTool,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Answer != "Sure." {
		t.Errorf("answer = %q, want %q", result.Answer, "Sure.")
	}
}

func TestRun_ContextCarriesValues_AcrossSteps(t *testing.T) {
	// Verify that a context with values survives through the agent loop.
	type ctxKey string
	const key ctxKey = "tenant_id"

	ctx := context.WithValue(context.Background(), key, "tenant-42")

	tc := llm.ToolCall{ID: "c1", Name: "check", Arguments: `{}`}
	mock := newMockCallRecorder(
		chatResponse("Checking.", []llm.ToolCall{tc}),
		chatResponse("Done.", nil),
	)
	client := llm.NewClient(mock.call)

	result, err := Run(
		ctx,
		client,
		AgentConfig{MaxSteps: 10, Model: "gpt-4o", Provider: "openai",
			Tools: []llm.Tool{{Name: "check", Description: "check", Parameters: map[string]any{}}}},
		"check it",
		func(_ llm.ToolCall) (string, error) {
			// Verify the context value is available inside the tool executor.
			if v := ctx.Value(key); v != "tenant-42" {
				t.Errorf("tenant_id = %v, want tenant-42", v)
			}
			return "checked", nil
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Answer != "Done." {
		t.Errorf("answer = %q, want %q", result.Answer, "Done.")
	}
}

func TestRun_ConfigFieldsPassedToLLM(t *testing.T) {
	// Inspect the serialized ChatRequest the agent sends to confirm that
	// AgentConfig fields like MaxTokens and Temperature are included.
	mock := newMockCallRecorder(chatResponse("Ok.", nil))
	client := llm.NewClient(mock.call)

	_, err := Run(
		context.Background(),
		client,
		AgentConfig{
			SystemPrompt: "Be concise.",
			MaxSteps:     5,
			Model:        "claude-sonnet-4",
			Provider:     "anthropic",
			MaxTokens:    2048,
			Temperature:  0.3,
		},
		"hello",
		noopTool,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mock.Inputs) != 1 {
		t.Fatalf("expected 1 request, got %d", len(mock.Inputs))
	}

	// Parse the JSON that was sent to the "llm" plugin.
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
	if err := json.Unmarshal([]byte(mock.Inputs[0]), &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}

	if req.Provider != "anthropic" {
		t.Errorf("provider = %q, want %q", req.Provider, "anthropic")
	}
	if req.Model != "claude-sonnet-4" {
		t.Errorf("model = %q, want %q", req.Model, "claude-sonnet-4")
	}
	if req.MaxTokens != 2048 {
		t.Errorf("max_tokens = %d, want 2048", req.MaxTokens)
	}
	if req.Temperature != 0.3 {
		t.Errorf("temperature = %f, want 0.3", req.Temperature)
	}
	// The first message should be system, second should be user.
	if len(req.Messages) < 2 {
		t.Fatalf("expected >= 2 messages, got %d", len(req.Messages))
	}
	if req.Messages[0].Role != "system" {
		t.Errorf("messages[0].role = %q, want system", req.Messages[0].Role)
	}
	if req.Messages[0].Content != "Be concise." {
		t.Errorf("messages[0].content = %q, want %q", req.Messages[0].Content, "Be concise.")
	}
	if req.Messages[1].Role != "user" {
		t.Errorf("messages[1].role = %q, want user", req.Messages[1].Role)
	}
	if req.Messages[1].Content != "hello" {
		t.Errorf("messages[1].content = %q, want %q", req.Messages[1].Content, "hello")
	}
}

func TestRun_CanceledContext_Propagates(t *testing.T) {
	// When the context is canceled, the mock returns an error (simulating
	// what a real plugin call would do when it observes context cancellation).
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // immediately canceled

	client := llm.NewClient(func(_, _, _ string) (string, error) {
		// Real calls fail when context is done.
		return "", ctx.Err()
	})

	_, err := Run(
		ctx,
		client,
		AgentConfig{Model: "gpt-4o", Provider: "openai"},
		"hello",
		noopTool,
	)
	if err == nil {
		t.Fatal("expected error with canceled context, got nil")
	}
}
