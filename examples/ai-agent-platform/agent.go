// AI agent platform — a durable agent loop with a per-run spend ceiling.
//
// This is the reference implementation behind docs/playbooks/ai-agent-platform.md:
// the CLEAT-SIDE half of an agent platform. Real and runnable end to end on a
// fresh checkout with nothing installed and no API key.
//
// **The rope half is three things, and they are stubbed differently.**
//
//   - **The model provider** is a local OpenAI-compatible stub. The call to it
//     is a genuine durable host call to the bundled `llm` plugin
//     (`plugins/llm`), configured with `base_url` at the stub — so cleat's half
//     of the round trip is real and the only stub is what answers. This is the
//     same shape as examples/integration-hub's `sink`, and for the same reason:
//     swapping in the real provider is a config line, not a rewrite.
//
//   - **The retrieval store** is canned documents inside this file. It is not a
//     local HTTP stub like the model, and searchDocs gives both reasons — the
//     second of which is a defect worth knowing about: **cleat ships no
//     retrieval plugin that could serve it.** `plugins/pgvector` exists with
//     `search`/`upsert`/`delete` and is deliberately NOT blank-imported into
//     cleat-worker, because its Migrations() creates an `embedding vector(1536)`
//     column and plugin.RunMigrations is FATAL at boot — so linking it would
//     stop the worker starting on any PostgreSQL without the extension. The
//     playbook's assembly table lists pgvector as this row with no such
//     warning; the README says so.
//
//   - **The eval harness** does not exist in cleat at all and is not faked here.
//
// What this exists to show, and it is one property rather than four:
//
//   - **A replayed agent run does not re-ask the model.** `llm.chat` is
//     registered with neither Idempotent nor SameValueOnReplay
//     (`plugins/llm/host_functions.go`), so on replay the engine returns the
//     RECORDED answer without re-invoking. That is simultaneously the cost
//     control, the determinism guarantee and the audit trail. The scenario
//     SIGKILLs the worker mid-loop and asserts, on resume, that the model stub
//     saw exactly one request per loop step — never a re-ask. See
//     scripts/run-ai-agent-platform-scenario.sh.
//
// The four hitch points (docs/playbooks/ai-agent-platform.md, "The assembly"):
//
//   - **host functions** — the model call (`llm.chat`), the artifact write
//     (`blobstore.put`) and the approval wait (`eventtriggers.await_event`)
//   - **HTTP routes** — `POST /api/events/publish`, where a human approval
//     arrives, plus `GET /blobs` for what the agent produced
//   - **edge middleware** — `ratelimiter`, limiting new runs per tenant
//   - **background loop** — `ratelimiter`'s reload, which reads every tenant's
//     limits with `plugin.AcrossAllTenants` by name
//
// Build:
//
//	cleat build -o /tmp/out ./examples/ai-agent-platform/
package aiagentplatform

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cleat-team/cleat/cleat"
)

// h is the package-level context object. The transformer auto-threads it into
// every function in the durable closure that references it — which is why the
// helpers below take domain values and not a HostCalls.
var h cleat.HostCalls

const (
	// SettleDelayMs is the window the scenario kills the worker inside.
	//
	// IT IS NOT DECORATION AND IT IS NOT A RETRY DELAY. A crash-resume
	// assertion needs a deterministic moment at which to crash, and without a
	// durable step between the last model call and the end of the run there is
	// no window wide enough to hit reliably -- the run would finish first. A
	// durable sleep makes the window explicit and the assertion reproducible.
	SettleDelayMs = 30_000

	// ApprovalPollMs is the gap between polls of the approval wait. See
	// awaitApproval for why the wait is a loop at all.
	ApprovalPollMs = 2_000

	// MaxApprovalPolls bounds the wait. An agent that waits forever on a human
	// who has gone home is a held resource in a conventional harness and a
	// free row here, but a run that never terminates is still a run that never
	// terminates -- the ceiling is what makes "waiting" distinguishable from
	// "stuck" in the run list.
	MaxApprovalPolls = 15
)

