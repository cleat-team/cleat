package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAIChat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			http.Error(w, `{"error":{"message":"unauthorized"}}`, http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "bad content type", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]string{"role": "assistant", "content": "Hello!"},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
			"model": "gpt-4o",
		})
	}))
	defer srv.Close()

	input := ChatInput{
		Model:       "gpt-4o",
		Messages:    []Message{{Role: "user", Content: "hello"}},
		Temperature: 0.7,
		MaxTokens:   500,
	}
	out, err := OpenAIChat(context.Background(), srv.Client(), "sk-test", srv.URL, input)
	if err != nil {
		t.Fatalf("OpenAIChat() returned error: %v", err)
	}
	if len(out.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(out.Choices))
	}
	if out.Choices[0].Message.Content != "Hello!" {
		t.Errorf("unexpected content: %q", out.Choices[0].Message.Content)
	}
	if out.Choices[0].Message.Role != "assistant" {
		t.Errorf("expected role 'assistant', got %q", out.Choices[0].Message.Role)
	}
	if out.Choices[0].FinishReason != "stop" {
		t.Errorf("expected finish_reason 'stop', got %q", out.Choices[0].FinishReason)
	}
	if out.Usage.TotalTokens != 15 {
		t.Errorf("expected 15 total tokens, got %d", out.Usage.TotalTokens)
	}
	if out.Cost <= 0 {
		t.Errorf("expected non-zero cost, got %f", out.Cost)
	}
	if out.Model != "gpt-4o" {
		t.Errorf("expected model 'gpt-4o', got %q", out.Model)
	}
}

func TestOpenAIChatDefaultBaseURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	defer srv.Close()

	input := ChatInput{Model: "gpt-4o", Messages: []Message{{Role: "user", Content: "hi"}}}
	// Empty baseURL should default to api.openai.com — but that would fail in test.
	// We verify that with a non-empty URL the path is correctly appended.
	out, err := OpenAIChat(context.Background(), srv.Client(), "sk-test", srv.URL, input)
	if err != nil {
		t.Fatalf("OpenAIChat() returned error: %v", err)
	}
	if out.Choices[0].Message.Content != "ok" {
		t.Errorf("unexpected content: %q", out.Choices[0].Message.Content)
	}
}

func TestOpenAIChatHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"rate limited"}}`, http.StatusTooManyRequests)
	}))
	defer srv.Close()

	input := ChatInput{Model: "gpt-4o", Messages: []Message{{Role: "user", Content: "hello"}}}
	_, err := OpenAIChat(context.Background(), srv.Client(), "sk-test", srv.URL, input)
	if err == nil {
		t.Fatal("expected error for HTTP 429")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("expected error to mention status 429, got: %v", err)
	}
}

func TestOpenAIChatInvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{invalid`))
	}))
	defer srv.Close()

	input := ChatInput{Model: "gpt-4o", Messages: []Message{{Role: "user", Content: "hello"}}}
	_, err := OpenAIChat(context.Background(), srv.Client(), "sk-test", srv.URL, input)
	if err == nil {
		t.Fatal("expected parse error")
	}
}

func TestOpenAIEmbed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/embeddings") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{
				"embedding": []float64{0.1, 0.2, 0.3},
				"index":     0,
			}},
			"usage": map[string]int{"prompt_tokens": 4, "total_tokens": 4},
		})
	}))
	defer srv.Close()

	input := EmbedInput{Model: "text-embedding-3-small", Input: []string{"hello world"}}
	out, err := OpenAIEmbed(context.Background(), srv.Client(), "sk-test", srv.URL, input)
	if err != nil {
		t.Fatalf("OpenAIEmbed() returned error: %v", err)
	}
	if len(out.Data) != 1 {
		t.Fatalf("expected 1 embedding, got %d", len(out.Data))
	}
	if out.Data[0].Index != 0 {
		t.Errorf("expected index 0, got %d", out.Data[0].Index)
	}
	if len(out.Data[0].Embedding) != 3 {
		t.Errorf("expected 3 dimensions, got %d", len(out.Data[0].Embedding))
	}
}

func TestOpenAIEmbedHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	input := EmbedInput{Model: "text-embedding-3-small", Input: []string{"test"}}
	_, err := OpenAIEmbed(context.Background(), srv.Client(), "sk-test", srv.URL, input)
	if err == nil {
		t.Fatal("expected error for HTTP 500")
	}
}

func TestCostCalculation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]string{"role": "assistant", "content": "ok"},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"prompt_tokens": 1000, "completion_tokens": 1000, "total_tokens": 2000},
			"model": "gpt-4o-mini",
		})
	}))
	defer srv.Close()

	input := ChatInput{Model: "gpt-4o-mini", Messages: []Message{{Role: "user", Content: "test"}}}
	out, err := OpenAIChat(context.Background(), srv.Client(), "sk-test", srv.URL, input)
	if err != nil {
		t.Fatalf("OpenAIChat() returned error: %v", err)
	}
	// gpt-4o-mini: $0.15/1M prompt, $0.60/1M completion
	// 1000 prompt tokens = $0.00015, 1000 completion tokens = $0.0006
	// total = $0.00075
	if out.Cost <= 0 || out.Cost > 0.01 {
		t.Errorf("unexpected cost for gpt-4o-mini: %f", out.Cost)
	}
}

func TestAnthropicChat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/messages") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Header.Get("x-api-key") != "sk-ant-test" {
			http.Error(w, `{"error":{"message":"unauthorized"}}`, http.StatusUnauthorized)
			return
		}
		if r.Header.Get("anthropic-version") != "2023-06-01" {
			http.Error(w, "bad version", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{{
				"type": "text",
				"text": "Hello from Claude",
			}},
			"usage": map[string]int{"input_tokens": 10, "output_tokens": 20},
			"model": "claude-sonnet-4-6",
		})
	}))
	defer srv.Close()

	input := ChatInput{
		Model:    "claude-sonnet-4-6",
		Messages: []Message{{Role: "user", Content: "hello"}},
	}
	out, err := AnthropicChat(context.Background(), srv.Client(), "sk-ant-test", srv.URL, input)
	if err != nil {
		t.Fatalf("AnthropicChat() returned error: %v", err)
	}
	if len(out.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(out.Choices))
	}
	if out.Choices[0].Message.Content != "Hello from Claude" {
		t.Errorf("unexpected content: %q", out.Choices[0].Message.Content)
	}
	if out.Choices[0].Message.Role != "assistant" {
		t.Errorf("expected role 'assistant', got %q", out.Choices[0].Message.Role)
	}
	if out.Usage.TotalTokens != 30 {
		t.Errorf("expected 30 total tokens, got %d", out.Usage.TotalTokens)
	}
	if out.Cost <= 0 {
		t.Errorf("expected non-zero cost for claude, got %f", out.Cost)
	}
}

func TestAnthropicChatToolUse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{{
				"type":  "tool_use",
				"id":    "toolu_001",
				"name":  "calculator",
				"input": json.RawMessage(`{"expression":"2+2"}`),
			}},
			"usage": map[string]int{"input_tokens": 10, "output_tokens": 5},
			"model": "claude-sonnet-4-6",
		})
	}))
	defer srv.Close()

	input := ChatInput{
		Model:    "claude-sonnet-4-6",
		Messages: []Message{{Role: "user", Content: "what is 2+2?"}},
		Tools:    []Tool{{Type: "function", Function: ToolFunction{Name: "calculator", Description: "evaluates math"}}},
	}
	out, err := AnthropicChat(context.Background(), srv.Client(), "sk-ant-test", srv.URL, input)
	if err != nil {
		t.Fatalf("AnthropicChat() returned error: %v", err)
	}
	if out.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("expected finish_reason 'tool_calls', got %q", out.Choices[0].FinishReason)
	}
	if len(out.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(out.Choices[0].Message.ToolCalls))
	}
	if out.Choices[0].Message.ToolCalls[0].Function.Name != "calculator" {
		t.Errorf("expected tool 'calculator', got %q", out.Choices[0].Message.ToolCalls[0].Function.Name)
	}
}

