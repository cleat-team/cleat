package providers

import (
	"context"
	"net/http"
	"strings"
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

	// Recalculate cost with Mistral-specific pricing since OpenAIChat uses
	// OpenAI pricing.
	out.Cost = mistralCost(input.Model, out.Usage)
	return out, nil
}

// mistralCost calculates approximate cost for a Mistral model.
func mistralCost(model string, usage Usage) float64 {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "mistral-large"):
		return float64(usage.PromptTokens)*2.0/1_000_000 +
			float64(usage.CompletionTokens)*6.0/1_000_000
	case strings.Contains(m, "mistral-medium"):
		return float64(usage.PromptTokens)*2.5/1_000_000 +
			float64(usage.CompletionTokens)*2.5/1_000_000
	case strings.Contains(m, "mistral-small"):
		return float64(usage.PromptTokens)*1.0/1_000_000 +
			float64(usage.CompletionTokens)*3.0/1_000_000
	case strings.Contains(m, "open-mistral-nemo"):
		return float64(usage.PromptTokens)*0.3/1_000_000 +
			float64(usage.CompletionTokens)*0.3/1_000_000
	default:
		return float64(usage.PromptTokens)*1.0/1_000_000 +
			float64(usage.CompletionTokens)*3.0/1_000_000
	}
}
