package cleat

// cleat#936. See ParentClosePolicy.Valid.

import (
	"strings"
	"testing"
)

func TestTheThreeConstantsAreValid(t *testing.T) {
	for _, p := range []ParentClosePolicy{
		ParentClosePolicyAbandon, ParentClosePolicyTerminate, ParentClosePolicyRequestCancel,
	} {
		if !p.Valid() {
			t.Errorf("%q is one of this package's own constants and was refused", p)
		}
	}
}

// TestAnUnsetPolicyIsValid — ChildWorkflow sends "" for every caller that does
// not choose a policy. Refusing it would break the two-argument form entirely.
func TestAnUnsetPolicyIsValid(t *testing.T) {
	if !ParentClosePolicy("").Valid() {
		t.Error("the empty policy was refused; ChildWorkflow sends it for every " +
			"caller that does not set one")
	}
}

func TestAMisCasedPolicyIsNotValid(t *testing.T) {
	for _, p := range []ParentClosePolicy{"terminate", "Abandon", "Request_Cancel"} {
		if p.Valid() {
			t.Errorf("%q was accepted; MySQL's default collation matches it against a "+
				"policy arm and PostgreSQL does not (cleat#936)", p)
		}
	}
}

// TestTheErrorNamesTheChildAndTheLegalSet — the message is the whole reason for
// checking in the SDK at all. The engine already refuses this; what the SDK adds
// is knowing WHICH call was wrong.
func TestTheErrorNamesTheChildAndTheLegalSet(t *testing.T) {
	h := &HostCallsImpl{}
	_, err := h.ChildWorkflowWithOptions("checkout", "{}",
		ChildWorkflowOptions{ParentClosePolicy: "terminate"})
	if err == nil {
		t.Fatal("a mis-cased policy was accepted")
	}
	for _, want := range []string{"checkout", "terminate", "TERMINATE", "ABANDON", "REQUEST_CANCEL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

// TestTheCheckHappensBeforeTheHostCall — the refusal must precede the host
// call, or a workflow would record a child_workflow event for a child it then
// refuses to start, which is worse than either behaviour on its own.
func TestTheCheckHappensBeforeTheHostCall(t *testing.T) {
	called := false
	h := &HostCallsImpl{
		childWorkflowWithOptions: func(_, _ string, _ int, _ string, _ int) (string, error) {
			called = true
			return "run-1", nil
		},
	}
	if _, err := h.ChildWorkflowWithOptions("checkout", "{}",
		ChildWorkflowOptions{ParentClosePolicy: "terminate"}); err == nil {
		t.Fatal("a mis-cased policy was accepted")
	}
	if called {
		t.Error("the host call ran anyway; the refusal must come first")
	}

	// Control: a legal policy must still reach the host call, or the test above
	// would pass against an SDK that refused everything.
	if _, err := h.ChildWorkflowWithOptions("checkout", "{}",
		ChildWorkflowOptions{ParentClosePolicy: ParentClosePolicyTerminate}); err != nil {
		t.Fatalf("a legal policy was refused: %v", err)
	}
	if !called {
		t.Error("a legal policy did not reach the host call")
	}
}
