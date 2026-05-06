package dag

import (
	"context"
	"fmt"
	"testing"

	"github.com/rcownie/durable/internal/plugin"
)

func TestInfo(t *testing.T) {
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

func TestInit(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{}
	err := p.Init(context.Background(), env)
	if err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if p.logger == nil {
		t.Error("expected logger to be set")
	}
}

func TestTopologicalSortLinear(t *testing.T) {
	d := NewDAG()
	d.AddTask("a", nil, nil)
	d.AddTask("b", []string{"a"}, nil)
	d.AddTask("c", []string{"b"}, nil)
	levels, err := d.topologicalSort()
	if err != nil {
		t.Fatal(err)
	}
	if len(levels) != 3 {
		t.Fatalf("expected 3 levels, got %d", len(levels))
	}
	if len(levels[0]) != 1 || levels[0][0].Name != "a" {
		t.Errorf("level 0: expected [a], got %v", levelNames(levels[0]))
	}
	if len(levels[1]) != 1 || levels[1][0].Name != "b" {
		t.Errorf("level 1: expected [b], got %v", levelNames(levels[1]))
	}
	if len(levels[2]) != 1 || levels[2][0].Name != "c" {
		t.Errorf("level 2: expected [c], got %v", levelNames(levels[2]))
	}
}

func TestTopologicalSortDiamond(t *testing.T) {
	d := NewDAG()
	d.AddTask("a", nil, nil)
	d.AddTask("b", []string{"a"}, nil)
	d.AddTask("c", []string{"a"}, nil)
	d.AddTask("d", []string{"b", "c"}, nil)
	levels, err := d.topologicalSort()
	if err != nil {
		t.Fatal(err)
	}
	if len(levels) != 3 {
		t.Fatalf("expected 3 levels, got %d", len(levels))
	}
	// Level 0: [a], Level 1: [b, c] (any order), Level 2: [d]
	if len(levels[0]) != 1 || levels[0][0].Name != "a" {
		t.Errorf("level 0: expected [a], got %v", levelNames(levels[0]))
	}
	if len(levels[1]) != 2 {
		t.Errorf("level 1: expected 2 tasks, got %d: %v", len(levels[1]), levelNames(levels[1]))
	}
	if len(levels[2]) != 1 || levels[2][0].Name != "d" {
		t.Errorf("level 2: expected [d], got %v", levelNames(levels[2]))
	}
}

func TestCycleDetection(t *testing.T) {
	d := NewDAG()
	d.AddTask("a", []string{"b"}, nil)
	d.AddTask("b", []string{"a"}, nil)
	_, err := d.topologicalSort()
	if err == nil {
		t.Fatal("expected cycle detection error")
	}
}

func TestSelfCycleDetection(t *testing.T) {
	d := NewDAG()
	d.AddTask("a", []string{"a"}, nil)
	_, err := d.topologicalSort()
	if err == nil {
		t.Fatal("expected cycle detection error for self-referencing task")
	}
}

func TestUnknownParent(t *testing.T) {
	d := NewDAG()
	d.AddTask("a", []string{"nonexistent"}, nil)
	err := d.validate()
	if err == nil {
		t.Fatal("expected unknown parent error")
	}
}

func TestRootTasksOnly(t *testing.T) {
	d := NewDAG()
	d.AddTask("a", nil, nil)
	d.AddTask("b", nil, nil)
	d.AddTask("c", nil, nil)
	levels, err := d.topologicalSort()
	if err != nil {
		t.Fatal(err)
	}
	// All root tasks should be in level 0.
	if len(levels) != 1 || len(levels[0]) != 3 {
		t.Errorf("expected 1 level with 3 tasks, got %d levels, %d tasks", len(levels), len(levels[0]))
	}
}

func TestFanOutFanIn(t *testing.T) {
	d := NewDAG()
	d.AddTask("root", nil, nil)
	for i := 0; i < 5; i++ {
		d.AddTask(fmt.Sprintf("worker-%d", i), []string{"root"}, nil)
	}
	d.AddTask("collector", []string{"worker-0", "worker-1", "worker-2", "worker-3", "worker-4"}, nil)
	levels, err := d.topologicalSort()
	if err != nil {
		t.Fatal(err)
	}
	if len(levels) != 3 {
		t.Fatalf("expected 3 levels, got %d", len(levels))
	}
	if len(levels[1]) != 5 {
		t.Errorf("expected 5 worker tasks in level 1, got %d", len(levels[1]))
	}
}

func TestEmptyDAG(t *testing.T) {
	d := NewDAG()
	levels, err := d.topologicalSort()
	if err != nil {
		t.Fatal(err)
	}
	if len(levels) != 0 {
		t.Errorf("expected 0 levels for empty DAG, got %d", len(levels))
	}
}

func TestDisconnectedGraphs(t *testing.T) {
	d := NewDAG()
	d.AddTask("a", nil, nil)
	d.AddTask("b", nil, nil)
	d.AddTask("c", []string{"a"}, nil)
	d.AddTask("d", []string{"b"}, nil)
	levels, err := d.topologicalSort()
	if err != nil {
		t.Fatal(err)
	}
	// Two independent chains: a->c and b->d
	// Level 0: [a,b] (both roots), Level 1: [c,d]
	if len(levels) != 2 {
		t.Fatalf("expected 2 levels, got %d", len(levels))
	}
	if len(levels[0]) != 2 {
		t.Errorf("level 0: expected 2 roots, got %d", len(levels[0]))
	}
	if len(levels[1]) != 2 {
		t.Errorf("level 1: expected 2 tasks, got %d", len(levels[1]))
	}
}

func TestValidatePasses(t *testing.T) {
	d := NewDAG()
	d.AddTask("a", nil, nil)
	d.AddTask("b", []string{"a"}, nil)
	d.AddTask("c", []string{"a", "b"}, nil)
	err := d.validate()
	if err != nil {
		t.Fatalf("validate() returned unexpected error: %v", err)
	}
}

func TestValidateEmptyDAG(t *testing.T) {
	d := NewDAG()
	err := d.validate()
	if err != nil {
		t.Fatalf("validate() on empty DAG returned error: %v", err)
	}
}

func TestBuildParentOutputs(t *testing.T) {
	d := NewDAG()
	d.AddTask("a", nil, nil)
	d.AddTask("b", []string{"a"}, nil)

	taskB := d.tasks["b"]
	outputs := map[string]string{
		"a": "result-a",
	}

	parentOuts := d.buildParentOutputs(taskB, outputs)
	if len(parentOuts) != 1 {
		t.Fatalf("expected 1 parent output, got %d", len(parentOuts))
	}
	if parentOuts["a"] != "result-a" {
		t.Errorf("expected parent output 'result-a', got %q", parentOuts["a"])
	}
}

// levelNames extracts task names from a level for readable test output.
func levelNames(tasks []*Task) []string {
	names := make([]string, len(tasks))
	for i, t := range tasks {
		names[i] = t.Name
	}
	return names
}
