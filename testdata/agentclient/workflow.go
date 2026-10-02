// Package agentclient is the Go half of cleat#1983's acceptance test: a
// workflow that runs the shipped agent as a CHILD, handed the same config the
// Python client is handed.
//
// THE CONFIG IS INPUT, NOT CODE, and that is deliberate. The acceptance is
// that "a Go workflow and a Python workflow each run the SAME agent config" --
// so the config has to come from one place, and the only place both clients
// can be given it is the run's own input. Two literals, one per language,
// would be two configs that agree on the day they are written and drift
// silently afterwards; a test comparing them would be checking the test.
//
// This file is also the Go SDK's surface under test: `agentworkflow.RunAsChild`
// does the start-and-await, so the fixture measures that call rather than
// hand-rolling the two host calls it is made of. A hand-rolled copy here would
// stay green if RunAsChild broke.
package agentclient

import (
	"encoding/json"
	"fmt"

	"github.com/cleat-team/cleat/cleat"
	"github.com/cleat-team/cleat/cleat/agentworkflow"
)

// RunAgentClient starts the agent child and awaits it.
//
// The whole input JSON is the agent's config, exactly as the Python client
// receives it. It is decoded into agentworkflow.Input rather than passed
// through as an opaque string so that a config this SDK cannot represent is a
// loud failure at the client rather than a puzzling one inside the child.
func RunAgentClient(h cleat.HostCalls, inputJSON string) (string, error) {
	var cfg agentworkflow.Input
	if err := json.Unmarshal([]byte(inputJSON), &cfg); err != nil {
		return "", fmt.Errorf("agentclient: decode agent config: %w", err)
	}

	res, err := agentworkflow.RunAsChild(h, cfg, cfg.Message)
	if err != nil {
		return "", err
	}

	out, err := json.Marshal(res)
	if err != nil {
		return "", fmt.Errorf("agentclient: encode result: %w", err)
	}
	return string(out), nil
}
