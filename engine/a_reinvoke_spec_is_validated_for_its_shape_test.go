package engine

import (
	"strings"
	"testing"
)

// cleat#4152, G3: validate()'s Reinvoke rule is load-bearing, and this test is what keeps it so.
//
// step() applies the stop and suspension refusals only to a non-Pure spec, and a Reinvoke step goes through them before Fresh, so a Reinvoke spec that is Pure, that
// is a multi-record step, that is not recordsOne, or that lists no RecordsNothingOn path would run its function live in a stopping or suspended session, which is the
// hole #4330 found in the old plugin_call. Disabling the rule left every test green until cleat-review3 asked for this one (cleat#4333).
//
// Each forbidden shape is a spec built to reach THIS rule and no other, refused AND asserted by text, so a spec refused for another reason is a failure here and the
// rule under test cannot decide nothing without it showing. The well-formed spec is the control: each refusal is attributable to the one thing changed.
func TestAReinvokeSpecIsValidatedForItsShape(t *testing.T) {
	var ran int32
	const rule = "has Reinvoke, which needs a single-record recordsOne step that is not Pure and lists RecordsNothingOn"
	reinvoke := func(*stepCtx, *EventRecord) bool { return true }

	good := func() *stepSpec {
		sp := fakeSpec(effectEffect, recordsOne, &ran, recordOneFake)
		sp.Reinvoke, sp.RecordsNothingOn = reinvoke, []string{"the call ran again and records nothing"}
		return sp
	}
	// CONTROL: the well-formed Reinvoke spec validates, so every refusal below is the one thing changed.
	if err := good().validate(); err != nil {
		t.Fatalf("PRECONDITION FAILED: the well-formed Reinvoke spec does not validate: %v", err)
	}

	var d, p []EventRecord
	for _, tc := range []struct {
		name string
		sp   func() *stepSpec
	}{
		{"a multi-record step (it has a Role)", func() *stepSpec {
			sp := runSpec(&ran, &d, &p)
			sp.Records = recordsOne // the Role is the thing under test, so the shape rule about recordsNone and the rest stay satisfied
			sp.Reinvoke, sp.RecordsNothingOn = reinvoke, []string{"the call ran again and records nothing"}
			return sp
		}},
		{"a step that does not declare recordsOne", func() *stepSpec {
			sp := good()
			sp.Records = recordsMany
			return sp
		}},
		{"a Pure step", func() *stepSpec {
			// recordsOne, NOT recordsNone: with recordsNone the not-recordsOne clause refuses this spec first and the Pure clause decides nothing (the first version
			// of this fixture did exactly that, and removing the Pure clause left the test green). Pure with recordsOne is a shape a later rule also refuses, by
			// other text, so only THIS rule's text passes the assertion below.
			sp := fakeSpec(effectPure, recordsOne, &ran, recordOneFake)
			sp.Reinvoke, sp.RecordsNothingOn = reinvoke, []string{"the call ran again and records nothing"}
			return sp
		}},
		{"no RecordsNothingOn path", func() *stepSpec {
			sp := good()
			sp.RecordsNothingOn = nil
			return sp
		}},
	} {
		err := tc.sp().validate()
		switch {
		case err == nil:
			t.Errorf("validate accepted a Reinvoke spec that is %s", tc.name)
		case !strings.Contains(err.Error(), rule):
			t.Errorf("a Reinvoke spec that is %s was refused for the wrong reason, so the Reinvoke rule decided nothing: %v", tc.name, err)
		}
	}
}
