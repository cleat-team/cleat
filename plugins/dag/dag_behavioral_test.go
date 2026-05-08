package dag

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// LoadFromJSON tests
// ---------------------------------------------------------------------------

func TestLoadFromJSON_ValidSpec(t *testing.T) {
	spec := `{"name":"test","tasks":[{"name":"parse","fn":"fn_parse"},{"name":"validate","fn":"fn_validate","parents":["parse"]}]}`
	registry := map[string]TaskFunc{
		"fn_parse":    func(ctx *TaskContext) (string, error) { return `{"ok":true}`, nil },
		"fn_validate": func(ctx *TaskContext) (string, error) { return `{"valid":true}`, nil },
	}
	d, err := LoadFromJSON(strings.NewReader(spec), registry)
	if err != nil {
		t.Fatalf("LoadFromJSON() returned unexpected error: %v", err)
	}
	if d == nil {
		t.Fatal("expected non-nil DAG")
	}
	// Verify tasks were loaded.
	taskParse, ok := d.tasks["parse"]
	if !ok {
		t.Fatal("expected task 'parse'")
	}
	if taskParse.Fn == nil {
		t.Error("expected task 'parse' to have a non-nil function")
	}
	if len(taskParse.Parents) != 0 {
		t.Errorf("expected 0 parents for 'parse', got %v", taskParse.Parents)
	}
	taskValidate, ok := d.tasks["validate"]
	if !ok {
		t.Fatal("expected task 'validate'")
	}
	if len(taskValidate.Parents) != 1 || taskValidate.Parents[0] != "parse" {
		t.Errorf("expected parent 'parse' for 'validate', got %v", taskValidate.Parents)
	}
	// Topological sort should produce 2 levels.
	levels, err := d.TopologicalSort()
	if err != nil {
		t.Fatal(err)
	}
	if len(levels) != 2 {
		t.Fatalf("expected 2 levels, got %d", len(levels))
	}
}

func TestLoadFromJSON_InvalidJSON(t *testing.T) {
	_, err := LoadFromJSON(strings.NewReader("{invalid json"), nil)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("expected decode error, got: %v", err)
	}
}

func TestLoadFromJSON_EmptySpec(t *testing.T) {
	_, err := LoadFromJSON(strings.NewReader(`{"name":"empty","tasks":[]}`), nil)
	if err == nil {
		t.Fatal("expected error for empty tasks list")
	}
	if !strings.Contains(err.Error(), "no tasks") {
		t.Errorf("expected 'no tasks' error, got: %v", err)
	}
}

func TestLoadFromJSON_DuplicateTasks(t *testing.T) {
	spec := `{"name":"test","tasks":[{"name":"a","fn":"fn_a"},{"name":"a","fn":"fn_b"}]}`
	_, err := LoadFromJSON(strings.NewReader(spec), nil)
	if err == nil {
		t.Fatal("expected error for duplicate task name")
	}
	if !strings.Contains(err.Error(), "duplicate task name") && !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("expected duplicate error, got: %v", err)
	}
}

func TestLoadFromJSON_UnknownParent(t *testing.T) {
	spec := `{"name":"test","tasks":[{"name":"a","fn":"fn_a","parents":["nonexistent"]}]}`
	_, err := LoadFromJSON(strings.NewReader(spec), nil)
	if err == nil {
		t.Fatal("expected error for unknown parent reference")
	}
	if !strings.Contains(err.Error(), "unknown parent") {
		t.Errorf("expected 'unknown parent' error, got: %v", err)
	}
}

func TestLoadFromJSON_UnknownFnWithRegistry(t *testing.T) {
	registry := map[string]TaskFunc{
		"fn_a": func(ctx *TaskContext) (string, error) { return "a", nil },
	}
	spec := `{"name":"test","tasks":[{"name":"a","fn":"fn_a"},{"name":"b","fn":"fn_missing"}]}`
	_, err := LoadFromJSON(strings.NewReader(spec), registry)
	if err == nil {
		t.Fatal("expected error for unknown function reference")
	}
	if !strings.Contains(err.Error(), "unknown function") {
		t.Errorf("expected 'unknown function' error, got: %v", err)
	}
}

