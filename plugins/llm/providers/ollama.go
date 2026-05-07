package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

type ollamaRequest struct {
	Model    string         `json:"model"`
	Messages []ollamaMsg    `json:"messages"`
	Stream   bool           `json:"stream"`
	Options  ollamaOptions  `json:"options,omitempty"`
}

type ollamaMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ollamaOptions struct {
	Temperature float64 `json:"temperature,omitempty"`
	NumPredict  int     `json:"num_predict,omitempty"`
}

type ollamaResponse struct {
	Message      ollamaMsg `json:"message"`
	Done         bool      `json:"done"`
	TotalDuration int64    `json:"total_duration"`
	EvalCount    int       `json:"eval_count"`
	PromptEvalCount int    `json:"prompt_eval_count"`
}

// OllamaChat calls a local Ollama instance.
func OllamaChat(ctx context.Context, client *http.Client, baseURL string, input ChatInput) (ChatOutput, error) {
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}

	messages := make([]ollamaMsg, 0, len(input.Messages))
	for _, m := range input.Messages {
		if m.Role == "system" || m.Role == "user" || m.Role == "assistant" {
			messages = append(messages, ollamaMsg{Role: m.Role, Content: m.Content})
		}
	}

	maxTokens := input.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}

	reqBody := ollamaRequest{
		Model:    input.Model,
		Messages: messages,
		Stream:   false,
		Options: ollamaOptions{
			Temperature: input.Temperature,
			NumPredict:  maxTokens,
		},
	}

	bodyJSON, err := json.Marshal(reqBody)
	if err != nil {
		return ChatOutput{}, fmt.Errorf("ollama: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/api/chat", bytes.NewReader(bodyJSON))
	if err != nil {
		return ChatOutput{}, fmt.Errorf("ollama: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return ChatOutput{}, fmt.Errorf("ollama: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return ChatOutput{}, fmt.Errorf("ollama: read response: %w", err)
	}

	if resp.StatusCode >= 300 {
		return ChatOutput{}, fmt.Errorf("ollama: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result ollamaResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return ChatOutput{}, fmt.Errorf("ollama: parse response: %w", err)
	}

	usage := Usage{
		PromptTokens:     result.PromptEvalCount,
		CompletionTokens: result.EvalCount,
		TotalTokens:      result.PromptEvalCount + result.EvalCount,
	}

	return ChatOutput{
		Choices: []Choice{{
			Message:      ChoiceMessage{Role: result.Message.Role, Content: result.Message.Content},
			FinishReason: "stop",
		}},
		Usage: usage,
		Cost:  0,
		Model: input.Model,
	}, nil
}
