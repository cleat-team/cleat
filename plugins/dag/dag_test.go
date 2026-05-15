package dag

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/cleat-team/cleat/cleat"
	"github.com/cleat-team/cleat/internal/plugin"
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
	levels, err := d.TopologicalSort()
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
	levels, err := d.TopologicalSort()
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
	_, err := d.TopologicalSort()
	if err == nil {
		t.Fatal("expected cycle detection error")
	}
}

func TestSelfCycleDetection(t *testing.T) {
	d := NewDAG()
	d.AddTask("a", []string{"a"}, nil)
	_, err := d.TopologicalSort()
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
	levels, err := d.TopologicalSort()
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
	levels, err := d.TopologicalSort()
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
	levels, err := d.TopologicalSort()
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
	levels, err := d.TopologicalSort()
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

// ---------------------------------------------------------------------------
// Mock HostCalls helper for e2e Execute tests
// ---------------------------------------------------------------------------

// dagTestHost creates a mock HostCalls that executes DAG task functions
// when ChildWorkflow is called. It parses task input JSON, builds a proper
// TaskContext with parent outputs, runs each task's Fn, and stores results
// for AwaitAllChildren to return.
func dagTestHost(d *DAG) cleat.HostCalls {
	var mu sync.Mutex
	childResults := make(map[string]cleat.ChildResult)
	var taskHC cleat.HostCalls

	opts := cleat.HostCallsOptions{
		ChildWorkflow: func(name, inputJSON string) (string, error) {
			mu.Lock()
			defer mu.Unlock()

			var taskInput struct {
				Task          string            `json:"task"`
				Input         json.RawMessage   `json:"input"`
				ParentOutputs map[string]string `json:"parent_outputs"`
			}
			if err := json.Unmarshal([]byte(inputJSON), &taskInput); err != nil {
				return "", err
			}

			task, ok := d.tasks[taskInput.Task]
			if !ok {
				return "", fmt.Errorf("unknown task: %s", taskInput.Task)
			}

			if task.Fn == nil {
				runID := "run-" + taskInput.Task
				childResults[runID] = cleat.ChildResult{RunID: runID, Result: "{}"}
				return runID, nil
			}

			ctx := &TaskContext{
				H:     taskHC,
				Input: taskInput.Input,
				ParentOutput: func(parentName string) (string, error) {
					val, ok := taskInput.ParentOutputs[parentName]
					if !ok {
						return "", fmt.Errorf("dag: parent %s not found", parentName)
					}
					return val, nil
				},
			}

			result, err := task.Fn(ctx)
			runID := "run-" + taskInput.Task
			if err != nil {
				childResults[runID] = cleat.ChildResult{RunID: runID, Error: err.Error()}
				return "", err
			}
			childResults[runID] = cleat.ChildResult{RunID: runID, Result: result}
			return runID, nil
		},
		AwaitAllChildren: func(runIDs []string) ([]cleat.ChildResult, error) {
			mu.Lock()
			defer mu.Unlock()
			results := make([]cleat.ChildResult, len(runIDs))
			for i, runID := range runIDs {
				cr, ok := childResults[runID]
				if !ok {
					return nil, fmt.Errorf("unknown child: %s", runID)
				}
				results[i] = cr
			}
			return results, nil
		},
		DurableLog: func(msg string) {},
		Now:        func() int64 { return 1000 },
		Random:     func() int64 { return 42 },
	}
	taskHC = cleat.NewHostCalls(opts)

	return taskHC
}

// ---------------------------------------------------------------------------
// Execute e2e tests
// ---------------------------------------------------------------------------

func TestExecuteEmptyDAG(t *testing.T) {
	d := NewDAG()
	h := dagTestHost(d)
	if err := d.Execute(h, nil); err != nil {
		t.Fatalf("expected no error for empty DAG, got: %v", err)
	}
}

func TestOutputBeforeExecute(t *testing.T) {
	d := NewDAG()
	d.AddTask("a", nil, func(ctx *TaskContext) (string, error) { return "result", nil })

	_, ok := d.Output("a")
	if ok {
		t.Error("Output should return false before Execute is called")
	}
}

func TestExecuteCycleDetection(t *testing.T) {
	d := NewDAG()
	d.AddTask("a", []string{"b"}, nil)
	d.AddTask("b", []string{"a"}, nil)

	h := dagTestHost(d)
	err := d.Execute(h, nil)
	if err == nil {
		t.Fatal("expected cycle detection error")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("expected error mentioning cycle, got: %v", err)
	}
}

func TestExecuteUnknownParent(t *testing.T) {
	d := NewDAG()
	d.AddTask("a", []string{"nonexistent"}, nil)

	h := dagTestHost(d)
	err := d.Execute(h, nil)
	if err == nil {
		t.Fatal("expected unknown parent error")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("expected error mentioning nonexistent parent, got: %v", err)
	}
}

func TestExecuteSingleNode(t *testing.T) {
	d := NewDAG()
	d.AddTask("greet", nil, func(ctx *TaskContext) (string, error) {
		return `{"message":"hello"}`, nil
	})

	h := dagTestHost(d)
	if err := d.Execute(h, nil); err != nil {
		t.Fatal(err)
	}

	out, ok := d.Output("greet")
	if !ok {
		t.Fatal("expected output for greet task")
	}
	if !strings.Contains(out, "hello") {
		t.Errorf("expected greet output to contain 'hello', got: %s", out)
	}
}

func TestExecuteDiamondDAG(t *testing.T) {
	d := NewDAG()

	// Diamond: extract -> classify+translate -> summarize
	d.AddTask("extract", nil, func(ctx *TaskContext) (string, error) {
		return `{"data":"extracted"}`, nil
	})
	d.AddTask("classify", []string{"extract"}, func(ctx *TaskContext) (string, error) {
		parentOut, err := ctx.ParentOutput("extract")
		if err != nil {
			return "", err
		}
		return `{"category":"tech","parent":` + parentOut + `}`, nil
	})
	d.AddTask("translate", []string{"extract"}, func(ctx *TaskContext) (string, error) {
		parentOut, err := ctx.ParentOutput("extract")
		if err != nil {
			return "", err
		}
		return `{"language":"es","parent":` + parentOut + `}`, nil
	})
	d.AddTask("summarize", []string{"classify", "translate"}, func(ctx *TaskContext) (string, error) {
		classOut, err := ctx.ParentOutput("classify")
		if err != nil {
			return "", err
		}
		transOut, err := ctx.ParentOutput("translate")
		if err != nil {
			return "", err
		}
		return `{"summary":{"classification":` + classOut + `,"translation":` + transOut + `}}`, nil
	})

	h := dagTestHost(d)
	if err := d.Execute(h, `{"doc":"test"}`); err != nil {
		t.Fatal(err)
	}

	// Verify all tasks produced output.
	extractOut, ok := d.Output("extract")
	if !ok {
		t.Fatal("expected output for extract")
	}
	if !strings.Contains(extractOut, "extracted") {
		t.Errorf("extract output missing 'extracted': %s", extractOut)
	}

	classifyOut, ok := d.Output("classify")
	if !ok {
		t.Fatal("expected output for classify")
	}
	if !strings.Contains(classifyOut, "tech") {
		t.Errorf("classify output missing 'tech': %s", classifyOut)
	}
	if !strings.Contains(classifyOut, "extracted") {
		t.Errorf("classify output should contain parent result: %s", classifyOut)
	}

	translateOut, ok := d.Output("translate")
	if !ok {
		t.Fatal("expected output for translate")
	}
	if !strings.Contains(translateOut, "es") {
		t.Errorf("translate output missing 'es': %s", translateOut)
	}
	if !strings.Contains(translateOut, "extracted") {
		t.Errorf("translate output should contain parent result: %s", translateOut)
	}

	summarizeOut, ok := d.Output("summarize")
	if !ok {
		t.Fatal("expected output for summarize")
	}
	if !strings.Contains(summarizeOut, "tech") || !strings.Contains(summarizeOut, "es") {
		t.Errorf("summarize output should contain both parent outputs: %s", summarizeOut)
	}
}

func TestExecuteDiamondDAGParentOutputErrors(t *testing.T) {
	t.Run("missing parent", func(t *testing.T) {
		d := NewDAG()
		d.AddTask("child", []string{"missing"}, func(ctx *TaskContext) (string, error) {
			_, err := ctx.ParentOutput("missing")
			return "", err
		})

		h := dagTestHost(d)
		err := d.Execute(h, nil)
		if err == nil {
			t.Fatal("expected error for missing parent")
		}
	})
}

func TestExecuteMaxParallelism(t *testing.T) {
	d := NewDAG()

	// Three independent root tasks (all in level 0).
	d.AddTask("a", nil, func(ctx *TaskContext) (string, error) {
		return `"result-a"`, nil
	})
	d.AddTask("b", nil, func(ctx *TaskContext) (string, error) {
		return `"result-b"`, nil
	})
	d.AddTask("c", nil, func(ctx *TaskContext) (string, error) {
		return `"result-c"`, nil
	})

	h := dagTestHost(d)
	if err := d.ExecuteWithOptions(h, nil, ExecuteOptions{MaxParallelism: 2}); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"a", "b", "c"} {
		out, ok := d.Output(name)
		if !ok {
			t.Fatalf("expected output for %s", name)
		}
		if !strings.Contains(out, name) {
			t.Errorf("output for %s should contain its result: %s", name, out)
		}
	}
}

