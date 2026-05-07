// Package agent provides a simple agent loop that uses the LLM client
// to run a tool-using agent. It manages conversation history, handles
// tool call execution, and enforces step limits.
//
// Usage:
//
//	result, err := agent.Run(ctx, llmClient, agent.AgentConfig{
//	    SystemPrompt: "You are a helpful assistant.",
//	    MaxSteps:     10,
//	    Tools:        myTools,
//	    Model:        "gpt-4o",
//	    Provider:     "openai",
//	}, "What is the weather in Tokyo?", executeTool)
package agent

import (
	"context"
	"fmt"

	"github.com/rcownie/durable/durable/ai/llm"
)

// AgentConfig configures the agent loop.
type AgentConfig struct {
	SystemPrompt string
	MaxSteps     int
	Tools        []llm.Tool
	Model        string
	Provider     string
	MaxTokens    int
	Temperature  float64
}

// AgentStep records a single step in the agent's execution, including the
// tool that was called and its result.
type AgentStep struct {
	StepNumber int
	Thought    string
	ToolCall   *llm.ToolCall
	ToolResult string
	Cost       float64
}

// AgentResult is the final result of an agent run, including the answer,
// all intermediate steps, and aggregated cost/token information.
type AgentResult struct {
	Answer      string
	Steps       []AgentStep
	TotalCost   float64
	TotalTokens llm.Usage
}

// Run executes an agent loop with the given LLM client and tool executor.
//
// The loop:
//  1. Sends the system prompt, user message, and conversation history to the LLM.
//  2. If the LLM returns tool calls, each tool is executed via executeTool,
//     the result is added to the conversation history, and the loop continues.
//  3. If the LLM returns a final answer (no tool calls), the loop stops.
//  4. The loop enforces the MaxSteps limit from AgentConfig.
//
// executeTool receives a ToolCall and returns the string result of execution.
// If executeTool returns an error, it is captured as the tool result string
// so the LLM can handle it gracefully.
func Run(
	ctx context.Context,
	llmClient *llm.Client,
	cfg AgentConfig,
	userMessage string,
	executeTool func(toolCall llm.ToolCall) (string, error),
) (*AgentResult, error) {
	if cfg.MaxSteps <= 0 {
		cfg.MaxSteps = 10
	}

	// Build the initial conversation history.
	messages := []llm.Message{}
	if cfg.SystemPrompt != "" {
		messages = append(messages, llm.Message{Role: "system", Content: cfg.SystemPrompt})
	}
	messages = append(messages, llm.Message{Role: "user", Content: userMessage})

	result := &AgentResult{}

	for step := 0; step < cfg.MaxSteps; step++ {
		req := llm.ChatRequest{
			Provider:    cfg.Provider,
			Model:       cfg.Model,
			Messages:    messages,
			Tools:       cfg.Tools,
			MaxTokens:   cfg.MaxTokens,
			Temperature: cfg.Temperature,
		}

		resp, err := llmClient.Chat(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("agent: step %d chat failed: %w", step, err)
		}

		// Accumulate costs and token usage.
		result.TotalCost += resp.Cost
		result.TotalTokens.PromptTokens += resp.Usage.PromptTokens
		result.TotalTokens.CompletionTokens += resp.Usage.CompletionTokens
		result.TotalTokens.TotalTokens += resp.Usage.TotalTokens

		if len(resp.ToolCalls) > 0 {
			// Add the assistant message (with tool calls) to history.
			assistantMsg := llm.Message{
				Role:      resp.Message.Role,
				Content:   resp.Message.Content,
				ToolCalls: resp.ToolCalls,
			}
			messages = append(messages, assistantMsg)

			// Execute each tool call and add the result to history.
			for _, tc := range resp.ToolCalls {
				toolResult, execErr := executeTool(tc)
				if execErr != nil {
					toolResult = fmt.Sprintf("error: %v", execErr)
				}

				result.Steps = append(result.Steps, AgentStep{
					StepNumber: step,
					Thought:    resp.Message.Content,
					ToolCall:   &llm.ToolCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments},
					ToolResult: toolResult,
					Cost:       resp.Cost / float64(len(resp.ToolCalls)),
				})

				messages = append(messages, llm.Message{
					Role:       "tool",
					Content:    toolResult,
					ToolCallID: tc.ID,
				})
			}
			continue
		}

		// No tool calls -- this is the final answer.
		result.Answer = resp.Message.Content
		return result, nil
	}

	// Exceeded MaxSteps without a final answer.
	return nil, fmt.Errorf("agent: exceeded max steps (%d)", cfg.MaxSteps)
}
