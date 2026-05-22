// Package dag provides a Directed Acyclic Graph composition model for
// workflows. It allows you to define workflows as a DAG of tasks with
// explicit parent-child dependencies, then execute them using an event-driven
// scheduler built on AwaitAnyChild.
//
// Each task becomes a child workflow. As children complete, newly-unblocked
// dependents are started immediately — the scheduler does not wait for an
// entire topological level to finish before starting the next level.
//
// This is a pure library plugin built on existing HostCalls primitives
// (ChildWorkflowWithOptions, AwaitAnyChild). No new WASM imports, no schema
// changes.
//
//cleat:require ChildWorkflowWithOptions,AwaitAnyChild
package dag

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/cleat-team/cleat/cleat"
	"github.com/cleat-team/cleat/internal/plugin"
)

// RawMessage is a raw JSON-encoded value, replacing RawMessage
// to avoid importing encoding/json in TinyGo WASM builds.
type RawMessage []byte

// Task represents a single node in the DAG.
type Task struct {
	Name         string
	Parents      []string
	Fn           func(ctx *TaskContext) (string, error)
	Priority     int
	WorkflowName string // the workflow name to call (from spec Fn); empty means use Name
	Description  string // from DAG spec, for task creation
	Contract     string // from DAG spec, for task creation
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
	// MaxParallelism limits the number of child workflows that may run
	// concurrently. 0 means unlimited (all ready tasks are started at once).
	MaxParallelism int
}

// NewDAG creates a new empty DAG.
func NewDAG() *DAG {
	return &DAG{tasks: make(map[string]*Task)}
}

// AddTask adds a task to the DAG. parents lists the names of tasks that must
// complete before this task starts. Root tasks have no parents.
// priority is an optional scheduling priority (0 = highest, lower values = higher priority).
func (d *DAG) AddTask(name string, parents []string, fn func(ctx *TaskContext) (string, error), priority ...int) *DAG {
	p := 0
	if len(priority) > 0 {
		p = priority[0]
	}
	d.tasks[name] = &Task{Name: name, Parents: parents, Fn: fn, Priority: p}
	return d
}

// Tasks returns a copy of all tasks in the DAG.
func (d *DAG) Tasks() map[string]*Task {
	out := make(map[string]*Task, len(d.tasks))
	for k, v := range d.tasks {
		out[k] = v
	}
	return out
}

