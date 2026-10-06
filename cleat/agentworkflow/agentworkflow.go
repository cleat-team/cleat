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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cleat-team/cleat/cleat"
)

// Tool kinds. A tool names one of the three ways a workflow can reach the
// outside world, which is what makes the agent usable without the model ever
// knowing which is which.
const (
	KindService  = "service"
	KindPlugin   = "plugin"
	KindWorkflow = "workflow"

	// KindApproval is a plugin call that is POLLED until it reports a
	// decision. It exists because cleat has no blocking await primitive yet
	// (cleat#2998): a human approval is a wait, and the wait has to be a loop
	// with durable sleeps in it. When the primitive lands, this kind becomes a
	// single call -- see DefaultApprovalPollSeconds.
	KindApproval = "approval"
)

// What a blocked tool call does when the model asks for a tool that does not
// exist, or a tool fails.
const (
	// ToolErrorInject (the default, and what this workflow has always done)
	// hands the error back to the model as the tool's result, so it can
	// correct itself. The library this replaces chose this, and a model
	// handles a tool error far better than a dead run does.
	ToolErrorInject = "inject"

	// ToolErrorFail returns the error to the caller and ends the run. A caller
	// that treats any tool failure as an incident wants this; so does a
	// workflow whose tools are the only untrusted part of it.
	ToolErrorFail = "fail"
)

// Approval polling defaults, matching the example this kind was written for.
const (
	DefaultApprovalPollSeconds = 2
	DefaultApprovalMaxPolls    = 15
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

	// Budget is this run's spend ceiling in dollars, against the `cost` the
	// llm plugin reports. Once the accumulated cost REACHES it the loop stops
	// and the result carries StatusBudgetExceeded. Zero or negative means
	// unbounded.
	//
	// WHAT IT CAN AND CANNOT DO, because the obvious sentence here is false
	// and this field is where a caller would read it: the turn that REACHES
	// the ceiling is always paid for, since a turn's cost is known only after
	// it returns -- the ceiling is reached rather than predicted. What the
	// check stops is the NEXT turn. So a budgeted run can end one turn's cost
	// over its budget, and never two.
	//
	// Checking after the paid call instead spends the same turns and the same
	// money, and moves only the reported Steps; the enforcement lives in the
	// `>=`, not in where the check sits. Both were measured -- see the seam
	// comment below, which says the same thing at the line it describes.
	Budget float64 `json:"budget,omitempty"`

	// TenantID is attribution and nothing else: the workflow echoes it into
	// Result and never requires or interprets it. A caller whose spend
	// boundary must be named -- examples/ai-agent-platform is one -- enforces
	// that before starting the run, because one caller's constraint is not
	// this workflow's contract, and requiring it here would refuse every
	// caller that does not have a tenant.
	TenantID string `json:"tenant_id,omitempty"`

	// ArtifactKey, when non-empty, writes the finished answer to the bundled
	// blobstore plugin under this key and echoes the key into Result. It is
	// written only when the model produced an answer, so a run stopped by its
	// budget writes nothing.
	ArtifactKey string `json:"artifact_key,omitempty"`

	// ToolErrorMode says what a failed tool call -- or a request for a tool
	// that does not exist -- does. Empty means ToolErrorInject.
	//
	// THIS IS NOT A PREFERENCE, IT IS A CONTRACT DIFFERENCE, which is why it is
	// configurable rather than changed. examples/ai-agent-platform asserts
	// that an unknown tool FAILS the run, and this workflow has always fed the
	// error back to the model instead. Both are defensible, they are
	// observably different, and a caller that migrated onto this workflow
	// should not have to discover that its error handling changed by
	// implication.
	ToolErrorMode string `json:"tool_error_mode,omitempty"`
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

	// Kind is one of KindService, KindPlugin, KindWorkflow, KindApproval.
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

	// PollIntervalSeconds and MaxPolls tune a KindApproval tool's wait. A
	// non-positive value takes the Default above.
	//
	// A TIMEOUT IS A RESULT, NOT AN ERROR. Exhausting MaxPolls returns a
	// claim with found=false and timed_out=true to the model, so it can say
	// "nobody approved this" and choose another action -- which is what the
	// example this kind was written for asserts, and the opposite of failing
	// the run.
	PollIntervalSeconds int `json:"poll_interval_seconds,omitempty"`
	MaxPolls            int `json:"max_polls,omitempty"`

	// ArgTransforms and StaticArgs are cleat#3169: KindPlugin and
	// KindApproval otherwise hand the model's own tool-call JSON to the
	// plugin VERBATIM, and once the loop is inside this workflow there is
	// nowhere else for a caller to interpose a translation -- the same
	// structural reason the budget ceiling could not be interposed
	// (cleat#3022's central finding). Both are optional, and a Tool with
	// neither declared dispatches exactly as it always has: the model's
	// JSON, unchanged. See applyArgMapping for the exact merge order.
	ArgTransforms []ArgTransform  `json:"arg_transforms,omitempty"`
	StaticArgs    json.RawMessage `json:"static_args,omitempty"`
}

