package scheduler

import (
	"fmt"

	"github.com/cleat-team/cleat/plugin"
)

// RegisterHostFunctions registers workflow-callable functions for the scheduler
// plugin. Currently, the scheduler does not expose any host functions to
// workflows; this method is a no-op that validates the registry is non-nil.
func (p *Plugin) RegisterHostFunctions(scope plugin.FuncRegistry) error {
	if scope == nil {
		return fmt.Errorf("scheduler: nil FuncRegistry")
	}
	return nil
}
