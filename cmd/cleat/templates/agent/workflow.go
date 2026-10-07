//go:build ignore

// A workflow that runs cleat's agent.
//
// THE LOOP IS NOT HERE, and that is the change cleat#1983 made. A library that
// runs inside the guest has to be rewritten in every language, so this file and
// its Python sibling each carried a hand-written copy of the same ReAct loop,
// and neither was tested. The loop now ships as a workflow, and all this file
// does is start it and await it.
//
// REQUIRES THE AGENT DEPLOYED UNDER THE NAME `agent`. A child is resolved by
// name against a deployed definition, so the name is a contract:
//
//	cleat deploy --name agent ./out/agent.wasm
//
// WHY THIS STARTS THE CHILD BY HAND rather than calling the SDK's
// `agentworkflow.RunAsChild`, which does exactly the same thing: a scaffolded
// project resolves the `cleat` SDK from the module proxy, exactly as an
// external user's would, and `agentworkflow` does not exist in any published
// version yet. A template that imported it would not build, and
// TestEveryGoTemplateScaffoldsIntoAProjectThatBuilds measures that rather than
// assuming it. Once a release carries it, this collapses to one call -- which
// is what the Python template can already do, because the Python SDK installs
// from this repository's git rather than from a published artifact.
//
// The input is passed straight through: it IS the agent's config, documented in
// "Agent Workflows" in docs/reference/sdk-api.md.
package main

import (
	"fmt"

	"github.com/cleat-team/cleat/cleat"
)

// @cleatEntry(name="research_agent")
func ResearchAgent(h cleat.HostCalls, inputJSON string) (string, error) {
	runID, err := h.ChildWorkflow("agent", inputJSON)
	if err != nil {
		return "", fmt.Errorf("start agent: %w", err)
	}
	out, err := h.AwaitChild(runID)
	if err != nil {
		return "", fmt.Errorf("await agent: %w", err)
	}
	return out, nil
}
