// End-to-end tests for the diamond DAG pipeline.
//
// These tests verify that the DAG composition model correctly orchestrates
// child workflows level by level, passes parent outputs to children, and
// surfaces errors. They use cleattest.TestEnv to simulate the host
// runtime with registered child workflow stubs.
package dagexample_test

import (
	"fmt"
	"strings"
	"testing"

	dagexample "github.com/cleat-team/cleat/examples/dag"
	"github.com/cleat-team/cleat/cleat/cleattest"

	dagplugin "github.com/cleat-team/cleat/plugins/dag"
)

// ---------------------------------------------------------------------------
// Pipeline function tests
// ---------------------------------------------------------------------------

// TestPipelineWithDurableTest verifies the full diamond DAG pipeline
// executes through the cleattest mock engine.
func TestPipelineWithDurableTest(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnChildWorkflow("extract").Return(`{"data":"extracted"}`, nil)
	env.OnChildWorkflow("classify").Return(`{"category":"tech"}`, nil)
	env.OnChildWorkflow("translate").Return(`{"language":"es"}`, nil)
	env.OnChildWorkflow("summarize").Return(`{"summary":"hello"}`, nil)

	result, err := dagexample.Pipeline(env.H(), dagexample.DocumentInput{Text: "hello"})
	if err != nil {
		t.Fatalf("Pipeline() returned error: %v", err)
	}
	if !strings.Contains(result, "hello") {
		t.Errorf("expected summary result to contain 'hello', got: %s", result)
	}
}