// ArgTransform copies one field from the model's tool-call arguments to the
// name the plugin or service actually expects, with an optional encoding.
// It is declarative on purpose -- see cleat#3169's issue body for why this
// cannot be a Go callback: Tool crosses the WASM/child-workflow boundary as
// plain JSON, so anything here has to be expressible as data.
type ArgTransform struct {
	// FromField is read from the model's arguments, and then REMOVED unless
	// ToField names the same key -- a transform exists to produce ToField
	// FROM FromField, and leaving an untransformed copy of (say) a
	// plain-text body sitting next to its base64 encoding would defeat the
	// point. If the model did not supply FromField, this transform is
	// silently skipped -- a missing optional argument is not this
	// mechanism's business to enforce; a plugin that requires the field
	// will refuse the call as it always would.
	FromField string `json:"from_field"`

	// ToField is written into what the plugin or service receives.
	ToField string `json:"to_field"`

	// Encoding transforms the value before it is written. "" copies it
	// verbatim. "base64" standard-encodes it, which is cleat#3169's whole
	// motivating case: a plugin whose field is `[]byte` (base64 in JSON,
	// e.g. blobstore's Data) no longer needs the MODEL to produce base64 --
	// the model supplies plain text under FromField, and this does the
	// encode a hand-written tool function used to do in Go.
	Encoding string `json:"encoding,omitempty"`
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
	// Status is the run's outcome: StatusDone when the model answered, or
	// StatusBudgetExceeded when the spend ceiling stopped it first.
	//
	// A budget stop is an OUTCOME, not a failure, which is why it is reported
	// here rather than as an error. A caller must not have to parse an error
	// string to tell "this run spent its budget" from "the LLM broke", and
	// those are the two things a caller of a budgeted agent needs to tell
	// apart.
	Status string `json:"status"`

	Answer    string           `json:"answer"`
	Steps     int              `json:"steps"`
	ToolCalls []ToolCallRecord `json:"tool_calls"`
	Model     string           `json:"model,omitempty"`
	Tokens    int              `json:"total_tokens,omitempty"`

	// Cost is what the run spent, in dollars: input.Budget is measured against
	// this. It is the same quantity the example this replaces calls SpentUSD,
	// under this type's own name -- one field, not two for one number.
	Cost float64 `json:"cost,omitempty"`

	// TenantID echoes Input.TenantID, and ArtifactKey is set only when an
	// artifact was written.
	TenantID    string `json:"tenant_id,omitempty"`
	ArtifactKey string `json:"artifact_key,omitempty"`
}

// Result.Status's only two values.
const (
	StatusDone           = "done"
	StatusBudgetExceeded = "budget_exceeded"
)

