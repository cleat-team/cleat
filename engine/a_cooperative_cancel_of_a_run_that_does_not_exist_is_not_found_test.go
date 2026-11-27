package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

// cleat#4503. A cooperative cancel (RequestCancellation, and the receipt form the HTTP handler calls) addressed to a run that matches no row answered success on all three dialects: the
// status read found no row, "fell through to the update, which matches nothing", and the update committed. The signal, the terminate and the pre-emptive cancel have answered
// ErrWorkflowNotFound for the same id since cleat#2213/#2227. With --require-auth=false nothing in front of the store reads the run first (callerOwnsTarget returns before it), so a client
// was told a cancel had been requested for a run that was not there.
//
// Real stores, every registered dialect. The rows are the id that exists nowhere, the same id through the receipt form, a run that belongs to ANOTHER tenant (which has to read exactly as
// a missing one does, cleat#2218), and the controls: a live run still accepts the request and is flagged by it, so the not-found below is the missing row's and not a verb that refuses
// everything.
func TestACooperativeCancelOfARunThatDoesNotExistIsNotFound(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			live := newIntentWorkflow(t, ctx, store, "coop-cancel-live")
			missing := fmt.Sprintf("no-such-run-4503-%d", time.Now().UnixNano())

			// CONTROL: the verb is not one that refuses everything. A live run accepts it and the flag is on the run.
			if flagged, _, err := store.CheckCancellation(ctx, live); err != nil || flagged {
				t.Fatalf("CONTROL: the live run is already flagged (%v, err %v) before the verb ran", flagged, err)
			}
			if err := store.RequestCancellation(ctx, live, "control"); err != nil {
				t.Fatalf("CONTROL: RequestCancellation to a live run = %v, want nil", err)
			}
			if flagged, _, err := store.CheckCancellation(ctx, live); err != nil || !flagged {
				t.Fatalf("CONTROL: RequestCancellation to a live run was accepted but the run is not flagged (%v, err %v)", flagged, err)
			}

			t.Run("the plain verb", func(t *testing.T) {
				err := store.RequestCancellation(ctx, missing, "no such run")
				if !errors.Is(err, ErrWorkflowNotFound) {
					t.Fatalf("RequestCancellation(%q), a run that exists nowhere = %v, want ErrWorkflowNotFound (it answered nil: a request stored for a run that is not there)", missing, err)
				}
			})

			t.Run("the receipt form the HTTP handler calls", func(t *testing.T) {
				rs, ok := store.(SettleReceiptStore)
				if !ok {
					t.Fatalf("%T is not a SettleReceiptStore", store)
				}
				_, err := rs.RequestCancellationReceipt(ctx, missing, "no such run")
				if !errors.Is(err, ErrWorkflowNotFound) {
					t.Fatalf("RequestCancellationReceipt(%q) = %v, want ErrWorkflowNotFound", missing, err)
				}
			})
		})
	}
}

// A run that belongs to another tenant reads as a run that does not exist (cleat#2218, cleat#2227): the cooperative cancel answers ErrWorkflowNotFound for it, and flags nothing.
func TestACooperativeCancelOfAnotherTenantsRunReadsAsNotFoundAndFlagsNothing(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			mt, ok := backend.(MultiTenantStoreBackend)
			if !ok {
				t.Fatalf("%s backend does not implement MultiTenantStoreBackend", backend.Name())
			}
			const tenantA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
			const tenantB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
			storeA, teardownA := mt.SetupForTenant(t, tenantA)
			defer teardownA()
			storeB, teardownB := mt.SetupForTenant(t, tenantB)
			defer teardownB()
			ctx := context.Background()

			def := &WorkflowDef{Name: "coop-cancel-foreign", Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1}
			if err := storeA.DeployWorkflowDef(ctx, def); err != nil {
				t.Fatalf("DeployWorkflowDef on store A: %v", err)
			}
			runA, _, err := storeA.StartNewRun(ctx, "", def.Name, 1, json.RawMessage(`{}`), "coop-cancel-foreign-1", tenantA, 0)
			if err != nil {
				t.Fatalf("StartNewRun on store A: %v", err)
			}

			err = storeB.RequestCancellation(ctx, runA, "not yours")
			if !errors.Is(err, ErrWorkflowNotFound) {
				t.Fatalf("tenant B's RequestCancellation to tenant A's run = %v, want ErrWorkflowNotFound, the answer for a run that does not exist", err)
			}
			if rs, ok := storeB.(SettleReceiptStore); ok {
				if _, err := rs.RequestCancellationReceipt(ctx, runA, "not yours"); !errors.Is(err, ErrWorkflowNotFound) {
					t.Fatalf("tenant B's RequestCancellationReceipt to tenant A's run = %v, want ErrWorkflowNotFound", err)
				}
			}
			if flagged, _, err := storeA.CheckCancellation(ctx, runA); err != nil || flagged {
				t.Fatalf("tenant B's refused cancel flagged tenant A's run (%v, err %v)", flagged, err)
			}

			// CONTROL: the owner is not refused by the same verb, so the answers above are about the tenant and not the id.
			if err := storeA.RequestCancellation(ctx, runA, "yours"); err != nil {
				t.Fatalf("CONTROL: tenant A's RequestCancellation to its own run = %v, want nil", err)
			}
			if flagged, _, err := storeA.CheckCancellation(ctx, runA); err != nil || !flagged {
				t.Fatalf("CONTROL: the owner's request was accepted but its run is not flagged (%v, err %v)", flagged, err)
			}
		})
	}
}
