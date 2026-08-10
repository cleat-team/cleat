package dagrun

import (
	"testing"
)

// Task input JSON is recorded in the event history and re-derived on replay,
// so it must serialise byte-identically every time. Go randomises map
// iteration order, so any encoder that walks a map directly will produce a
// different key order per run and surface as a spurious replay divergence.
// encoding/json sorts map keys; a hand-rolled encoder here previously did not.
//
// These tests fail reliably (not flakily) against a map-order-dependent
// encoder: 100 iterations over a 5-key map leaves essentially no chance of
// every run agreeing by luck.

func TestMarshalToJSONIsDeterministic(t *testing.T) {
	cases := []struct {
		name string
		val  any
	}{
		{
			name: "map[string]any with mixed values",
			val: map[string]any{
				"task":           "charge_card",
				"input":          "payload",
				"parent_outputs": map[string]string{"b": "2", "a": "1", "c": "3"},
				"zebra":          "last",
				"alpha":          "first",
			},
		},
		{
			name: "map[string]string",
			val:  map[string]string{"e": "5", "d": "4", "c": "3", "b": "2", "a": "1"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first, err := marshalToJSON(tc.val)
			if err != nil {
				t.Fatalf("marshalToJSON: %v", err)
			}
			for i := 0; i < 100; i++ {
				got, err := marshalToJSON(tc.val)
				if err != nil {
					t.Fatalf("marshalToJSON iteration %d: %v", i, err)
				}
				if string(got) != string(first) {
					t.Fatalf("non-deterministic output on iteration %d:\n first: %s\n   got: %s",
						i, first, got)
				}
			}
		})
	}
}

// buildTaskInput is the actual production path: it wraps task name, input and
// parent outputs into a map[string]any and marshals it. Guard the whole path,
// not just the encoder.
func TestBuildTaskInputIsDeterministic(t *testing.T) {
	d := NewDAG()
	d.AddTask("a", nil, nil)
	d.AddTask("b", nil, nil)
	d.AddTask("c", []string{"a", "b"}, nil)
	d.outputs = map[string]string{"a": "out-a", "b": "out-b"}

	task := d.Task("c")
	if task == nil {
		t.Fatal("task c not found")
	}

	first, err := d.buildTaskInput(task, "the-input")
	if err != nil {
		t.Fatalf("buildTaskInput: %v", err)
	}
	for i := 0; i < 100; i++ {
		got, err := d.buildTaskInput(task, "the-input")
		if err != nil {
			t.Fatalf("buildTaskInput iteration %d: %v", i, err)
		}
		if string(got) != string(first) {
			t.Fatalf("non-deterministic task input on iteration %d:\n first: %s\n   got: %s",
				i, first, got)
		}
	}
}
