package providers

import "context"
import "net/http"

// GroqChat calls the Groq API, which is OpenAI-compatible.
func GroqChat(ctx context.Context, client *http.Client, apiKey, baseURL string, input ChatInput) (ChatOutput, error) {
	if baseURL == "" {
		baseURL = "https://api.groq.com/openai/v1"
	}
	return OpenAIChat(ctx, client, apiKey, baseURL, input)
}

// GroqChatStream calls the Groq API in streaming mode via the OpenAI-compatible endpoint.
func GroqChatStream(ctx context.Context, client *http.Client, apiKey, baseURL string, input ChatInput) (<-chan StreamChunk, error) {
	if baseURL == "" {
		baseURL = "https://api.groq.com/openai/v1"
	}
	return OpenAIChatStream(ctx, client, apiKey, baseURL, input)
}
