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

type ollamaRequest struct {
	Model    string        `json:"model"`
	Messages []ollamaMsg   `json:"messages"`
	Stream   bool          `json:"stream"`
	Options  ollamaOptions `json:"options,omitempty"`
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
	Message         ollamaMsg `json:"message"`
	Done            bool      `json:"done"`
	TotalDuration   int64     `json:"total_duration"`
	EvalCount       int       `json:"eval_count"`
	PromptEvalCount int       `json:"prompt_eval_count"`
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

// OllamaChatStream calls the Ollama API in streaming mode.
// Returns a channel of StreamChunk that is closed when the stream is complete.
func OllamaChatStream(ctx context.Context, client *http.Client, baseURL string, input ChatInput) (<-chan StreamChunk, error) {
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}

	bodyMap := map[string]any{}
	data, _ := json.Marshal(input)
	_ = json.Unmarshal(data, &bodyMap)
	bodyMap["stream"] = true

	bodyJSON, err := json.Marshal(bodyMap)
	if err != nil {
		return nil, fmt.Errorf("ollama: marshal stream request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/api/chat", bytes.NewReader(bodyJSON))
	if err != nil {
		return nil, fmt.Errorf("ollama: create stream request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama: stream request failed: %w", err)
	}

	ch := make(chan StreamChunk, 64)
	go func() {
		defer resp.Body.Close()
		defer close(ch)

		scanner := bufio.NewScanner(resp.Body)
		index := 0
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}

			var sseData struct {
				Message *struct {
					Content string `json:"content"`
				} `json:"message,omitempty"`
				Done bool `json:"done"`
			}
			if err := json.Unmarshal([]byte(line), &sseData); err != nil {
				continue
			}

			var text string
			if sseData.Message != nil {
				text = sseData.Message.Content
			}

			ch <- StreamChunk{
				Content: text,
				Index:   index,
				Done:    sseData.Done,
			}
			index++

			if sseData.Done {
				return
			}
		}
	}()

	return ch, nil
}