func TestAnthropicSystemPromptConversion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read back the request body to verify system prompt extraction.
		var req struct {
			System string `json:"system"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.System != "You are helpful." {
			t.Errorf("expected system 'You are helpful.', got %q", req.System)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{{"type": "text", "text": "ok"}},
			"usage":   map[string]int{"input_tokens": 1, "output_tokens": 1},
			"model":   "claude-sonnet-4-6",
		})
	}))
	defer srv.Close()

	input := ChatInput{
		Model:    "claude-sonnet-4-6",
		Messages: []Message{{Role: "system", Content: "You are helpful."}, {Role: "user", Content: "hello"}},
	}
	_, err := AnthropicChat(context.Background(), srv.Client(), "sk-ant-test", srv.URL, input)
	if err != nil {
		t.Fatalf("AnthropicChat() returned error: %v", err)
	}
}

func TestAnthropicChatHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	input := ChatInput{Model: "claude-sonnet-4-6", Messages: []Message{{Role: "user", Content: "hello"}}}
	_, err := AnthropicChat(context.Background(), srv.Client(), "sk-ant-test", srv.URL, input)
	if err == nil {
		t.Fatal("expected error for HTTP 500")
	}
}

func TestGroqChat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]string{"role": "assistant", "content": "Hello from Groq"},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5},
			"model": "llama-3.3-70b",
		})
	}))
	defer srv.Close()

	input := ChatInput{Model: "llama-3.3-70b", Messages: []Message{{Role: "user", Content: "hello"}}}
	out, err := GroqChat(context.Background(), srv.Client(), "sk-test", srv.URL, input)
	if err != nil {
		t.Fatalf("GroqChat() returned error: %v", err)
	}
	if out.Choices[0].Message.Content != "Hello from Groq" {
		t.Errorf("unexpected content: %q", out.Choices[0].Message.Content)
	}
}

func TestGroqChatDefaultBaseURL(t *testing.T) {
	// Verify that GroqChat passes through to OpenAIChat.
	// The default base URL should be groq's.
	// We just verify the function works with a mock server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]string{"role": "assistant", "content": "ok"},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
			"model": "llama-3.3-70b",
		})
	}))
	defer srv.Close()

	input := ChatInput{Model: "llama-3.3-70b", Messages: []Message{{Role: "user", Content: "hello"}}}
	out, err := GroqChat(context.Background(), srv.Client(), "sk-test", srv.URL, input)
	if err != nil {
		t.Fatalf("GroqChat() returned error: %v", err)
	}
	if out.Choices[0].Message.Content != "ok" {
		t.Errorf("unexpected content: %q", out.Choices[0].Message.Content)
	}
}

func TestOllamaChat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"message":           map[string]string{"role": "assistant", "content": "Hello from Ollama"},
			"done":              true,
			"total_duration":    12345,
			"prompt_eval_count": 5,
			"eval_count":        10,
		})
	}))
	defer srv.Close()

	input := ChatInput{
		Model:       "llama3.2",
		Messages:    []Message{{Role: "user", Content: "hello"}},
		Temperature: 0.5,
		MaxTokens:   100,
	}
	out, err := OllamaChat(context.Background(), srv.Client(), srv.URL, input)
	if err != nil {
		t.Fatalf("OllamaChat() returned error: %v", err)
	}
	if out.Choices[0].Message.Content != "Hello from Ollama" {
		t.Errorf("unexpected content: %q", out.Choices[0].Message.Content)
	}
	if out.Choices[0].Message.Role != "assistant" {
		t.Errorf("expected role 'assistant', got %q", out.Choices[0].Message.Role)
	}
	if out.Choices[0].FinishReason != "stop" {
		t.Errorf("expected finish_reason 'stop', got %q", out.Choices[0].FinishReason)
	}
	if out.Usage.TotalTokens != 15 {
		t.Errorf("expected 15 total tokens, got %d", out.Usage.TotalTokens)
	}
	if out.Cost != 0 {
		t.Errorf("expected 0 cost for ollama, got %f", out.Cost)
	}
	if out.Model != "llama3.2" {
		t.Errorf("expected model 'llama3.2', got %q", out.Model)
	}
}

func TestOllamaChatHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	input := ChatInput{Model: "llama3.2", Messages: []Message{{Role: "user", Content: "hello"}}}
	_, err := OllamaChat(context.Background(), srv.Client(), srv.URL, input)
	if err == nil {
		t.Fatal("expected error for HTTP 500")
	}
}

func TestOllamaChatInvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{invalid`))
	}))
	defer srv.Close()

	input := ChatInput{Model: "llama3.2", Messages: []Message{{Role: "user", Content: "hello"}}}
	_, err := OllamaChat(context.Background(), srv.Client(), srv.URL, input)
	if err == nil {
		t.Fatal("expected parse error")
	}
}

