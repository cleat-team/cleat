// Package llm provides typed Go wrappers for the "llm" plugin.
//
// Usage:
//
//	client := llm.NewClient(h.PluginCall)
//	resp, err := client.Chat(ctx, llm.ChatRequest{
//	    Provider: "openai",
//	    Model:    "gpt-4o",
//	    Messages: []llm.Message{{Role: "user", Content: "hello"}},
//	    Tools:    []llm.Tool{{Name: "search", Description: "web search", Parameters: ...}},
//	})
//
// Or use the convenience function with a HostCalls directly:
//
//	resp, err := llm.Chat(h, req)
package llm

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/rcownie/durable/durable"
)

// Message represents a chat message in a conversation.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"-"`
}

// Tool represents a function tool definition available to the LLM.
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

// ToolCall represents a tool call request made by the LLM.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ChatRequest is the typed request for an LLM chat completion.
type ChatRequest struct {
	Provider    string    `json:"provider"`
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Tools       []Tool    `json:"tools,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Temperature float64   `json:"temperature,omitempty"`
}

// ChatResponse is the typed response from an LLM chat completion.
type ChatResponse struct {
	Message   Message    `json:"message"`
	Usage     Usage      `json:"usage"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	Cost      float64    `json:"cost"`
}

// Usage tracks token consumption for a request.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// EmbedRequest is the typed request for generating embeddings.
type EmbedRequest struct {
	Provider string   `json:"provider"`
	Model    string   `json:"model"`
	Input    []string `json:"input"`
}

// EmbedResponse is the typed response from an embedding request.
type EmbedResponse struct {
	Embeddings [][]float64 `json:"embeddings"`
	Usage      Usage       `json:"usage"`
}

// ModelInfo describes an available model from a provider.
type ModelInfo struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
}

// Client wraps a plugin call function for typed LLM operations.
type Client struct {
	call func(pluginName, functionName, inputJSON string) (string, error)
}

// NewClient creates a new LLM Client backed by the given call function.
// The call function should match the signature of durable.HostCalls.PluginCall.
func NewClient(call func(pluginName, functionName, inputJSON string) (string, error)) *Client {
	return &Client{call: call}
}

