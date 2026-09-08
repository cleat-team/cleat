package main

// `cleat vet --lang python` used to exit 1 having written nothing to either
// stream. Two sessions on two machines spent time on it independently and got
// the same message out of TestVetPython, because that message was a guess:
//
//	"cleat_sdk not importable, or another real tooling failure"
//
// It named one cause and hedged the rest, so two different environments printed
// the same sentence. These tests cover the parts that replaced the guess.

import (
	"fmt"
	"strings"
	"testing"
)

// TestATooOldInterpreterIsNamedAsTheCause — the traceback for this failure is
// accurate and unreadable. It ends at a line in signal_envelope.py and says
// nothing about interpreters.
func TestATooOldInterpreterIsNamedAsTheCause(t *testing.T) {
	stderr := `Traceback (most recent call last):
  File ".../cleat_sdk/signal_envelope.py", line 57, in <module>
    def decode_signal_envelope(raw: str) -> tuple[str, str] | None:
TypeError: unsupported operand type(s) for |: 'types.GenericAlias' and 'NoneType'`

	hint := pythonVetFailureHint(stderr)
	if hint == "" {
		t.Fatal("the most common python vet failure produced no hint at all")
	}
	for _, want := range []string{pythonSDKMinVersion, "PATH", "PEP 604"} {
		if !strings.Contains(hint, want) {
			t.Errorf("the hint does not mention %q:\n%s", want, hint)
		}
	}
}

func TestAMissingSDKIsNamedAsTheCause(t *testing.T) {
	hint := pythonVetFailureHint("ModuleNotFoundError: No module named 'cleat_sdk'")
	if !strings.Contains(hint, "PYTHONPATH") {
		t.Errorf("a missing cleat_sdk should point at PYTHONPATH:\n%s", hint)
	}
	if strings.Contains(hint, "PEP 604") {
		t.Errorf("a missing module should not be blamed on the interpreter version:\n%s", hint)
	}
}

// TestAnUnrecognisedFailureGetsNoHint is the control, and it is the point of
// the whole change.
//
// A hint offered for every failure is another guess presented as a diagnosis --
// exactly what was replaced. Silence is the correct output when there is
// nothing to add; the traceback is printed either way.
func TestAnUnrecognisedFailureGetsNoHint(t *testing.T) {
	for _, stderr := range []string{
		"",
		"Killed: 9",
		"PermissionError: [Errno 13] Permission denied: '/x/workflow.py'",
	} {
		if hint := pythonVetFailureHint(stderr); hint != "" {
			t.Errorf("an unrecognised failure was given a hint anyway:\nstderr: %q\nhint: %s",
				stderr, hint)
		}
	}
}

// TestVersionComparisonHandlesTheBoundaries — pythonAtLeast gates a skip, so
// getting it wrong either disables the test everywhere or leaves the red it was
// meant to remove.
func TestVersionComparisonHandlesTheBoundaries(t *testing.T) {
	for _, tc := range []struct {
		have string
		want bool
	}{
		{"3.9.6", false},
		{"3.10.0", true}, // the boundary itself is enough
		{"3.12.1", true},
		{"3.100.0", true}, // 100 > 10 numerically; a string compare says otherwise
		{"4.0.0", true},   // a newer major is newer
		{"2.7.18", false},
	} {
		var maj, min int
		if _, err := fmt.Sscanf(tc.have, "%d.%d", &maj, &min); err != nil {
			t.Fatalf("test setup: cannot parse %q: %v", tc.have, err)
		}
		got := versionAtLeast(maj, min, 3, 10)
		if got != tc.want {
			t.Errorf("python %s: got atLeast(3.10)=%v, want %v", tc.have, got, tc.want)
		}
	}
}
