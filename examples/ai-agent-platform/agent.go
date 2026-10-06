// AI agent platform — a durable agent loop with a per-run spend ceiling.
//
// This is the reference implementation behind docs/playbooks/ai-agent-platform.md:
// the CLEAT-SIDE half of an agent platform. Real and runnable end to end on a
// fresh checkout with nothing installed and no API key.
//
// THE LOOP IS NOT HERE. cleat#1983 moved the ReAct loop itself into
// `cleat/agentworkflow`, written once and usable from every SDK; this file is
// now a CLIENT of it, the same shape cmd/cleat/templates/agent/workflow.go is
// for a freshly scaffolded project. See cleat#2980 for the migration, and
// cleat#3022/cleat#3169/cleat#3170 for the three extensions it needed first —
// found by building this migration, not by reading the original issue.
//
// CALLED DIRECTLY, NOT AS A CHILD. The template calls agentworkflow as a child
// workflow because it has to: a Python or Rust caller cannot import a Go
// package, so the generic loop has to be a separately deployed workflow
// resolved by name. This file is the reference Go implementation IN THIS
// REPOSITORY, where `agentworkflow.Run` is an ordinary importable function —
// calling it directly keeps this example's deployment shape exactly as it
// was (one wasm module, one deploy), which cleat#2980 scoped out of this
// migration. `RunAsChild`'s own doc comment says what the indirection is
// for: cross-SDK reuse, not this file.
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
//   - **The retrieval store** no longer has a tool here. `agentworkflow.Tool`'s
//     dispatch kinds (service, plugin, workflow, approval) each reach
//     something OUTSIDE the workflow; the original `search_docs` was pure
//     in-process Go computation reaching nothing, and the generic loop has no
//     kind for that — there is nowhere left for a local closure to live once
//     the loop moved out of this package (cleat#3169's "Related" section
//     records this as a neighbouring, deliberately uncovered gap). cleat
//     ships no retrieval plugin either, so there is also no real backend to
//     repoint it at. Dropped rather than faked; the README says so.
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
//     scripts/run-ai-agent-platform-scenario.sh. `agentworkflow.Run` makes the
//     same `h.PluginCall("llm", "chat", ...)` call this property always rested
//     on, so it survives the migration unchanged.
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
	"encoding/json"
	"fmt"

	"github.com/cleat-team/cleat/cleat"
	"github.com/cleat-team/cleat/cleat/agentworkflow"
)

// ---- Domain types ----
//
// AgentInput/AgentOutput keep their pre-migration names and fields, per
// cleat#2980's "Done when" -- a caller of this example's entry point sees no
// difference, even though everything behind RunAgent changed.

type AgentInput struct {
	// TenantID is the spend boundary. It is REQUIRED HERE, not in
	// agentworkflow.Input (which treats it as optional attribution, echoed and
	// nothing else — see that package's doc comment on why it does not
	// enforce one). The whole premise of this scenario is that one customer's
	// spend is bounded without bounding another's, and that is this file's
	// opinion to hold, not the generic workflow's.
	TenantID string `json:"tenant_id"`

	Task     string `json:"task"`
	MaxSteps int    `json:"max_steps"`

	// BudgetUSD is THIS RUN's ceiling, in dollars. See agentworkflow.Input.Budget
	// for the enforcement point this now delegates to -- same `>=` comparison,
	// same reason.
	//
	// It is per-run and not cumulative, and the README says so plainly: a
	// cumulative per-tenant ceiling needs an accumulator that survives the run,
	// and cleat has no token or cost metric (IMPROVEMENT-PLAN, and
	// docs/playbooks/ai-agent-platform.md "What you still have to build",
	// item 1).
	BudgetUSD float64 `json:"budget_usd"`

	// ArtifactKey is where the agent's finished output is written, through the
	// bundled `blobstore` plugin.
	ArtifactKey string `json:"artifact_key"`
}

