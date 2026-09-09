package dag

import (
	"strings"
	"testing"
)

// TestAPriorityIsDecodedRatherThanScanned is cleat#1051, the plugins/dag twin
// of the WASM-codegen defect fixed in cleat#1036.
//
// ExtractJSONInt was a hand-rolled digit scanner with no sign handling, no
// error return and no bounds check. It stopped at the first non-digit, so a
// leading `-` broke the loop immediately and the field bound 0 having consumed
// nothing.
//
// A NEGATIVE PRIORITY IS MEANINGFUL, NOT MALFORMED, which is what makes this
// worth a test rather than a note. The column is `priority INTEGER NOT NULL
// DEFAULT 0` with no CHECK and dispatch orders by `priority ASC`, so negative
// is how a task goes ahead of normal work without renumbering everything
// already at 0. `Priority` is then consumed twice for real: as the child's
// dispatch priority in dagrun.go, and for ordering within the DAG.
//
// Measured before the fix, through `cleat dag validate` on a real spec file:
// -5 bound 0, 7.9 bound 7, "3" bound 0, and the command printed
// "DAG spec is valid." in every case.
func TestAPriorityIsDecodedRatherThanScanned(t *testing.T) {
	parse := func(t *testing.T, priorityJSON string) (int, error) {
		t.Helper()
		spec := `{"name":"s","tasks":[{"name":"t","fn":"f","priority":` + priorityJSON + `}]}`
		d, err := ParseSpec(strings.NewReader(spec))
		if err != nil {
			return 0, err
		}
		if len(d.Tasks) != 1 {
			t.Fatalf("expected 1 task, got %d -- the spec did not parse as intended and "+
				"the priority below is not evidence", len(d.Tasks))
		}
		return d.Tasks[0].Priority, nil
	}

	t.Run("a negative binds", func(t *testing.T) {
		got, err := parse(t, "-5")
		if err != nil {
			t.Fatalf("a negative priority was rejected: %v", err)
		}
		if got != -5 {
			t.Errorf("priority bound %d, want -5.\n\n"+
				"0 here is cleat#1051: the scanner broke on the leading '-' and returned "+
				"0 having consumed nothing, so a task meant to run AHEAD of normal work "+
				"is silently demoted to normal -- in dispatch and in DAG ordering both.",
				got)
		}
	})

	t.Run("a positive still binds", func(t *testing.T) {
		// CONTROL. Without it a parser that bound 0 for everything would be
		// indistinguishable from one that only mishandles the sign.
		got, err := parse(t, "10")
		if err != nil {
			t.Fatalf("an ordinary positive priority was rejected: %v", err)
		}
		if got != 10 {
			t.Errorf("priority bound %d, want 10", got)
		}
	})

	t.Run("an absent priority is zero, not an error", func(t *testing.T) {
		// CONTROL, and the reason ExtractJSONInt distinguishes absent from
		// malformed: 0 is the documented default and most specs omit the field.
		// A fix that rejected absence would break every existing spec.
		spec := `{"name":"s","tasks":[{"name":"t","fn":"f"}]}`
		d, err := ParseSpec(strings.NewReader(spec))
		if err != nil {
			t.Fatalf("a spec with no priority was rejected: %v", err)
		}
		if d.Tasks[0].Priority != 0 {
			t.Errorf("an absent priority bound %d, want 0", d.Tasks[0].Priority)
		}
	})

	t.Run("a quoted number is rejected rather than silently zeroed", func(t *testing.T) {
		if got, err := parse(t, `"3"`); err == nil {
			t.Errorf("a quoted priority bound %d and the spec validated.\n\n"+
				"This is reached from `cleat dag validate`, whose job is to say a "+
				"malformed field is malformed. The scanner returned 0 with no error.", got)
		}
	})

	t.Run("a float is rejected rather than truncated", func(t *testing.T) {
		if got, err := parse(t, "7.9"); err == nil {
			t.Errorf("priority 7.9 bound %d rather than being rejected.\n\n"+
				"The scanner truncated to 7 silently. Truncation is a decision the "+
				"spec author did not make and cannot see.", got)
		}
	})
}