// Query-state keys and values a caller reads WHILE a run is in progress,
// through the ordinary query-state mechanism -- not through Result, which is
// only readable once Run has returned. This is cleat#3170, completing the
// approval gate cleat#3022 added: a workflow whose whole point is surviving
// a wait of hours has to be able to say it is waiting, since the alternative
// is a caller inferring it from silence.
//
// STATUS VALUES ARE A PUBLIC VOCABULARY. Once a caller matches on
// QueryStatusAwaitingApproval, changing what gets written there is the same
// kind of break as changing a JSON field name -- treat the strings below as
// fixed once this ships, the same discipline Result.Status already gets.
const (
	// QueryKeyStatus is the key. Its value progresses through every constant
	// below in the run's actual order, ending at StatusDone or
	// StatusBudgetExceeded -- the SAME two values Result.Status reports, so a
	// caller who only ever reads the live key still sees a value Result.Status
	// would agree with once the run finishes.
	QueryKeyStatus = "status"

	QueryStatusRunning          = "running"
	QueryStatusToolCall         = "tool_call"
	QueryStatusThinking         = "thinking"
	QueryStatusAwaitingApproval = "awaiting_approval"
	QueryStatusApprovalReceived = "approval_received"
	QueryStatusApprovalTimeout  = "approval_timeout"
	QueryStatusFailed           = "failed"
)

