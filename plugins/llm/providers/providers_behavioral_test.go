package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Context cancellation
// ---------------------------------------------------------------------------

func TestOpenAIChatContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	input := ChatInput{Model: "gpt-4o", Messages: []Message{{Role: "user", Content: "hello"}}}
	_, err := OpenAIChat(ctx, srv.Client(), "sk-test", srv.URL, input)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

func TestAnthropicChatContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	input := ChatInput{Model: "claude-sonnet-4-6", Messages: []Message{{Role: "user", Content: "hello"}}}
	_, err := AnthropicChat(ctx, srv.Client(), "sk-ant-test", srv.URL, input)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

func TestGeminiChatContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	input := ChatInput{Model: "gemini-2.5-flash", Messages: []Message{{Role: "user", Content: "hello"}}}
	_, err := GeminiChat(ctx, srv.Client(), "test-key", srv.URL, input)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

func TestMistralChatContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	input := ChatInput{Model: "mistral-large-latest", Messages: []Message{{Role: "user", Content: "hello"}}}
	_, err := MistralChat(ctx, srv.Client(), "mistral-key", srv.URL, input)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

func TestOllamaChatContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	input := ChatInput{Model: "llama3.2", Messages: []Message{{Role: "user", Content: "hello"}}}
	_, err := OllamaChat(ctx, srv.Client(), srv.URL, input)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

// ---------------------------------------------------------------------------
// Streaming
// ---------------------------------------------------------------------------

func TestOpenAIChatStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Accept") != "text/event-stream" {
			http.Error(w, "bad accept", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\" world\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	input := ChatInput{Model: "gpt-4o", Messages: []Message{{Role: "user", Content: "hello"}}}
	ch, err := OpenAIChatStream(context.Background(), srv.Client(), "sk-test", srv.URL, input)
	if err != nil {
		t.Fatalf("OpenAIChatStream() returned error: %v", err)
	}

	var chunks []StreamChunk
	for chunk := range ch {
		chunks = append(chunks, chunk)
	}

	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(chunks))
	}
	if chunks[0].Content != "Hello" {
		t.Errorf("expected chunk[0].Content='Hello', got %q", chunks[0].Content)
	}
	if chunks[1].Content != " world" {
		t.Errorf("expected chunk[1].Content=' world', got %q", chunks[1].Content)
	}
	if !chunks[2].Done {
		t.Error("expected last chunk to have Done=true")
	}
}

func TestOpenAIChatStreamHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
	}))
	defer srv.Close()

	input := ChatInput{Model: "gpt-4o", Messages: []Message{{Role: "user", Content: "hello"}}}
	ch, err := OpenAIChatStream(context.Background(), srv.Client(), "sk-test", srv.URL, input)
	if err != nil {
		t.Fatalf("OpenAIChatStream() returned unexpected error: %v", err)
	}
	// Streaming functions do not return an error for HTTP error status codes;
	// the channel simply contains no valid SSE chunks and closes.
	chunks := 0
	for range ch {
		chunks++
	}
	if chunks != 0 {
		t.Errorf("expected 0 chunks from 429 response, got %d", chunks)
	}
}

func TestAnthropicChatStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "sk-ant-test" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"content_block\":{\"type\":\"text\",\"text\":\"Hello\"}}\n\n")
		fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\" world\"}}\n\n")
		fmt.Fprintf(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	input := ChatInput{Model: "claude-sonnet-4-6", Messages: []Message{{Role: "user", Content: "hello"}}}
	ch, err := AnthropicChatStream(context.Background(), srv.Client(), "sk-ant-test", srv.URL, input)
	if err != nil {
		t.Fatalf("AnthropicChatStream() returned error: %v", err)
	}

	var chunks []StreamChunk
	for chunk := range ch {
		chunks = append(chunks, chunk)
	}

	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(chunks))
	}
	if chunks[0].Content != "Hello" {
		t.Errorf("expected chunk[0].Content='Hello', got %q", chunks[0].Content)
	}
	if chunks[1].Content != " world" {
		t.Errorf("expected chunk[1].Content=' world', got %q", chunks[1].Content)
	}
	if !chunks[2].Done {
		t.Error("expected last chunk (message_stop) to have Done=true")
	}
}

func TestOllamaChatStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)

		fmt.Fprintf(w, "{\"message\":{\"role\":\"assistant\",\"content\":\"Hello\"},\"done\":false}\n")
		fmt.Fprintf(w, "{\"message\":{\"role\":\"assistant\",\"content\":\" world\"},\"done\":false}\n")
		fmt.Fprintf(w, "{\"message\":{\"role\":\"assistant\",\"content\":\"\"},\"done\":true}\n")
	}))
	defer srv.Close()

	input := ChatInput{Model: "llama3.2", Messages: []Message{{Role: "user", Content: "hello"}}}
	ch, err := OllamaChatStream(context.Background(), srv.Client(), srv.URL, input)
	if err != nil {
		t.Fatalf("OllamaChatStream() returned error: %v", err)
	}

	var chunks []StreamChunk
	for chunk := range ch {
		chunks = append(chunks, chunk)
	}

	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(chunks))
	}
	if chunks[0].Content != "Hello" {
		t.Errorf("expected chunk[0].Content='Hello', got %q", chunks[0].Content)
	}
	if chunks[1].Content != " world" {
		t.Errorf("expected chunk[1].Content=' world', got %q", chunks[1].Content)
	}
	if !chunks[2].Done {
		t.Error("expected last chunk to have Done=true")
	}
}

// ---------------------------------------------------------------------------
// Request header verification
// ---------------------------------------------------------------------------

func TestOpenAIChatRequestHeaders(t *testing.T) {
	var authHeader, contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		contentType = r.Header.Get("Content-Type")
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
	OpenAIChat(context.Background(), srv.Client(), "sk-test-key", srv.URL, input)

	if authHeader != "Bearer sk-test-key" {
		t.Errorf("expected Authorization 'Bearer sk-test-key', got %q", authHeader)
	}
	if contentType != "application/json" {
		t.Errorf("expected Content-Type 'application/json', got %q", contentType)
	}
}

func TestAnthropicChatRequestHeaders(t *testing.T) {
	var apiKey, version, contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiKey = r.Header.Get("x-api-key")
		version = r.Header.Get("anthropic-version")
		contentType = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{{"type": "text", "text": "ok"}},
			"usage":   map[string]int{"input_tokens": 1, "output_tokens": 1},
			"model":   "claude-sonnet-4-6",
		})
	}))
	defer srv.Close()

	input := ChatInput{Model: "claude-sonnet-4-6", Messages: []Message{{Role: "user", Content: "hi"}}}
	AnthropicChat(context.Background(), srv.Client(), "sk-ant-test-key", srv.URL, input)

	if apiKey != "sk-ant-test-key" {
		t.Errorf("expected x-api-key 'sk-ant-test-key', got %q", apiKey)
	}
	if version != "2023-06-01" {
		t.Errorf("expected anthropic-version '2023-06-01', got %q", version)
	}
	if contentType != "application/json" {
		t.Errorf("expected Content-Type 'application/json', got %q", contentType)
	}
}

func TestGeminiChatRequestHeaders(t *testing.T) {
	var apiKey, contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiKey = r.Header.Get("x-goog-api-key")
		contentType = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"candidates": []map[string]any{{
				"content": map[string]any{
					"role":  "model",
					"parts": []map[string]any{{"text": "ok"}},
				},
				"finishReason": "STOP",
			}},
			"usageMetadata": map[string]int{
				"promptTokenCount": 1, "candidatesTokenCount": 1, "totalTokenCount": 2,
			},
		})
	}))
	defer srv.Close()

	input := ChatInput{Model: "gemini-2.5-flash", Messages: []Message{{Role: "user", Content: "hi"}}}
	GeminiChat(context.Background(), srv.Client(), "test-goog-key", srv.URL, input)

	if apiKey != "test-goog-key" {
		t.Errorf("expected x-goog-api-key 'test-goog-key', got %q", apiKey)
	}
	if contentType != "application/json" {
		t.Errorf("expected Content-Type 'application/json', got %q", contentType)
	}
}