// ---- Domain types ----

// Message is one entry in the conversation the model sees. The field names are
// the OpenAI wire format's, because that is what `llm.chat` forwards.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

// LLMRequest is the input to `llm.chat`.
//
// APIKey is deliberately absent, and that is the scenario's most important
// configuration decision rather than an omission. A per-tenant key is supplied
// through the PLUGIN's provider config (`llm.providers.<provider>.api_key`,
// a deployment secret), not per call, because a key in a request body is a key
// in an event history. The README carries the reasoning; cleat#1988 is the
// defect that made it so, and cleat#2043 refuses a literal here outright.
type LLMRequest struct {
	Provider    string    `json:"provider"`
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Tools       []Tool    `json:"tools,omitempty"`
}

// LLMResponse is what `llm.chat` returns. Cost and Usage are the spend inputs:
// the loop accumulates them, which is what makes the ceiling enforceable.
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

type AgentInput struct {
	// TenantID is the spend boundary. It is carried here rather than inferred
	// because the whole premise of the scenario is that one customer's spend is
	// bounded without bounding another's, and a boundary you cannot name in the
	// request is a boundary you cannot test.
	TenantID string `json:"tenant_id"`

	Task     string `json:"task"`
	MaxSteps int    `json:"max_steps"`

	// BudgetUSD is THIS RUN's ceiling, in dollars. The loop checks it before
	// every paid call, so the failure mode is "the run stopped and said why"
	// rather than "the invoice arrived".
	//
	// It is per-run and not cumulative, and the README says so plainly: a
	// cumulative per-tenant ceiling needs an accumulator that survives the run,
	// and cleat has no token or cost metric (IMPROVEMENT-PLAN, and
	// docs/playbooks/ai-agent-platform.md "What you still have to build",
	// item 1). What this file demonstrates is the ENFORCEMENT POINT -- the
	// check that sits between the model's tool request and the next paid call
	// -- which is the half that has a wrong answer you cannot correct later.
	BudgetUSD float64 `json:"budget_usd"`

	// ArtifactKey is where the agent's finished output is written, through the
	// bundled `blobstore` plugin.
	ArtifactKey string `json:"artifact_key"`
}

type AgentOutput struct {
	TenantID string `json:"tenant_id"`

	// Status is the RUN's outcome, and it has exactly two values. There is no
	// `approval_denied`: a denial is a tool RESULT, handed back to the model so
	// it can choose another action, and the run continues to `done`. An earlier
	// version of this comment listed a state nothing writes -- and it read as
	// though the run could END on a denial, which is the opposite of what
	// requestApproval does.
	//
	// `awaiting_approval`, `approval_received` and `approval_timeout` are NOT
	// values of this field: they are QUERY STATE, written while the run is in
	// flight, and this field is only ever read once the run has finished.
	Status string `json:"status"` // done | budget_exceeded

	Output      string   `json:"output"`
	Steps       int      `json:"steps"`
	Tokens      int      `json:"total_tokens"`
	SpentUSD    float64  `json:"spent_usd"`
	BudgetUSD   float64  `json:"budget_usd"`
	ToolsUsed   []string `json:"tools_used"`
	ArtifactKey string   `json:"artifact_key,omitempty"`
}

// ---- Entry point ----

