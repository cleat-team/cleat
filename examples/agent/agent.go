// The agent workflow: cleat's agent loop as a deployable definition.
//
// THE LOOP IS NOT IN THIS FILE. It ships in the SDK (`cleat/agentworkflow`),
// because a loop that runs inside the guest has to be rewritten in every
// language -- which is what had happened before cleat#1983: the two agent
// templates each carried a hand-written copy of it, and neither was tested. One
// workflow, started as a child by any SDK, replaces every copy.
//
// DEPLOY IT AS `agent`. A child is resolved by name against a deployed
// definition, so the name is a contract -- `run_agent` in every SDK starts the
// child called "agent" and awaits it:
//
//	cleat build -o /tmp/out ./examples/agent/
//	cleat deploy --name agent /tmp/out/agent.wasm
//
// WHY THIS STAYS HERE TOO, now that cleat#2973 has added the scaffoldable
// form at cmd/cleat/templates/agent-workflow/ (cleat init --template
// agent-workflow): this copy is not a stale fork of that one, it is the one
// tests/crash/agent_resume_test.go depends on building IN PLACE, against the
// LOCAL cleat/agentworkflow -- which is what lets that test deploy and crash-
// resume the agent loop as it exists on the CURRENT tree, not as it existed
// in whichever release cleat/agentworkflow last shipped under. A scaffolded
// project resolves the SDK from the module proxy, exactly as an external
// user's would, and could not stand in for that. See buildWorkflowWASM's own
// comment in that test file.
//
// The move cleat#2973 originally proposed -- delete this directory, keep only
// the template -- would have broken that test silently on the next edit to
// cleat/agentworkflow: the template path is copied into a fresh scaffold and
// built against whatever version go.mod.txt resolves, not against HEAD.
//
// TOOLS ARE INPUT, not code. What an agent can do arrives in the run's input as
// a list of {name, description, parameters, kind}, where kind is "service"
// (a DurableCall), "plugin" (a PluginCall) or "workflow" (a child, awaited).
// That is why this file declares none: one deployment serves every caller's
// tool set.
package agentexample

import (
	"github.com/cleat-team/cleat/cleat"
	"github.com/cleat-team/cleat/cleat/agentworkflow"
)

// @cleatEntry(name="agent")
func Agent(h cleat.HostCalls, inputJSON string) (string, error) {
	return agentworkflow.Run(h, inputJSON)
}