// Chat sends a chat completion request to the LLM plugin and returns the
// assistant response, including any tool calls.
func (c *Client) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	// Build the outgoing request in the format the plugin expects.

	// Convert Tools from our simplified format to the plugin's nested format.
	// Our format:  {name, description, parameters}
	// Plugin format: {type:"function", function:{name, description, parameters}}
	type pluginTool struct {
		Type     string          `json:"type"`
		Function json.RawMessage `json:"function"`
	}
	tools := make([]pluginTool, len(req.Tools))
	for i, t := range req.Tools {
		fn := map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"parameters":  t.Parameters,
		}
		fnJSON, err := json.Marshal(fn)
		if err != nil {
			return nil, fmt.Errorf("llm: marshal tool function: %w", err)
		}
		tools[i] = pluginTool{Type: "function", Function: fnJSON}
	}

	// Convert Messages to the plugin format.
	type pluginToolCall struct {
		ID       string          `json:"id"`
		Type     string          `json:"type"`
		Function json.RawMessage `json:"function"`
	}
	type pluginMsg struct {
		Role       string           `json:"role"`
		Content    string           `json:"content,omitempty"`
		ToolCallID string           `json:"tool_call_id,omitempty"`
		ToolCalls  []pluginToolCall `json:"tool_calls,omitempty"`
	}
	messages := make([]pluginMsg, len(req.Messages))
	for i, msg := range req.Messages {
		pm := pluginMsg{
			Role:       msg.Role,
			Content:    msg.Content,
			ToolCallID: msg.ToolCallID,
		}
		for _, tc := range msg.ToolCalls {
			fn := map[string]any{"name": tc.Name, "arguments": tc.Arguments}
			fnJSON, err := json.Marshal(fn)
			if err != nil {
				return nil, fmt.Errorf("llm: marshal tool call function: %w", err)
			}
			pm.ToolCalls = append(pm.ToolCalls, pluginToolCall{
				ID:       tc.ID,
				Type:     "function",
				Function: fnJSON,
			})
		}
		messages[i] = pm
	}

	// Assemble the full plugin request.
	requestMap := map[string]any{
		"provider":    req.Provider,
		"model":       req.Model,
		"messages":    messages,
		"temperature": req.Temperature,
		"max_tokens":  req.MaxTokens,
	}
	if len(tools) > 0 {
		requestMap["tools"] = tools
	}

	inputJSON, err := json.Marshal(requestMap)
	if err != nil {
		return nil, fmt.Errorf("llm: marshal chat request: %w", err)
	}

	outputJSON, err := c.call("llm", "chat", string(inputJSON))
	if err != nil {
		return nil, fmt.Errorf("llm: chat call failed: %w", err)
	}

	// Parse the plugin response into our typed response.
	var pluginResp struct {
		Choices []struct {
			Message struct {
				Role      string `json:"role"`
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls,omitempty"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage Usage   `json:"usage"`
		Cost  float64 `json:"cost"`
		Error string  `json:"error,omitempty"`
	}
	if err := json.Unmarshal([]byte(outputJSON), &pluginResp); err != nil {
		return nil, fmt.Errorf("llm: unmarshal chat response: %w", err)
	}
	if pluginResp.Error != "" {
		return nil, fmt.Errorf("llm: %s", pluginResp.Error)
	}
	if len(pluginResp.Choices) == 0 {
		return nil, fmt.Errorf("llm: no choices in response")
	}

	choice := pluginResp.Choices[0]
	resp := &ChatResponse{
		Message: Message{
			Role:    choice.Message.Role,
			Content: choice.Message.Content,
		},
		Usage: pluginResp.Usage,
		Cost:  pluginResp.Cost,
	}
	for _, tc := range choice.Message.ToolCalls {
		resp.ToolCalls = append(resp.ToolCalls, ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}

	return resp, nil
}

// Embed sends an embedding request to the LLM plugin and returns the
// embedding vectors and token usage.
func (c *Client) Embed(ctx context.Context, req EmbedRequest) (*EmbedResponse, error) {
	inputJSON, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("llm: marshal embed request: %w", err)
	}

	outputJSON, err := c.call("llm", "embed", string(inputJSON))
	if err != nil {
		return nil, fmt.Errorf("llm: embed call failed: %w", err)
	}

	// Parse the plugin response.
	var pluginResp struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
			Index     int       `json:"index"`
		} `json:"data"`
		Usage Usage   `json:"usage"`
		Cost  float64 `json:"cost"`
		Error string  `json:"error,omitempty"`
	}
	if err := json.Unmarshal([]byte(outputJSON), &pluginResp); err != nil {
		return nil, fmt.Errorf("llm: unmarshal embed response: %w", err)
	}
	if pluginResp.Error != "" {
		return nil, fmt.Errorf("llm: %s", pluginResp.Error)
	}

	resp := &EmbedResponse{
		Embeddings: make([][]float64, len(pluginResp.Data)),
		Usage:      pluginResp.Usage,
	}
	for i, d := range pluginResp.Data {
		resp.Embeddings[i] = d.Embedding
	}

	return resp, nil
}

// ListModels returns the available models for the given provider. If provider
// is empty, models from all enabled providers are returned.
func (c *Client) ListModels(ctx context.Context, provider string) ([]ModelInfo, error) {
	inputJSON, err := json.Marshal(map[string]string{"provider": provider})
	if err != nil {
		return nil, fmt.Errorf("llm: marshal list_models request: %w", err)
	}

	outputJSON, err := c.call("llm", "list_models", string(inputJSON))
	if err != nil {
		return nil, fmt.Errorf("llm: list_models call failed: %w", err)
	}

	// The plugin returns two possible shapes depending on whether a provider
	// was specified:
	//   Single:  {"models": [...], "provider": "openai"}
	//   All:     {"providers": {"openai": [...], "anthropic": [...]}}
	// Try the single-provider format first.

	var singleResp struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
		Provider string `json:"provider"`
	}
	if err := json.Unmarshal([]byte(outputJSON), &singleResp); err == nil && singleResp.Provider != "" {
		models := make([]ModelInfo, len(singleResp.Models))
		for i, m := range singleResp.Models {
			models[i] = ModelInfo{ID: m.Name, Provider: singleResp.Provider}
		}
		return models, nil
	}

	// Try the all-providers format.
	var allResp struct {
		Providers map[string][]struct {
			Name string `json:"name"`
		} `json:"providers"`
	}
	if err := json.Unmarshal([]byte(outputJSON), &allResp); err != nil {
		return nil, fmt.Errorf("llm: unmarshal list_models response: %w", err)
	}

	var models []ModelInfo
	for prov, ms := range allResp.Providers {
		for _, m := range ms {
			models = append(models, ModelInfo{ID: m.Name, Provider: prov})
		}
	}
	return models, nil
}

// Chat is a convenience function that creates a Client from a HostCalls and
// calls Chat. Uses context.Background() since PluginCall does not take a context.
func Chat(h durable.HostCalls, req ChatRequest) (*ChatResponse, error) {
	return NewClient(h.PluginCall).Chat(context.Background(), req)
}