// ---------------------------------------------------------------------------
// API key / auth edge cases
// ---------------------------------------------------------------------------

func TestOpenAIChatEmptyAPIKey(t *testing.T) {
	var capturedKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedKey = r.Header.Get("Authorization")
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
	_, err := OpenAIChat(context.Background(), srv.Client(), "", srv.URL, input)
	if err != nil {
		t.Fatalf("OpenAIChat() with empty key returned error: %v", err)
	}
	// The request should still be sent with "Bearer" prefix and empty key value.
	// Note: Go's HTTP server trims trailing whitespace from header values.
	if capturedKey != "Bearer" {
		t.Errorf("expected 'Bearer', got %q", capturedKey)
	}
}

func TestGeminiChatInvalidKey(t *testing.T) {
	var capturedKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedKey = r.Header.Get("x-goog-api-key")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":{"code":403,"message":"API key not valid","status":"PERMISSION_DENIED"}}`))
	}))
	defer srv.Close()

	input := ChatInput{Model: "gemini-2.5-flash", Messages: []Message{{Role: "user", Content: "hi"}}}
	_, err := GeminiChat(context.Background(), srv.Client(), "bad-key", srv.URL, input)
	if err == nil {
		t.Fatal("expected error for invalid API key")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("expected error to mention 403, got: %v", err)
	}
	if capturedKey != "bad-key" {
		t.Errorf("expected x-goog-api-key 'bad-key', got %q", capturedKey)
	}
}

// ---------------------------------------------------------------------------
// Structured error handling
// ---------------------------------------------------------------------------

func TestGeminiChatStructuredError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code":    429,
				"message": "Quota exceeded: monthly token limit",
				"status":  "RESOURCE_EXHAUSTED",
			},
		})
	}))
	defer srv.Close()

	input := ChatInput{Model: "gemini-2.5-flash", Messages: []Message{{Role: "user", Content: "hello"}}}
	_, err := GeminiChat(context.Background(), srv.Client(), "test-key", srv.URL, input)
	if err == nil {
		t.Fatal("expected error for HTTP 429 (test adjusted)")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("expected error to mention status 429, got: %v", err)
	}
	if !strings.Contains(err.Error(), "Quota exceeded") {
		t.Errorf("expected error to include structured message, got: %v", err)
	}
	if !strings.Contains(err.Error(), "RESOURCE_EXHAUSTED") {
		t.Errorf("expected error to include status, got: %v", err)
	}
}

func TestGeminiChatStructuredErrorWithEmptyBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer srv.Close()

	input := ChatInput{Model: "gemini-2.5-flash", Messages: []Message{{Role: "user", Content: "hello"}}}
	_, err := GeminiChat(context.Background(), srv.Client(), "test-key", srv.URL, input)
	if err == nil {
		t.Fatal("expected error for HTTP 500")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("expected error to mention status 500, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Provider-specific pricing (table-driven)
// ---------------------------------------------------------------------------

func TestOpenAICostByModel(t *testing.T) {
	// Echoes back the requested model as the served one, like a real,
	// non-substituting provider -- pricing keys on the served model
	// (result.Model, cleat#2572), so a handler that always answered
	// "test-model" would price every case here as unknown regardless of
	// tt.model. The substitution case itself is covered directly against
	// CostFor in TestCostForUnknownModel.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]string{"role": "assistant", "content": "ok"},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"prompt_tokens": 1000, "completion_tokens": 1000, "total_tokens": 2000},
			"model": req.Model,
		})
	}))
	defer srv.Close()

	tests := []struct {
		name          string
		model         string
		minCost       float64
		maxCost       float64
		wantEstimated bool
	}{
		{"gpt-4-turbo", "gpt-4-turbo", 0.01, 0.05, false},
		{"gpt-4o", "gpt-4o", 0.001, 0.02, false},
		{"unknown-model", "gpt-3.5-turbo", 0.03, 0.05, true}, // priced at gpt-4-turbo's rate, the highest known
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := ChatInput{Model: tt.model, Messages: []Message{{Role: "user", Content: "test"}}}
			out, err := OpenAIChat(context.Background(), srv.Client(), "sk-test", srv.URL, input)
			if err != nil {
				t.Fatalf("OpenAIChat() returned error: %v", err)
			}
			if out.Cost < tt.minCost || out.Cost > tt.maxCost {
				t.Errorf("model %q: expected cost in [%f, %f], got %f", tt.model, tt.minCost, tt.maxCost, out.Cost)
			}
			if out.EstimatedCost != tt.wantEstimated {
				t.Errorf("model %q: EstimatedCost = %v, want %v", tt.model, out.EstimatedCost, tt.wantEstimated)
			}
		})
	}
}

