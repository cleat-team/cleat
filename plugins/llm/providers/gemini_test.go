package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGeminiChat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !strings.HasSuffix(r.URL.Path, ":generateContent") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Header.Get("x-goog-api-key") != "test-key" {
			http.Error(w, `{"error":{"message":"unauthorized"}}`, http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "bad content type", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"candidates": []map[string]any{{
				"content": map[string]any{
					"role":  "model",
					"parts": []map[string]any{{"text": "Hello from Gemini"}},
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
		Model:       "gemini-2.5-flash",
		Messages:    []Message{{Role: "user", Content: "hello"}},
		Temperature: 0.7,
		MaxTokens:   500,
	}
	out, err := GeminiChat(context.Background(), srv.Client(), "test-key", srv.URL, input)
	if err != nil {
		t.Fatalf("GeminiChat() returned error: %v", err)
	}
	if len(out.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(out.Choices))
	}
	if out.Choices[0].Message.Content != "Hello from Gemini" {
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
	if out.Model != "gemini-2.5-flash" {
		t.Errorf("expected model 'gemini-2.5-flash', got %q", out.Model)
	}
}

func TestGeminiChatSystemPrompt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			SystemInstruction *struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"system_instruction"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.SystemInstruction == nil {
			t.Error("expected system_instruction to be set")
		} else if len(req.SystemInstruction.Parts) == 0 || req.SystemInstruction.Parts[0].Text != "You are helpful." {
			t.Errorf("unexpected system instruction: %+v", req.SystemInstruction)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"candidates": []map[string]any{{
				"content": map[string]any{
					"role":  "model",
					"parts": []map[string]any{{"text": "Understood."}},
				},
				"finishReason": "STOP",
			}},
			"usageMetadata": map[string]int{
				"promptTokenCount":     5,
				"candidatesTokenCount": 3,
				"totalTokenCount":      8,
			},
		})
	}))
	defer srv.Close()

	input := ChatInput{
		Model:    "gemini-2.5-flash",
		System:   "You are helpful.",
		Messages: []Message{{Role: "user", Content: "hello"}},
	}
	_, err := GeminiChat(context.Background(), srv.Client(), "test-key", srv.URL, input)
	if err != nil {
		t.Fatalf("GeminiChat() returned error: %v", err)
	}
}

func TestGeminiChatSystemMessageConversion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			SystemInstruction *struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"system_instruction"`
			Contents []struct {
				Role  string `json:"role"`
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"contents"`
		}
		json.NewDecoder(r.Body).Decode(&req)

		// System message should be extracted to system_instruction, not contents.
		if req.SystemInstruction == nil || req.SystemInstruction.Parts[0].Text != "Be concise." {
			t.Errorf("expected system_instruction 'Be concise.', got %+v", req.SystemInstruction)
		}
		// Contents should not include the system message.
		for _, c := range req.Contents {
			if c.Role == "system" {
				t.Error("system message should not appear in contents")
			}
		}
		if len(req.Contents) != 1 {
			t.Errorf("expected 1 content entry, got %d", len(req.Contents))
		}

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
				"promptTokenCount":     1,
				"candidatesTokenCount": 1,
				"totalTokenCount":      2,
			},
		})
	}))
	defer srv.Close()

	input := ChatInput{
		Model:    "gemini-2.5-flash",
		Messages: []Message{{Role: "system", Content: "Be concise."}, {Role: "user", Content: "hello"}},
	}
	_, err := GeminiChat(context.Background(), srv.Client(), "test-key", srv.URL, input)
	if err != nil {
		t.Fatalf("GeminiChat() returned error: %v", err)
	}
}

func TestGeminiChatToolUse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"candidates": []map[string]any{{
				"content": map[string]any{
					"role": "model",
					"parts": []map[string]any{{
						"functionCall": map[string]any{
							"name": "calculator",
							"args": map[string]any{"expression": "2+2"},
						},
					}},
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
		Model:    "gemini-2.5-flash",
		Messages: []Message{{Role: "user", Content: "what is 2+2?"}},
		Tools:    []Tool{{Type: "function", Function: ToolFunction{Name: "calculator", Description: "evaluates math"}}},
	}
	out, err := GeminiChat(context.Background(), srv.Client(), "test-key", srv.URL, input)
	if err != nil {
		t.Fatalf("GeminiChat() returned error: %v", err)
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

func TestGeminiChatHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code":    429,
				"message": "Quota exceeded",
				"status":  "RESOURCE_EXHAUSTED",
			},
		})
	}))
	defer srv.Close()

	input := ChatInput{Model: "gemini-2.5-flash", Messages: []Message{{Role: "user", Content: "hello"}}}
	_, err := GeminiChat(context.Background(), srv.Client(), "test-key", srv.URL, input)
	if err == nil {
		t.Fatal("expected error for HTTP 429")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("expected error to mention status 429, got: %v", err)
	}
}

func TestGeminiChatInvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{invalid`))
	}))
	defer srv.Close()

	input := ChatInput{Model: "gemini-2.5-flash", Messages: []Message{{Role: "user", Content: "hello"}}}
	_, err := GeminiChat(context.Background(), srv.Client(), "test-key", srv.URL, input)
	if err == nil {
		t.Fatal("expected parse error")
	}
}

func TestGeminiChatNoCandidates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"candidates":    []any{},
			"usageMetadata": map[string]int{},
		})
	}))
	defer srv.Close()

	input := ChatInput{Model: "gemini-2.5-flash", Messages: []Message{{Role: "user", Content: "hello"}}}
	_, err := GeminiChat(context.Background(), srv.Client(), "test-key", srv.URL, input)
	if err == nil {
		t.Fatal("expected error for no candidates")
	}
}

func TestGeminiChatDefaultBaseURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
				"promptTokenCount":     1,
				"candidatesTokenCount": 1,
				"totalTokenCount":      2,
			},
		})
	}))
	defer srv.Close()

	input := ChatInput{Model: "gemini-2.5-flash", Messages: []Message{{Role: "user", Content: "hi"}}}
	out, err := GeminiChat(context.Background(), srv.Client(), "test-key", srv.URL, input)
	if err != nil {
		t.Fatalf("GeminiChat() returned error: %v", err)
	}
	if out.Choices[0].Message.Content != "ok" {
		t.Errorf("unexpected content: %q", out.Choices[0].Message.Content)
	}
}

func TestGeminiCostCalculation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
				"promptTokenCount":     1000,
				"candidatesTokenCount": 1000,
				"totalTokenCount":      2000,
			},
		})
	}))
	defer srv.Close()

	input := ChatInput{Model: "gemini-2.0-flash", Messages: []Message{{Role: "user", Content: "test"}}}
	out, err := GeminiChat(context.Background(), srv.Client(), "test-key", srv.URL, input)
	if err != nil {
		t.Fatalf("GeminiChat() returned error: %v", err)
	}
	// gemini-2.0-flash: $0.10/1M input, $0.40/1M output
	// 1000 prompt tokens = $0.00010, 1000 completion tokens = $0.00040
	// total = $0.00050
	if out.Cost <= 0 || out.Cost > 0.01 {
		t.Errorf("unexpected cost for gemini-2.0-flash: %f", out.Cost)
	}
}

func TestGeminiChatFinishReasons(t *testing.T) {
	tests := []struct {
		name         string
		finishReason string
		want         string
	}{
		{"stop", "STOP", "stop"},
		{"max_tokens", "MAX_TOKENS", "length"},
		{"safety", "SAFETY", "content_filter"},
		{"recitation", "RECITATION", "content_filter"},
		{"blocklist", "BLOCKLIST", "content_filter"},
		{"other", "OTHER", "stop"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{
					"candidates": []map[string]any{{
						"content": map[string]any{
							"role":  "model",
							"parts": []map[string]any{{"text": "response"}},
						},
						"finishReason": tt.finishReason,
					}},
					"usageMetadata": map[string]int{
						"promptTokenCount":     1,
						"candidatesTokenCount": 1,
						"totalTokenCount":      2,
					},
				})
			}))
			defer srv.Close()

			input := ChatInput{Model: "gemini-2.5-flash", Messages: []Message{{Role: "user", Content: "hi"}}}
			out, err := GeminiChat(context.Background(), srv.Client(), "test-key", srv.URL, input)
			if err != nil {
				t.Fatalf("GeminiChat() returned error: %v", err)
			}
			if out.Choices[0].FinishReason != tt.want {
				t.Errorf("expected finish_reason %q, got %q", tt.want, out.Choices[0].FinishReason)
			}
		})
	}
}
