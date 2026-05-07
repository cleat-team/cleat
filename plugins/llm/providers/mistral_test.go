package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMistralChat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer mistral-key" {
			http.Error(w, `{"error":{"message":"unauthorized"}}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]string{"role": "assistant", "content": "Hello from Mistral"},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
			"model": "mistral-large-latest",
		})
	}))
	defer srv.Close()

	input := ChatInput{
		Model:       "mistral-large-latest",
		Messages:    []Message{{Role: "user", Content: "hello"}},
		Temperature: 0.7,
		MaxTokens:   500,
	}
	out, err := MistralChat(context.Background(), srv.Client(), "mistral-key", srv.URL, input)
	if err != nil {
		t.Fatalf("MistralChat() returned error: %v", err)
	}
	if len(out.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(out.Choices))
	}
	if out.Choices[0].Message.Content != "Hello from Mistral" {
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
	if out.Model != "mistral-large-latest" {
		t.Errorf("expected model 'mistral-large-latest', got %q", out.Model)
	}
}

func TestMistralChatDefaultBaseURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]string{"role": "assistant", "content": "ok"},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
			"model": "mistral-large-latest",
		})
	}))
	defer srv.Close()

	input := ChatInput{Model: "mistral-large-latest", Messages: []Message{{Role: "user", Content: "hi"}}}
	out, err := MistralChat(context.Background(), srv.Client(), "mistral-key", srv.URL, input)
	if err != nil {
		t.Fatalf("MistralChat() returned error: %v", err)
	}
	if out.Choices[0].Message.Content != "ok" {
		t.Errorf("unexpected content: %q", out.Choices[0].Message.Content)
	}
}

func TestMistralChatHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"rate limited"}}`, http.StatusTooManyRequests)
	}))
	defer srv.Close()

	input := ChatInput{Model: "mistral-large-latest", Messages: []Message{{Role: "user", Content: "hello"}}}
	_, err := MistralChat(context.Background(), srv.Client(), "mistral-key", srv.URL, input)
	if err == nil {
		t.Fatal("expected error for HTTP 429")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("expected error to mention status 429, got: %v", err)
	}
}

func TestMistralChatInvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{invalid`))
	}))
	defer srv.Close()

	input := ChatInput{Model: "mistral-large-latest", Messages: []Message{{Role: "user", Content: "hello"}}}
	_, err := MistralChat(context.Background(), srv.Client(), "mistral-key", srv.URL, input)
	if err == nil {
		t.Fatal("expected parse error")
	}
}

func TestMistralCostOverride(t *testing.T) {
	// Verify that MistralChat overrides the cost with Mistral-specific pricing
	// rather than using OpenAI pricing from the delegated OpenAIChat call.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]string{"role": "assistant", "content": "ok"},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"prompt_tokens": 1000, "completion_tokens": 1000, "total_tokens": 2000},
			"model": "mistral-large-latest",
		})
	}))
	defer srv.Close()

	input := ChatInput{Model: "mistral-large-latest", Messages: []Message{{Role: "user", Content: "test"}}}
	out, err := MistralChat(context.Background(), srv.Client(), "mistral-key", srv.URL, input)
	if err != nil {
		t.Fatalf("MistralChat() returned error: %v", err)
	}
	// mistral-large-latest: $2/1M input, $6/1M output
	// 1000 prompt tokens = $0.002, 1000 completion tokens = $0.006
	// total = $0.008
	if out.Cost != 0.008 {
		t.Errorf("expected cost 0.008 for mistral-large-latest, got %f", out.Cost)
	}
}