func TestAnthropicCostByModel(t *testing.T) {
	// See TestOpenAICostByModel: echoes the requested model as served, since
	// pricing now keys on the served model.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{{"type": "text", "text": "ok"}},
			"usage":   map[string]int{"input_tokens": 1000, "output_tokens": 1000},
			"model":   req.Model,
		})
	}))
	defer srv.Close()

	tests := []struct {
		name          string
		model         string
		minCost       float64
		maxCost       float64
		wantEstimated bool
	}{
		{"opus-4-7", "claude-opus-4-7", 0.08, 0.10, false},
		{"haiku-4-5", "claude-haiku-4-5", 0.001, 0.01, false},
		{"sonnet-4-6", "claude-sonnet-4-6", 0.01, 0.03, false},
		{"default", "claude-unknown", 0.08, 0.10, true}, // priced at opus's rate, the highest known
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := ChatInput{Model: tt.model, Messages: []Message{{Role: "user", Content: "test"}}}
			out, err := AnthropicChat(context.Background(), srv.Client(), "sk-ant-test", srv.URL, input)
			if err != nil {
				t.Fatalf("AnthropicChat() returned error: %v", err)
			}
			if out.Cost < tt.minCost || out.Cost > tt.maxCost {
				t.Errorf("model %q: expected cost in [%f, %f], got %f", tt.model, tt.minCost, tt.maxCost, out.Cost)
			}
			if out.EstimatedCost != tt.wantEstimated {
				t.Errorf("model %q: EstimatedCost = %v, want %v", tt.model, out.EstimatedCost, tt.wantEstimated)
			}
		})
	}
}

func TestGeminiCostByModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"candidates": []map[string]any{{
				"content":      map[string]any{"role": "model", "parts": []map[string]any{{"text": "ok"}}},
				"finishReason": "STOP",
			}},
			"usageMetadata": map[string]int{
				"promptTokenCount": 1000, "candidatesTokenCount": 1000, "totalTokenCount": 2000,
			},
		})
	}))
	defer srv.Close()

	tests := []struct {
		name          string
		model         string
		minCost       float64
		maxCost       float64
		wantEstimated bool
	}{
		{"gemini-2.5-flash", "gemini-2.5-flash", 0.0001, 0.001, false},
		{"gemini-2.5-pro", "gemini-2.5-pro", 0.001, 0.01, false},
		{"gemini-2.0-flash-lite", "gemini-2.0-flash-lite", 0.00001, 0.001, false},
		{"gemini-2.0-flash", "gemini-2.0-flash", 0.0001, 0.001, false},
		{"default", "gemini-unknown", 0.001, 0.01, true}, // priced at gemini-2.5-pro's rate, the highest known
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := ChatInput{Model: tt.model, Messages: []Message{{Role: "user", Content: "test"}}}
			out, err := GeminiChat(context.Background(), srv.Client(), "test-key", srv.URL, input)
			if err != nil {
				t.Fatalf("GeminiChat() returned error: %v", err)
			}
			if out.Cost < tt.minCost || out.Cost > tt.maxCost {
				t.Errorf("model %q: expected cost in [%f, %f], got %f", tt.model, tt.minCost, tt.maxCost, out.Cost)
			}
			if out.EstimatedCost != tt.wantEstimated {
				t.Errorf("model %q: EstimatedCost = %v, want %v", tt.model, out.EstimatedCost, tt.wantEstimated)
			}
		})
	}
}

