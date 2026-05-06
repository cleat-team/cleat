// Package dag provides a Directed Acyclic Graph composition model for
// workflows. It allows you to define workflows as a DAG of tasks with
// explicit parent-child dependencies, then execute them level by level
// using child workflow primitives.
//
// This is a pure library plugin built on existing HostCalls primitives
// (ChildWorkflow, AwaitAllChildren). No new WASM imports, no schema changes.
package dag

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/rcownie/durable/durable"
	"github.com/rcownie/durable/internal/plugin"
)

// Task represents a single node in the DAG.
type Task struct {
	Name    string
	Parents []string
	Fn      func(ctx *TaskContext) (string, error)
}

// TaskContext provides the task with HostCalls and access to parent outputs.
type TaskContext struct {
	H            durable.HostCalls
	Input        interface{}
	ParentOutput func(parentName string) (string, error)
}

// DAG is a directed acyclic graph of tasks.
type DAG struct {
	tasks map[string]*Task
}

// NewDAG creates a new empty DAG.
func NewDAG() *DAG {
	return &DAG{tasks: make(map[string]*Task)}
}

// AddTask adds a task to the DAG. parents lists the names of tasks that must
// complete before this task starts. Root tasks have no parents.
func (d *DAG) AddTask(name string, parents []string, fn func(ctx *TaskContext) (string, error)) *DAG {
	d.tasks[name] = &Task{Name: name, Parents: parents, Fn: fn}
	return d
}

// Execute runs the DAG using the provided HostCalls and input.
// It topologically sorts tasks, detects cycles, then executes tasks level by level.
// Each task runs as a child workflow. Parent outputs are available via the input JSON.
func (d *DAG) Execute(h durable.HostCalls, input interface{}) error {
	if err := d.validate(); err != nil {
		return err
	}

	levels, err := d.topologicalSort()
	if err != nil {
		return err
	}

	// Parent outputs are tracked in-memory during Execute.
	// Map: taskName -> output
	outputs := make(map[string]string)

	for _, level := range levels {
		var runIDs []string
		var levelTasks []*Task

		for _, task := range level {
			// Build task input with parent outputs.
			taskInput := map[string]interface{}{
				"task":           task.Name,
				"input":          input,
				"parent_outputs": d.buildParentOutputs(task, outputs),
			}
			inputJSON, err := json.Marshal(taskInput)
			if err != nil {
				return fmt.Errorf("dag: failed to marshal input for %s: %w", task.Name, err)
			}

			runID, err := h.ChildWorkflow(task.Name, string(inputJSON))
			if err != nil {
				return fmt.Errorf("dag: failed to start child workflow %s: %w", task.Name, err)
			}
			runIDs = append(runIDs, runID)
			levelTasks = append(levelTasks, task)
		}

		// Wait for all tasks at this level to complete.
		if len(runIDs) > 0 {
			results, err := h.AwaitAllChildren(runIDs)
			if err != nil {
				return fmt.Errorf("dag: failed to await children: %w", err)
			}
			for i, result := range results {
				if result.Error != "" {
					return fmt.Errorf("dag: task %s failed: %s", levelTasks[i].Name, result.Error)
				}
				outputs[levelTasks[i].Name] = result.Result
			}
		}
	}

	return nil
}

func (d *DAG) buildParentOutputs(task *Task, outputs map[string]string) map[string]string {
	result := make(map[string]string)
	for _, parent := range task.Parents {
		if output, ok := outputs[parent]; ok {
			result[parent] = output
		}
	}
	return result
}

// validate checks that all parent references exist and there are no cycles.
func (d *DAG) validate() error {
	for name, task := range d.tasks {
		for _, parent := range task.Parents {
			if _, ok := d.tasks[parent]; !ok {
				return fmt.Errorf("dag: task %s references unknown parent %s", name, parent)
			}
		}
	}
	return nil
}

// topologicalSort returns tasks grouped by level (Kahn's algorithm).
func (d *DAG) topologicalSort() ([][]*Task, error) {
	inDegree := make(map[string]int)
	children := make(map[string][]string)

	for name := range d.tasks {
		inDegree[name] = 0
	}
	for name, task := range d.tasks {
		for _, parent := range task.Parents {
			inDegree[name]++
			children[parent] = append(children[parent], name)
		}
	}

	var levels [][]*Task
	var queue []*Task

	// Start with root tasks (in-degree 0).
	for name, degree := range inDegree {
		if degree == 0 {
			queue = append(queue, d.tasks[name])
		}
	}

	for len(queue) > 0 {
		levels = append(levels, queue)
		var nextQueue []*Task

		for _, task := range queue {
			for _, child := range children[task.Name] {
				inDegree[child]--
				if inDegree[child] == 0 {
					nextQueue = append(nextQueue, d.tasks[child])
				}
			}
		}
		queue = nextQueue
	}

	// Check for cycles.
	for _, degree := range inDegree {
		if degree > 0 {
			return nil, fmt.Errorf("dag: cycle detected")
		}
	}

	return levels, nil
}

// Plugin implementation

// Plugin registers the dag library as a loadable plugin.
type Plugin struct {
	logger *slog.Logger
}

func init() {
	plugin.Register(plugin.PluginInfo{
		Name:        "dag",
		Version:     "0.1.0",
		Description: "DAG composition model -- execute workflows as directed acyclic graphs built on child workflow primitives",
		Author:      "cleat",
	}, func() plugin.Plugin { return &Plugin{} })
}

// Info returns plugin metadata for discovery and documentation.
func (p *Plugin) Info() plugin.PluginInfo {
	return plugin.PluginInfo{
		Name:        "dag",
		Version:     "0.1.0",
		Description: "DAG composition model -- execute workflows as directed acyclic graphs built on child workflow primitives",
		Author:      "cleat",
	}
}

// Init initializes the plugin with the given environment.
func (p *Plugin) Init(ctx context.Context, env *plugin.Environment) error {
	if env.Logger != nil {
		p.logger = env.Logger
	} else {
		p.logger = slog.Default()
	}
	p.logger.Info("dag plugin initialized")
	return nil
}