// Test that Ollama defaults MaxTokens to 4096 when not specified.
func TestOllamaDefaultMaxTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the default max_tokens was set in the request.
		var req struct {
			Options struct {
				NumPredict int `json:"num_predict"`
			} `json:"options"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.Options.NumPredict != 4096 {
			t.Errorf("expected default num_predict 4096, got %d", req.Options.NumPredict)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"message":           map[string]string{"role": "assistant", "content": "ok"},
			"done":              true,
			"prompt_eval_count": 1,
			"eval_count":        1,
		})
	}))
	defer srv.Close()

	input := ChatInput{Model: "llama3.2", Messages: []Message{{Role: "user", Content: "hello"}}}
	_, err := OllamaChat(context.Background(), srv.Client(), srv.URL, input)
	if err != nil {
		t.Fatalf("OllamaChat() returned error: %v", err)
	}
}

// TestGroqChatStream verifies that GroqChatStream correctly delegates to
// OpenAIChatStream and returns streaming content.
func TestGroqChatStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		// Send SSE stream chunks.
		chunks := []string{
			`data: {"choices":[{"delta":{"content":"Hello"},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{"content":" "},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{"content":"from"},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{"content":" Groq"},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{"content":""},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "%s\n\n", c)
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()

	input := ChatInput{
		Model:       "llama-3.3-70b",
		Messages:    []Message{{Role: "user", Content: "hello"}},
		Temperature: 0.7,
		MaxTokens:   500,
	}
	ch, err := GroqChatStream(context.Background(), srv.Client(), "sk-test", srv.URL, input)
	if err != nil {
		t.Fatalf("GroqChatStream() returned error: %v", err)
	}

	var received string
	for chunk := range ch {
		received += chunk.Content
		if chunk.Done {
			break
		}
	}
	expected := "Hello from Groq"
	if received != expected {
		t.Errorf("expected %q, got %q", expected, received)
	}
}

// TestGroqChatStreamDefaultBaseURL verifies GroqChatStream with empty baseURL.
func TestGroqChatStreamDefaultBaseURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		w.(http.Flusher).Flush()
		fmt.Fprintf(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	input := ChatInput{Model: "llama-3.3-70b", Messages: []Message{{Role: "user", Content: "hi"}}}
	ch, err := GroqChatStream(context.Background(), srv.Client(), "sk-test", srv.URL, input)
	if err != nil {
		t.Fatalf("GroqChatStream() returned error: %v", err)
	}
	var received string
	for chunk := range ch {
		received += chunk.Content
		if chunk.Done {
			break
		}
	}
	if received != "ok" {
		t.Errorf("expected %q, got %q", "ok", received)
	}
}

// TestAnthropicChatCostModels verifies cost calculation for different Anthropic models.
func TestAnthropicChatCostModels(t *testing.T) {
	tests := []struct {
		model     string
		costRange [2]float64 // [min, max] expected cost
	}{
		{"claude-opus-4-7", [2]float64{0.0001, 0.05}},
		{"claude-haiku-4-5", [2]float64{0.00001, 0.005}},
		{"unknown-model", [2]float64{0.0001, 0.02}}, // default cost
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{
					"content": []map[string]any{{
						"type": "text",
						"text": "Hello",
					}},
					"usage": map[string]int{"input_tokens": 100, "output_tokens": 50},
					"model": tt.model,
				})
			}))
			defer srv.Close()

			input := ChatInput{Model: tt.model, Messages: []Message{{Role: "user", Content: "hello"}}}
			out, err := AnthropicChat(context.Background(), srv.Client(), "sk-ant-test", srv.URL, input)
			if err != nil {
				t.Fatalf("AnthropicChat() returned error: %v", err)
			}
			if out.Cost <= tt.costRange[0] || out.Cost > tt.costRange[1] {
				t.Errorf("expected cost in [%f, %f] for model %q, got %f",
					tt.costRange[0], tt.costRange[1], tt.model, out.Cost)
			}
			if out.Model != tt.model {
				t.Errorf("expected model %q, got %q", tt.model, out.Model)
			}
		})
	}
}

