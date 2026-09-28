package providers

import "context"
import "net/http"

// GroqChat calls the Groq API, which is OpenAI-compatible.
func GroqChat(ctx context.Context, client *http.Client, apiKey, baseURL string, input ChatInput) (ChatOutput, error) {
	if baseURL == "" {
		baseURL = "https://api.groq.com/openai/v1"
	}
	out, err := OpenAIChat(ctx, client, apiKey, baseURL, input)
	if err != nil {
		return out, err
	}

	// Recompute with Groq's own rates, keyed on the model Groq actually
	// served (out.Model, set from the response by OpenAIChat). Groq's model
	// names (llama-3.3-70b, mixtral-8x7b, ...) never match a case in
	// OpenAIChat's table, so before this every Groq call priced at OpenAI's
	// default rate -- not just unrecognised ones, all of them. cleat#2572.
	cost, known := CostFor("groq", out.Model, out.Usage)
	out.Cost = cost
	out.EstimatedCost = !known
	return out, nil
}

// GroqChatStream calls the Groq API in streaming mode via the OpenAI-compatible endpoint.
func GroqChatStream(ctx context.Context, client *http.Client, apiKey, baseURL string, input ChatInput) (<-chan StreamChunk, error) {
	if baseURL == "" {
		baseURL = "https://api.groq.com/openai/v1"
	}
	return OpenAIChatStream(ctx, client, apiKey, baseURL, input)
}