// RunAgent runs one agent task to completion, or to a ceiling.
//
// The result is a JSON string rather than a struct: a WASM entry point hands
// back bytes, and string is the one shape every language SDK expresses
// identically (IMPROVEMENT-PLAN 3.228).
//
// THE INPUT IS A STRING, NOT AN AgentInput, and the binding rule is why
// (`wasm/exports.go`; cleat#824): an entry point with exactly one `string`
// parameter receives the whole input JSON verbatim, and every other shape binds
// by Go parameter NAME. examples/order-lifecycle/order.go has the long form and
// a real worker failing the other way.
func RunAgent(h cleat.HostCalls, input string) (string, error) {
	var in AgentInput
	if err := json.Unmarshal([]byte(input), &in); err != nil {
		return "", fmt.Errorf("decode agent input: %w", err)
	}
	if in.TenantID == "" {
		return "", fmt.Errorf("tenant_id is required; the spend ceiling is per tenant and this run cannot be attributed without one")
	}
	if in.Task == "" {
		return "", fmt.Errorf("task is required")
	}
	if in.MaxSteps <= 0 {
		in.MaxSteps = 8
	}
	if in.BudgetUSD <= 0 {
		in.BudgetUSD = 0.05
	}

	h.SetQueryState("tenant_id", in.TenantID)
	h.SetQueryState("status", "running")
	h.SetQueryState("budget_usd", fmt.Sprintf("%.6f", in.BudgetUSD))

	conversation := []Message{
		{Role: "system", Content: systemPrompt()},
		{Role: "user", Content: in.Task},
	}

	var (
		spent     float64
		tokens    int
		toolsUsed []string
		steps     int
		status    = "done"
		output    string
	)

	for step := 0; step < in.MaxSteps; step++ {
		// ---- 1. The ceiling ----
		//
		// `>=` and not `>`, and the difference is a whole call: at $0.01 a
		// call against a $0.03 budget, `>` makes a fourth. The comparison is
		// what the scenario's budget test asserts, by counting the model's
		// calls rather than reading the reported status.
		//
		// WHERE THE CHECK SITS WITHIN THE ITERATION IS NOT OBSERVABLE, and it
		// is worth saying rather than implying otherwise: a call's cost is only
		// known once it returns, so a check at the foot of the loop refuses the
		// next call at exactly the same spend as a check at the head of it.
		// What is load-bearing is that the ceiling is applied INSIDE the loop
		// at all -- without it the run proceeds to MaxSteps and the ceiling
		// becomes a report rather than a bound.
		if spent >= in.BudgetUSD {
			status = "budget_exceeded"
			output = fmt.Sprintf(
				"stopped before step %d: this run has spent $%.6f of its $%.6f ceiling",
				step+1, spent, in.BudgetUSD)
			h.SetQueryState("status", status)
			h.SetQueryState("stopped_reason", "budget")
			h.Log("agent stopped at its ceiling",
				"tenant_id", in.TenantID, "spent_usd", spent, "budget_usd", in.BudgetUSD)
			break
		}

		// ---- 2. The model call — THE RECORDED PAID CALL ----
		//
		// `llm.chat` declares neither Idempotent nor SameValueOnReplay, so on
		// replay the engine returns the answer recorded here and does NOT ask
		// the model again. This single line is the scenario's claim: the
		// scenario kills the worker after it and asserts the stub's request
		// count did not go up on resume.
		resp, err := callModel(in, conversation)
		if err != nil {
			h.SetQueryState("status", "failed")
			h.SetQueryState("failed_step", "call_model")
			return "", fmt.Errorf("tenant %s: model call: %w", in.TenantID, err)
		}
		steps++
		spent += resp.Cost
		tokens += resp.Usage.TotalTokens

		// Query state is how the UI watches a run that may wait for hours
		// without reading event history. It is written every step, so the last
		// write before a crash is the one the page shows.
		h.SetQueryState("steps", fmt.Sprintf("%d", steps))
		h.SetQueryState("spent_usd", fmt.Sprintf("%.6f", spent))
		h.SetQueryState("total_tokens", fmt.Sprintf("%d", tokens))

		if len(resp.Choices) == 0 {
			return "", fmt.Errorf("model returned no choices at step %d", step+1)
		}
		choice := resp.Choices[0]

		// ---- 3. No tool call: the model has answered ----
		if len(choice.Message.ToolCalls) == 0 {
			output = choice.Message.Content
			break
		}

		// ---- 4. Tool calls ----
		conversation = append(conversation, Message{
			Role:      "assistant",
			Content:   choice.Message.Content,
			ToolCalls: choice.Message.ToolCalls,
		})

		for _, tc := range choice.Message.ToolCalls {
			h.SetQueryState("status", "tool_call")
			h.SetQueryState("current_tool", tc.Function.Name)

			result, err := runTool(in, tc)
			if err != nil {
				h.SetQueryState("status", "failed")
				h.SetQueryState("failed_step", "tool:"+tc.Function.Name)
				return "", fmt.Errorf("tenant %s: tool %s: %w", in.TenantID, tc.Function.Name, err)
			}
			toolsUsed = append(toolsUsed, tc.Function.Name)

			// A tool result is fed back as its OWN message rather than
			// appended to the conversation as prose, because that is the shape
			// the model was trained on -- a tool result folded into a user turn
			// reads to the model as the user having said it.
			conversation = append(conversation, Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    result,
			})
		}

		h.SetQueryState("status", "thinking")
	}

	// ---- 5. Settle ----
	//
	// A durable sleep, so the crash window exists and so the resume point is
	// unambiguous. Recorded, so a worker that dies here resumes PAST it rather
	// than repeating it.
	h.DurableSleepMs(SettleDelayMs)

	if in.ArtifactKey != "" && status == "done" && output != "" {
		if err := saveArtifact(in, output); err != nil {
			h.SetQueryState("status", "failed")
			h.SetQueryState("failed_step", "save_artifact")
			return "", fmt.Errorf("tenant %s: saving the artifact: %w", in.TenantID, err)
		}
	}

	h.SetQueryState("status", status)
	out := AgentOutput{
		TenantID:    in.TenantID,
		Status:      status,
		Output:      output,
		Steps:       steps,
		Tokens:      tokens,
		SpentUSD:    spent,
		BudgetUSD:   in.BudgetUSD,
		ToolsUsed:   toolsUsed,
		ArtifactKey: in.ArtifactKey,
	}
	return mustJSON(out)
}

