package engine

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// undialedDB is a *sql.DB that never dials, so eviction can be tested without
// a database. It reuses deadConnector from the batch-flush tests.
//
// Close() still does what matters here -- it marks the *sql.DB closed, and
// every later operation reports "sql: database is closed" -- which is exactly
// the failure a store holding an evicted pool would meet.
func undialedDB() *sql.DB { return sql.OpenDB(deadConnector{}) }

// isClosed reports whether db has been Close()d, distinguishing that from the
// connector's own refusal to dial. Without the distinction this helper would
// answer "yes" for an open pool too, and every assertion built on it would be
// vacuous.
func isClosed(db *sql.DB) bool {
	err := db.PingContext(context.Background())
	return err != nil && strings.Contains(err.Error(), "database is closed")
}

// waitClosed polls because evictIdleLeasedPools closes asynchronously, on
// purpose: (*sql.DB).Close waits for in-use connections, so closing under the
// factory's mutex would block every other tenant's OpenStore behind one
// tenant's in-flight query. A sleep long enough to be reliable here would be
// measuring the scheduler; a poll with a deadline measures the close.
func waitClosed(t *testing.T, db *sql.DB) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if isClosed(db) {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

// A pool a store still holds is not evicted, however idle it looks.
//
// THIS IS THE PROPERTY THE LEASE EXISTS FOR, and an idle timestamp alone
// cannot supply it. A workflow can sit inside one activity for an hour without
// touching the database, so "nothing has opened a store for this tenant
// recently" says nothing about whether one is in use. Evicting on the
// timestamp alone would close the pool under a live run, whose next query then
// fails with "sql: database is closed" -- turning a memory tidy-up into a
// workflow failure.
func TestALeasedTenantPoolIsNotEvicted(t *testing.T) {
	held, idle := uuid.NewString(), uuid.NewString()
	heldDB, idleDB := undialedDB(), undialedDB()

	now := time.Now()
	clock := func() time.Time { return now }
	f := &MySQLStoreFactory{
		tenantDBs:          map[string]*leasedPool{},
		tenantPoolMaxConns: 25,
		clock:              clock,
	}
	f.tenantDBs[held] = newLeasedPool(heldDB, now)
	f.tenantDBs[idle] = newLeasedPool(idleDB, now)

	// One tenant has a store out; the other does not. Both are then aged well
	// past any window.
	store, lease, err := f.OpenStore(context.Background(), held)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if store == nil {
		t.Fatal("OpenStore returned no store and no error")
	}
	now = now.Add(24 * time.Hour)

	if n := f.EvictIdle(time.Minute); n != 1 {
		t.Fatalf("EvictIdle closed %d pools, want exactly 1 -- the unleased one", n)
	}
	if _, still := f.tenantDBs[held]; !still {
		t.Error("a pool with a store still out was evicted. Whatever holds that store -- " +
			"a workflow inside a long activity, most often -- fails on its next query with " +
			"\"sql: database is closed\".")
	}
	if isClosed(heldDB) {
		t.Error("the leased pool's *sql.DB was closed")
	}
	// The control. Without it every assertion above passes against an
	// EvictIdle that evicts nothing at all.
	if _, gone := f.tenantDBs[idle]; gone {
		t.Error("the unleased, day-old pool was kept, so this test proves nothing about the leased one")
	}
	if !waitClosed(t, idleDB) {
		t.Error("the evicted pool's *sql.DB was never closed, so the map entry went and the " +
			"connection-opener goroutine behind it stayed -- the leak, minus the evidence")
	}

	// Releasing makes it collectable, which is the other half of the contract:
	// a lease defers eviction, it does not cancel it.
	if err := lease.Close(); err != nil {
		t.Fatalf("releasing the lease: %v", err)
	}
	now = now.Add(24 * time.Hour)
	if n := f.EvictIdle(time.Minute); n != 1 {
		t.Errorf("EvictIdle closed %d pools after the lease was released, want 1", n)
	}
	if got := f.TenantPoolCount(); got != 0 {
		t.Errorf("%d pools remain, want 0", got)
	}
}

// Releasing a lease twice must not decrement the count twice.
//
// A caller that both defers Close and closes explicitly -- or a wrapper that
// closes what it was handed and what it built -- would otherwise drive the
// count below zero. A negative count reads as "nobody is using this" to the
// sweep while a live store is still writing through the pool: the exact
// failure the lease prevents, reached through the tidiest-looking call site.
func TestReleasingALeaseTwiceIsNotTwoReleases(t *testing.T) {
	tenant := uuid.NewString()
	now := time.Now()
	f := &MSSQLStoreFactory{
		tenantDBs:          map[string]*leasedPool{tenant: newLeasedPool(undialedDB(), now)},
		tenantPoolMaxConns: 25,
		clock:              func() time.Time { return now },
	}

	ctx := context.Background()
	if _, first, err := f.OpenStore(ctx, tenant); err != nil {
		t.Fatalf("OpenStore: %v", err)
	} else {
		_ = first.Close()
		_ = first.Close()
		_ = first.Close()
	}
	// A second store is still out, so the pool must survive.
	_, second, err := f.OpenStore(ctx, tenant)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	now = now.Add(24 * time.Hour)
	if n := f.EvictIdle(time.Minute); n != 0 {
		t.Fatalf("EvictIdle closed %d pools while a store was still out, want 0. "+
			"Three closes of one lease released three leases.", n)
	}

	// The control: one release of the one outstanding lease is enough. The
	// clock moves again first because releasing stamps the pool as used -- see
	// TestUsingATenantPoolKeepsItFromBeingReaped, which is that stamp's own
	// test.
	_ = second.Close()
	now = now.Add(time.Hour)
	if n := f.EvictIdle(time.Minute); n != 1 {
		t.Errorf("EvictIdle closed %d pools once every lease was released, want 1 -- "+
			"the assertion above would pass just as well against a pool that can never "+
			"be evicted at all", n)
	}
}

// Opening a store keeps its pool out of the sweep's way, and so does releasing
// one.
//
// The release stamp is the half that is easy to omit. Without it a workflow
// that has just finished an hour-long run leaves a pool whose last stamp is an
// hour old, so the first sweep after the lease drops closes it -- and the next
// workflow for that tenant, which is usually seconds away, pays a full
// reconnect for a pool that was in use moments ago.
func TestUsingATenantPoolKeepsItFromBeingReaped(t *testing.T) {
	tenant := uuid.NewString()
	now := time.Now()
	f := &MySQLStoreFactory{
		tenantDBs:          map[string]*leasedPool{tenant: newLeasedPool(undialedDB(), now)},
		tenantPoolMaxConns: 25,
		clock:              func() time.Time { return now },
	}

	// A long run: opened now, released an hour later.
	_, lease, err := f.OpenStore(context.Background(), tenant)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	now = now.Add(time.Hour)
	_ = lease.Close()

	if n := f.EvictIdle(15 * time.Minute); n != 0 {
		t.Errorf("a pool released moments ago was evicted (%d). Its last OPEN was an hour "+
			"back, so without a stamp on release it looks an hour idle the instant the "+
			"lease drops.", n)
	}

	// The control. It is not simply un-evictable.
	now = now.Add(time.Hour)
	if n := f.EvictIdle(15 * time.Minute); n != 1 {
		t.Errorf("EvictIdle closed %d pools an hour after the release, want 1", n)
	}
}

// A non-positive window evicts nothing, matching plugin.TenantPools.EvictIdle.
//
// "Idle for zero seconds" describes every pool including one handed out
// microseconds ago, so taking it literally is a stall dressed as a policy --
// and a zero here is far likelier to be an unset config value than a request
// to close everything.
func TestANonPositiveIdleWindowEvictsNothing(t *testing.T) {
	tenant := uuid.NewString()
	now := time.Now()
	f := &MySQLStoreFactory{
		tenantDBs:          map[string]*leasedPool{tenant: newLeasedPool(undialedDB(), now.Add(-24*time.Hour))},
		tenantPoolMaxConns: 25,
		clock:              func() time.Time { return now },
	}
	for _, d := range []time.Duration{0, -time.Second, -time.Hour} {
		if n := f.EvictIdle(d); n != 0 {
			t.Errorf("EvictIdle(%v) closed %d pools, want 0", d, n)
		}
	}
	// The control: the pool really is old enough to go, so the zeroes above
	// are the window's doing and not the fixture's.
	if n := f.EvictIdle(time.Minute); n != 1 {
		t.Errorf("EvictIdle(1m) closed %d pools, want 1", n)
	}
}

// Both per-tenant-pool factories are reapers, and PostgresStoreFactory is not.
//
// The negative half is the point. Postgres shares one *sql.DB across tenants,
// so there is nothing per-tenant to reap; implementing the interface as a
// no-op would make "nothing to reap" and "reaped nothing this time"
// indistinguishable to cmd/cleat-worker, which uses the type assertion to
// decide whether to launch the reaper loop at all.
func TestOnlyPerTenantPoolFactoriesAreReapers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		factory StoreFactory
		want    bool
	}{
		{"mysql", &MySQLStoreFactory{}, true},
		{"mssql", &MSSQLStoreFactory{}, true},
		{"postgres", &PostgresStoreFactory{}, false},
	} {
		if _, got := tc.factory.(TenantPoolReaper); got != tc.want {
			t.Errorf("%s: TenantPoolReaper = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// tenantPoolDB is getOrCreateTenantPool's *sql.DB.
//
// The factory's map holds *leasedPool now, and production code reaches the
// handle through OpenStore -- where the lease is the point. These tests run
// statements directly against a tenant's pool to observe SESSION_CONTEXT and
// RLS, which is a test concern, so the unwrapping lives here rather than as a
// second method on the factory that only tests would call.
//
// NO LEASE IS TAKEN. Nothing sweeps a factory inside a test, and a test that
// held one would be asserting less: it could not observe an eviction at all.
func tenantPoolDB(ctx context.Context, f *MSSQLStoreFactory, tenantID string) (*sql.DB, error) {
	p, err := f.getOrCreateTenantPool(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	return p.db, nil
}