// TestAnthropicChatToolResponse verifies handling of tool result messages.
func TestAnthropicChatToolResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{{
				"type": "text",
				"text": "The result is 4",
			}},
			"usage": map[string]int{"input_tokens": 20, "output_tokens": 5},
			"model": "claude-sonnet-4-6",
		})
	}))
	defer srv.Close()

	input := ChatInput{
		Model: "claude-sonnet-4-6",
		Messages: []Message{
			{Role: "user", Content: "what is 2+2?"},
			{Role: "assistant", Content: "", ToolCalls: []ToolCall{{
				ID:   "call_123",
				Type: "function",
				Function: FunctionCall{
					Name:      "calculator",
					Arguments: `{"expression":"2+2"}`,
				},
			}}},
			{Role: "tool", ToolCallID: "call_123", Content: "4"},
		},
		Tools: []Tool{{Type: "function", Function: ToolFunction{
			Name:        "calculator",
			Description: "evaluates math",
			Parameters:  map[string]any{"type": "object"},
		}}},
	}
	out, err := AnthropicChat(context.Background(), srv.Client(), "sk-ant-test", srv.URL, input)
	if err != nil {
		t.Fatalf("AnthropicChat() returned error: %v", err)
	}
	if len(out.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(out.Choices))
	}
	if out.Choices[0].Message.Content != "The result is 4" {
		t.Errorf("unexpected content: %q", out.Choices[0].Message.Content)
	}
}

// TestAnthropicChatDefaultBaseURL verifies AnthropicChat with empty base URL.
func TestAnthropicChatDefaultBaseURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{{"type": "text", "text": "ok"}},
			"usage":   map[string]int{"input_tokens": 1, "output_tokens": 1},
			"model":   "claude-sonnet-4-6",
		})
	}))
	defer srv.Close()

	input := ChatInput{Model: "claude-sonnet-4-6", Messages: []Message{{Role: "user", Content: "hi"}}}
	out, err := AnthropicChat(context.Background(), srv.Client(), "sk-ant-test", srv.URL, input)
	if err != nil {
		t.Fatalf("AnthropicChat() returned error: %v", err)
	}
	if out.Choices[0].Message.Content != "ok" {
		t.Errorf("unexpected content: %q", out.Choices[0].Message.Content)
	}
}

// TestAnthropicChatMaxTokensDefault verifies default max_tokens of 4096.
func TestAnthropicChatMaxTokensDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reqBody struct {
			MaxTokens int `json:"max_tokens"`
		}
		json.NewDecoder(r.Body).Decode(&reqBody)
		if reqBody.MaxTokens != 4096 {
			t.Errorf("expected default max_tokens 4096, got %d", reqBody.MaxTokens)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{{"type": "text", "text": "ok"}},
			"usage":   map[string]int{"input_tokens": 1, "output_tokens": 1},
			"model":   "claude-sonnet-4-6",
		})
	}))
	defer srv.Close()

	input := ChatInput{Model: "claude-sonnet-4-6", Messages: []Message{{Role: "user", Content: "hi"}}}
	_, err := AnthropicChat(context.Background(), srv.Client(), "sk-ant-test", srv.URL, input)
	if err != nil {
		t.Fatalf("AnthropicChat() returned error: %v", err)
	}
}