// ---- Steps ----

// callModel is the paid call. Everything the scenario claims rests on this
// being `llm.chat` and not an HTTP request the workflow makes itself: a fetch
// is recorded too, but the plugin is where the per-tenant key, the provider
// failover and the two replay properties live.
func callModel(in AgentInput, conversation []Message) (LLMResponse, error) {
	req := LLMRequest{
		Provider:    "openai",
		Model:       "agent-stub-1",
		Messages:    conversation,
		Temperature: 0.2,
		MaxTokens:   1024,
		Tools:       availableTools(),
	}
	reqJSON, err := json.Marshal(req)
	if err != nil {
		return LLMResponse{}, fmt.Errorf("marshal model request: %w", err)
	}

	respJSON, err := h.PluginCall("llm", "chat", string(reqJSON))
	if err != nil {
		return LLMResponse{}, fmt.Errorf("llm.chat: %w", err)
	}

	var resp LLMResponse
	if err := json.Unmarshal([]byte(respJSON), &resp); err != nil {
		return LLMResponse{}, fmt.Errorf("decode model response: %w", err)
	}
	if resp.Error != "" {
		return LLMResponse{}, fmt.Errorf("provider error: %s", resp.Error)
	}
	return resp, nil
}

// runTool dispatches one tool call. The three tools are chosen to be the three
// SHAPES an agent platform needs, not three variations of one:
//
//	search_docs   -- a read against a store that is NOT cleat's (rope)
//	save_report   -- a write through a bundled plugin (host function, real)
//	request_approval -- a wait for a human (the durable pause)
func runTool(in AgentInput, tc ToolCall) (string, error) {
	switch tc.Function.Name {
	case "search_docs":
		return searchDocs(in, tc.Function.Arguments)
	case "save_report":
		return saveReport(tc.Function.Arguments)
	case "request_approval":
		return requestApproval(tc.Function.Arguments)
	default:
		// A tool the model invented is REPORTED, not silently ignored. The
		// model is told what happened and gets to try again, which is how a
		// stub-driven test surfaces a prompt that asks for a tool that is not
		// registered -- silently returning "" would look like a successful
		// no-op search.
		return "", fmt.Errorf("unknown tool %q; available: %s",
			tc.Function.Name, toolNames())
	}
}

