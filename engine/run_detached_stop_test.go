package engine

import (
	"context"
	"testing"
)

// TestADeferSegmentDoesNotStartADetachedWorkflow is IMPROVEMENT-PLAN 3.111's
// remaining half, first item.
//
// RunDetached calls the SAME store method childWorkflowWithVersion calls --
// StartChildWorkflow -- and leaves the same claimable workflow_instances row
// behind. That one has been refused in a defer segment since 3.84 and this one
// was not, two functions apart in engine/children.go. So a terminated
// workflow's cleanup pass could still create live work, through the identical
// call, by asking for it under the other name.
//
// This is a direct session test rather than a WASM fixture on purpose: Go
// guests cannot reach cleat_run_detached at all. It is imported by the Rust,
// Java and AssemblyScript SDKs only -- cleat.HostCalls.RunDetached is a
// different thing that runs a closure and never touches the import -- so there
// is no Go entry point to write a fixture against, and a fixture in another
// language would put a tier-2 toolchain between this assertion and the defect.
func TestADeferSegmentDoesNotStartADetachedWorkflow(t *testing.T) {
	ctx := context.Background()

	newSession := func(deferPhase bool) (*execSession, *countingChildStore) {
		children := &countingChildStore{}
		opts := []EngineOption{WithChildWorkflowStore(children)}
		if deferPhase {
			opts = append(opts, WithDeferPhase())
		}
		return &execSession{engine: NewEngine(nil, &mockCaller{}, opts...)}, children
	}

	// The control comes FIRST, and it is not decoration. Without it, an
	// assertion that no detached run started passes just as well against a
	// RunDetached that never starts one -- a broken store, a nil guard, a
	// method that returns early for an unrelated reason. It has to be shown
	// working before its refusal means anything.
	t.Run("outside a defer segment it starts one", func(t *testing.T) {
		s, children := newSession(false)
		if got := s.RunDetached(ctx, nil, "billing-retry", `{}`); got != 0 {
			t.Fatalf("RunDetached = %d, want 0 -- the ordinary path is broken, so the "+
				"refusal asserted below would prove nothing", got)
		}
		if len(children.started) != 1 || children.started[0] != "billing-retry" {
			t.Fatalf("started = %v, want [billing-retry]", children.started)
		}
	})

	t.Run("inside a defer segment it is refused", func(t *testing.T) {
		s, children := newSession(true)
		got := s.RunDetached(ctx, nil, "billing-retry", `{}`)
		if got != callSuspendSentinel {
			t.Errorf("RunDetached = %#x, want callSuspendSentinel (%#x).\n\n"+
				"A terminated workflow's cleanup pass started a detached run. The guest "+
				"decodes this as a simple result, in which bit 31 is not a field, so a "+
				"missing sentinel is errCode 0 -- an ordinary success -- and the guest "+
				"carries on. See IMPROVEMENT-PLAN 3.111.", got, callSuspendSentinel)
		}
		if len(children.started) != 0 {
			t.Errorf("started = %v, want none.\n\nThe row outlives the segment: it is a "+
				"claimable workflow_instances row created by a workflow that has already "+
				"terminated.", children.started)
		}
	})

	// The defer bodies' own calls must still go through, or the stop has
	// swallowed the cleanup it exists to run -- 3.81 measured that refusing them
	// CONSUMES the defer table rather than skipping it. inDeferDrain is what the
	// host sets around its own call to the guest's defer runner.
	t.Run("a defer body inside the segment is still allowed", func(t *testing.T) {
		s, children := newSession(true)
		s.setDeferDrain(true)
		if got := s.RunDetached(ctx, nil, "cleanup-child", `{}`); got != 0 {
			t.Fatalf("RunDetached = %#x during the defer drain, want 0.\n\n"+
				"The stop reached a defer body's own call. A segment that refuses "+
				"everything passes the assertion above while destroying the cleanup.", got)
		}
		if len(children.started) != 1 {
			t.Fatalf("started = %v, want [cleanup-child]", children.started)
		}
	})
}