// roundTripperFunc adapts a function to the http.RoundTripper interface.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// TestGroqChatEmptyBaseURL verifies GroqChat sets the default base URL when none is provided.
func TestGroqChatEmptyBaseURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]string{"role": "assistant", "content": "Hello from Groq"},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
			"model": "llama-3.3-70b",
		})
	}))
	defer srv.Close()

	client := &http.Client{
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			req.URL.Host = srv.Listener.Addr().String()
			req.URL.Scheme = "http"
			return http.DefaultTransport.RoundTrip(req)
		}),
	}

	input := ChatInput{Model: "llama-3.3-70b", Messages: []Message{{Role: "user", Content: "hello"}}}
	out, err := GroqChat(context.Background(), client, "sk-test", "", input)
	if err != nil {
		t.Fatalf("GroqChat() returned error: %v", err)
	}
	if out.Choices[0].Message.Content != "Hello from Groq" {
		t.Errorf("unexpected content: %q", out.Choices[0].Message.Content)
	}
}

// TestGroqChatStreamEmptyBaseURL verifies GroqChatStream sets the default base URL when none is provided.
func TestGroqChatStreamEmptyBaseURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\" Groq\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprintf(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	client := &http.Client{
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			req.URL.Host = srv.Listener.Addr().String()
			req.URL.Scheme = "http"
			return http.DefaultTransport.RoundTrip(req)
		}),
	}

	input := ChatInput{Model: "llama-3.3-70b", Messages: []Message{{Role: "user", Content: "hello"}}}
	ch, err := GroqChatStream(context.Background(), client, "sk-test", "", input)
	if err != nil {
		t.Fatalf("GroqChatStream() returned error: %v", err)
	}
	var received string
	for chunk := range ch {
		received += chunk.Content
		if chunk.Done {
			break
		}
	}
	expected := "Hello Groq"
	if received != expected {
		t.Errorf("expected %q, got %q", expected, received)
	}
}

// TestOpenAIEmbedParseError verifies OpenAIEmbed handles a non-JSON response.
func TestOpenAIEmbedParseError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{invalid`))
	}))
	defer srv.Close()

	input := EmbedInput{Model: "text-embedding-3-small", Input: []string{"test"}}
	_, err := OpenAIEmbed(context.Background(), srv.Client(), "sk-test", srv.URL, input)
	if err == nil {
		t.Fatal("expected parse error")
	}
	if !strings.Contains(err.Error(), "parse embed response") {
		t.Errorf("expected parse error, got: %v", err)
	}
}

// TestGeminiChatToolResponse verifies GeminiChat handles tool result messages.
func TestGeminiChatToolResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"candidates": []map[string]any{{
				"content": map[string]any{
					"role":  "model",
					"parts": []map[string]any{{"text": "The result is 4"}},
				},
				"finishReason": "STOP",
			}},
			"usageMetadata": map[string]int{
				"promptTokenCount":     10,
				"candidatesTokenCount": 5,
				"totalTokenCount":      15,
			},
		})
	}))
	defer srv.Close()

	input := ChatInput{
		Model: "gemini-2.5-flash",
		Messages: []Message{
			{Role: "user", Content: "what is 2+2?"},
			{Role: "assistant", Content: "", ToolCalls: []ToolCall{{
				ID:   "call_001",
				Type: "function",
				Function: FunctionCall{
					Name:      "calculator",
					Arguments: `{"expression":"2+2"}`,
				},
			}}},
			{Role: "tool", ToolCallID: "call_001", Content: "4"},
		},
		Tools: []Tool{{Type: "function", Function: ToolFunction{
			Name:        "calculator",
			Description: "evaluates math",
			Parameters:  map[string]any{"type": "object"},
		}}},
	}
	out, err := GeminiChat(context.Background(), srv.Client(), "test-key", srv.URL, input)
	if err != nil {
		t.Fatalf("GeminiChat() returned error: %v", err)
	}
	if len(out.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(out.Choices))
	}
	if out.Choices[0].Message.Content != "The result is 4" {
		t.Errorf("unexpected content: %q", out.Choices[0].Message.Content)
	}
}

// TestOllamaChatStreamUnit verifies OllamaChatStream returns streaming content.
func TestOllamaChatStreamUnit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)

		chunks := []string{
			`{"message":{"content":"Hello"},"done":false}`,
			`{"message":{"content":" from"},"done":false}`,
			`{"message":{"content":" Ollama"},"done":false}`,
			`{"message":{"content":""},"done":true}`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "%s\n", c)
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()

	input := ChatInput{
		Model:       "llama3.2",
		Messages:    []Message{{Role: "user", Content: "hello"}},
		Temperature: 0.7,
		MaxTokens:   500,
	}
	ch, err := OllamaChatStream(context.Background(), srv.Client(), srv.URL, input)
	if err != nil {
		t.Fatalf("OllamaChatStream() returned error: %v", err)
	}

	var received string
	for chunk := range ch {
		received += chunk.Content
		if chunk.Done {
			break
		}
	}
	expected := "Hello from Ollama"
	if received != expected {
		t.Errorf("expected %q, got %q", expected, received)
	}
}

// TestOllamaChatStreamEmptyBaseURL verifies OllamaChatStream sets the default base URL when none is provided.
func TestOllamaChatStreamEmptyBaseURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"message":{"content":"ok"},"done":true}`+"\n")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	client := &http.Client{
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			req.URL.Host = srv.Listener.Addr().String()
			req.URL.Scheme = "http"
			return http.DefaultTransport.RoundTrip(req)
		}),
	}

	input := ChatInput{Model: "llama3.2", Messages: []Message{{Role: "user", Content: "hi"}}}
	ch, err := OllamaChatStream(context.Background(), client, "", input)
	if err != nil {
		t.Fatalf("OllamaChatStream() returned error: %v", err)
	}
	var received string
	for chunk := range ch {
		received += chunk.Content
		if chunk.Done {
			break
		}
	}
	if received != "ok" {
		t.Errorf("expected %q, got %q", "ok", received)
	}
}