// searchDocs is the RETRIEVAL step, and it is the rope one.
//
// **It reads a canned set out of this file rather than calling anything, and
// that is a decision rather than a shortcut.** The reader's document store is
// theirs to supply; what the scenario owes them is the SHAPE of the seam, and
// the shape is "a function that takes a query and returns passages". Replace
// its body and nothing else changes.
//
// WHY NOT A LOCAL HTTP STUB, which is what the model provider gets. Two
// reasons, and the first is the one worth knowing:
//
//  1. **A workflow's own fetch is refused a private address, and unlike a
//     plugin call it has no flag of its own to permit one.**
//     `--plugin-egress-allow-private` is explicitly plugin-only ("a guest is
//     code cleat did not write, and nothing it supplies should reach a private
//     address whatever is configured here" -- cmd/cleat-worker/config.go). A
//     guest fetch to a private host is reachable only through the DEPLOYMENT
//     allowlist, `--egress-allowlist`, and only when the tenant's own list
//     permits it as well: egress needs BOTH. That is two extra moving parts
//     for a stub whose whole job is to stand still, and the pinned behaviour is
//     already covered by
//     engine/a_guest_cannot_reach_the_hosts_own_network_test.go.
//
//  2. **cleat ships no retrieval plugin that could serve it anyway.** See the
//     package comment for the pgvector reason. So the choice is not "stub it
//     over HTTP or use cleat's" -- there is no cleat's today.
//
// The DOCUMENTS are constant and the retrieval is not recorded as an event,
// which has one consequence a reader should not be surprised by: unlike every
// other step in this loop, a replay re-runs this function. That is safe here
// because it is pure, and it would NOT be safe against a real index -- see the
// playbook's "a paused run resumes against stale retrieval", which is the
// failure mode a real store reintroduces the moment this becomes a fetch.
func searchDocs(in AgentInput, args string) (string, error) {
	var q struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(args), &q); err != nil {
		return "", fmt.Errorf("decode search_docs arguments: %w", err)
	}
	if q.Query == "" {
		return "", fmt.Errorf("search_docs: query is required")
	}

	// A real store returns ranked passages with scores and source ids. The
	// shape is kept so a reader swapping in their own sees the same contract.
	docs := []map[string]any{
		{
			"id":    "runbook/incidents",
			"score": 0.91,
			"text":  "Two incidents were opened yesterday, both due to the same upstream timeout. Both were resolved by a rollback.",
		},
		{
			"id":    "runbook/rollback",
			"score": 0.77,
			"text":  "A rollback is the documented remedy for an upstream timeout that affects more than one service.",
		},
	}
	return mustJSON(map[string]any{"query": q.Query, "documents": docs})
}

// saveReport is a WRITE through a bundled plugin, so the scenario has a tool
// whose side effect is real and countable.
func saveReport(args string) (string, error) {
	var a struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return "", fmt.Errorf("decode save_report arguments: %w", err)
	}
	req, err := json.Marshal(map[string]any{
		"key":  "report",
		"data": base64.StdEncoding.EncodeToString([]byte(a.Body)),
	})
	if err != nil {
		return "", err
	}
	return h.PluginCall("blobstore", "put", string(req))
}

