package engine

import (
	"context"
	"encoding/json"
	"testing"
)

// cleat#4102's mandatory control. signal_workflow now tells the guest when the store refused the delivery, and one of the refusals is
// "the target is not there". cleat#2218 and cleat#2227 made a target that does not exist and a target that belongs to ANOTHER TENANT
// return the same error from DeliverSignal on purpose: a different answer would let a workflow enumerate other tenants' run ids. The new
// guest-visible code must not reopen that, so this drives the real SignalWorkflow, over the real tenant-scoped store every non-admin
// caller uses, at both targets and requires the SAME result word and the SAME recorded outcome.
//
// The positive control is the caller's own workflow. Without it, a SignalWorkflow that reported "not found" for everything would pass
// both of the cases above.
func TestASignalToAForeignOrNonexistentTargetReportsOneOutcome(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			if !backend.Enabled() {
				t.Skipf("%s backend not enabled", backend.Name())
			}
			mtBackend, ok := backend.(MultiTenantStoreBackend)
			if !ok {
				// Kept as a Fatal, not a Skip, for the reason the sibling test gives: a backend without SetupForTenant would leave this
				// guarantee untested for it, silently.
				t.Fatalf("BUG: %s backend does not implement MultiTenantStoreBackend (SetupForTenant)", backend.Name())
			}

			const tenantA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
			const tenantB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
			storeA, teardownA := mtBackend.SetupForTenant(t, tenantA)
			defer teardownA()
			storeB, teardownB := mtBackend.SetupForTenant(t, tenantB)
			defer teardownB()

			ctx := context.Background()
			def := &WorkflowDef{Name: "signal-send-outcome", Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1}
			if err := storeA.DeployWorkflowDef(ctx, def); err != nil {
				t.Fatalf("DeployWorkflowDef on store A: %v", err)
			}
			runIDA, _, err := storeA.StartNewRun(ctx, "", def.Name, 1, json.RawMessage(`{}`), "signal-send-outcome-a-1", tenantA, 0)
			if err != nil {
				t.Fatalf("StartNewRun on store A: %v", err)
			}

			// send runs ONE fresh signal_workflow from a workflow whose signal store is `from`, aimed at `target`.
			send := func(from SignalStore, target string) (int64, EventRecord) {
				t.Helper()
				s := newTestExecSession()
				s.engine.signalStore = from
				got := s.SignalWorkflow(ctx, nil, target, "approve", `{"from":"tenant-b"}`)
				if len(s.history) != 1 {
					t.Fatalf("a send to %q recorded %d events, want 1", target, len(s.history))
				}
				return got, s.history[0]
			}
			sigB, ok := storeB.(SignalStore)
			if !ok {
				t.Fatalf("%s store does not satisfy SignalStore", backend.Name())
			}
			sigA := storeA.(SignalStore)

			foreignCode, foreign := send(sigB, runIDA) // exists, under tenant A
			noneCode, none := send(sigB, "signal-send-outcome-does-not-exist")
			if foreignCode != errSignalTargetNotFoundInt {
				t.Fatalf("a send to another tenant's run returned %d, want errSignalTargetNotFoundInt (%d)", foreignCode, errSignalTargetNotFoundInt)
			}
			if noneCode != foreignCode {
				t.Fatalf("a send to a nonexistent run returned %d and a send to another tenant's run returned %d: the guest can now tell them "+
					"apart, which reopens the existence oracle cleat#2218 and cleat#2227 closed", noneCode, foreignCode)
			}
			if foreign.Response != none.Response || foreign.Response != signalOutcomeNotFound {
				t.Fatalf("recorded outcomes differ or are not %q: foreign %q, nonexistent %q", signalOutcomeNotFound, foreign.Response, none.Response)
			}

			// Positive control: the caller's own workflow is delivered to, reported as success, and recorded with no outcome.
			ownCode, own := send(sigA, runIDA)
			if ownCode != 0 || own.Response != "" {
				t.Fatalf("a send to the caller's own workflow returned %d with outcome %q, want 0 and none: not-found is being reported for "+
					"everything", ownCode, own.Response)
			}
			if _, found, err := storeA.PollSignal(ctx, runIDA, "approve"); err != nil || !found {
				t.Fatalf("PollSignal after a successful send = found %v, err %v: the signal was reported delivered and is not in the store", found, err)
			}
		})
	}
}