func TestExecuteTaskError(t *testing.T) {
	d := NewDAG()
	d.AddTask("failing", nil, func(ctx *TaskContext) (string, error) {
		return "", fmt.Errorf("simulated task failure")
	})

	h := dagTestHost(d)
	err := d.Execute(h, nil)
	if err == nil {
		t.Fatal("expected task error")
	}
	if !strings.Contains(err.Error(), "failing") {
		t.Errorf("expected error mentioning failing task, got: %v", err)
	}
	if !strings.Contains(err.Error(), "simulated task failure") {
		t.Errorf("expected error containing original failure message, got: %v", err)
	}
}

func TestExecuteWithOptionsZeroParallelism(t *testing.T) {
	// Verify that ExecuteWithOptions with MaxParallelism=0 behaves the same
	// as Execute (sequential start per level).
	d := NewDAG()
	d.AddTask("a", nil, func(ctx *TaskContext) (string, error) {
		return `"ok"`, nil
	})

	h := dagTestHost(d)
	if err := d.ExecuteWithOptions(h, nil, ExecuteOptions{MaxParallelism: 0}); err != nil {
		t.Fatal(err)
	}
	out, ok := d.Output("a")
	if !ok || !strings.Contains(out, "ok") {
		t.Errorf("unexpected output: %s, %v", out, ok)
	}
}

