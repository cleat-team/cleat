package engine

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// cleat#2218's EXISTS-gated INSERT (engine/store_signals.go, engine/mysql_store.go,
// engine/mssql_signals_promises.go) closes an existence oracle DeliverSignal
// had on every dialect: before it, a NONEXISTENT workflow id threw a
// foreign-key error while a FOREIGN id -- one that exists, just under another
// tenant -- returned nil, either silently overwriting the target (pre-3.215)
// or writing a harmless orphan row under the caller's own tenant (post-3.215,
// pre-2218). Error-versus-nil told a caller which was true; a webhook-driven
// signal endpoint turns that into a way to enumerate real workflow ids.
//
// cleat#2218's fix made both cases write nothing -- and also made both
// return nil, which traded one oracle for a different failure: a signal
// that silently found nothing looked exactly like a signal that was
// delivered, so eventtriggers' awaiter never learned its target was gone
// and never unregistered (cleat#2213). cleat#2227 makes the "nothing
// written" case loud again, WITHOUT reopening the oracle it replaces:
// RowsAffected()==0 on the EXISTS-gated INSERT now returns
// ErrWorkflowNotFound identically whether the id is foreign or genuinely
// nonexistent, so the two remain indistinguishable from each other -- only
// "found" versus "not found" is now visible, not which reason.
//
// engine/mssql_admin_login_control_plane_tenant_test.go proves the same
// guarantee under SQL Server's cleat_admin bypass role specifically, which
// this file does not exercise (Postgres and MySQL have no equivalent
// concept). This file proves the dialect-independent half: that
// "nonexistent" and "foreign tenant" both return ErrWorkflowNotFound and
// write nothing, on every registered backend, through the SAME ordinary
// tenant-scoped store every non-admin caller actually uses.
func TestDeliverSignalToAnIDTheCallerCannotTouchWritesNothing(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			if !backend.Enabled() {
				t.Skipf("%s backend not enabled", backend.Name())
			}
			mtBackend, ok := backend.(MultiTenantStoreBackend)
			if !ok {
				// Unreachable with the current backend set -- see the
				// file-level comment on tenant_isolation_test.go's identical
				// tripwire. Kept as a Fatal, not a Skip: a backend
				// registered without SetupForTenant would mean this
				// guarantee goes untested for it, silently.
				t.Fatalf("BUG: %s backend does not implement MultiTenantStoreBackend (SetupForTenant)", backend.Name())
			}

			const tenantA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
			const tenantB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
			storeA, teardownA := mtBackend.SetupForTenant(t, tenantA)
			defer teardownA()
			storeB, teardownB := mtBackend.SetupForTenant(t, tenantB)
			defer teardownB()

			ctx := context.Background()

			def := &WorkflowDef{
				Name:       "signal-existence-oracle",
				Version:    1,
				WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1,
				MinVersion: 1,
			}
			if err := storeA.DeployWorkflowDef(ctx, def); err != nil {
				t.Fatalf("DeployWorkflowDef on store A: %v", err)
			}

			runIDA, _, err := storeA.StartNewRun(ctx, "", def.Name, 1,
				json.RawMessage(`{}`), "existence-oracle-a-1", tenantA, 0)
			if err != nil {
				t.Fatalf("StartNewRun on store A: %v", err)
			}

			const sig = "approve"

			// FOREIGN ID: exists, just not under tenant B.
			t.Run("ForeignID", func(t *testing.T) {
				if err := storeB.DeliverSignal(ctx, runIDA, sig, `{"from":"tenant-b"}`); !errors.Is(err, ErrWorkflowNotFound) {
					t.Fatalf("DeliverSignal on a foreign-tenant id = %v, want ErrWorkflowNotFound "+
						"(cleat#2227) -- anything else either reopens the existence oracle cleat#2218 "+
						"closed, or silently writes nothing the way cleat#2213 leaked awaiters", err)
				}
				if _, ok, err := storeB.PollSignal(ctx, runIDA, sig); err != nil {
					t.Fatalf("tenant B PollSignal(runIDA): %v", err)
				} else if ok {
					t.Errorf("tenant B's cross-tenant delivery left a row IT can see -- " +
						"the EXISTS gate did not fire")
				}
				if _, ok, err := storeA.PollSignal(ctx, runIDA, sig); err != nil {
					t.Fatalf("tenant A PollSignal(runIDA): %v", err)
				} else if ok {
					t.Errorf("tenant B's cross-tenant delivery wrote a row under tenant A -- " +
						"it must write NOTHING, not even under the row's own owner")
				}
			})

			// NONEXISTENT ID: does not exist under any tenant.
			t.Run("NonexistentID", func(t *testing.T) {
				const noSuchID = "signal-oracle-does-not-exist"
				if err := storeB.DeliverSignal(ctx, noSuchID, sig, `{"from":"tenant-b"}`); !errors.Is(err, ErrWorkflowNotFound) {
					t.Fatalf("DeliverSignal on a nonexistent id = %v, want ErrWorkflowNotFound (cleat#2227) "+
						"-- identical to the ForeignID case above, which is the point: neither reopens "+
						"the FK-error existence oracle cleat#2218 closed", err)
				}
				if _, ok, err := storeB.PollSignal(ctx, noSuchID, sig); err != nil {
					t.Fatalf("PollSignal(noSuchID): %v", err)
				} else if ok {
					t.Errorf("a nonexistent id left a row the caller can see -- the EXISTS gate did not fire")
				}
			})

			// Positive control. Without it, a DeliverSignal that had stopped
			// writing ANYTHING would pass both cases above.
			t.Run("OwnID", func(t *testing.T) {
				if err := storeA.DeliverSignal(ctx, runIDA, sig, `{"from":"tenant-a"}`); err != nil {
					t.Fatalf("DeliverSignal on the caller's own workflow: %v", err)
				}
				d, ok, err := storeA.PollSignal(ctx, runIDA, sig)
				if err != nil {
					t.Fatalf("tenant A PollSignal(runIDA): %v", err)
				}
				if !ok {
					t.Fatalf("a delivery to the caller's OWN workflow wrote nothing -- " +
						"the EXISTS gate is refusing legitimate writes, not just illegitimate ones")
				}
				if d.Payload != `{"from":"tenant-a"}` {
					t.Errorf("payload = %q, want %q", d.Payload, `{"from":"tenant-a"}`)
				}
			})
		})
	}
}