// TestPipelineTaskError verifies that when a child workflow returns an error,
// the Pipeline propagates it.
func TestPipelineTaskError(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnChildWorkflow("extract").Return(`{"data":"extracted"}`, nil)
	env.OnChildWorkflow("classify").Return("", fmt.Errorf("classification failed"))
	env.OnChildWorkflow("translate").Return(`{"language":"es"}`, nil)
	env.OnChildWorkflow("summarize").Return(`{"summary":"done"}`, nil)

	_, err := dagexample.Pipeline(env.H(), dagexample.DocumentInput{Text: "test"})
	if err == nil {
		t.Fatal("expected error from classify task failure")
	}
	if !strings.Contains(err.Error(), "classify") {
		t.Errorf("expected error mentioning 'classify', got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// DAG orchestration tests using cleattest
// ---------------------------------------------------------------------------

// TestDAGExecuteDiamond verifies a diamond DAG executes correctly through
// the cleattest mock engine.
func TestDAGExecuteDiamond(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnChildWorkflow("extract").Return(`{"data":"extracted"}`, nil)
	env.OnChildWorkflow("classify").Return(`{"category":"tech"}`, nil)
	env.OnChildWorkflow("translate").Return(`{"language":"es"}`, nil)
	env.OnChildWorkflow("summarize").Return(`{"summary":"done"}`, nil)

	d := dagplugin.NewDAG()
	d.AddTask("extract", nil, nil)
	d.AddTask("classify", []string{"extract"}, nil)
	d.AddTask("translate", []string{"extract"}, nil)
	d.AddTask("summarize", []string{"classify", "translate"}, nil)

	if err := d.Execute(env.H(), `{"doc":"test"}`); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	out, ok := d.Output("summarize")
	if !ok {
		t.Fatal("expected output for summarize")
	}
	if !strings.Contains(out, "done") {
		t.Errorf("expected summarize output to contain 'done', got: %s", out)
	}
}

// TestDAGExecuteLinearChain verifies a linear chain of tasks.
func TestDAGExecuteLinearChain(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnChildWorkflow("a").Return(`"level0"`, nil)
	env.OnChildWorkflow("b").Return(`"level1"`, nil)
	env.OnChildWorkflow("c").Return(`"level2"`, nil)

	d := dagplugin.NewDAG()
	d.AddTask("a", nil, nil)
	d.AddTask("b", []string{"a"}, nil)
	d.AddTask("c", []string{"b"}, nil)

	if err := d.Execute(env.H(), nil); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	out, ok := d.Output("c")
	if !ok {
		t.Fatal("expected output for c")
	}
	if !strings.Contains(out, "level2") {
		t.Errorf("expected 'level2' in output, got: %s", out)
	}
}

// TestDAGExecuteCycleDetection verifies that cyclic graphs are rejected.
func TestDAGExecuteCycleDetection(t *testing.T) {
	env := cleattest.NewTestEnv()

	d := dagplugin.NewDAG()
	d.AddTask("a", []string{"b"}, nil)
	d.AddTask("b", []string{"a"}, nil)

	err := d.Execute(env.H(), nil)
	if err == nil {
		t.Fatal("expected cycle detection error")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("expected error mentioning 'cycle', got: %v", err)
	}
}

// TestDAGExecuteUnknownParent verifies that unknown parent references are
// caught during execution.
func TestDAGExecuteUnknownParent(t *testing.T) {
	env := cleattest.NewTestEnv()

	d := dagplugin.NewDAG()
	d.AddTask("a", nil, nil)
	d.AddTask("b", []string{"nonexistent"}, nil)

	err := d.Execute(env.H(), nil)
	if err == nil {
		t.Fatal("expected unknown parent error")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("expected error mentioning 'nonexistent', got: %v", err)
	}
}

// TestDAGExecuteDisconnectedNodes verifies that multiple independent
// sub-graphs execute without interfering.
func TestDAGExecuteDisconnectedNodes(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnChildWorkflow("a").Return(`"a"`, nil)
	env.OnChildWorkflow("b").Return(`"b"`, nil)
	env.OnChildWorkflow("c").Return(`"c"`, nil)
	env.OnChildWorkflow("d").Return(`"d"`, nil)

	d := dagplugin.NewDAG()
	// Two disconnected linear chains: a->c and b->d
	d.AddTask("a", nil, nil)
	d.AddTask("b", nil, nil)
	d.AddTask("c", []string{"a"}, nil)
	d.AddTask("d", []string{"b"}, nil)

	if err := d.Execute(env.H(), nil); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	for _, name := range []string{"a", "b", "c", "d"} {
		out, ok := d.Output(name)
		if !ok {
			t.Fatalf("expected output for %s", name)
		}
		if !strings.Contains(out, name) {
			t.Errorf("output for %s should contain its result, got: %s", name, out)
		}
	}
}

// TestDAGExecuteMaxParallelism verifies that MaxParallelism limits the
// number of concurrent child workflows within a level.
func TestDAGExecuteMaxParallelism(t *testing.T) {
	env := cleattest.NewTestEnv()
	env.OnChildWorkflow("a").Return(`"a"`, nil)
	env.OnChildWorkflow("b").Return(`"b"`, nil)
	env.OnChildWorkflow("c").Return(`"c"`, nil)

	d := dagplugin.NewDAG()
	d.AddTask("a", nil, nil)
	d.AddTask("b", nil, nil)
	d.AddTask("c", nil, nil)

	if err := d.ExecuteWithOptions(env.H(), nil, dagplugin.ExecuteOptions{MaxParallelism: 2}); err != nil {
		t.Fatalf("ExecuteWithOptions failed: %v", err)
	}

	for _, name := range []string{"a", "b", "c"} {
		out, ok := d.Output(name)
		if !ok {
			t.Fatalf("expected output for %s", name)
		}
		if !strings.Contains(out, name) {
			t.Errorf("output for %s should contain its result, got: %s", name, out)
		}
	}
}

// TestDAGExecuteEmpty verifies that an empty DAG executes without error.
func TestDAGExecuteEmpty(t *testing.T) {
	env := cleattest.NewTestEnv()

	d := dagplugin.NewDAG()
	if err := d.Execute(env.H(), nil); err != nil {
		t.Fatalf("Execute on empty DAG returned error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// DAG spec loading tests
// ---------------------------------------------------------------------------

// TestDAGSpecLoadFromJSON verifies that LoadFromJSON parses a valid spec
// and constructs the correct DAG structure.
func TestDAGSpecLoadFromJSON(t *testing.T) {
	specJSON := `{
		"name": "test-pipeline",
		"tasks": [
			{"name": "extract", "fn": "ExtractText"},
			{"name": "classify", "fn": "ClassifyDoc", "parents": ["extract"]},
			{"name": "summarize", "fn": "Summarize", "parents": ["classify"]}
		]
	}`

	d, err := dagplugin.LoadFromJSON(
		strings.NewReader(specJSON),
		map[string]dagplugin.TaskFunc{
			"ExtractText": func(ctx *dagplugin.TaskContext) (string, error) { return `"ok"`, nil },
			"ClassifyDoc": func(ctx *dagplugin.TaskContext) (string, error) { return `"ok"`, nil },
			"Summarize":   func(ctx *dagplugin.TaskContext) (string, error) { return `"ok"`, nil },
		},
	)
	if err != nil {
		t.Fatalf("LoadFromJSON failed: %v", err)
	}

	// Verify structure via topological sort.
	levels, err := d.TopologicalSort()
	if err != nil {
		t.Fatalf("TopologicalSort failed: %v", err)
	}
	if len(levels) != 3 {
		t.Fatalf("expected 3 levels, got %d", len(levels))
	}
	if len(levels[0]) != 1 || levels[0][0].Name != "extract" {
		t.Errorf("level 0: expected [extract], got %v", levelNames(levels[0]))
	}
	if len(levels[1]) != 1 || levels[1][0].Name != "classify" {
		t.Errorf("level 1: expected [classify], got %v", levelNames(levels[1]))
	}
	if len(levels[2]) != 1 || levels[2][0].Name != "summarize" {
		t.Errorf("level 2: expected [summarize], got %v", levelNames(levels[2]))
	}
}

// TestDAGSpecLoadFromJSONNoRegistry verifies that LoadFromJSON with nil
// registry still validates structure.
func TestDAGSpecLoadFromJSONNoRegistry(t *testing.T) {
	specJSON := `{
		"name": "test",
		"tasks": [
			{"name": "a", "fn": "FA"},
			{"name": "b", "fn": "FB", "parents": ["a"]}
		]
	}`

	d, err := dagplugin.LoadFromJSON(strings.NewReader(specJSON), nil)
	if err != nil {
		t.Fatalf("LoadFromJSON with nil registry failed: %v", err)
	}
	if d == nil {
		t.Fatal("expected non-nil DAG")
	}
}

// TestDAGSpecLoadFromJSONDuplicateTasks verifies that duplicate task names
// are rejected.
func TestDAGSpecLoadFromJSONDuplicateTasks(t *testing.T) {
	specJSON := `{
		"name": "test",
		"tasks": [
			{"name": "a", "fn": "FA"},
			{"name": "a", "fn": "FB"}
		]
	}`

	_, err := dagplugin.LoadFromJSON(strings.NewReader(specJSON), nil)
	if err == nil {
		t.Fatal("expected error for duplicate task name")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("expected error mentioning 'duplicate', got: %v", err)
	}
}

// TestDAGSpecLoadFromJSONUnknownParent verifies that references to
// undeclared parents are rejected.
func TestDAGSpecLoadFromJSONUnknownParent(t *testing.T) {
	specJSON := `{
		"name": "test",
		"tasks": [
			{"name": "a", "fn": "FA", "parents": ["missing"]}
		]
	}`

	_, err := dagplugin.LoadFromJSON(strings.NewReader(specJSON), nil)
	if err == nil {
		t.Fatal("expected error for unknown parent")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("expected error mentioning 'missing', got: %v", err)
	}
}

// TestDAGSpecLoadFromJSONEmptyTasks verifies that a spec with no tasks
// is rejected.
func TestDAGSpecLoadFromJSONEmptyTasks(t *testing.T) {
	specJSON := `{"name": "empty", "tasks": []}`

	_, err := dagplugin.LoadFromJSON(strings.NewReader(specJSON), nil)
	if err == nil {
		t.Fatal("expected error for empty tasks")
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// levelNames extracts task names from a topological level for readable output.
func levelNames(tasks []*dagplugin.Task) []string {
	names := make([]string, len(tasks))
	for i, t := range tasks {
		names[i] = t.Name
	}
	return names
}