// requestApproval is THE HUMAN WAIT, and the loop is not optional.
//
// `await_event`'s own doc comment says a `found:false` result means "the
// workflow engine will retry according to its retry policy"
// (plugins/eventtriggers/host_functions.go). **Measured against a real worker
// it does not**, and examples/integration-hub records the same finding for
// `await_webhook`: a call that SUCCEEDS is not retried, whatever it returned --
// the retry policy is on the durable call, and the durable call succeeded.
// Returning an error to force the wait fails the run instead.
//
// So the wait is a loop with a durable sleep in it. THIS IS THE POLLING SHAPE
// cleat#2522 is about: the branch where the first poll misses and the second
// hits is reachable in production and NOT reachable in a `cleattest` unit test,
// because a sequenced stub cannot be expressed. The test covers the
// immediately-approved path and says so; the miss-then-hit path is the gap.
func requestApproval(args string) (string, error) {
	var a struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return "", fmt.Errorf("decode request_approval arguments: %w", err)
	}

	h.SetQueryState("status", "awaiting_approval")
	h.SetQueryState("approval_reason", a.Reason)
	// A MARKER WRITTEN ON ENTRY, not on the miss path. `approval_polls` below is
	// only set when a poll comes back empty, so a run approved on its first poll
	// never writes it -- and "the run finished" would then be the only evidence
	// that the wait happened at all. This key is set before any poll, so a run
	// whose state carries it demonstrably entered the wait.
	h.SetQueryState("approval_wait", "entered")

	for poll := 0; poll < MaxApprovalPolls; poll++ {
		req, err := json.Marshal(map[string]any{
			"event_type": "agent.approval",
			"timeout_ms": 0,
		})
		if err != nil {
			return "", err
		}
		outJSON, err := h.PluginCall("event-triggers", "await_event", string(req))
		if err != nil {
			return "", fmt.Errorf("await_event: %w", err)
		}

		var out struct {
			Found     bool            `json:"found"`
			EventData json.RawMessage `json:"event_data"`
		}
		if err := json.Unmarshal([]byte(outJSON), &out); err != nil {
			return "", fmt.Errorf("decode await_event output: %w", err)
		}

		if out.Found {
			var decision struct {
				Approved bool   `json:"approved"`
				Note     string `json:"note"`
			}
			if err := json.Unmarshal(out.EventData, &decision); err != nil {
				return "", fmt.Errorf("decode the approval event: %w", err)
			}
			h.SetQueryState("status", "approval_received")
			if !decision.Approved {
				return mustJSON(map[string]any{
					"approved": false,
					"note":     decision.Note,
				})
			}
			return mustJSON(map[string]any{
				"approved": true,
				"note":     decision.Note,
			})
		}

		h.SetQueryState("approval_polls", fmt.Sprintf("%d", poll+1))
		h.DurableSleepMs(ApprovalPollMs)
	}

	// The wait ran out. This is reported as a RESULT the model can see and act
	// on, not as an error: a run that gave up waiting is not a broken run, and
	// failing it here would make "nobody approved in time" indistinguishable
	// from "the approval machinery is down" in the run list.
	h.SetQueryState("status", "approval_timeout")
	return mustJSON(map[string]any{
		"approved": false,
		"note":     "no decision arrived before the wait expired",
	})
}

// saveArtifact writes the finished output through the bundled blobstore plugin.
func saveArtifact(in AgentInput, output string) error {
	req, err := json.Marshal(map[string]any{
		"key":  in.ArtifactKey,
		"data": base64.StdEncoding.EncodeToString([]byte(output)),
	})
	if err != nil {
		return err
	}
	_, err = h.PluginCall("blobstore", "put", string(req))
	return err
}

// ---- Tool schema ----

func availableTools() []Tool {
	return []Tool{
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "search_docs",
				Description: "Search the team's document store for passages relevant to a query.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"query": map[string]any{"type": "string", "description": "What to search for"},
					},
					"required": []string{"query"},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "save_report",
				Description: "Save a report the agent has written, so a human can read it later.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"body": map[string]any{"type": "string", "description": "The report body"},
					},
					"required": []string{"body"},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name: "request_approval",
				Description: "Ask a human to approve the next action. Blocks the run until a " +
					"decision arrives or the wait expires.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"reason": map[string]any{"type": "string", "description": "What is being approved"},
					},
					"required": []string{"reason"},
				},
			},
		},
	}
}

func toolNames() string {
	names := make([]string, 0, 3)
	for _, t := range availableTools() {
		names = append(names, t.Function.Name)
	}
	return strings.Join(names, ", ")
}

func systemPrompt() string {
	return "You are a careful operations agent. Use the tools available to you. " +
		"Ask for approval before anything a human would want to sign off on. " +
		"When you have the answer, reply with it as plain text and no tool call."
}

func mustJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
