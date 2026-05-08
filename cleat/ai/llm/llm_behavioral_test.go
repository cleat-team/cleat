package llm

import (
	"errors"
	"testing"

	"github.com/rcownie/cleat/cleat"
)

// ---------------------------------------------------------------------------
// Chat convenience function (line 331)
// ---------------------------------------------------------------------------

func TestChatConvenienceFunc(t *testing.T) {
	mock := newMockCallRecorder(pluginChatResponseJSON("Hello from Chat!", nil, defaultUsage, 0.002))
	h := cleat.NewHostCalls(cleat.HostCallsOptions{
		PluginCall: mock.call,
	})

	resp, err := Chat(h, ChatRequest{
		Provider: "openai",
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}
	if resp.Message.Content != "Hello from Chat!" {
		t.Errorf("Content = %q, want %q", resp.Message.Content, "Hello from Chat!")
	}
	if resp.Cost != 0.002 {
		t.Errorf("Cost = %f, want 0.002", resp.Cost)
	}
	if resp.Usage.TotalTokens != 30 {
		t.Errorf("TotalTokens = %d, want 30", resp.Usage.TotalTokens)
	}
	if len(resp.ToolCalls) != 0 {
		t.Errorf("expected 0 ToolCalls, got %d", len(resp.ToolCalls))
	}

	// Verify the request was sent to the plugin.
	if len(mock.Inputs) != 1 {
		t.Fatalf("expected 1 plugin call, got %d", len(mock.Inputs))
	}
}

func TestChatConvenienceFunc_PluginError(t *testing.T) {
	h := cleat.NewHostCalls(cleat.HostCallsOptions{
		PluginCall: func(_, _, _ string) (string, error) {
			return "", errors.New("connection refused")
		},
	})

	_, err := Chat(h, ChatRequest{
		Provider: "openai",
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error from Chat when PluginCall fails")
	}
	if err.Error() != "llm: chat call failed: connection refused" {
		t.Errorf("err = %q, want %q", err.Error(), "llm: chat call failed: connection refused")
	}
}

func TestChatConvenienceFunc_ErrorResponse(t *testing.T) {
	h := cleat.NewHostCalls(cleat.HostCallsOptions{
		PluginCall: func(_, _, _ string) (string, error) {
			return pluginErrorJSON("rate limit exceeded"), nil
		},
	})

	_, err := Chat(h, ChatRequest{
		Provider: "openai",
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error from Chat when response contains error")
	}
	if err.Error() != "llm: rate limit exceeded" {
		t.Errorf("err = %q, want %q", err.Error(), "llm: rate limit exceeded")
	}
}

func TestChatConvenienceFunc_ToolCalls(t *testing.T) {
	toolCalls := []ToolCall{
		{ID: "call_1", Name: "get_weather", Arguments: `{"location":"Tokyo"}`},
	}
	mock := newMockCallRecorder(pluginChatResponseJSON("", toolCalls, defaultUsage, 0.003))
	h := cleat.NewHostCalls(cleat.HostCallsOptions{
		PluginCall: mock.call,
	})

	resp, err := Chat(h, ChatRequest{
		Provider: "openai",
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "weather in Tokyo"}},
		Tools: []Tool{
			{Name: "get_weather", Description: "Get weather", Parameters: map[string]any{"type": "object"}},
		},
	})
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 ToolCall, got %d", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].Name != "get_weather" {
		t.Errorf("ToolCall.Name = %q, want %q", resp.ToolCalls[0].Name, "get_weather")
	}
	if resp.ToolCalls[0].Arguments != `{"location":"Tokyo"}` {
		t.Errorf("ToolCall.Arguments = %q, want %q", resp.ToolCalls[0].Arguments, `{"location":"Tokyo"}`)
	}
}

func TestChatConvenienceFunc_NoChoices(t *testing.T) {
	// Response with no choices should error.
	respJSON := `{"choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0},"cost":0}`
	h := cleat.NewHostCalls(cleat.HostCallsOptions{
		PluginCall: func(_, _, _ string) (string, error) {
			return respJSON, nil
		},
	})

	_, err := Chat(h, ChatRequest{
		Provider: "openai",
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error for empty choices")
	}
	if err.Error() != "llm: no choices in response" {
		t.Errorf("err = %q, want %q", err.Error(), "llm: no choices in response")
	}
}

// ---------------------------------------------------------------------------
// Chat convenience function with context.Background() implicit behavior
// ---------------------------------------------------------------------------

func TestChatConvenienceFunc_UsesBackgroundContext(t *testing.T) {
	// Verify the convenience function works (it uses context.Background() internally).
	mock := newMockCallRecorder(pluginChatResponseJSON("ok", nil, defaultUsage, 0))
	h := cleat.NewHostCalls(cleat.HostCallsOptions{
		PluginCall: mock.call,
	})

	_, err := Chat(h, ChatRequest{
		Provider: "test",
		Model:    "test",
		Messages: []Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}
}
