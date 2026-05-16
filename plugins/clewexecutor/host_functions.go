package clewexecutor

import (
	"fmt"

	"github.com/cleat-team/cleat/internal/plugin"
)

// RegisterHostFunctions registers the run_phase host function.
func (p *Plugin) RegisterHostFunctions(scope plugin.FuncRegistry) error {
	if scope == nil {
		return fmt.Errorf("clew-executor: nil function registry")
	}
	return scope.Register(plugin.FuncOptions{
		Name:       "run_phase",
		Idempotent: true,
	}, p.runPhase)
}