// Query-state keys written only inside an approval wait (KindApproval),
// and only while that wait is live.
const (
	// QueryKeyApprovalWait is "entered", written ONCE, before the first poll.
	// Not on a later poll: a run approved on its very first poll must still
	// be distinguishable from a run that never reached the wait at all, which
	// is exactly the thing a "the run finished" check alone cannot tell apart
	// -- see run-ai-agent-platform-scenario.sh's own comment on why it checks
	// this separately from the run's completion.
	QueryKeyApprovalWait = "approval_wait"

	// QueryKeyApprovalWaitSince is an RFC3339 timestamp, written at the same
	// point as QueryKeyApprovalWait. Taken from h.Now(), the workflow's
	// durable clock, never time.Now() -- this executes inside the loop and
	// must replay identically, and the package comment's determinism
	// constraint applies here exactly as it does to everything else in it.
	QueryKeyApprovalWaitSince = "approval_wait_since"

	// QueryKeyApprovalPolls is the 1-based count of misses so far, updated on
	// every miss. Unset (never written) for a run approved on its first poll
	// -- that is not a bug to fix, it is the signal that distinguishes "hit
	// immediately" from "missed N times then hit".
	QueryKeyApprovalPolls = "approval_polls"
)

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

	// A tool error is a CONTRACT choice, so an unrecognised mode is refused
	// rather than silently treated as the default: a caller that asked for
	// ToolErrorFail and got injection would see its run continue past a
	// failure it meant to stop on.
	failOnToolError := false
	switch in.ToolErrorMode {
	case "", ToolErrorInject:
	case ToolErrorFail:
		failOnToolError = true
	default:
		return "", fmt.Errorf("agent: unknown tool_error_mode %q (want %q or %q)",
			in.ToolErrorMode, ToolErrorInject, ToolErrorFail)
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

	res := Result{Model: in.Model, TenantID: in.TenantID}
	h.SetQueryState(QueryKeyStatus, QueryStatusRunning)

	// fail writes the one QueryStatusFailed value shared by every error return
	// below, so every one of them stays in sync rather than each repeating the
	// write and risking one that forgets it. Not used for the three input
	// errors above this point: this run has not yet announced itself as
	// QueryStatusRunning, so there is nothing for a poller to have been
	// reading that "failed" would correct.
	fail := func(err error) (string, error) {
		h.SetQueryState(QueryKeyStatus, QueryStatusFailed)
		return "", err
	}

	for step := 0; step < in.MaxSteps; step++ {
		// THE SEAM. The ceiling is enforced before a turn is STARTED, so a run
		// that has already spent its budget does not begin another one.
		//
		// THE COMPARISON IS `>=`, AND THAT IS THE ENFORCEMENT. With `>` the
		// loop makes one more paid call past the ceiling -- measured at $2.00
		// against a $1.50 budget by the test that pins this.
		//
		// WHAT THE CEILING CANNOT DO, stated because the obvious sentence here
		// is wrong: the turn that REACHES the ceiling is unavoidable. A turn's
		// cost is known only after it returns, so the crossing turn is always
		// paid for and the ceiling is reached rather than predicted. What is
		// enforced is that nothing is spent AFTER it is reached -- so a
		// budgeted run can end one turn's cost over its budget, and never two.
		//
		// res.Cost starts at zero, so a budget of zero means "unbounded"
		// (see Input.Budget), never "do nothing".
		if in.Budget > 0 && res.Cost >= in.Budget {
			res.Status = StatusBudgetExceeded
			res.Steps = step
			h.SetQueryState(QueryKeyStatus, res.Status)
			out, merr := marshal(res)
			if merr != nil {
				return fail(merr)
			}
			return out, nil
		}
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
			return fail(fmt.Errorf("agent: marshal llm request: %w", err))
		}

		// THE DURABLE STEP. Recorded in the event history and replayed on
		// resume, so a completed turn is not asked again.
		respJSON, err := h.PluginCall("llm", "chat", string(reqJSON))
		if err != nil {
			return fail(fmt.Errorf("agent: llm call failed at step %d: %w", step, err))
		}

		var resp chatResponse
		if err := json.Unmarshal([]byte(respJSON), &resp); err != nil {
			return fail(fmt.Errorf("agent: invalid llm response: %w", err))
		}
		if resp.Error != "" {
			return fail(fmt.Errorf("agent: llm error: %s", resp.Error))
		}
		if len(resp.Choices) == 0 {
			return fail(fmt.Errorf("agent: llm returned no choices"))
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
			res.Status = StatusDone
			if in.ArtifactKey != "" {
				if err := writeArtifact(h, in.ArtifactKey, res.Answer); err != nil {
					return fail(err)
				}
				res.ArtifactKey = in.ArtifactKey
			}
			h.SetQueryState(QueryKeyStatus, res.Status)
			out, merr := marshal(res)
			if merr != nil {
				return fail(merr)
			}
			return out, nil
		}

		messages = append(messages, Message{
			Role:      "assistant",
			Content:   choice.Message.Content,
			ToolCalls: choice.Message.ToolCalls,
		})

		for _, tc := range choice.Message.ToolCalls {
			h.SetQueryState(QueryKeyStatus, QueryStatusToolCall)
			rec := ToolCallRecord{
				Step: step + 1,
				Name: tc.Function.Name,
				Args: tc.Function.Arguments,
			}
			tool, ok := byName[tc.Function.Name]
			if !ok {
				// A tool the model invented. Reported back to the model
				// rather than failed, so it can correct itself -- and it is
				// recorded, so an operator can see the model doing it. Under
				// ToolErrorFail it ends the run instead.
				//
				// rec.Result IS SET, not only rec.Error, and that is a fix
				// rather than tidiness: Message.Content carries `omitempty`, so
				// leaving Result empty sent the model a `role: tool` message
				// with NO content at all. The error was recorded in the result
				// struct -- which the CALLER reads, and which
				// TestAnUnknownToolIsReportedToTheModel asserted -- while the
				// model was told nothing and could not distinguish "that tool
				// returned empty" from "there is no such tool". The failing-tool
				// branch below always did set it, so the two disagreed.
				msg := fmt.Sprintf("no such tool %q", tc.Function.Name)
				if failOnToolError {
					return fail(fmt.Errorf("agent: %s", msg))
				}
				rec.Error = msg
				rec.Result = "error: " + msg
			} else {
				out, cerr := callTool(h, tool, tc.Function.Arguments)
				rec.Result = out
				if cerr != nil {
					// Same reasoning as the library this replaces: the model
					// handles a tool error far better than a dead run does.
					// ToolErrorFail is the caller saying it disagrees.
					if failOnToolError {
						return fail(fmt.Errorf("agent: tool %q failed: %w", tc.Function.Name, cerr))
					}
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

		// Every tool call this step is resolved (an approval tool included --
		// awaitApproval has already returned, found or timed out, by the time
		// control reaches here). The model's next turn is what happens after
		// this status, so QueryStatusThinking is the honest description of
		// what the run is doing until the next llm.chat call lands.
		h.SetQueryState(QueryKeyStatus, QueryStatusThinking)
	}

	// Exhausted the step budget without a final answer. A stable prefix, so a
	// caller can match on it.
	return fail(fmt.Errorf("agent: exceeded max steps (%d)", in.MaxSteps))
}

// ChildName is the workflow name the agent must be DEPLOYED under for
// RunAsChild to find it. A child is resolved by name against a deployed
// workflow_defs row (the worker loads its bytes by def_name), so this is a
// deployment contract, not a convention.
const ChildName = "agent"

// RunAsChild is the `run_agent(config, message)` wrapper the SDKs expose: it
// starts the agent as a child workflow and awaits it.
//
// THIS IS THE WHOLE PER-LANGUAGE SURFACE. Everything else -- the loop, the tool
// dispatch, the step budget, the durability -- lives in the workflow, which is
// why the same agent works from Go, Python, Rust, Java or AssemblyScript
// without any of them implementing it. Before cleat#1983 each language
// reimplemented the loop, and two of those implementations were untested.
//
// The caller's workflow needs the agent deployed under ChildName. A deployment
// under a different name means calling h.ChildWorkflow directly with the
// marshalled Input -- two lines, and the reason this is a convenience rather
// than a mechanism.
func RunAsChild(h cleat.HostCalls, cfg Input, message string) (Result, error) {
	cfg.Message = message
	payload, err := json.Marshal(cfg)
	if err != nil {
		return Result{}, fmt.Errorf("agent: marshal config: %w", err)
	}

	runID, err := h.ChildWorkflow(ChildName, string(payload))
	if err != nil {
		return Result{}, fmt.Errorf("agent: start child %q: %w", ChildName, err)
	}
	out, err := h.AwaitChild(runID)
	if err != nil {
		return Result{}, fmt.Errorf("agent: await child %q: %w", ChildName, err)
	}

	var res Result
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		return Result{}, fmt.Errorf("agent: decode child result: %w", err)
	}
	return res, nil
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
	case KindApproval:
		if t.Plugin == "" || t.Function == "" {
			return fmt.Errorf("agent: approval tool %q needs plugin and function to poll", t.Name)
		}
	default:
		return fmt.Errorf("agent: tool %q has unknown kind %q (want %q, %q, %q or %q)",
			t.Name, t.Kind, KindService, KindPlugin, KindWorkflow, KindApproval)
	}

	// Validated at declaration time, not at first dispatch, same reasoning
	// as every branch above: a malformed tool should not look like a model
	// failure three turns later.
	for _, xf := range t.ArgTransforms {
		if xf.FromField == "" || xf.ToField == "" {
			return fmt.Errorf("agent: tool %q has an arg_transform missing from_field or to_field", t.Name)
		}
		switch xf.Encoding {
		case "", "base64":
		default:
			return fmt.Errorf("agent: tool %q has an arg_transform with unknown encoding %q (want \"\" or %q)",
				t.Name, xf.Encoding, "base64")
		}
	}
	if len(t.StaticArgs) > 0 {
		var static map[string]any
		if err := json.Unmarshal(t.StaticArgs, &static); err != nil {
			return fmt.Errorf("agent: tool %q has static_args that is not a JSON object: %w", t.Name, err)
		}
	}
	return nil
}

