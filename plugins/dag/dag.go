// Package dag provides a Directed Acyclic Graph composition model for
// workflows. It allows you to define workflows as a DAG of tasks with
// explicit parent-child dependencies, then execute them level by level
// using child workflow primitives.
//
// This is a pure library plugin built on existing HostCalls primitives
// (ChildWorkflow, AwaitAllChildren). No new WASM imports, no schema changes.
//
// NOTE: This plugin intentionally uses goroutines and channels for parallel
// DAG execution. The goroutine lifecycle is fully managed (bounded by a
// channel semaphore, barrier-collected via results channel). These patterns
// are safe here because this is SDK plugin infrastructure, not user workflow
// code. Each violation is marked with // cleat:allow comments.
package dag

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/rcownie/cleat/durable"
	"github.com/rcownie/cleat/internal/plugin"
)

// Task represents a single node in the DAG.
type Task struct {
	Name    string
	Parents []string
	Fn      func(ctx *TaskContext) (string, error)
}

// TaskContext provides the task with HostCalls and access to parent outputs.
type TaskContext struct {
	H            cleat.HostCalls
	Input        interface{}
	ParentOutput func(parentName string) (string, error)
}

// DAG is a directed acyclic graph of tasks.
type DAG struct {
	tasks   map[string]*Task
	outputs map[string]string // results from the last Execute call
}

// ExecuteOptions controls DAG execution behavior.
type ExecuteOptions struct {
	// MaxParallelism limits concurrent ChildWorkflow calls within each
	// topological level. 0 means unlimited (all tasks in a level are
	// started sequentially, then awaited concurrently via AwaitAllChildren).
	MaxParallelism int
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

// Output returns the output of the named task after Execute completes.
// Returns the task output and true, or empty string and false if the task
// has no output (either because Execute has not been called or the task
// name is unknown).
func (d *DAG) Output(name string) (string, bool) {
	if d.outputs == nil {
		return "", false
	}
	val, ok := d.outputs[name]
	return val, ok
}

// Execute runs the DAG using the provided HostCalls and input.
// It delegates to ExecuteWithOptions with default options.
func (d *DAG) Execute(h cleat.HostCalls, input interface{}) error {
	return d.ExecuteWithOptions(h, input, ExecuteOptions{})
}

// ExecuteWithOptions runs the DAG with the given options.
// It topologically sorts tasks, detects cycles, then executes tasks level by
// level. Each task runs as a child workflow. Parent outputs are available via
// the input JSON.
//
// When MaxParallelism > 0, ChildWorkflow calls within each topological level
// are started concurrently, limited by the semaphore to at most MaxParallelism
// concurrent calls. When MaxParallelism is 0, all ChildWorkflow calls are
// made sequentially before AwaitAllChildren is called.
func (d *DAG) ExecuteWithOptions(h cleat.HostCalls, input interface{}, opts ExecuteOptions) error {
	if err := d.validate(); err != nil {
		return err
	}

	levels, err := d.TopologicalSort()
	if err != nil {
		return err
	}

	d.outputs = make(map[string]string)

	for _, level := range levels {
		runIDs, levelTasks, startErr := d.startLevel(h, input, level, opts)
		if startErr != nil {
			return startErr
		}

		// Wait for all tasks at this level to complete.
		if len(runIDs) > 0 {
			results, awaitErr := h.AwaitAllChildren(runIDs)
			if awaitErr != nil {
				return fmt.Errorf("dag: failed to await children: %w", awaitErr)
			}
			for i, result := range results {
				if result.Error != "" {
					return fmt.Errorf("dag: task %s failed: %s", levelTasks[i].Name, result.Error)
				}
				d.outputs[levelTasks[i].Name] = result.Result
			}
		}
	}

	return nil
}

// startLevel dispatches to sequential or parallel level start based on opts.
func (d *DAG) startLevel(h cleat.HostCalls, input interface{}, level []*Task, opts ExecuteOptions) ([]string, []*Task, error) {
	if opts.MaxParallelism > 0 {
		return d.startLevelParallel(h, input, level, opts.MaxParallelism)
	}
	return d.startLevelSequential(h, input, level)
}

// startLevelSequential starts all tasks in the level one at a time.
func (d *DAG) startLevelSequential(h cleat.HostCalls, input interface{}, level []*Task) ([]string, []*Task, error) {
	var runIDs []string
	var levelTasks []*Task

	for _, task := range level {
		inputJSON, err := d.buildTaskInput(task, input)
		if err != nil {
			return nil, nil, err
		}

		runID, err := h.ChildWorkflow(task.Name, string(inputJSON))
		if err != nil {
			return nil, nil, fmt.Errorf("dag: failed to start child workflow %s: %w", task.Name, err)
		}
		runIDs = append(runIDs, runID)
		levelTasks = append(levelTasks, task)
	}

	return runIDs, levelTasks, nil
}

// startLevelParallel starts all tasks in the level concurrently, limiting
// concurrency with a buffered-channel semaphore.
func (d *DAG) startLevelParallel(h cleat.HostCalls, input interface{}, level []*Task, maxParallelism int) ([]string, []*Task, error) {
	sem := make(chan struct{}, maxParallelism)

	type levelItem struct {
		index int
		runID string
		task  *Task
		err   error
	}
	ch := make(chan levelItem, len(level))

	for i, task := range level {
		sem <- struct{}{} // cleat:allow E002 -- SDK plugin, not user workflow; channel semaphore is safe here
		go func(idx int, t *Task) { // cleat:allow E001 -- SDK plugin, not user workflow; goroutine lifecycle is fully managed
			defer func() { <-sem }()

			inputJSON, err := d.buildTaskInput(t, input)
			if err != nil {
				ch <- levelItem{idx, "", t, err} // cleat:allow E002
				return
			}

			runID, err := h.ChildWorkflow(t.Name, string(inputJSON))
			if err != nil {
				ch <- levelItem{idx, "", t, fmt.Errorf("dag: failed to start child workflow %s: %w", t.Name, err)} // cleat:allow E002
				return
			}
			ch <- levelItem{idx, runID, t, nil}
		}(i, task)
	}

	// Collect all results (channel acts as barrier for all goroutines).
	results := make([]levelItem, len(level))
	for i := 0; i < len(level); i++ {
		results[i] = <-ch // cleat:allow E002
	}

	// Check for errors.
	for _, r := range results {
		if r.err != nil {
			return nil, nil, r.err
		}
	}

	// Build ordered slices by index.
	runIDs := make([]string, len(level))
	tasks := make([]*Task, len(level))
	for _, r := range results {
		runIDs[r.index] = r.runID
		tasks[r.index] = r.task
	}

	return runIDs, tasks, nil
}

// buildTaskInput constructs the JSON input for a child workflow task.
func (d *DAG) buildTaskInput(task *Task, input interface{}) ([]byte, error) {
	taskInput := map[string]interface{}{
		"task":           task.Name,
		"input":          input,
		"parent_outputs": d.buildParentOutputs(task, d.outputs),
	}
	return json.Marshal(taskInput)
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

// TopologicalSort returns tasks grouped by level (Kahn's algorithm).
func (d *DAG) TopologicalSort() ([][]*Task, error) {
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
			return nil, fmt.Errorf("dag: cycle detected involving tasks: %v", inDegree)
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
