package dag

import (
	"encoding/json"
	"fmt"
	"io"
)

// DAGSpec is a JSON-serializable DAG specification.
type DAGSpec struct {
	Name  string     `json:"name"`
	Tasks []TaskSpec `json:"tasks"`
}

// TaskSpec describes a single task in a DAG spec.
type TaskSpec struct {
	Name    string   `json:"name"`
	Fn      string   `json:"fn"`
	Parents []string `json:"parents,omitempty"`
}

// TaskFunc is the signature for a DAG task function.
type TaskFunc func(ctx *TaskContext) (string, error)

// LoadFromJSON decodes a JSON DAG spec and constructs a *DAG.
// registry maps function names to TaskFunc implementations. If registry is nil
// (or a task's fn is not found), the task's function is left nil — validation
// of structure (duplicates, parents, cycles) still runs, which is useful for
// speculative loading such as the "validate" CLI command.
func LoadFromJSON(r io.Reader, registry map[string]TaskFunc) (*DAG, error) {
	var spec DAGSpec
	dec := json.NewDecoder(r)
	if err := dec.Decode(&spec); err != nil {
		return nil, fmt.Errorf("dag: decode spec: %w", err)
	}

	if len(spec.Tasks) == 0 {
		return nil, fmt.Errorf("dag: spec has no tasks")
	}

	// Validate: no duplicate task names.
	seen := make(map[string]bool)
	for _, ts := range spec.Tasks {
		if seen[ts.Name] {
			return nil, fmt.Errorf("dag: duplicate task name %q", ts.Name)
		}
		seen[ts.Name] = true
	}

	// Validate: all fn values exist in the registry.
	if registry != nil {
		for _, ts := range spec.Tasks {
			if _, ok := registry[ts.Fn]; !ok {
				return nil, fmt.Errorf("dag: task %q references unknown function %q", ts.Name, ts.Fn)
			}
		}
	}

	// Validate: all parents reference declared task names.
	for _, ts := range spec.Tasks {
		for _, parent := range ts.Parents {
			if !seen[parent] {
				return nil, fmt.Errorf("dag: task %q references unknown parent %q", ts.Name, parent)
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
		d.AddTask(ts.Name, ts.Parents, fn)
	}

	return d, nil
}
