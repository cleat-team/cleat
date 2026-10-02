// Package agentworkflow implements cleat's agent loop as a WORKFLOW rather
// than as a library.
//
// WHY A WORKFLOW AND NOT A LIBRARY. A library that runs inside the guest has
// to be rewritten in every language, and until this package existed it had
// been: cleat/ai/agent was a Go-only ReAct loop with no importer, and the two
// agent templates carried hand-written copies of the same loop (one Go, one
// Python) that nothing tested. A workflow is written once and started as a
// child by any SDK, so the copies collapse into a thin `run_agent` wrapper per
// language. The owner ruled this on 2026-09-22 (cleat#1983).
//
// EACH LLM TURN AND EACH TOOL CALL IS A DURABLE STEP, which is the whole
// reason the loop belongs in a workflow. `h.PluginCall` is recorded in the
// event history and REPLAYED rather than re-executed (engine/plugins.go:470
// branches on isReplay into replayPluginCall), so a run that dies mid
// conversation resumes without asking the model again for turns it already
// completed and without repeating a tool call whose effect already happened.
//
// THE CONSTRAINT THAT FOLLOWS, and it is not stylistic: replay is POSITIONAL.
// replayPluginCall takes s.history[s.stepCount] and requires it to be the
// expected event type, so the sequence of durable steps a run performs must be
// identical on every execution of the same history. That is why nothing here
// iterates a map, reads a clock, or branches on anything but the replayed
// responses -- the conversation is REBUILT from them on resume rather than
// stored. A single nondeterministic step would misalign the history and turn a
// resume into a divergence.
//
//cleat:require AwaitChild,ChildWorkflow,DurableCall,PluginCall,SetQueryState
package agentworkflow

import (
	"encoding/json"
	"fmt"

	"github.com/cleat-team/cleat/cleat"
)

// Tool kinds. A tool names one of the three ways a workflow can reach the
// outside world, which is what makes the agent usable without the model ever
// knowing which is which.
const (
	KindService  = "service"
	KindPlugin   = "plugin"
	KindWorkflow = "workflow"
)

// Input is the agent workflow's input.
type Input struct {
	// SystemPrompt is the model's system message. Empty uses
	// DefaultSystemPrompt.
	SystemPrompt string `json:"system_prompt,omitempty"`

	// Provider and Model identify the LLM. Defaults are OpenAI's
	// gpt-4o-mini, matching the templates this replaces.
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`

	// MaxSteps bounds tool-calling rounds. <=0 uses DefaultMaxSteps.
	MaxSteps int `json:"max_steps,omitempty"`

	// Temperature is passed through to the provider. Zero means the
	// provider's own default rather than 0.0.
	Temperature float64 `json:"temperature,omitempty"`

	// Tools are the tools the model may call. Order is the order the model
	// sees them in, and is preserved.
	Tools []Tool `json:"tools,omitempty"`

	// Message is the user's message. Required.
	Message string `json:"message"`
}

// Tool is one tool the model may call, and how to call it.
//
// The model sees Name, Description and Parameters. The remaining fields are
// the dispatch, and they are opaque to the model -- which is the point: the
// same tool list drives DurableCall, PluginCall or a child workflow without
// the model being told which.
type Tool struct {
	// Name is the function name the model calls. Required, unique.
	Name string `json:"name"`

	// Description and Parameters are what the model is shown.
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`

	// Kind is one of KindService, KindPlugin, KindWorkflow.
	Kind string `json:"kind"`

	// Service and Operation dispatch a KindService tool via DurableCall.
	// The service must be registered on the worker with
	// --service-endpoints, or the call fails permanently.
	Service   string `json:"service,omitempty"`
	Operation string `json:"operation,omitempty"`

	// Plugin and Function dispatch a KindPlugin tool via PluginCall.
	Plugin   string `json:"plugin,omitempty"`
	Function string `json:"function,omitempty"`

	// Workflow dispatches a KindWorkflow tool: it is started as a child and
	// awaited.
	Workflow string `json:"workflow,omitempty"`
}

// Defaults. MaxSteps matches the library this replaces, which the issue fixed
// at 10.
const (
	DefaultMaxSteps     = 10
	DefaultProvider     = "openai"
	DefaultModel        = "gpt-4o-mini"
	DefaultSystemPrompt = "You are a helpful AI assistant. Use the tools you are given when they are " +
		"relevant, and explain your reasoning."
)

