package dagrun

import (
	"reflect"
	"testing"
)

// A workflow that starts one child per task in TopologicalSort's level order
// records the run IDs in that order, and replay hands them back in that same
// order. If a level's order differs between a run and its replay, each run ID
// is paired with another task's name and nothing reports a divergence, because
// every child is the same workflow function (cleat#3393).
//
// Go randomises map iteration on every call, so these tests call
// TopologicalSort repeatedly on ONE DAG: an implementation that orders a level
// by map iteration cannot agree with the exact expected order 200 times in a
// row by luck.

func namesByLevel(levels [][]*Task) [][]string {
	out := make([][]string, len(levels))
	for i, level := range levels {
		for _, task := range level {
			out[i] = append(out[i], task.Name)
		}
	}
	return out
}

func TestTopologicalSortOrdersEachLevelByPriorityThenName(t *testing.T) {
	noop := func(ctx *TaskContext) (string, error) { return "", nil }

	d := NewDAG()
	// Roots: priority decides first, name breaks ties.
	d.AddTask("root-d", nil, noop, 0)
	d.AddTask("root-a", nil, noop, 2)
	d.AddTask("root-c", nil, noop, 0)
	d.AddTask("root-b", nil, noop, 1)
	// Level 1: children of different parents, so the order they become ready
	// depends on both the order of their parents and the order the children
	// were recorded under each parent.
	d.AddTask("mid-z", []string{"root-a"}, noop, 0)
	d.AddTask("mid-y", []string{"root-d"}, noop, 0)
	d.AddTask("mid-x", []string{"root-b", "root-c"}, noop, 1)
	d.AddTask("mid-w", []string{"root-c"}, noop, 0)
	d.AddTask("mid-v", []string{"root-a", "root-d"}, noop, 1)
	// Level 2.
	d.AddTask("leaf-2", []string{"mid-z", "mid-y"}, noop, 0)
	d.AddTask("leaf-1", []string{"mid-x"}, noop, 0)
	d.AddTask("leaf-3", []string{"mid-w", "mid-v"}, noop, 0)

	want := [][]string{
		{"root-c", "root-d", "root-b", "root-a"},
		{"mid-w", "mid-y", "mid-z", "mid-v", "mid-x"},
		{"leaf-1", "leaf-2", "leaf-3"},
	}

	for i := 0; i < 200; i++ {
		levels, err := d.TopologicalSort()
		if err != nil {
			t.Fatalf("TopologicalSort iteration %d: %v", i, err)
		}
		if got := namesByLevel(levels); !reflect.DeepEqual(got, want) {
			t.Fatalf("iteration %d: level order is not (priority, name)\n  got: %v\n want: %v", i, got, want)
		}
	}
}

func TestTopologicalSortLevelOrderMatchesTheExecutorsOrder(t *testing.T) {
	noop := func(ctx *TaskContext) (string, error) { return "", nil }

	d := NewDAG()
	for _, name := range []string{"e", "b", "d", "a", "c"} {
		d.AddTask(name, nil, noop)
	}

	levels, err := d.TopologicalSort()
	if err != nil {
		t.Fatalf("TopologicalSort: %v", err)
	}
	if len(levels) != 1 {
		t.Fatalf("expected one level, got %d", len(levels))
	}

	// The executor orders ready tasks with sortTasksByPriority. The exported
	// function must agree with it, or the two disagree about what "the order"
	// of a level is.
	executor := append([]*Task(nil), levels[0]...)
	sortTasksByPriority(executor)
	if got, want := namesByLevel([][]*Task{levels[0]}), namesByLevel([][]*Task{executor}); !reflect.DeepEqual(got, want) {
		t.Fatalf("TopologicalSort level %v differs from the executor's order %v", got, want)
	}
}
