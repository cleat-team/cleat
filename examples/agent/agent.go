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
// WHY THIS IS AN EXAMPLE RATHER THAN A TEMPLATE, which is where it is headed: a
// scaffolded project resolves the SDK from the module proxy, exactly as an
// external user's would, so a template cannot import an SDK package that no
// published version carries yet. `TestEveryGoTemplateScaffoldsIntoAProjectThatBuilds`
// measures that -- it refused the first version of this change, which had the
// template import agentworkflow, with "module ...@latest found (v0.3.2), but
// does not contain package .../agentworkflow". An example in this repository
// builds against the local module, so it is verifiable now and the template
// form can follow the release that carries the package.
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