// Task returns the named task, or nil if not found.
func (d *DAG) Task(name string) *Task {
	return d.tasks[name]
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
//
// input can be:
//   - map[string]RawMessage — per-task inputs keyed by task name
//   - any other value — used as the input for all tasks
func (d *DAG) Execute(h cleat.HostCalls, input interface{}) error {
	return d.ExecuteWithOptions(h, input, ExecuteOptions{})
}

// ExecuteWithOptions runs the DAG with the given options using an
// event-driven scheduler.
//
// Tasks are topologically sorted for validation only. Execution is
// event-driven: ready tasks (all parents satisfied) are started as child
// workflows, and AwaitAnyChild is used to block until at least one
// completes. When a child completes, any newly-unblocked dependents are
// immediately added to the ready queue. This allows DAG execution to
// proceed as fast as dependencies are satisfied, rather than waiting for
// an entire topological level to finish.
//
// MaxParallelism limits how many child workflows may be in-flight at once.
// When 0 (default), all ready tasks are started at once.
//
// input can be:
//   - map[string]RawMessage — per-task inputs keyed by task name
//   - any other value — used as the input for all tasks
func (d *DAG) ExecuteWithOptions(h cleat.HostCalls, input interface{}, opts ExecuteOptions) error {
	if err := d.validate(); err != nil {
		return err
	}

	// Validate structure with topological sort (also catches cycles).
	if _, err := d.TopologicalSort(); err != nil {
		return err
	}

	d.outputs = make(map[string]string)

	// Build dependency graph.
	unsatisfied := make(map[string]int)    // task name -> count of unsatisfied deps
	dependents := make(map[string][]string) // task name -> dependent task names

	for name, task := range d.tasks {
		unsatisfied[name] = len(task.Parents)
		for _, parent := range task.Parents {
			dependents[parent] = append(dependents[parent], name)
		}
	}

	// Initialize ready queue with root tasks (no unsatisfied dependencies).
	var ready []*Task
	for name, deg := range unsatisfied {
		if deg == 0 {
			ready = append(ready, d.tasks[name])
		}
	}
	sortTasksByPriority(ready)

	maxParallel := opts.MaxParallelism
	if maxParallel <= 0 {
		maxParallel = len(d.tasks)
	}

	running := make(map[string]*Task) // runID -> task
	perTaskInputs, _ := input.(map[string]RawMessage)
	if perTaskInputs == nil {
		// Try map[string]string — callers outside the dag package can't
		// construct map[string]RawMessage directly (TinyGo type identity).
		if strMap, ok := input.(map[string]string); ok {
			perTaskInputs = make(map[string]RawMessage, len(strMap))
			for k, v := range strMap {
				perTaskInputs[k] = RawMessage(v)
			}
		}
	}

	for len(ready) > 0 || len(running) > 0 {
		// Start as many ready tasks as the parallelism limit allows.
		for len(ready) > 0 && len(running) < maxParallel {
			task := ready[0]
			ready = ready[1:]

			runID, err := d.startChild(h, task, input, perTaskInputs)
			if err != nil {
				return err
			}
			running[runID] = task
		}

		if len(running) == 0 {
			break
		}

		// Collect runIDs for AwaitAnyChild.
		runIDs := make([]string, 0, len(running))
		for rid := range running {
			runIDs = append(runIDs, rid)
		}

		// Wait for at least one child to complete.
		completedRunID, result, awaitErr := h.AwaitAnyChild(runIDs)
		if awaitErr != nil {
			return fmt.Errorf("dag: await any child failed: %v", awaitErr)
		}

		task := running[completedRunID]
		delete(running, completedRunID)
		if task == nil {
			return fmt.Errorf("dag: internal error: completed child %q not found in running set", completedRunID)
		}

		d.outputs[task.Name] = result

		// Enqueue newly-unblocked dependents.
		for _, depName := range dependents[task.Name] {
			unsatisfied[depName]--
			if unsatisfied[depName] == 0 {
				depTask := d.tasks[depName]
				if depTask == nil {
					return fmt.Errorf("dag: internal error: dependent %q not found in tasks", depName)
				}
				ready = insertSortedByPriority(ready, depTask)
			}
		}
	}

	return nil
}

// startChild creates a single child workflow for the given task.
func (d *DAG) startChild(h cleat.HostCalls, task *Task, defaultInput interface{}, perTaskInputs map[string]RawMessage) (string, error) {
	inputJSON, err := d.taskInput(task, defaultInput, perTaskInputs)
	if err != nil {
		return "", err
	}

	wfName := task.WorkflowName
	if wfName == "" {
		wfName = task.Name
	}
	inputStr := "{}"
	if len(inputJSON) > 0 {
		inputStr = string(inputJSON)
	}
	runID, err := h.ChildWorkflowWithOptions(wfName, inputStr, cleat.ChildWorkflowOptions{Priority: task.Priority})
	if err != nil {
		return "", fmt.Errorf("dag: failed to start child workflow %s: %v", task.Name, err)
	}
	return runID, nil
}

// sortTasksByPriority sorts tasks by priority (lower = higher priority),
// with name as tiebreaker for determinism.
// Uses insertion sort to avoid sort.Slice which is not TinyGo-WASM-safe.
// Safe to call with nil or empty slice.
func sortTasksByPriority(tasks []*Task) {
	if len(tasks) < 2 {
		return
	}
	for i := 1; i < len(tasks); i++ {
		key := tasks[i]
		if key == nil {
			continue
		}
		j := i - 1
		for j >= 0 && taskLess(key, tasks[j]) {
			tasks[j+1] = tasks[j]
			j--
		}
		tasks[j+1] = key
	}
}

// insertSortedByPriority inserts a task into a priority-sorted slice.
// Uses linear scan to avoid sort.Search which is not TinyGo-WASM-safe.
// Safe to call with nil or empty tasks slice.
func insertSortedByPriority(tasks []*Task, task *Task) []*Task {
	if task == nil {
		return tasks
	}
	i := 0
	for i < len(tasks) && tasks[i] != nil && !taskLess(task, tasks[i]) {
		i++
	}
	tasks = append(tasks, nil)
	copy(tasks[i+1:], tasks[i:])
	tasks[i] = task
	return tasks
}

// taskLess returns true if a should come before b (lower priority = higher precedence).
func taskLess(a, b *Task) bool {
	if a.Priority != b.Priority {
		return a.Priority < b.Priority
	}
	return a.Name < b.Name
}

// taskInput returns the JSON input for a task, using per-task inputs when
// available. When task.WorkflowName is set, the input is passed through as
// raw JSON without wrapping — the target workflow receives its native input
// format. Otherwise the input is wrapped with task name, input, and parent
// outputs via buildTaskInput.
func (d *DAG) taskInput(task *Task, defaultInput interface{}, perTaskInputs map[string]RawMessage) ([]byte, error) {
	if task.WorkflowName != "" {
		// Pass raw input through — the target workflow expects its own format.
		if raw, ok := perTaskInputs[task.Name]; ok {
			return raw, nil
		}
		// No per-task input; marshal the default input directly.
		if defaultInput != nil {
			if raw, ok := defaultInput.(RawMessage); ok {
				return raw, nil
			}
			return marshalToJSON(defaultInput), nil
		}
		return []byte("{}"), nil
	}
	// No WorkflowName set — wrap in {task, input, parent_outputs}.
	inp := defaultInput
	if raw, ok := perTaskInputs[task.Name]; ok {
		inp = raw
	}
	return d.buildTaskInput(task, inp)
}

// buildTaskInput constructs the wrapped JSON input for a child workflow task.
func (d *DAG) buildTaskInput(task *Task, input interface{}) ([]byte, error) {
	wrapped := map[string]interface{}{
		"task":           task.Name,
		"input":          input,
		"parent_outputs": d.buildParentOutputs(task, d.outputs),
	}
	return marshalToJSON(wrapped), nil
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

// marshalToJSON encodes common types to JSON without encoding/json.
func marshalToJSON(v interface{}) []byte {
	switch val := v.(type) {
	case RawMessage:
		if len(val) == 0 {
			return []byte("{}")
		}
		return val
	case string:
		return []byte(`"` + val + `"`)
	case map[string]string:
		return buildStringMapJSON(val)
	case map[string]interface{}:
		return buildInterfaceMapJSON(val)
	default:
		return []byte("{}")
	}
}

func buildStringMapJSON(m map[string]string) []byte {
	if len(m) == 0 {
		return []byte("{}")
	}
	var b []byte
	b = append(b, '{')
	first := true
	for k, v := range m {
		if !first {
			b = append(b, ',')
		}
		first = false
		b = append(b, '"')
		b = append(b, k...)
		b = append(b, '"', ':')
		if v == "" {
			b = append(b, '"', '"')
		} else {
			b = append(b, '"')
			b = append(b, v...)
			b = append(b, '"')
		}
	}
	b = append(b, '}')
	return b
}

func buildInterfaceMapJSON(m map[string]interface{}) []byte {
	if len(m) == 0 {
		return []byte("{}")
	}
	var b []byte
	b = append(b, '{')
	first := true
	for k, v := range m {
		if !first {
			b = append(b, ',')
		}
		first = false
		b = append(b, '"')
		b = append(b, k...)
		b = append(b, '"', ':')
		switch val := v.(type) {
		case string:
			b = append(b, '"')
			b = append(b, val...)
			b = append(b, '"')
		case RawMessage:
			b = append(b, val...)
		case map[string]string:
			b = append(b, buildStringMapJSON(val)...)
		default:
			b = append(b, '"', '"')
		}
	}
	b = append(b, '}')
	return b
}

// validate checks that all parent references exist and there are no cycles.
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
