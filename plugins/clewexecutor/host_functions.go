package clewexecutor

import (
	"fmt"

	"github.com/cleat-team/cleat/internal/plugin"
)

// RegisterHostFunctions registers run_phase and check_ci host functions.
func (p *Plugin) RegisterHostFunctions(scope plugin.FuncRegistry) error {
	if scope == nil {
		return fmt.Errorf("clew-executor: nil function registry")
	}
	if err := scope.Register(plugin.FuncOptions{
		Name:       "run_phase",
		Idempotent: true,
	}, p.runPhase); err != nil {
		return err
	}
	return scope.Register(plugin.FuncOptions{
		Name:       "check_ci",
		Idempotent: false,
	}, p.checkCI)
}