func TestMistralCostByModel(t *testing.T) {
	// See TestOpenAICostByModel: echoes the requested model as served, since
	// pricing now keys on the served model.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]string{"role": "assistant", "content": "ok"},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"prompt_tokens": 1000, "completion_tokens": 1000, "total_tokens": 2000},
			"model": req.Model,
		})
	}))
	defer srv.Close()

	tests := []struct {
		name          string
		model         string
		minCost       float64
		maxCost       float64
		wantEstimated bool
	}{
		{"mistral-large", "mistral-large-latest", 0.001, 0.01, false},
		{"mistral-medium", "mistral-medium-latest", 0.001, 0.01, false},
		{"mistral-small", "mistral-small-latest", 0.001, 0.01, false},
		{"open-mistral-nemo", "open-mistral-nemo", 0.0001, 0.001, false},
		{"default", "mistral-unknown", 0.001, 0.01, true}, // priced at mistral-large's rate, the highest known
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := ChatInput{Model: tt.model, Messages: []Message{{Role: "user", Content: "test"}}}
			out, err := MistralChat(context.Background(), srv.Client(), "mistral-key", srv.URL, input)
			if err != nil {
				t.Fatalf("MistralChat() returned error: %v", err)
			}
			if out.Cost < tt.minCost || out.Cost > tt.maxCost {
				t.Errorf("model %q: expected cost in [%f, %f], got %f", tt.model, tt.minCost, tt.maxCost, out.Cost)
			}
			if out.EstimatedCost != tt.wantEstimated {
				t.Errorf("model %q: EstimatedCost = %v, want %v", tt.model, out.EstimatedCost, tt.wantEstimated)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Provider-specific features
// ---------------------------------------------------------------------------

func TestAnthropicChatDefaultMaxTokens(t *testing.T) {
	var captured struct {
		MaxTokens int `json:"max_tokens"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{{"type": "text", "text": "ok"}},
			"usage":   map[string]int{"input_tokens": 1, "output_tokens": 1},
			"model":   "claude-sonnet-4-6",
		})
	}))
	defer srv.Close()

	input := ChatInput{Model: "claude-sonnet-4-6", Messages: []Message{{Role: "user", Content: "hello"}}}
	_, err := AnthropicChat(context.Background(), srv.Client(), "sk-ant-test", srv.URL, input)
	if err != nil {
		t.Fatalf("AnthropicChat() returned error: %v", err)
	}
	if captured.MaxTokens != 4096 {
		t.Errorf("expected default max_tokens 4096, got %d", captured.MaxTokens)
	}
}

func TestAnthropicChatSystemAndMessagesSystem(t *testing.T) {
	// Test that when both System field and system message are present, the
	// System field takes precedence.
	var captured struct {
		System string `json:"system"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&captured)
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
		System:   "System field prompt.",
		Messages: []Message{{Role: "system", Content: "Message system prompt."}, {Role: "user", Content: "hello"}},
	}
	_, err := AnthropicChat(context.Background(), srv.Client(), "sk-ant-test", srv.URL, input)
	if err != nil {
		t.Fatalf("AnthropicChat() returned error: %v", err)
	}
	// System field takes precedence over system message content.
	if captured.System != "System field prompt." {
		t.Errorf("expected System='System field prompt.', got %q", captured.System)
	}
}

func TestOllamaChatWithSystemMessage(t *testing.T) {
	var capturedRequest ollamaRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&capturedRequest)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"message":           map[string]string{"role": "assistant", "content": "ok"},
			"done":              true,
			"prompt_eval_count": 1,
			"eval_count":        1,
		})
	}))
	defer srv.Close()

	input := ChatInput{
		Model:    "llama3.2",
		Messages: []Message{{Role: "system", Content: "You are helpful."}, {Role: "user", Content: "hello"}},
	}
	_, err := OllamaChat(context.Background(), srv.Client(), srv.URL, input)
	if err != nil {
		t.Fatalf("OllamaChat() returned error: %v", err)
	}
	// Ollama should include the system message in the messages array.
	found := false
	for _, m := range capturedRequest.Messages {
		if m.Role == "system" && m.Content == "You are helpful." {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected system message in Ollama request messages, got %+v", capturedRequest.Messages)
	}
}

