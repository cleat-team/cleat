package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ---------------------------------------------------------------------------
// Gemini request types
// ---------------------------------------------------------------------------

type geminiRequest struct {
	SystemInstruction *geminiContent  `json:"system_instruction,omitempty"`
	Contents          []geminiContent `json:"contents"`
	Tools             []geminiTool    `json:"tools,omitempty"`
	GenerationConfig  geminiConfig    `json:"generationConfig"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text            string                  `json:"text,omitempty"`
	FunctionCall    *geminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`
}

type geminiFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type geminiFunctionResponse struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

type geminiTool struct {
	FunctionDeclarations []geminiFunctionDecl `json:"functionDeclarations"`
}

type geminiFunctionDecl struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

type geminiConfig struct {
	Temperature     float64 `json:"temperature,omitempty"`
	MaxOutputTokens int     `json:"maxOutputTokens,omitempty"`
}

// ---------------------------------------------------------------------------
// Gemini response types
// ---------------------------------------------------------------------------

type geminiResponse struct {
	Candidates    []geminiCandidate `json:"candidates"`
	UsageMetadata geminiUsage       `json:"usageMetadata"`
	Error         *geminiAPIError   `json:"error,omitempty"`
}

type geminiCandidate struct {
	Content       geminiContent `json:"content"`
	FinishReason  string        `json:"finishReason"`
}

type geminiUsage struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}

type geminiAPIError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

// ---------------------------------------------------------------------------
// GeminiChat
// ---------------------------------------------------------------------------

