package workflows

import (
	"fmt"

	"github.com/cleat-team/cleat/cleat"
)

// LLMInput configures the AI agent loop simulation.
type LLMInput struct {
	// Prompts is the number of LLM prompt iterations to simulate.
	Prompts int `json:"prompts"`
	// ToolsPerPrompt is the number of tool invocations per prompt iteration.
	ToolsPerPrompt int `json:"tools_per_prompt"`
}

// LLMOutput is the result of the LLM simulation workflow.
type LLMOutput struct {
	// TotalCalls is the total number of durable calls made (LLM + tools).
	TotalCalls int `json:"total_calls"`
}

// LLMWorkflow simulates an AI agent loop with LLM calls and tool invocations.
// Each "prompt" involves one LLM chat call followed by multiple tool
// invocations. This pattern matches typical agentic workflows
// (e.g., ReAct, function calling, chain-of-thought with tools).
//
// Equivalent Temporal: a workflow with N activity loops.
// Equivalent DBOS: a workflow with N iteration steps.
func LLMWorkflow(h cleat.HostCalls, input LLMInput) (LLMOutput, error) {
	totalCalls := 0
	for i := 0; i < input.Prompts; i++ {
		// Simulate an LLM chat call (e.g., GPT-4, Claude).
		if _, err := h.DurableCall("llm", "chat", fmt.Sprintf(
			`{"prompt":"benchmark_prompt_%d","model":"gpt-4"}`, i,
		)); err != nil {
			return LLMOutput{}, err
		}
		totalCalls++

		// Simulate tool invocations from the LLM response.
		for j := 0; j < input.ToolsPerPrompt; j++ {
			if _, err := h.DurableCall("tools", "invoke", fmt.Sprintf(
				`{"tool":"bench_tool_%d","iteration":%d}`, j, i,
			)); err != nil {
				return LLMOutput{}, err
			}
			totalCalls++
		}
	}
	return LLMOutput{TotalCalls: totalCalls}, nil
}