// ---------------------------------------------------------------------------
// Helper function unit tests
// ---------------------------------------------------------------------------

func TestWrapToolResult(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantJSON string
	}{
		{
			"valid json object",
			`{"result": 42}`,
			`{"result":42}`,
		},
		{
			"plain string",
			"plain result",
			`{"result":"plain result"}`,
		},
		{
			"json array",
			`[1, 2, 3]`,
			`{"result":[1,2,3]}`,
		},
		{
			"json number",
			`42`,
			`{"result":42}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := wrapToolResult(tt.input)
			if string(got) != tt.wantJSON {
				t.Errorf("wrapToolResult(%q):\nexpected: %s\ngot:      %s", tt.input, tt.wantJSON, string(got))
			}
		})
	}
}

// TestCostForKnownModels pins CostFor's exact output for a known model on
// every priced provider -- cleat#2572. Any per-provider `default:` switch
// this once was is gone; there is exactly one lookup now, and this is its
// contract.
func TestCostForKnownModels(t *testing.T) {
	usage := Usage{PromptTokens: 1_000_000, CompletionTokens: 1_000_000}

	tests := []struct {
		provider string
		model    string
		want     float64
	}{
		{"openai", "gpt-4o", 2.50 + 10.0},
		{"openai", "gpt-4o-mini", 0.15 + 0.60},
		{"openai", "gpt-4-turbo", 10.0 + 30.0},
		{"anthropic", "claude-opus-4-7", 15.0 + 75.0},
		{"anthropic", "claude-sonnet-4-6", 3.0 + 15.0},
		{"anthropic", "claude-haiku-4-5", 0.80 + 4.0},
		{"groq", "llama-3.3-70b", 0.59 + 0.79},
		{"groq", "mixtral-8x7b", 0.24 + 0.24},
		{"gemini", "gemini-2.5-flash", 0.15 + 0.60},
		{"gemini", "gemini-2.5-pro", 1.25 + 5.00},
		{"mistral", "mistral-large-latest", 2.0 + 6.0},
	}
	for _, tt := range tests {
		t.Run(tt.provider+"/"+tt.model, func(t *testing.T) {
			cost, known := CostFor(tt.provider, tt.model, usage)
			if !known {
				t.Fatalf("CostFor(%q, %q): expected known=true", tt.provider, tt.model)
			}
			if cost != tt.want {
				t.Errorf("CostFor(%q, %q) = %f, want %f", tt.provider, tt.model, cost, tt.want)
			}
		})
	}
}

// TestCostForUnknownModel is the acceptance test cleat#2572 asks for: a model
// absent from the table must not be silently priced as one of the provider's
// known models -- known must come back false, and the price must be the
// provider's highest rate (the safe direction for a spend ceiling), not a
// mid-range guess.
func TestCostForUnknownModel(t *testing.T) {
	usage := Usage{PromptTokens: 1_000_000, CompletionTokens: 1_000_000}

	tests := []struct {
		provider string
		want     float64 // the provider's most expensive known rate
	}{
		{"openai", 10.0 + 30.0},    // gpt-4-turbo, not gpt-4o
		{"anthropic", 15.0 + 75.0}, // claude-opus-4-7, not claude-sonnet-4-6
		{"groq", 0.59 + 0.79},      // llama-3.3-70b
		{"gemini", 1.25 + 5.00},    // gemini-2.5-pro, not gemini-2.5-flash
		{"mistral", 2.0 + 6.0},     // mistral-large-latest, not mistral-small
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			cost, known := CostFor(tt.provider, "a-model-cleat-has-never-heard-of", usage)
			if known {
				t.Fatalf("CostFor(%q, unknown model): expected known=false", tt.provider)
			}
			if cost != tt.want {
				t.Errorf("CostFor(%q, unknown model) = %f, want %f (the provider's highest known rate)",
					tt.provider, cost, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Embedding edge cases
// ---------------------------------------------------------------------------

func TestOpenAIEmbedInvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{invalid`))
	}))
	defer srv.Close()

	input := EmbedInput{Model: "text-embedding-3-small", Input: []string{"test"}}
	_, err := OpenAIEmbed(context.Background(), srv.Client(), "sk-test", srv.URL, input)
	if err == nil {
		t.Fatal("expected parse error for invalid JSON")
	}
}

