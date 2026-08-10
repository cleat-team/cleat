package dag

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// ParseSpec tests
//
// These cover the structural-validation half only: valid JSON, at least one
// task, no duplicate task names, every parent reference resolves. Registry
// resolution and runtime DAG construction (TaskFunc, TopologicalSort) are
// guest-side and tested against cleat/dagrun.LoadFromJSON instead -- see
// cleat/dagrun/loadfromjson_test.go.
// ---------------------------------------------------------------------------

func TestParseSpec_ValidSpec(t *testing.T) {
	spec := `{"name":"test","tasks":[{"name":"parse","fn":"fn_parse"},{"name":"validate","fn":"fn_validate","parents":["parse"]}]}`
	s, err := ParseSpec(strings.NewReader(spec))
	if err != nil {
		t.Fatalf("ParseSpec() returned unexpected error: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil DAGSpec")
	}
	if len(s.Tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(s.Tasks))
	}
	var parse, validate *TaskSpec
	for i := range s.Tasks {
		switch s.Tasks[i].Name {
		case "parse":
			parse = &s.Tasks[i]
		case "validate":
			validate = &s.Tasks[i]
		}
	}
	if parse == nil {
		t.Fatal("expected task 'parse'")
	}
	if len(parse.Parents) != 0 {
		t.Errorf("expected 0 parents for 'parse', got %v", parse.Parents)
	}
	if validate == nil {
		t.Fatal("expected task 'validate'")
	}
	if len(validate.Parents) != 1 || validate.Parents[0] != "parse" {
		t.Errorf("expected parent 'parse' for 'validate', got %v", validate.Parents)
	}
}

func TestParseSpec_InvalidJSON(t *testing.T) {
	_, err := ParseSpec(strings.NewReader("{invalid json"))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("expected decode error, got: %v", err)
	}
}

func TestParseSpec_EmptySpec(t *testing.T) {
	_, err := ParseSpec(strings.NewReader(`{"name":"empty","tasks":[]}`))
	if err == nil {
		t.Fatal("expected error for empty tasks list")
	}
	if !strings.Contains(err.Error(), "no tasks") {
		t.Errorf("expected 'no tasks' error, got: %v", err)
	}
}

func TestParseSpec_DuplicateTasks(t *testing.T) {
	spec := `{"name":"test","tasks":[{"name":"a","fn":"fn_a"},{"name":"a","fn":"fn_b"}]}`
	_, err := ParseSpec(strings.NewReader(spec))
	if err == nil {
		t.Fatal("expected error for duplicate task name")
	}
	if !strings.Contains(err.Error(), "duplicate task name") && !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("expected duplicate error, got: %v", err)
	}
}

func TestParseSpec_UnknownParent(t *testing.T) {
	spec := `{"name":"test","tasks":[{"name":"a","fn":"fn_a","parents":["nonexistent"]}]}`
	_, err := ParseSpec(strings.NewReader(spec))
	if err == nil {
		t.Fatal("expected error for unknown parent reference")
	}
	if !strings.Contains(err.Error(), "unknown parent") {
		t.Errorf("expected 'unknown parent' error, got: %v", err)
	}
}

func TestParseSpec_EmptyJSONObj(t *testing.T) {
	_, err := ParseSpec(strings.NewReader(`{}`))
	if err == nil {
		t.Fatal("expected error for empty JSON object (no tasks field)")
	}
	if !strings.Contains(err.Error(), "no tasks") {
		t.Errorf("expected 'no tasks' error, got: %v", err)
	}
}

func TestParseSpec_NoTasksField(t *testing.T) {
	// JSON without a "tasks" field at all — tasks will be nil/zero-value.
	_, err := ParseSpec(strings.NewReader(`{"name":"test"}`))
	if err == nil {
		t.Fatal("expected error for spec with no tasks field")
	}
	if !strings.Contains(err.Error(), "no tasks") {
		t.Errorf("expected 'no tasks' error, got: %v", err)
	}
}

func TestParseSpec_UnknownFnIsNotAnError(t *testing.T) {
	// ParseSpec never resolves function names against anything -- that is
	// the guest-side registry's job (cleat/dagrun.LoadFromJSON). A "fn" that
	// names nothing real is structurally fine at this layer.
	spec := `{"name":"test","tasks":[{"name":"a","fn":"nonexistent_fn"}]}`
	s, err := ParseSpec(strings.NewReader(spec))
	if err != nil {
		t.Fatalf("ParseSpec() returned unexpected error: %v", err)
	}
	if len(s.Tasks) != 1 || s.Tasks[0].Fn != "nonexistent_fn" {
		t.Fatalf("expected task 'a' with fn 'nonexistent_fn', got %+v", s.Tasks)
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
