package dagrun

import (
	"fmt"
	"io"

	"github.com/cleat-team/cleat/plugins/dag"
)

// TaskFunc is the signature for a DAG task function.
type TaskFunc func(ctx *TaskContext) (string, error)

// LoadFromJSON decodes a JSON DAG spec and constructs a runtime *DAG,
// wiring each task's Fn from registry by name.
//
// It re-uses plugins/dag's ParseSpec (host-side: parsing and structural
// validation -- no duplicate task names, every parent reference resolves)
// and adds the one check that only makes sense guest-side: every task's fn
// name must exist in registry, when registry is non-nil. A nil registry
// means "validate structure only, leave every Fn nil" -- speculative
// loading, used e.g. by a dry validation pass that doesn't have real task
// functions to wire yet.
func LoadFromJSON(r io.Reader, registry map[string]TaskFunc) (*DAG, error) {
	spec, err := dag.ParseSpec(r)
	if err != nil {
		return nil, err
	}

	// Validate: all fn values exist in the registry.
	if registry != nil {
		for _, ts := range spec.Tasks {
			if _, ok := registry[ts.Fn]; !ok {
				return nil, fmt.Errorf("dag: task %q references unknown function %q", ts.Name, ts.Fn)
			}
		}
	}

	// Build the DAG.
	d := NewDAG()
	for _, ts := range spec.Tasks {
		var fn func(ctx *TaskContext) (string, error)
		if registry != nil {
			if f, ok := registry[ts.Fn]; ok {
				fn = f
			}
		}
		d.AddTask(ts.Name, ts.Parents, fn, ts.Priority)
		d.tasks[ts.Name].WorkflowName = ts.Fn
		d.tasks[ts.Name].Description = ts.Description
		d.tasks[ts.Name].Contract = ts.Contract
	}

	return d, nil
}
