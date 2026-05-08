//go:build ignore

package main

import (
	"encoding/json"
	"fmt"

	"github.com/rcownie/cleat/cleat"
)

// ---- Types ----

// Message represents a conversation message.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// ToolCall represents a model's request to call a tool.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall contains the name and arguments for a tool invocation.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool defines a function tool available to the model.
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction describes a callable function.
type ToolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

// LLMRequest is the JSON input for an LLM chat call.
type LLMRequest struct {
	Provider    string    `json:"provider"`
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Tools       []Tool    `json:"tools,omitempty"`
}

// LLMResponse is the JSON output from an LLM chat call.
type LLMResponse struct {
	Choices []struct {
		Message struct {
			Role      string     `json:"role"`
			Content   string     `json:"content"`
			ToolCalls []ToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Cost  float64 `json:"cost"`
	Model string  `json:"model"`
	Error string  `json:"error,omitempty"`
}

// ---- Agent workflow ----

// AgentLoop is a cleat AI agent that uses the LLM plugin.
// It survives crashes and replays LLM responses from event history.
//
// @cleatEntry(name="agent")
func AgentLoop(h cleat.HostCalls, inputJSON string) (string, error) {
	var input struct {
		Task         string  `json:"task"`
		Provider     string  `json:"provider"`
		Model        string  `json:"model"`
		SystemPrompt string  `json:"system_prompt"`
		MaxSteps     int     `json:"max_steps"`
		Temperature  float64 `json:"temperature"`
	}
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}

	// Set defaults.
	if input.Provider == "" {
		input.Provider = "openai"
	}
	if input.Model == "" {
		input.Model = "gpt-4o-mini"
	}
	if input.SystemPrompt == "" {
		input.SystemPrompt = "You are a helpful AI assistant. Use tools when appropriate and explain your reasoning."
	}
	if input.MaxSteps == 0 {
		input.MaxSteps = 10
	}
	if input.Temperature == 0 {
		input.Temperature = 0.7
	}

	conversation := []Message{
		{Role: "system", Content: input.SystemPrompt},
		{Role: "user", Content: input.Task},
	}

	tools := getTools()

	for step := 0; step < input.MaxSteps; step++ {
		req := LLMRequest{
			Provider:    input.Provider,
			Model:       input.Model,
			Messages:    conversation,
			Temperature: input.Temperature,
			MaxTokens:   4096,
			Tools:       tools,
		}
		reqJSON, err := json.Marshal(req)
		if err != nil {
			return "", fmt.Errorf("marshal llm request: %w", err)
		}

		// Call LLM via plugin — response is recorded in event history
		// and replayed deterministically on crash recovery.
		respJSON, err := h.PluginCall("llm", "chat", string(reqJSON))
		if err != nil {
			return "", fmt.Errorf("llm call failed: %w", err)
		}

		var resp LLMResponse
		if err := json.Unmarshal([]byte(respJSON), &resp); err != nil {
			return "", fmt.Errorf("invalid llm response: %w", err)
		}
		if resp.Error != "" {
			return "", fmt.Errorf("llm error: %s", resp.Error)
		}

		// Store usage in query state for dashboard visibility.
		h.SetQueryState("llm_cost", fmt.Sprintf("%.6f", resp.Cost))
		h.SetQueryState("llm_tokens", fmt.Sprintf("%d", resp.Usage.TotalTokens))
		h.SetQueryState("agent_steps", fmt.Sprintf("%d", step+1))

		if len(resp.Choices) == 0 {
			return "", fmt.Errorf("no choices in llm response")
		}
		choice := resp.Choices[0]

		// If the LLM wants to use a tool, execute it and continue the loop.
		if len(choice.Message.ToolCalls) > 0 {
			conversation = append(conversation, Message{
				Role:      "assistant",
				Content:   choice.Message.Content,
				ToolCalls: choice.Message.ToolCalls,
			})
			for _, tc := range choice.Message.ToolCalls {
				toolResult := executeTool(h, tc.Function.Name, tc.Function.Arguments)
				conversation = append(conversation, Message{
					Role:       "tool",
					ToolCallID: tc.ID,
					Content:    toolResult,
					Name:       tc.Function.Name,
				})
			}
			continue
		}

		// LLM responded with text — agent is done.
		conversation = append(conversation, Message{
			Role:    "assistant",
			Content: choice.Message.Content,
		})

		result := map[string]any{
			"output":       choice.Message.Content,
			"steps":        step + 1,
			"total_tokens": resp.Usage.TotalTokens,
			"cost":         resp.Cost,
			"model":        resp.Model,
		}
		resultJSON, _ := json.Marshal(result)
		return string(resultJSON), nil
	}

	return "", fmt.Errorf("agent exceeded max steps (%d)", input.MaxSteps)
}
