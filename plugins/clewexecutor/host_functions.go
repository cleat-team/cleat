package clewexecutor

import (
	"fmt"

	"github.com/cleat-team/cleat/internal/plugin"
)

// RegisterHostFunctions registers all clew-executor host functions.
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
	if err := scope.Register(plugin.FuncOptions{
		Name:       "check_ci",
		Idempotent: false,
	}, p.checkCI); err != nil {
		return err
	}
	if err := scope.Register(plugin.FuncOptions{
		Name:       "write_status",
		Idempotent: true,
	}, p.writeStatus); err != nil {
		return err
	}
	if err := scope.Register(plugin.FuncOptions{
		Name:       "validate_files",
		Idempotent: true,
	}, p.validateFiles); err != nil {
		return err
	}
	return nil
}
