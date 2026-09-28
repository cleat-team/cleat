package providers

import (
	"context"
	"net/http"
)

// MistralChat calls the Mistral API, which is OpenAI-compatible.
// Default base URL: https://api.mistral.ai/v1
func MistralChat(ctx context.Context, client *http.Client, apiKey, baseURL string, input ChatInput) (ChatOutput, error) {
	if baseURL == "" {
		baseURL = "https://api.mistral.ai/v1"
	}

	out, err := OpenAIChat(ctx, client, apiKey, baseURL, input)
	if err != nil {
		return out, err
	}

	// Recompute with Mistral's own rates, keyed on the model Mistral actually
	// served (out.Model, set from the response by OpenAIChat) -- the request
	// went through OpenAIChat's parser, which priced it against OpenAI's
	// table and would misprice every Mistral model. cleat#2572.
	cost, known := CostFor("mistral", out.Model, out.Usage)
	out.Cost = cost
	out.EstimatedCost = !known
	return out, nil
}
