//go:build ignore

// The agent workflow: cleat's agent loop as a deployable definition.
//
// THE LOOP IS NOT IN THIS FILE. It ships in the SDK (`cleat/agentworkflow`),
// because a loop that runs inside the guest has to be rewritten in every
// language -- which is what had happened before cleat#1983: the two agent
// client templates each carried a hand-written copy of it, and neither was
// tested. One workflow, started as a child by any SDK, replaces every copy.
//
// DEPLOY IT AS `agent`. A child is resolved by name against a deployed
// definition, so the name is a contract -- `run_agent` in every SDK, and the
// `agent` client template's own workflow.go, start the child called "agent"
// and await it. That is why the Makefile below deploys under `--name agent`
// rather than `--name {{.ProjectName}}`, unlike every other template: the
// deployed NAME is a contract other code relies on, independent of what you
// called this project.
//
// TOOLS ARE INPUT, not code. What an agent can do arrives in the run's input
// as a list of {name, description, parameters, kind}, where kind is
// "service" (a DurableCall), "plugin" (a PluginCall) or "workflow" (a child,
// awaited). That is why this file declares none: one deployment serves every
// caller's tool set.
package agentexample

import (
	"github.com/cleat-team/cleat/cleat"
	"github.com/cleat-team/cleat/cleat/agentworkflow"
)

// @cleatEntry(name="agent")
func Agent(h cleat.HostCalls, inputJSON string) (string, error) {
	return agentworkflow.Run(h, inputJSON)
}