// GeminiChat calls the Google Gemini generateContent API.
func GeminiChat(ctx context.Context, client *http.Client, apiKey, baseURL string, input ChatInput) (ChatOutput, error) {
	if baseURL == "" {
		baseURL = "https://generativelanguage.googleapis.com/v1beta"
	}

	// --- Build the Gemini request --------------------------------------------------

	// Extract system instruction from input.System or the first system message.
	systemText := input.System
	var contents []geminiContent

	// Build a map of tool_call_id -> function name so we can fill in the function
	// name when the caller sends a "tool" role message (which carries only the ID).
	toolCallNames := make(map[string]string)
	for _, m := range input.Messages {
		for _, tc := range m.ToolCalls {
			toolCallNames[tc.ID] = tc.Function.Name
		}
	}

	for _, m := range input.Messages {
		switch m.Role {
		case "system":
			if systemText == "" {
				systemText = m.Content
			}
			// system messages are not added to contents[]

		case "user":
			contents = append(contents, geminiContent{
				Role:  "user",
				Parts: []geminiPart{{Text: m.Content}},
			})

		case "assistant":
			parts := []geminiPart{}
			if m.Content != "" {
				parts = append(parts, geminiPart{Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				parts = append(parts, geminiPart{
					FunctionCall: &geminiFunctionCall{
						Name: tc.Function.Name,
						Args: json.RawMessage(tc.Function.Arguments),
					},
				})
			}
			contents = append(contents, geminiContent{
				Role:  "model",
				Parts: parts,
			})

		case "tool":
			name := m.Name
			if name == "" {
				if n, ok := toolCallNames[m.ToolCallID]; ok {
					name = n
				}
			}
			responseObj := wrapToolResult(m.Content)
			parts := []geminiPart{{
				FunctionResponse: &geminiFunctionResponse{
					Name:     name,
					Response: responseObj,
				},
			}}
			contents = append(contents, geminiContent{
				Role:  "function",
				Parts: parts,
			})
		}
	}

	// Convert tools (OpenAI format -> Gemini function declarations)
	var tools []geminiTool
	for _, t := range input.Tools {
		if t.Type == "function" {
			tools = append(tools, geminiTool{
				FunctionDeclarations: []geminiFunctionDecl{{
					Name:        t.Function.Name,
					Description: t.Function.Description,
					Parameters:  t.Function.Parameters,
				}},
			})
		}
	}

	reqBody := geminiRequest{
		GenerationConfig: geminiConfig{
			Temperature:     input.Temperature,
			MaxOutputTokens: input.MaxTokens,
		},
		Contents: contents,
		Tools:    tools,
	}
	if systemText != "" {
		reqBody.SystemInstruction = &geminiContent{
			Parts: []geminiPart{{Text: systemText}},
		}
	}

	bodyJSON, err := json.Marshal(reqBody)
	if err != nil {
		return ChatOutput{}, fmt.Errorf("gemini: marshal request: %w", err)
	}

	// --- Send request --------------------------------------------------------------

	endpoint := baseURL + "/models/" + input.Model + ":generateContent"

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(bodyJSON))
	if err != nil {
		return ChatOutput{}, fmt.Errorf("gemini: create request: %w", err)
	}
	req.Header.Set("x-goog-api-key", apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return ChatOutput{}, fmt.Errorf("gemini: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return ChatOutput{}, fmt.Errorf("gemini: read response: %w", err)
	}

	if resp.StatusCode >= 300 {
		// Try to extract a structured error from the body.
		var errResp struct {
			Error *geminiAPIError `json:"error"`
		}
		if json.Unmarshal(respBody, &errResp) == nil && errResp.Error != nil {
			return ChatOutput{}, fmt.Errorf("gemini: HTTP %d: %s (%s)",
				resp.StatusCode, errResp.Error.Message, errResp.Error.Status)
		}
		return ChatOutput{}, fmt.Errorf("gemini: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	// --- Parse response ------------------------------------------------------------

	var result geminiResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return ChatOutput{}, fmt.Errorf("gemini: parse response: %w", err)
	}

	if len(result.Candidates) == 0 {
		return ChatOutput{}, fmt.Errorf("gemini: no candidates in response")
	}

	candidate := result.Candidates[0]

	choice := Choice{}
	choice.Message.Role = "assistant"

	for _, part := range candidate.Content.Parts {
		if part.Text != "" {
			choice.Message.Content = part.Text
		}
		if part.FunctionCall != nil {
			choice.Message.ToolCalls = append(choice.Message.ToolCalls, ToolCall{
				ID:   "", // Gemini does not supply an ID for function calls
				Type: "function",
				Function: FunctionCall{
					Name:      part.FunctionCall.Name,
					Arguments: string(part.FunctionCall.Args),
				},
			})
		}
	}

	// Map Gemini finish reason -> OpenAI-style finish reason.
	switch candidate.FinishReason {
	case "STOP":
		if len(choice.Message.ToolCalls) > 0 {
			choice.FinishReason = "tool_calls"
		} else {
			choice.FinishReason = "stop"
		}
	case "MAX_TOKENS":
		choice.FinishReason = "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT":
		choice.FinishReason = "content_filter"
	default:
		choice.FinishReason = "stop"
	}

	usage := Usage{
		PromptTokens:     result.UsageMetadata.PromptTokenCount,
		CompletionTokens: result.UsageMetadata.CandidatesTokenCount,
		TotalTokens:      result.UsageMetadata.TotalTokenCount,
	}

	return ChatOutput{
		Choices: []Choice{choice},
		Usage:   usage,
		Cost:    geminiCost(input.Model, usage),
		Model:   input.Model,
	}, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// geminiCost calculates an approximate cost for a Gemini model.
func geminiCost(model string, usage Usage) float64 {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "gemini-2.5-flash"):
		// $0.15/1M input, $0.60/1M output (<=200K tokens range)
		return float64(usage.PromptTokens)*0.15/1_000_000 +
			float64(usage.CompletionTokens)*0.60/1_000_000
	case strings.Contains(m, "gemini-2.5-pro"):
		return float64(usage.PromptTokens)*1.25/1_000_000 +
			float64(usage.CompletionTokens)*5.00/1_000_000
	case strings.Contains(m, "gemini-2.0-flash-lite"):
		return float64(usage.PromptTokens)*0.075/1_000_000 +
			float64(usage.CompletionTokens)*0.30/1_000_000
	case strings.Contains(m, "gemini-2.0-flash"):
		return float64(usage.PromptTokens)*0.10/1_000_000 +
			float64(usage.CompletionTokens)*0.40/1_000_000
	default:
		return float64(usage.PromptTokens)*0.15/1_000_000 +
			float64(usage.CompletionTokens)*0.60/1_000_000
	}
}

// wrapToolResult converts a tool result string into a JSON object suitable for
// Gemini's functionResponse.response field.  If the content is already a JSON
// object it is used as-is; otherwise it is wrapped in {"result": ...}.
func wrapToolResult(content string) json.RawMessage {
	var parsed any
	if err := json.Unmarshal([]byte(content), &parsed); err == nil {
		if obj, ok := parsed.(map[string]any); ok {
			raw, _ := json.Marshal(obj)
			return raw
		}
		// Valid JSON but not an object — wrap it.
		raw, _ := json.Marshal(map[string]any{"result": parsed})
		return raw
	}
	// Not valid JSON at all — wrap as a string.
	raw, _ := json.Marshal(map[string]string{"result": content})
	return raw
}
