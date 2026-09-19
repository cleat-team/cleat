package main

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// reapableFactory is a StoreFactory that counts the eviction sweeps it is
// asked for. It implements engine.TenantPoolReaper.
type reapableFactory struct {
	recordingFactory
	sweeps  int
	windows []time.Duration
	evicts  int
}

func (f *reapableFactory) EvictIdle(maxIdle time.Duration) int {
	f.sweeps++
	f.windows = append(f.windows, maxIdle)
	return f.evicts
}
func (f *reapableFactory) TenantPoolCount() int { return 0 }

// plainFactory is a StoreFactory with no per-tenant pools, like
// engine.PostgresStoreFactory.
type plainFactory struct{ recordingFactory }

func (f *plainFactory) OpenStore(ctx context.Context, tenantID string, q ...string) (engine.WorkflowStore, io.Closer, error) {
	return f.recordingFactory.OpenStore(ctx, tenantID, q...)
}

// The reaper reaps the STORE FACTORY's pools, not only plugin.TenantPools.
//
// # The defect
//
// plugin.TenantPools is built solely under --tenant-isolation=role, which
// resolveTenantIsolation permits on PostgreSQL only -- see
// TestTenantPoolsAreBuiltOnlyUnderRoleIsolation. The reaper loop was launched
// on `w.tenantPools != nil`, so it never ran on MySQL or SQL Server: precisely
// the two dialects whose store factory keeps a connection pool per tenant,
// because a tenant's database (MySQL) or its SESSION_CONTEXT (SQL Server)
// cannot be shared across one. A worker there held a *sql.DB -- and the
// connection-opener goroutine database/sql runs behind it -- for every tenant
// it had ever served, until the process ended.
//
// The asymmetry was invisible from either side: the role pools had a reaper
// and looked complete, the store pools had none and nothing said so.
func TestTheStoreFactorysPoolsAreReapedToo(t *testing.T) {
	const window = 15 * time.Minute

	t.Run("a factory with per-tenant pools is swept", func(t *testing.T) {
		probe := &tenantScopeProbe{}
		w := newTenantScopeWorker(t, tenantSelf, probe)
		defer w.cancel()
		f := &reapableFactory{evicts: 3}
		w.storeFactory = f

		if !w.hasReapablePools() {
			t.Fatal("a worker whose factory holds per-tenant pools reported nothing to reap, " +
				"so the reaper loop would never be launched for it at all")
		}
		if got := w.reapIdleTenantPools(window); got != 3 {
			t.Errorf("reaped %d pools, want the factory's 3", got)
		}
		if f.sweeps != 1 {
			t.Errorf("the factory was asked to sweep %d times, want 1", f.sweeps)
		}
		if len(f.windows) != 1 || f.windows[0] != window {
			t.Errorf("the factory was swept with %v, want the loop's own window %v", f.windows, window)
		}
	})

	t.Run("a factory that shares one pool is not", func(t *testing.T) {
		// The control. Without it the assertion above passes against a worker
		// that reports every factory as reapable and launches a loop that
		// ticks forever over nothing -- a health-tracked goroutine reporting
		// success for doing no work.
		probe := &tenantScopeProbe{}
		w := newTenantScopeWorker(t, tenantSelf, probe)
		defer w.cancel()
		w.storeFactory = &plainFactory{}

		if w.hasReapablePools() {
			t.Error("a worker with no per-tenant pools reported something to reap")
		}
		if got := w.reapIdleTenantPools(window); got != 0 {
			t.Errorf("reaped %d pools from a factory that has none", got)
		}
	})

	t.Run("no factory at all", func(t *testing.T) {
		probe := &tenantScopeProbe{}
		w := newTenantScopeWorker(t, tenantSelf, probe)
		defer w.cancel()
		w.storeFactory = nil

		if w.hasReapablePools() {
			t.Error("a worker with no factory reported something to reap")
		}
		// A nil factory must not panic the sweep: the loop is not launched in
		// this configuration, but the watchdog can restart a loop and the
		// decision is read in both places.
		if got := w.reapIdleTenantPools(window); got != 0 {
			t.Errorf("reaped %d pools with no factory", got)
		}
	})
}
