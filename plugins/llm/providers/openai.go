package providers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// OpenAIChat calls the OpenAI chat completions API.
func OpenAIChat(ctx context.Context, client *http.Client, apiKey, baseURL string, input ChatInput) (ChatOutput, error) {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}

	bodyJSON, err := json.Marshal(input)
	if err != nil {
		return ChatOutput{}, fmt.Errorf("openai: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/chat/completions", bytes.NewReader(bodyJSON))
	if err != nil {
		return ChatOutput{}, fmt.Errorf("openai: create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return ChatOutput{}, fmt.Errorf("openai: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return ChatOutput{}, fmt.Errorf("openai: read response: %w", err)
	}

	if resp.StatusCode >= 300 {
		return ChatOutput{}, fmt.Errorf("openai: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Choices []struct {
			Message      ChoiceMessage `json:"message"`
			FinishReason string        `json:"finish_reason"`
		} `json:"choices"`
		Usage Usage  `json:"usage"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return ChatOutput{}, fmt.Errorf("openai: parse response: %w", err)
	}

	choices := make([]Choice, len(result.Choices))
	for i, c := range result.Choices {
		choices[i] = Choice{Message: c.Message, FinishReason: c.FinishReason}
	}

	var cost float64
	switch input.Model {
	case "gpt-4o":
		cost = float64(result.Usage.PromptTokens)*2.50/1_000_000 + float64(result.Usage.CompletionTokens)*10.0/1_000_000
	case "gpt-4o-mini":
		cost = float64(result.Usage.PromptTokens)*0.15/1_000_000 + float64(result.Usage.CompletionTokens)*0.60/1_000_000
	case "gpt-4-turbo":
		cost = float64(result.Usage.PromptTokens)*10.0/1_000_000 + float64(result.Usage.CompletionTokens)*30.0/1_000_000
	default:
		cost = float64(result.Usage.PromptTokens)*2.50/1_000_000 + float64(result.Usage.CompletionTokens)*10.0/1_000_000
	}

	return ChatOutput{
		Choices: choices,
		Usage:   result.Usage,
		Cost:    cost,
		Model:   result.Model,
	}, nil
}

// OpenAIChatStream calls the OpenAI chat completions API in streaming mode.
// Returns a channel of StreamChunk that is closed when the stream is complete.
func OpenAIChatStream(ctx context.Context, client *http.Client, apiKey, baseURL string, input ChatInput) (<-chan StreamChunk, error) {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}

	// Clone input and enable streaming.
	bodyMap := map[string]any{}
	data, _ := json.Marshal(input)
	_ = json.Unmarshal(data, &bodyMap)
	bodyMap["stream"] = true

	bodyJSON, err := json.Marshal(bodyMap)
	if err != nil {
		return nil, fmt.Errorf("openai: marshal stream request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/chat/completions", bytes.NewReader(bodyJSON))
	if err != nil {
		return nil, fmt.Errorf("openai: create stream request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai: stream request failed: %w", err)
	}

	ch := make(chan StreamChunk, 64)
	go func() {
		defer resp.Body.Close()
		defer close(ch)

		scanner := bufio.NewScanner(resp.Body)
		index := 0
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				return
			}

			var sseData struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
					FinishReason *string `json:"finish_reason"`
				} `json:"choices"`
			}
			if err := json.Unmarshal([]byte(payload), &sseData); err != nil {
				continue
			}
			if len(sseData.Choices) == 0 {
				continue
			}
			content := sseData.Choices[0].Delta.Content
			chunk := StreamChunk{
				Content: content,
				Index:   index,
			}
			index++
			if sseData.Choices[0].FinishReason != nil {
				chunk.Done = true
			}
			ch <- chunk
		}
	}()

	return ch, nil
}

// OpenAIEmbed calls the OpenAI embeddings API.
func OpenAIEmbed(ctx context.Context, client *http.Client, apiKey, baseURL string, input EmbedInput) (EmbedOutput, error) {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}

	bodyJSON, err := json.Marshal(input)
	if err != nil {
		return EmbedOutput{}, fmt.Errorf("openai: marshal embed request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/embeddings", bytes.NewReader(bodyJSON))
	if err != nil {
		return EmbedOutput{}, fmt.Errorf("openai: create embed request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return EmbedOutput{}, fmt.Errorf("openai: embed request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return EmbedOutput{}, fmt.Errorf("openai: read embed response: %w", err)
	}

	if resp.StatusCode >= 300 {
		return EmbedOutput{}, fmt.Errorf("openai: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Data  []EmbeddingData `json:"data"`
		Usage Usage           `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return EmbedOutput{}, fmt.Errorf("openai: parse embed response: %w", err)
	}

	cost := float64(result.Usage.TotalTokens) * 0.02 / 1_000_000

	return EmbedOutput{
		Data:  result.Data,
		Usage: result.Usage,
		Cost:  cost,
	}, nil
}

// OpenAIChatStream calls the OpenAI chat completions API in streaming mode.
