package main

import "testing"

// cleat#3571. Another tenant's beat uses the isolated heartbeat pool only where
// the store can be re-scoped to a tenant per call, which is PostgreSQL and SQL
// Server. Anything else -- MySQL, whose tenant is the database the pool was
// opened on; a sharded store, which routes by tenant; no isolated pool at all --
// must go on asking the tenant's own store, or the run is beaten through a store
// whose tenant is not its own and reported lost (cleat#3566).
//
// A recording store stands in for "a store type that cannot be re-scoped". These
// are the routing half; the real-database half, which shows the re-scoped beat
// lands under row-level security with the execution pool saturated, is
// a_tenants_beat_does_not_queue_behind_the_execution_pool_test.go.
func TestAnotherTenantsBeatFallsBackToItsOwnStoreWhereThePoolCannotBeRescoped(t *testing.T) {
	for _, tc := range []struct {
		name        string
		withHBStore bool
	}{
		{"no isolated heartbeat pool", false},
		{"an isolated pool whose store cannot be re-scoped", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			own := newHeartbeatProbe(nil, nil)
			other := newHeartbeatProbe(nil, nil)
			w := newTestWorker(own.mockStore)
			defer w.cancel()
			w.storeTenantID = tenantSelf
			factory := &recordingFactory{store: other.mockStore}
			w.storeFactory = factory
			var pool *heartbeatProbe
			if tc.withHBStore {
				pool = newHeartbeatProbe(nil, nil)
				w.heartbeatStore = pool.mockStore
			}

			registerTenantExecution(w, "own-run", tenantSelf, 1)
			registerTenantExecution(w, "other-run", tenantOther, 1)
			if landed := w.heartbeatAndFenceInFlight(); !landed {
				t.Fatal("every group was answered, so the call must report that it landed")
			}

			if got := other.asked(); !sameSet(got, []string{"other-run"}) {
				t.Errorf("the other tenant's own store was asked about %v, want [other-run]", got)
			}
			if got := factory.openedTenants(); len(got) != 1 || got[0] != tenantOther {
				t.Errorf("the factory opened %v, want exactly [%s]", got, tenantOther)
			}
			if tc.withHBStore {
				// The worker's own tenant still uses the pool, and only it: a store
				// that cannot be re-scoped must not be asked about another tenant.
				if got := pool.asked(); !sameSet(got, []string{"own-run"}) {
					t.Errorf("the heartbeat pool's store was asked about %v, want only the worker's own [own-run]", got)
				}
				if got := own.asked(); len(got) != 0 {
					t.Errorf("the worker's execution store was asked about %v although a heartbeat pool is wired", got)
				}
			} else if got := own.asked(); !sameSet(got, []string{"own-run"}) {
				t.Errorf("the worker's own store was asked about %v, want [own-run]", got)
			}
		})
	}
}