func TestLoadFromJSON_NilRegistry(t *testing.T) {
	// With nil registry, structure validation still runs, but functions are
	// left nil (speculative loading / CLI validate mode).
	spec := `{"name":"test","tasks":[{"name":"a","fn":"fn_a"},{"name":"b","fn":"fn_b","parents":["a"]}]}`
	d, err := LoadFromJSON(strings.NewReader(spec), nil)
	if err != nil {
		t.Fatalf("LoadFromJSON() returned unexpected error: %v", err)
	}
	if d == nil {
		t.Fatal("expected non-nil DAG")
	}
	taskA := d.tasks["a"]
	if taskA == nil {
		t.Fatal("expected task 'a'")
	}
	if taskA.Fn != nil {
		t.Error("expected nil function for task when using nil registry")
	}
	taskB := d.tasks["b"]
	if taskB == nil {
		t.Fatal("expected task 'b'")
	}
	if taskB.Fn != nil {
		t.Error("expected nil function for task when using nil registry")
	}
}

func TestLoadFromJSON_CycleSpec(t *testing.T) {
	// a depends on b, b depends on a
	spec := `{"name":"test","tasks":[{"name":"a","fn":"fn_a","parents":["b"]},{"name":"b","fn":"fn_b","parents":["a"]}]}`
	d, err := LoadFromJSON(strings.NewReader(spec), nil)
	if err != nil {
		t.Fatalf("LoadFromJSON() returned unexpected error: %v", err)
	}
	// The DAG was constructed, but TopologicalSort should detect the cycle.
	_, err = d.TopologicalSort()
	if err == nil {
		t.Fatal("expected cycle detection error")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("expected 'cycle' error, got: %v", err)
	}
}

func TestLoadFromJSON_EmptyJSONObj(t *testing.T) {
	_, err := LoadFromJSON(strings.NewReader(`{}`), nil)
	if err == nil {
		t.Fatal("expected error for empty JSON object (no tasks field)")
	}
	if !strings.Contains(err.Error(), "no tasks") {
		t.Errorf("expected 'no tasks' error, got: %v", err)
	}
}

func TestLoadFromJSON_FnNotInRegistry(t *testing.T) {
	// fn_a is in registry but fn_b is not.
	registry := map[string]TaskFunc{
		"fn_a": func(ctx *TaskContext) (string, error) { return "a", nil },
	}
	spec := `{"name":"test","tasks":[{"name":"a","fn":"fn_a"},{"name":"b","fn":"fn_b"}]}`
	_, err := LoadFromJSON(strings.NewReader(spec), registry)
	if err == nil {
		t.Fatal("expected error for function not in registry")
	}
}

func TestLoadFromJSON_EmptyRegistry(t *testing.T) {
	// Empty but non-nil registry means ALL function references are unknown.
	registry := map[string]TaskFunc{}
	spec := `{"name":"test","tasks":[{"name":"a","fn":"fn_a"}]}`
	_, err := LoadFromJSON(strings.NewReader(spec), registry)
	if err == nil {
		t.Fatal("expected error for fn not in empty registry")
	}
}

func TestLoadFromJSON_TaskFnMatchInRegistry(t *testing.T) {
	// Verify that when a function exists in the registry, it is assigned to the task.
	registry := map[string]TaskFunc{
		"greet": func(ctx *TaskContext) (string, error) { return "hello", nil },
	}
	spec := `{"name":"test","tasks":[{"name":"say_hi","fn":"greet"}]}`
	d, err := LoadFromJSON(strings.NewReader(spec), registry)
	if err != nil {
		t.Fatalf("LoadFromJSON() returned unexpected error: %v", err)
	}
	task := d.tasks["say_hi"]
	if task.Fn == nil {
		t.Fatal("expected task function to be assigned from registry")
	}
	result, err := task.Fn(nil)
	if err != nil {
		t.Fatalf("task function returned error: %v", err)
	}
	if result != "hello" {
		t.Errorf("expected 'hello', got %q", result)
	}
}

// ---------------------------------------------------------------------------
// DAGSpec validation edge cases
// ---------------------------------------------------------------------------

func TestLoadFromJSON_MissingEntryPoint(t *testing.T) {
	// All tasks reference parents that form a cycle, no root task exists.
	// This should produce a DAG whose TopologicalSort reports a cycle.
	spec := `{"name":"test","tasks":[{"name":"x","fn":"fn_x","parents":["y"]},{"name":"y","fn":"fn_y","parents":["z"]},{"name":"z","fn":"fn_z","parents":["x"]}]}`
	d, err := LoadFromJSON(strings.NewReader(spec), nil)
	if err != nil {
		t.Fatalf("LoadFromJSON() returned unexpected error: %v", err)
	}
	_, err = d.TopologicalSort()
	if err == nil {
		t.Fatal("expected cycle detection error for missing entry point")
	}
}