// ---------------------------------------------------------------------------
// Groq (OpenAI-compatible) delegation
// ---------------------------------------------------------------------------

func TestGroqChatDelegatesToOpenAI(t *testing.T) {
	var capturedAuth, capturedURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		capturedURL = r.URL.String()
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

	input := ChatInput{Model: "llama-3.3-70b", Messages: []Message{{Role: "user", Content: "hi"}}}
	_, err := GroqChat(context.Background(), srv.Client(), "gsk-test", srv.URL, input)
	if err != nil {
		t.Fatalf("GroqChat() returned error: %v", err)
	}
	if capturedAuth != "Bearer gsk-test" {
		t.Errorf("expected Bearer auth, got %q", capturedAuth)
	}
	if !strings.HasSuffix(capturedURL, "/chat/completions") {
		t.Errorf("expected path /chat/completions, got %q", capturedURL)
	}
}

// ---------------------------------------------------------------------------
// Provider-specific: Anthropic tool message handling
// ---------------------------------------------------------------------------

func TestAnthropicChatToolResultMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{{"type": "text", "text": "Result: 4"}},
			"usage":   map[string]int{"input_tokens": 10, "output_tokens": 5},
			"model":   "claude-sonnet-4-6",
		})
	}))
	defer srv.Close()

	input := ChatInput{
		Model: "claude-sonnet-4-6",
		Messages: []Message{
			{Role: "user", Content: "what is 2+2?"},
			{Role: "assistant", Content: "", ToolCalls: []ToolCall{{
				ID: "call_123", Type: "function",
				Function: FunctionCall{Name: "calc", Arguments: `{"expr":"2+2"}`},
			}}},
			{Role: "tool", ToolCallID: "call_123", Content: "4"},
		},
	}
	_, err := AnthropicChat(context.Background(), srv.Client(), "sk-ant-test", srv.URL, input)
	if err != nil {
		t.Fatalf("AnthropicChat() returned error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Streaming: context cancellation
// ---------------------------------------------------------------------------

func TestOpenAIChatStreamContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Write data slowly so the context can cancel before it completes
		for i := 0; i < 100; i++ {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"%d\"},\"finish_reason\":null}]}\n\n", i)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())

	input := ChatInput{Model: "gpt-4o", Messages: []Message{{Role: "user", Content: "hello"}}}
	ch, err := OpenAIChatStream(ctx, srv.Client(), "sk-test", srv.URL, input)
	if err != nil {
		t.Fatalf("OpenAIChatStream() returned error: %v", err)
	}

	cancel() // cancel context mid-stream

	// Reading should eventually terminate (channel closed by goroutine
	// when scanner.Scan() returns false on the cancelled transport).
	collected := 0
	for range ch {
		collected++
	}
	// We don't check collected > 0 since cancellation may happen before
	// any data arrives; we only verify the channel closes without panic.
	t.Logf("collected %d chunks before cancellation", collected)
}

// ---------------------------------------------------------------------------
// Request body verification
// ---------------------------------------------------------------------------

func TestOpenAIChatRequestBody(t *testing.T) {
	var bodyMap map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&bodyMap)
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

	input := ChatInput{
		Model:       "gpt-4o",
		Messages:    []Message{{Role: "user", Content: "hello"}},
		Temperature: 0.7,
		MaxTokens:   500,
	}
	_, err := OpenAIChat(context.Background(), srv.Client(), "sk-test", srv.URL, input)
	if err != nil {
		t.Fatalf("OpenAIChat() returned error: %v", err)
	}
	if bodyMap["model"] != "gpt-4o" {
		t.Errorf("expected model 'gpt-4o', got %v", bodyMap["model"])
	}
	if bodyMap["temperature"] != 0.7 {
		t.Errorf("expected temperature 0.7, got %v", bodyMap["temperature"])
	}
}

