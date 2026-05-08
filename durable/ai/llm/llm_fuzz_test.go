package llm

import (
	"context"
	"testing"
)

// FuzzLLMJSONResponse fuzzes the JSON deserialization paths in the LLM client.
// For each fuzz input, it feeds the same random JSON into the Chat, Embed,
// and ListModels response parsers via mock call functions. The goal is to
// verify that malformed JSON from the LLM plugin never causes a panic.
func FuzzLLMJSONResponse(f *testing.F) {
	// Seed corpus: valid responses
	f.Add(`{"choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30},"cost":0.002}`)
	f.Add(`{"data":[{"embedding":[0.1,0.2],"index":0}],"usage":{"prompt_tokens":4,"completion_tokens":0,"total_tokens":4},"cost":0.001}`)
	f.Add(`{"models":[{"name":"gpt-4o"}],"provider":"openai"}`)
	f.Add(`{"providers":{"openai":[{"name":"gpt-4o"}]}}`)

	// Seed corpus: edge cases
	f.Add(``)
	f.Add(`{`)
	f.Add(`}`)
	f.Add(`not json`)
	f.Add(`null`)
	f.Add(`{"error": "rate limited"}`)
	f.Add(`{"choices":[]}`)
	f.Add(`{"data":[]}`)
	f.Add(`{"providers":{}}`)
	f.Add(`[1,2,3]`)
	f.Add(`"string"`)
	f.Add(`42`)

	f.Fuzz(func(t *testing.T, responseJSON string) {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("panic on input %q: %v", responseJSON, r)
			}
		}()

		// Build a request that will trigger each response parsing path.
		req := ChatRequest{
			Provider: "test",
			Model:    "test",
			Messages: []Message{{Role: "user", Content: "hi"}},
		}

		// --- Fuzz Chat response deserialization ---
		chatMock := newMockCallRecorder(responseJSON)
		chatClient := NewClient(chatMock.call)
		_, _ = chatClient.Chat(context.Background(), req)

		// --- Fuzz Embed response deserialization ---
		embedMock := newMockCallRecorder(responseJSON)
		embedClient := NewClient(embedMock.call)
		_, _ = embedClient.Embed(context.Background(), EmbedRequest{
			Provider: "test",
			Model:    "test",
			Input:    []string{"hello"},
		})

		// --- Fuzz ListModels response deserialization ---
		// ListModels tries two JSON shapes: single-provider and multi-provider.
		listMock := newMockCallRecorder(responseJSON)
		listClient := NewClient(listMock.call)
		_, _ = listClient.ListModels(context.Background(), "test")

		listAllMock := newMockCallRecorder(responseJSON)
		listAllClient := NewClient(listAllMock.call)
		_, _ = listAllClient.ListModels(context.Background(), "")
	})
}