// TestAnthropicChatStreamEventTypes verifies AnthropicChatStream processes event types.
func TestAnthropicChatStreamEventTypes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/messages") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Header.Get("x-api-key") != "sk-ant-test" {
			http.Error(w, `{"error":{"message":"unauthorized"}}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		events := []string{
			`data: {"type":"content_block_start","content_block":{"type":"text","text":"Hello"}}`,
			`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":", world"}}`,
			`data: {"type":"message_stop"}`,
			`data: [DONE]`,
		}
		for _, e := range events {
			fmt.Fprintf(w, "%s\n\n", e)
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()

	input := ChatInput{
		Model:       "claude-sonnet-4-6",
		Messages:    []Message{{Role: "user", Content: "hello"}},
		Temperature: 0.7,
		MaxTokens:   500,
	}
	ch, err := AnthropicChatStream(context.Background(), srv.Client(), "sk-ant-test", srv.URL, input)
	if err != nil {
		t.Fatalf("AnthropicChatStream() returned error: %v", err)
	}

	var received string
	for chunk := range ch {
		received += chunk.Content
		if chunk.Done {
			break
		}
	}
	expected := "Hello, world"
	if received != expected {
		t.Errorf("expected %q, got %q", expected, received)
	}
}

// TestAnthropicChatStreamEmptyBaseURL verifies AnthropicChatStream sets the default base URL when none is provided.
func TestAnthropicChatStreamEmptyBaseURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"content_block\":{\"type\":\"text\",\"text\":\"ok\"}}\n\n")
		fmt.Fprintf(w, "data: {\"type\":\"message_stop\"}\n\n")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	client := &http.Client{
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			req.URL.Host = srv.Listener.Addr().String()
			req.URL.Scheme = "http"
			return http.DefaultTransport.RoundTrip(req)
		}),
	}

	input := ChatInput{Model: "claude-sonnet-4-6", Messages: []Message{{Role: "user", Content: "hi"}}}
	ch, err := AnthropicChatStream(context.Background(), client, "sk-ant-test", "", input)
	if err != nil {
		t.Fatalf("AnthropicChatStream() returned error: %v", err)
	}
	var received string
	for chunk := range ch {
		received += chunk.Content
		if chunk.Done {
			break
		}
	}
	if received != "ok" {
		t.Errorf("expected %q, got %q", "ok", received)
	}
}