func TestLoadFromJSON_MissingEdges(t *testing.T) {
	// A DAG with no edges (all root tasks) is valid.
	spec := `{"name":"test","tasks":[{"name":"a","fn":"fn_a"},{"name":"b","fn":"fn_b"},{"name":"c","fn":"fn_c"}]}`
	d, err := LoadFromJSON(strings.NewReader(spec), nil)
	if err != nil {
		t.Fatalf("LoadFromJSON() returned unexpected error: %v", err)
	}
	levels, err := d.TopologicalSort()
	if err != nil {
		t.Fatalf("TopologicalSort() returned unexpected error: %v", err)
	}
	if len(levels) != 1 {
		t.Fatalf("expected 1 level (all roots), got %d", len(levels))
	}
	if len(levels[0]) != 3 {
		t.Fatalf("expected 3 tasks in level 0, got %d", len(levels[0]))
	}
}

func TestLoadFromJSON_SelfCycle(t *testing.T) {
	// A task that references itself as a parent.
	spec := `{"name":"test","tasks":[{"name":"a","fn":"fn_a","parents":["a"]}]}`
	d, err := LoadFromJSON(strings.NewReader(spec), nil)
	if err != nil {
		t.Fatalf("LoadFromJSON() returned unexpected error: %v", err)
	}
	_, err = d.TopologicalSort()
	if err == nil {
		t.Fatal("expected cycle detection error for self-referencing task")
	}
}

// ---------------------------------------------------------------------------
// Spec type tests (DAGSpec, TaskSpec)
// ---------------------------------------------------------------------------

func TestDAGSpecDefaults(t *testing.T) {
	// Verify that zero-value DAGSpec and TaskSpec have expected defaults.
	spec := DAGSpec{}
	if spec.Name != "" {
		t.Errorf("expected empty Name, got %q", spec.Name)
	}
	if len(spec.Tasks) != 0 {
		t.Errorf("expected empty Tasks, got %d", len(spec.Tasks))
	}
	ts := TaskSpec{}
	if ts.Name != "" {
		t.Errorf("expected empty Name, got %q", ts.Name)
	}
	if ts.Fn != "" {
		t.Errorf("expected empty Fn, got %q", ts.Fn)
	}
	if ts.Parents != nil {
		t.Errorf("expected nil Parents, got %v", ts.Parents)
	}
}

// ---------------------------------------------------------------------------
// Plugin metadata tests
// ---------------------------------------------------------------------------

func TestDAGPluginInfo(t *testing.T) {
	p := &Plugin{}
	info := p.Info()
	if info.Name != "dag" {
		t.Errorf("expected Name 'dag', got %q", info.Name)
	}
	if info.Version != "0.1.0" {
		t.Errorf("expected Version '0.1.0', got %q", info.Version)
	}
	if info.Description == "" {
		t.Error("expected non-empty Description")
	}
	if info.Author != "cleat" {
		t.Errorf("expected Author 'cleat', got %q", info.Author)
	}
}

// ---------------------------------------------------------------------------
// Edge cases for LoadFromJSON
// ---------------------------------------------------------------------------

func TestLoadFromJSON_NoTasksField(t *testing.T) {
	// JSON without a "tasks" field at all — tasks will be nil/zero-value.
	_, err := LoadFromJSON(strings.NewReader(`{"name":"test"}`), nil)
	if err == nil {
		t.Fatal("expected error for spec with no tasks field")
	}
	if !strings.Contains(err.Error(), "no tasks") {
		t.Errorf("expected 'no tasks' error, got: %v", err)
	}
}

func TestLoadFromJSON_UnknownFnNilRegistry(t *testing.T) {
	// Unknown function with nil registry should succeed (speculative mode).
	spec := `{"name":"test","tasks":[{"name":"a","fn":"nonexistent_fn"}]}`
	d, err := LoadFromJSON(strings.NewReader(spec), nil)
	if err != nil {
		t.Fatalf("LoadFromJSON() returned unexpected error: %v", err)
	}
	if _, ok := d.tasks["a"]; !ok {
		t.Fatal("expected task 'a' to be loaded")
	}
	if d.tasks["a"].Fn != nil {
		t.Error("expected nil function for unknown fn with nil registry")
	}
}

// ---------------------------------------------------------------------------
// RegisterHostFunctions — dag plugin does not implement HasHostFunctions.
// There is no RegisterHostFunctions method to test.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Migrations — dag plugin does not implement HasMigrations.
// There is no Migrations method to test.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Routes — dag plugin does not implement HasRoutes.
// There is no RegisterRoutes method to test.
// ---------------------------------------------------------------------------