// applyArgMapping returns the plugin/service-facing arguments for a KindPlugin
// or KindApproval tool call: the model's own JSON, with each of
// t.ArgTransforms copied in (CONSUMING its source field -- see below), then
// t.StaticArgs merged over everything last (so a static field always wins a
// collision, with either the model's own argument or a transform's output).
// A Tool with neither set returns args completely unchanged -- not even
// round-tripped through JSON -- so an existing caller's behaviour, and the
// exact bytes any existing test asserts on, are untouched. See cleat#3169.
func applyArgMapping(t Tool, args string) (string, error) {
	if len(t.ArgTransforms) == 0 && len(t.StaticArgs) == 0 {
		return args, nil
	}

	out := map[string]any{}
	if args != "" {
		if err := json.Unmarshal([]byte(args), &out); err != nil {
			return "", fmt.Errorf("agent: tool %q: model arguments are not a JSON object: %w", t.Name, err)
		}
	}

	for _, xf := range t.ArgTransforms {
		raw, ok := out[xf.FromField]
		if !ok {
			// Missing is not this mechanism's business to enforce -- see
			// ArgTransform's doc comment. A plugin that requires the field
			// refuses the call as it always would.
			continue
		}
		switch xf.Encoding {
		case "":
			out[xf.ToField] = raw
		case "base64":
			s, ok := raw.(string)
			if !ok {
				return "", fmt.Errorf("agent: tool %q: arg_transform to %q wants base64 encoding, but the model's %q was not a string",
					t.Name, xf.ToField, xf.FromField)
			}
			out[xf.ToField] = base64.StdEncoding.EncodeToString([]byte(s))
		}
		// CONSUME the source field: a transform exists to produce ToField
		// FROM FromField, and leaving the untransformed original sitting
		// alongside it defeats the point for exactly the case this issue is
		// about -- save_report's whole reason for existing is that the
		// plugin should never see an unencoded copy of the body next to the
		// encoded one. Skipped when they are the same key, so a transform
		// that merely relabels a field in place does not delete what it just
		// wrote.
		if xf.FromField != xf.ToField {
			delete(out, xf.FromField)
		}
	}

	if len(t.StaticArgs) > 0 {
		var static map[string]any
		if err := json.Unmarshal(t.StaticArgs, &static); err != nil {
			// validate() already checked this at declaration time; a tool
			// reaching dispatch with invalid static_args would mean validate
			// was bypassed, not that the model did anything wrong.
			return "", fmt.Errorf("agent: tool %q: static_args is not a JSON object: %w", t.Name, err)
		}
		for k, v := range static {
			out[k] = v
		}
	}

	b, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("agent: tool %q: marshal mapped arguments: %w", t.Name, err)
	}
	return string(b), nil
}