type AgentOutput struct {
	TenantID string `json:"tenant_id"`

	// Status is the RUN's outcome, and it has exactly two values. There is no
	// `approval_denied`: a denial is a tool RESULT, handed back to the model so
	// it can choose another action, and the run continues to `done`.
	//
	// `awaiting_approval`, `approval_received` and `approval_timeout` are NOT
	// values of this field: they are QUERY STATE, written while the run is in
	// flight by agentworkflow.Run (cleat#3170), and this field is only ever
	// read once the run has finished.
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
	h.SetQueryState("budget_usd", fmt.Sprintf("%.6f", in.BudgetUSD))

	cfg := agentworkflow.Input{
		SystemPrompt: systemPrompt(),
		Provider:     "openai",
		Model:        "agent-stub-1",
		MaxSteps:     in.MaxSteps,
		Message:      in.Task,
		Budget:       in.BudgetUSD,
		TenantID:     in.TenantID,
		ArtifactKey:  in.ArtifactKey,
		// ToolErrorFail, not the default: this example has always ended the
		// run on a tool failure (including an unknown tool) rather than
		// feeding the error back to the model.
		ToolErrorMode: agentworkflow.ToolErrorFail,
		Tools:         availableTools(),
	}
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("marshal agentworkflow input: %w", err)
	}

	// THE DURABLE LOOP. Everything from here down -- the model turns, the tool
	// dispatch, the ceiling, the artifact write, the live "status" query state
	// (cleat#3170) -- is agentworkflow.Run's, not this file's. Called
	// directly (not via RunAsChild): see the package comment for why that
	// keeps this example's deployment shape unchanged. Writing to the SAME h
	// means its query-state writes land under the exact keys this package's
	// own scenario reads -- no replay boundary between this call and them.
	outJSON, err := agentworkflow.Run(h, string(cfgJSON))
	if err != nil {
		return "", fmt.Errorf("tenant %s: %w", in.TenantID, err)
	}

	var res agentworkflow.Result
	if err := json.Unmarshal([]byte(outJSON), &res); err != nil {
		return "", fmt.Errorf("decode agentworkflow result: %w", err)
	}

	// THE SETTLE SLEEP. Not decoration, and not optional: it is the ONLY
	// thing that gives scripts/run-ai-agent-platform-scenario.sh a wide,
	// reliable real-time window in which to SIGKILL the worker between the
	// last model call and the run actually finishing (its own comment:
	// "THE KILL HAS TO LAND AFTER THE MODEL CALLS AND BEFORE THE RUN ENDS").
	// Before the migration this lived inside the hand-written loop, between
	// the last model call and the artifact write; it lives HERE now because
	// that loop is agentworkflow.Run's, not this file's, and this is the
	// first point after it where this file has control again. Everything
	// agentworkflow.Run did -- every model call, every tool call, the
	// artifact write -- is already durably recorded by the time this line
	// runs, so a crash during this sleep resumes past all of it and repeats
	// none of it, same property, same width, one layer up.
	h.DurableSleepMs(SettleDelayMs)

	toolsUsed := make([]string, 0, len(res.ToolCalls))
	for _, tc := range res.ToolCalls {
		toolsUsed = append(toolsUsed, tc.Name)
	}

	out := AgentOutput{
		TenantID:    res.TenantID,
		Status:      res.Status,
		Output:      res.Answer,
		Steps:       res.Steps,
		Tokens:      res.Tokens,
		SpentUSD:    res.Cost,
		BudgetUSD:   in.BudgetUSD,
		ToolsUsed:   toolsUsed,
		ArtifactKey: res.ArtifactKey,
	}
	return mustJSON(out)
}

// ---- Tool schema ----
//
// Two tools, not three: search_docs had no declarative shape to migrate to
// (see the package comment). Both remaining ones now use cleat#3169's
// ArgTransforms/StaticArgs to keep their pre-migration wire contracts exactly
// -- the model is shown the same arguments it always was, and the plugin
// receives the same bytes it always did.

func availableTools() []agentworkflow.Tool {
	return []agentworkflow.Tool{
		{
			Name:        "save_report",
			Description: "Save a report the agent has written, so a human can read it later.",
			Kind:        agentworkflow.KindPlugin,
			Plugin:      "blobstore",
			Function:    "put",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"body": {"type": "string", "description": "The report body"}
				},
				"required": ["body"]
			}`),
			// The model supplies plain text under "body", exactly as before
			// RunAgent's own saveReport wrapped it in Go. base64 is produced
			// HERE, not asked of the model -- cleat#3169's whole motivating
			// case, since blobstore.put's Data field is []byte (base64 in
			// JSON) and a real model asked to produce base64 directly is a
			// genuine reliability regression, not a style choice.
			ArgTransforms: []agentworkflow.ArgTransform{
				{FromField: "body", ToField: "data", Encoding: "base64"},
			},
			StaticArgs: json.RawMessage(`{"key":"report"}`),
		},
		{
			Name: "request_approval",
			Description: "Ask a human to approve the next action. Blocks the run until a " +
				"decision arrives or the wait expires.",
			Kind:     agentworkflow.KindApproval,
			Plugin:   "event-triggers",
			Function: "await_event",
			// No parameters: event_type and timeout_ms never vary for this
			// tool, so the model is not asked for them at all -- it is
			// taught a tool that takes nothing, not one whose two fields
			// happen to be constant. Exactly what the original hand-written
			// requestApproval did: it took a "reason" argument but never
			// forwarded it to the plugin either, using it only for display.
			Parameters:          json.RawMessage(`{"type": "object", "properties": {}}`),
			StaticArgs:          json.RawMessage(`{"event_type":"agent.approval","timeout_ms":0}`),
			PollIntervalSeconds: ApprovalPollSeconds,
			MaxPolls:            MaxApprovalPolls,
		},
	}
}

const (
	// SettleDelayMs is the window scripts/run-ai-agent-platform-scenario.sh
	// kills the worker inside, between agentworkflow.Run returning and
	// RunAgent returning. See RunAgent's own comment for why it moved here.
	SettleDelayMs = 30_000

	// ApprovalPollSeconds is the gap between polls of the approval wait.
	ApprovalPollSeconds = 2

	// MaxApprovalPolls bounds the wait. An agent that waits forever on a human
	// who has gone home is a held resource in a conventional harness and a
	// free row here, but a run that never terminates is still a run that never
	// terminates -- the ceiling is what makes "waiting" distinguishable from
	// "stuck" in the run list.
	MaxApprovalPolls = 15
)

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