// ToolCallRecord is one tool call the agent made, for the result.
type ToolCallRecord struct {
	Step   int    `json:"step"`
	Name   string `json:"name"`
	Args   string `json:"arguments,omitempty"`
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Result is the agent workflow's output.
type Result struct {
	Answer    string           `json:"answer"`
	Steps     int              `json:"steps"`
	ToolCalls []ToolCallRecord `json:"tool_calls"`
	Model     string           `json:"model,omitempty"`
	Tokens    int              `json:"total_tokens,omitempty"`
	Cost      float64          `json:"cost,omitempty"`
}

// ---- LLM wire types (the llm plugin's `chat` contract) ----

// Message is one turn of the conversation.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// ToolCall is the model's request to call a tool.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type,omitempty"`
	Function FunctionCall `json:"function"`
}

// FunctionCall is the name and JSON-encoded arguments of a tool call.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// toolSpec is one entry of the `tools` array the model is shown.
type toolSpec struct {
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
}

type toolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type chatRequest struct {
	Provider    string     `json:"provider"`
	Model       string     `json:"model"`
	Messages    []Message  `json:"messages"`
	Temperature float64    `json:"temperature,omitempty"`
	MaxTokens   int        `json:"max_tokens,omitempty"`
	Tools       []toolSpec `json:"tools,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Role      string     `json:"role"`
			Content   string     `json:"content"`
			ToolCalls []ToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
	Cost  float64 `json:"cost"`
	Model string  `json:"model"`
	Error string  `json:"error,omitempty"`
}

// MaxTokens bounds each completion. Fixed rather than configurable for now:
// it is a provider safety valve, not an agent knob.
const maxCompletionTokens = 4096

// Run is the agent workflow.
//
// It is an ordinary cleat workflow entry point in the sense that matters -- it
// takes a HostCalls and makes every external call durably -- but the deployable
// entry point is a thin template function that calls it, because the analyzer
// finds entry points in the workflow's own package.
func Run(h cleat.HostCalls, inputJSON string) (string, error) {
	var in Input
	if err := json.Unmarshal([]byte(inputJSON), &in); err != nil {
		return "", fmt.Errorf("agent: invalid input: %w", err)
	}
	if in.Message == "" {
		return "", fmt.Errorf("agent: input.message is required")
	}
	if in.MaxSteps <= 0 {
		in.MaxSteps = DefaultMaxSteps
	}
	if in.Provider == "" {
		in.Provider = DefaultProvider
	}
	if in.Model == "" {
		in.Model = DefaultModel
	}
	if in.SystemPrompt == "" {
		in.SystemPrompt = DefaultSystemPrompt
	}

	// Lookup by name is a map, but NOTE: it is only ever INDEXED, never
	// iterated. Order comes from in.Tools, which is a slice, because map
	// iteration order differs between runs and replay is positional.
	byName := make(map[string]Tool, len(in.Tools))
	specs := make([]toolSpec, 0, len(in.Tools))
	for _, t := range in.Tools {
		if t.Name == "" {
			return "", fmt.Errorf("agent: every tool needs a name")
		}
		if _, dup := byName[t.Name]; dup {
			return "", fmt.Errorf("agent: tool %q is declared twice", t.Name)
		}
		if err := t.validate(); err != nil {
			return "", err
		}
		byName[t.Name] = t
		specs = append(specs, toolSpec{
			Type: "function",
			Function: toolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}

	messages := []Message{
		{Role: "system", Content: in.SystemPrompt},
		{Role: "user", Content: in.Message},
	}

	res := Result{Model: in.Model}

	for step := 0; step < in.MaxSteps; step++ {
		req := chatRequest{
			Provider:    in.Provider,
			Model:       in.Model,
			Messages:    messages,
			Temperature: in.Temperature,
			MaxTokens:   maxCompletionTokens,
			Tools:       specs,
		}
		reqJSON, err := json.Marshal(req)
		if err != nil {
			return "", fmt.Errorf("agent: marshal llm request: %w", err)
		}

		// THE DURABLE STEP. Recorded in the event history and replayed on
		// resume, so a completed turn is not asked again.
		respJSON, err := h.PluginCall("llm", "chat", string(reqJSON))
		if err != nil {
			return "", fmt.Errorf("agent: llm call failed at step %d: %w", step, err)
		}

		var resp chatResponse
		if err := json.Unmarshal([]byte(respJSON), &resp); err != nil {
			return "", fmt.Errorf("agent: invalid llm response: %w", err)
		}
		if resp.Error != "" {
			return "", fmt.Errorf("agent: llm error: %s", resp.Error)
		}
		if len(resp.Choices) == 0 {
			return "", fmt.Errorf("agent: llm returned no choices")
		}
		if resp.Model != "" {
			res.Model = resp.Model
		}
		res.Tokens += resp.Usage.TotalTokens
		res.Cost += resp.Cost

		// Published as the loop advances, so a poller sees progress rather
		// than only a result. Not part of the durable contract -- the engine
		// writes this on every execution, replay included, and it is
		// last-write-wins.
		h.SetQueryState("agent_step", fmt.Sprintf("%d", step+1))
		h.SetQueryState("agent_tokens", fmt.Sprintf("%d", res.Tokens))
		h.SetQueryState("agent_cost", fmt.Sprintf("%.6f", res.Cost))

		choice := resp.Choices[0]

		if len(choice.Message.ToolCalls) == 0 {
			res.Answer = choice.Message.Content
			res.Steps = step + 1
			return marshal(res)
		}

		messages = append(messages, Message{
			Role:      "assistant",
			Content:   choice.Message.Content,
			ToolCalls: choice.Message.ToolCalls,
		})

		for _, tc := range choice.Message.ToolCalls {
			rec := ToolCallRecord{
				Step: step + 1,
				Name: tc.Function.Name,
				Args: tc.Function.Arguments,
			}
			tool, ok := byName[tc.Function.Name]
			if !ok {
				// A tool the model invented. Reported back to the model
				// rather than failed, so it can correct itself -- and it is
				// recorded, so an operator can see the model doing it.
				rec.Error = fmt.Sprintf("no such tool %q", tc.Function.Name)
			} else {
				out, cerr := callTool(h, tool, tc.Function.Arguments)
				rec.Result = out
				if cerr != nil {
					// Same reasoning as the library this replaces: the model
					// handles a tool error far better than a dead run does.
					rec.Error = cerr.Error()
					rec.Result = "error: " + cerr.Error()
				}
			}
			res.ToolCalls = append(res.ToolCalls, rec)
			messages = append(messages, Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    rec.Result,
			})
		}
	}

	// Exhausted the step budget without a final answer. A stable prefix, so a
	// caller can match on it.
	return "", fmt.Errorf("agent: exceeded max steps (%d)", in.MaxSteps)
}

// validate rejects a tool that cannot be dispatched, at the point the tool is
// declared rather than when the model first calls it. A malformed tool would
// otherwise look like a model failure.
func (t Tool) validate() error {
	switch t.Kind {
	case KindService:
		if t.Service == "" || t.Operation == "" {
			return fmt.Errorf("agent: service tool %q needs service and operation", t.Name)
		}
	case KindPlugin:
		if t.Plugin == "" || t.Function == "" {
			return fmt.Errorf("agent: plugin tool %q needs plugin and function", t.Name)
		}
	case KindWorkflow:
		if t.Workflow == "" {
			return fmt.Errorf("agent: workflow tool %q needs workflow", t.Name)
		}
	default:
		return fmt.Errorf("agent: tool %q has unknown kind %q (want %q, %q or %q)",
			t.Name, t.Kind, KindService, KindPlugin, KindWorkflow)
	}
	return nil
}

// callTool dispatches one tool call. Each branch is a durable host call, so
// the child workflow's history records it the same way it records the LLM
// turn.
func callTool(h cleat.HostCalls, t Tool, args string) (string, error) {
	switch t.Kind {
	case KindService:
		return h.DurableCall(t.Service, t.Operation, args)
	case KindPlugin:
		return h.PluginCall(t.Plugin, t.Function, args)
	case KindWorkflow:
		runID, err := h.ChildWorkflow(t.Workflow, args)
		if err != nil {
			return "", fmt.Errorf("agent: start child %q: %w", t.Workflow, err)
		}
		return h.AwaitChild(runID)
	}
	return "", fmt.Errorf("agent: tool %q has unknown kind %q", t.Name, t.Kind)
}

func marshal(r Result) (string, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("agent: marshal result: %w", err)
	}
	return string(b), nil
}