func TestOutputAfterExecute(t *testing.T) {
	d := NewDAG()
	d.AddTask("greet", nil, func(ctx *TaskContext) (string, error) {
		return `"hello world"`, nil
	})

	h := dagTestHost(d)
	if err := d.Execute(h, nil); err != nil {
		t.Fatal(err)
	}

	out, ok := d.Output("greet")
	if !ok {
		t.Fatal("Output should return true for completed task")
	}
	if out != `"hello world"` {
		t.Errorf("expected 'hello world', got: %s", out)
	}

	// Unknown task name.
	_, ok = d.Output("nonexistent")
	if ok {
		t.Error("Output should return false for unknown task name")
	}
}

func TestExecuteMultipleLevels(t *testing.T) {
	// Linear chain: a -> b -> c -> d
	d := NewDAG()
	d.AddTask("a", nil, func(ctx *TaskContext) (string, error) {
		return `"level0"`, nil
	})
	d.AddTask("b", []string{"a"}, func(ctx *TaskContext) (string, error) {
		p, _ := ctx.ParentOutput("a")
		return `"level1:" + ` + p, nil
	})
	d.AddTask("c", []string{"b"}, func(ctx *TaskContext) (string, error) {
		p1, _ := ctx.ParentOutput("b")
		return `"level2:" + ` + p1, nil
	})
	d.AddTask("d", []string{"c"}, func(ctx *TaskContext) (string, error) {
		p2, _ := ctx.ParentOutput("c")
		return `"level3:" + ` + p2, nil
	})

	h := dagTestHost(d)
	if err := d.Execute(h, nil); err != nil {
		t.Fatal(err)
	}

	out, ok := d.Output("d")
	if !ok {
		t.Fatal("expected output for d")
	}
	if !strings.Contains(out, "level0") {
		t.Errorf("d output should transitively contain level0: %s", out)
	}
}