func TestAnthropicChatRequestBody(t *testing.T) {
	var bodyMap map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&bodyMap)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{{"type": "text", "text": "ok"}},
			"usage":   map[string]int{"input_tokens": 1, "output_tokens": 1},
			"model":   "claude-sonnet-4-6",
		})
	}))
	defer srv.Close()

	input := ChatInput{
		Model:       "claude-sonnet-4-6",
		Messages:    []Message{{Role: "user", Content: "hello"}},
		Temperature: 0.5,
	}
	_, err := AnthropicChat(context.Background(), srv.Client(), "sk-ant-test", srv.URL, input)
	if err != nil {
		t.Fatalf("AnthropicChat() returned error: %v", err)
	}
	if bodyMap["model"] != "claude-sonnet-4-6" {
		t.Errorf("expected model 'claude-sonnet-4-6', got %v", bodyMap["model"])
	}
	if bodyMap["max_tokens"] != float64(4096) {
		t.Errorf("expected max_tokens 4096, got %v", bodyMap["max_tokens"])
	}
}

func TestGeminiChatRequestBody(t *testing.T) {
	var bodyMap map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&bodyMap)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"candidates": []map[string]any{{
				"content":      map[string]any{"role": "model", "parts": []map[string]any{{"text": "ok"}}},
				"finishReason": "STOP",
			}},
			"usageMetadata": map[string]int{
				"promptTokenCount": 1, "candidatesTokenCount": 1, "totalTokenCount": 2,
			},
		})
	}))
	defer srv.Close()

	input := ChatInput{
		Model:       "gemini-2.5-flash",
		Messages:    []Message{{Role: "user", Content: "hello"}},
		Temperature: 0.3,
		MaxTokens:   100,
	}
	_, err := GeminiChat(context.Background(), srv.Client(), "test-key", srv.URL, input)
	if err != nil {
		t.Fatalf("GeminiChat() returned error: %v", err)
	}

	// Check generationConfig is present and correct.
	genCfg, ok := bodyMap["generationConfig"].(map[string]any)
	if !ok {
		t.Fatal("expected generationConfig in request body")
	}
	if genCfg["temperature"] != 0.3 {
		t.Errorf("expected temperature 0.3, got %v", genCfg["temperature"])
	}
	if genCfg["maxOutputTokens"] != float64(100) {
		t.Errorf("expected maxOutputTokens 100, got %v", genCfg["maxOutputTokens"])
	}
}

// ---------------------------------------------------------------------------
// Anthropic streaming: different event types
// ---------------------------------------------------------------------------

func TestAnthropicChatStreamContentBlockStart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"content_block\":{\"type\":\"text\",\"text\":\"Hello from block\"}}\n\n")
		fmt.Fprintf(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	input := ChatInput{Model: "claude-sonnet-4-6", Messages: []Message{{Role: "user", Content: "hi"}}}
	ch, err := AnthropicChatStream(context.Background(), srv.Client(), "sk-ant-test", srv.URL, input)
	if err != nil {
		t.Fatalf("AnthropicChatStream() returned error: %v", err)
	}

	var chunks []StreamChunk
	for chunk := range ch {
		chunks = append(chunks, chunk)
	}
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(chunks))
	}
	if chunks[0].Content != "Hello from block" {
		t.Errorf("expected 'Hello from block', got %q", chunks[0].Content)
	}
	if !chunks[1].Done {
		t.Error("expected message_stop chunk to have Done=true")
	}
}

func TestAnthropicChatStreamContentBlockDelta(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello via delta\"}}\n\n")
		fmt.Fprintf(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	input := ChatInput{Model: "claude-sonnet-4-6", Messages: []Message{{Role: "user", Content: "hi"}}}
	ch, err := AnthropicChatStream(context.Background(), srv.Client(), "sk-ant-test", srv.URL, input)
	if err != nil {
		t.Fatalf("AnthropicChatStream() returned error: %v", err)
	}

	var chunks []StreamChunk
	for chunk := range ch {
		chunks = append(chunks, chunk)
	}
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(chunks))
	}
	if chunks[0].Content != "Hello via delta" {
		t.Errorf("expected 'Hello via delta', got %q", chunks[0].Content)
	}
}