// writeArtifact puts the run's answer in the bundled blobstore plugin, in the
// shape that plugin's `put` takes: {"key": ..., "data": <base64>}. It is the
// same call examples/ai-agent-platform's saveReport makes, so an artifact
// written by a migrated example lands under the same contract it did before.
//
// A failed write is an ERROR rather than a warning, and deliberately: the
// caller asked for an artifact, so a run that reported success while the
// artifact was missing would be the worse outcome of the two.
func writeArtifact(h cleat.HostCalls, key, body string) error {
	req, err := json.Marshal(map[string]string{
		"key":  key,
		"data": base64.StdEncoding.EncodeToString([]byte(body)),
	})
	if err != nil {
		return fmt.Errorf("agent: marshal artifact for %q: %w", key, err)
	}
	if _, err := h.PluginCall("blobstore", "put", string(req)); err != nil {
		return fmt.Errorf("agent: write artifact %q: %w", key, err)
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
		mapped, err := applyArgMapping(t, args)
		if err != nil {
			return "", err
		}
		return h.PluginCall(t.Plugin, t.Function, mapped)
	case KindWorkflow:
		runID, err := h.ChildWorkflow(t.Workflow, args)
		if err != nil {
			return "", fmt.Errorf("agent: start child %q: %w", t.Workflow, err)
		}
		return h.AwaitChild(runID)
	case KindApproval:
		mapped, err := applyArgMapping(t, args)
		if err != nil {
			return "", err
		}
		return awaitApproval(h, t, mapped)
	}
	return "", fmt.Errorf("agent: tool %q has unknown kind %q", t.Name, t.Kind)
}