func TestInitWithLogger(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		Logger: slog.Default(),
	}
	err := p.Init(context.Background(), env)
	if err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if p.logger == nil {
		t.Error("expected logger to be set after Init")
	}
}

func TestExecuteWithOptionsAwaitAllChildrenError(t *testing.T) {
	d := NewDAG()
	d.AddTask("a", nil, nil)

	h := cleat.NewHostCalls(cleat.HostCallsOptions{
		ChildWorkflow: func(name, inputJSON string) (string, error) {
			return "run-a", nil
		},
		AwaitAllChildren: func(runIDs []string) ([]cleat.ChildResult, error) {
			return nil, fmt.Errorf("simulated await failure")
		},
		DurableLog: func(msg string) {},
		Now:        func() int64 { return 1000 },
		Random:     func() int64 { return 42 },
	})

	err := d.ExecuteWithOptions(h, nil, ExecuteOptions{})
	if err == nil {
		t.Fatal("expected await error")
	}
	if !strings.Contains(err.Error(), "await") {
		t.Errorf("expected error mentioning await, got: %v", err)
	}
}

func TestExecuteWithOptionsTaskResultError(t *testing.T) {
	d := NewDAG()
	d.AddTask("a", nil, nil)

	h := cleat.NewHostCalls(cleat.HostCallsOptions{
		ChildWorkflow: func(name, inputJSON string) (string, error) {
			return "run-a", nil
		},
		AwaitAllChildren: func(runIDs []string) ([]cleat.ChildResult, error) {
			return []cleat.ChildResult{
				{RunID: "run-a", Result: "", Error: "simulated task result error"},
			}, nil
		},
		DurableLog: func(msg string) {},
		Now:        func() int64 { return 1000 },
		Random:     func() int64 { return 42 },
	})

	err := d.ExecuteWithOptions(h, nil, ExecuteOptions{})
	if err == nil {
		t.Fatal("expected task result error")
	}
	if !strings.Contains(err.Error(), "a") {
		t.Errorf("expected error mentioning task a, got: %v", err)
	}
}

func TestStartLevelSequentialBuildInputError(t *testing.T) {
	d := NewDAG()
	d.AddTask("a", nil, nil)

	h := dagTestHost(d)
	err := d.ExecuteWithOptions(h, make(chan int), ExecuteOptions{})
	if err == nil {
		t.Fatal("expected JSON marshal error for channel input")
	}
	if !strings.Contains(err.Error(), "json") && !strings.Contains(err.Error(), "marshal") {
		t.Errorf("expected error mentioning json or marshal, got: %v", err)
	}
}

func TestExecuteMaxParallelismTaskError(t *testing.T) {
	d := NewDAG()
	d.AddTask("a", nil, func(ctx *TaskContext) (string, error) {
		return `"ok"`, nil
	})
	d.AddTask("b", nil, func(ctx *TaskContext) (string, error) {
		return "", fmt.Errorf("task b failed")
	})

	h := dagTestHost(d)
	err := d.ExecuteWithOptions(h, nil, ExecuteOptions{MaxParallelism: 2})
	if err == nil {
		t.Fatal("expected task b error")
	}
	if !strings.Contains(err.Error(), "b") {
		t.Errorf("expected error mentioning task b, got: %v", err)
	}
}
