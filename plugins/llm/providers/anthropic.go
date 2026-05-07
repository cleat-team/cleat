package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

type anthropicRequest struct {
	Model       string           `json:"model"`
	MaxTokens   int              `json:"max_tokens"`
	Messages    []anthropicMsg   `json:"messages"`
	System      string           `json:"system,omitempty"`
	Temperature float64          `json:"temperature,omitempty"`
	Tools       []anthropicTool  `json:"tools,omitempty"`
}

type anthropicMsg struct {
	Role    string              `json:"role"`
	Content []anthropicContent  `json:"content"`
}

type anthropicContent struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	Content      string          `json:"content,omitempty"`
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	ID           string          `json:"id,omitempty"`
	Name         string          `json:"name,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
}

type anthropicTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"input_schema"`
}

type anthropicResponse struct {
	Content []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		ID    string          `json:"id,omitempty"`
		Name  string          `json:"name,omitempty"`
		Input json.RawMessage `json:"input,omitempty"`
	} `json:"content"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Model string `json:"model"`
}

// AnthropicChat calls the Anthropic Messages API.
func AnthropicChat(ctx context.Context, client *http.Client, apiKey, baseURL string, input ChatInput) (ChatOutput, error) {
	if baseURL == "" {
		baseURL = "https://api.anthropic.com/v1"
	}

	systemPrompt := input.System
	messages := make([]anthropicMsg, 0, len(input.Messages))
	for _, m := range input.Messages {
		if m.Role == "system" {
			if systemPrompt == "" {
				systemPrompt = m.Content
			}
			continue
		}
		content := []anthropicContent{}
		if m.Content != "" {
			content = append(content, anthropicContent{Type: "text", Text: m.Content})
		}
		for _, tc := range m.ToolCalls {
			content = append(content, anthropicContent{
				Type:      "tool_use",
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Input:     json.RawMessage(tc.Function.Arguments),
			})
		}
		if m.Role == "tool" {
			content = append(content, anthropicContent{
				Type:      "tool_result",
				ToolUseID: m.ToolCallID,
				Content:   m.Content,
			})
			if len(content) == 0 {
				content = append(content, anthropicContent{
					Type:      "tool_result",
					ToolUseID: m.ToolCallID,
				})
			}
		}
		role := m.Role
		if role == "assistant" {
			role = "assistant"
		}
		messages = append(messages, anthropicMsg{Role: role, Content: content})
	}

	// Convert OpenAI-format tools to Anthropic format
	anthropicTools := make([]anthropicTool, 0, len(input.Tools))
	for _, t := range input.Tools {
		anthropicTools = append(anthropicTools, anthropicTool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: t.Function.Parameters,
		})
	}

	maxTokens := input.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}

	reqBody := anthropicRequest{
		Model:       input.Model,
		MaxTokens:   maxTokens,
		Messages:    messages,
		System:      systemPrompt,
		Temperature: input.Temperature,
		Tools:       anthropicTools,
	}

	bodyJSON, err := json.Marshal(reqBody)
	if err != nil {
		return ChatOutput{}, fmt.Errorf("anthropic: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/messages", bytes.NewReader(bodyJSON))
	if err != nil {
		return ChatOutput{}, fmt.Errorf("anthropic: create request: %w", err)
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return ChatOutput{}, fmt.Errorf("anthropic: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return ChatOutput{}, fmt.Errorf("anthropic: read response: %w", err)
	}

	if resp.StatusCode >= 300 {
		return ChatOutput{}, fmt.Errorf("anthropic: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result anthropicResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return ChatOutput{}, fmt.Errorf("anthropic: parse response: %w", err)
	}

	choice := Choice{FinishReason: "stop"}
	for _, c := range result.Content {
		switch c.Type {
		case "text":
			choice.Message.Content = c.Text
		case "tool_use":
			choice.Message.ToolCalls = append(choice.Message.ToolCalls, ToolCall{
				ID:   c.ID,
				Type: "function",
				Function: FunctionCall{
					Name:      c.Name,
					Arguments: string(c.Input),
				},
			})
			choice.FinishReason = "tool_calls"
		}
	}
	choice.Message.Role = "assistant"

	usage := Usage{
		PromptTokens:     result.Usage.InputTokens,
		CompletionTokens: result.Usage.OutputTokens,
		TotalTokens:      result.Usage.InputTokens + result.Usage.OutputTokens,
	}

	var cost float64
	switch input.Model {
	case "claude-opus-4-7":
		cost = float64(usage.PromptTokens)*15.0/1_000_000 + float64(usage.CompletionTokens)*75.0/1_000_000
	case "claude-sonnet-4-6":
		cost = float64(usage.PromptTokens)*3.0/1_000_000 + float64(usage.CompletionTokens)*15.0/1_000_000
	case "claude-haiku-4-5":
		cost = float64(usage.PromptTokens)*0.80/1_000_000 + float64(usage.CompletionTokens)*4.0/1_000_000
	default:
		cost = float64(usage.PromptTokens)*3.0/1_000_000 + float64(usage.CompletionTokens)*15.0/1_000_000
	}

	return ChatOutput{
		Choices: []Choice{choice},
		Usage:   usage,
		Cost:    cost,
		Model:   result.Model,
	}, nil
}