// awaitApproval polls a plugin function until it reports a decision.
//
// THE FUNCTION IS CLAIM-SHAPED: its JSON carries a boolean `found`, and the
// decision itself is whatever else the payload holds. That split is deliberate
// and it is the same one WS-1's blocking primitive uses (cleat#2998) -- the
// callee reports whether it has an answer, never what the answer MEANS, because
// the meaning belongs to the caller. It is also what makes the swap to that
// primitive a deletion rather than a redesign: the loop goes, the shape stays.
//
// A TIMEOUT IS A RESULT. Exhausting the polls returns a claim with
// found=false and timed_out=true, which the model receives as an ordinary tool
// result and can reason about -- an agent that waits forever on a human who has
// gone home is a held resource, and an agent whose run FAILS on a timeout
// cannot say "nobody approved this" and pick another action.
func awaitApproval(h cleat.HostCalls, t Tool, args string) (string, error) {
	interval := t.PollIntervalSeconds
	if interval <= 0 {
		interval = DefaultApprovalPollSeconds
	}
	maxPolls := t.MaxPolls
	if maxPolls <= 0 {
		maxPolls = DefaultApprovalMaxPolls
	}

	// Written ONCE, before the first poll -- cleat#3170. A run that hits this
	// line has genuinely entered the wait, which is the one thing "the run
	// finished" cannot tell a caller: a run that never called an approval
	// tool at all also finishes, and would otherwise look identical from the
	// outside to one that was approved instantly.
	h.SetQueryState(QueryKeyStatus, QueryStatusAwaitingApproval)
	h.SetQueryState(QueryKeyApprovalWait, "entered")
	// h.Now(), not time.Now(): this runs inside the durable loop and must
	// replay to the same value every time, which only the workflow's own
	// clock guarantees -- see the package comment's determinism constraint.
	h.SetQueryState(QueryKeyApprovalWaitSince, h.Now().UTC().Format(time.RFC3339))

	for poll := 0; poll < maxPolls; poll++ {
		out, err := h.PluginCall(t.Plugin, t.Function, args)
		if err != nil {
			// A plugin that cannot be REACHED is different from one that has
			// no answer yet, and it is an error either way -- polling a broken
			// plugin only spends the wait budget.
			return "", fmt.Errorf("agent: approval tool %q: %w", t.Name, err)
		}
		var claim struct {
			Found bool `json:"found"`
		}
		if err := json.Unmarshal([]byte(out), &claim); err != nil {
			return "", fmt.Errorf("agent: approval tool %q returned unparsable JSON %q: %w", t.Name, out, err)
		}
		if claim.Found {
			h.SetQueryState(QueryKeyStatus, QueryStatusApprovalReceived)
			return out, nil
		}
		// approval_polls is the 1-based MISS count, so a run approved on its
		// first poll never writes it -- that absence is the signal that
		// distinguishes "hit immediately" from "missed once then hit", the
		// same reasoning approval_wait's unconditional write above exists for.
		h.SetQueryState(QueryKeyApprovalPolls, fmt.Sprintf("%d", poll+1))
		// Sleep only BETWEEN polls: a last sleep after the final miss would
		// add the interval to every timeout for nothing.
		if poll < maxPolls-1 {
			h.DurableSleep(time.Duration(interval) * time.Second)
		}
	}
	h.SetQueryState(QueryKeyStatus, QueryStatusApprovalTimeout)
	return `{"found":false,"timed_out":true}`, nil
}

func marshal(r Result) (string, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("agent: marshal result: %w", err)
	}
	return string(b), nil
}
