package plugin

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A role-lookup driver that COUNTS its queries and can be held open.
//
// Counting is the point. "Both calls returned an error" cannot distinguish a
// retry from a cached failure, and "one pool came back" cannot distinguish
// single-flight from luck on a race that did not happen to fire this run. The
// number of role lookups is the observable that separates them.
type countingRoleDriver struct {
	queries atomic.Int64
	noRows  atomic.Bool

	// gate is read by driver goroutines and written by the test, so it is
	// guarded rather than plain. An earlier version of this file had it as a
	// bare field and -race caught the write in Cleanup against a query still
	// in flight -- a defect in the harness, which would have been reported as
	// a defect in the code under test.
	gateMu sync.Mutex
	gate   chan struct{}
}

func (d *countingRoleDriver) setGate(c chan struct{}) {
	d.gateMu.Lock()
	d.gate = c
	d.gateMu.Unlock()
}

func (d *countingRoleDriver) currentGate() chan struct{} {
	d.gateMu.Lock()
	defer d.gateMu.Unlock()
	return d.gate
}

var roleDriver = &countingRoleDriver{}

func (d *countingRoleDriver) Open(string) (driver.Conn, error) { return countingRoleConn{d}, nil }

type countingRoleConn struct{ d *countingRoleDriver }

// The conversion rather than countingRoleStmt{c.d}: both types are single-field
// wrappers over the same *countingRoleDriver, and gosimple's S1016 rejects the
// literal form.
func (c countingRoleConn) Prepare(string) (driver.Stmt, error) { return countingRoleStmt(c), nil }
func (c countingRoleConn) Close() error                        { return nil }
func (c countingRoleConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

type countingRoleStmt struct{ d *countingRoleDriver }

func (countingRoleStmt) Close() error                                 { return nil }
func (countingRoleStmt) NumInput() int                                { return -1 }
func (countingRoleStmt) Exec(_ []driver.Value) (driver.Result, error) { return nil, driver.ErrSkip }
func (s countingRoleStmt) Query(_ []driver.Value) (driver.Rows, error) {
	s.d.queries.Add(1)
	if g := s.d.currentGate(); g != nil {
		<-g
	}
	return &countingRoleRows{noRows: s.d.noRows.Load()}, nil
}

type countingRoleRows struct {
	done   bool
	noRows bool
}

func (r *countingRoleRows) Columns() []string { return []string{"role_name"} }
func (r *countingRoleRows) Close() error      { return nil }
func (r *countingRoleRows) Next(dest []driver.Value) error {
	if r.done || r.noRows {
		return io.EOF
	}
	r.done = true
	dest[0] = "tenant_role_x"
	return nil
}

func init() { sql.Register("cleat-plugin-counting-role", roleDriver) }

func newCountingPools(t *testing.T) (*TenantPools, *countingRoleDriver) {
	t.Helper()
	roleDriver.queries.Store(0)
	roleDriver.noRows.Store(false)
	roleDriver.setGate(nil)
	owner, err := sql.Open("cleat-plugin-counting-role", "")
	if err != nil {
		t.Fatalf("open owner: %v", err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	return NewTenantPools(owner, "host=localhost dbname=cleat sslmode=disable", 25,
		make([]byte, TenantRoleSecretMinBytes)), roleDriver
}

// cleat#1508: concurrent first-touches for ONE tenant opened one pool EACH and
// orphaned all but the last.
//
// An orphan is not in tp.pools, so Close() never reaches it and no eviction
// could; it is a live *sql.DB that will open up to maxConns connections on
// demand and hold them until the process exits. Measured before the fix at
// 20 distinct pools from 32 concurrent calls -- up to 475 connections from one
// tenant's first burst.
func TestOneTenantOpensOnePool(t *testing.T) {
	tp, d := newCountingPools(t)
	t.Cleanup(tp.Close)

	const n = 32
	var wg sync.WaitGroup
	got := make([]*sql.DB, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release them together, so they genuinely contend
			got[i], errs[i] = tp.For(context.Background(), "tenant-A")
		}(i)
	}
	close(start)
	wg.Wait()

	distinct := map[*sql.DB]bool{}
	for i, db := range got {
		if errs[i] != nil {
			t.Fatalf("For: %v", errs[i])
		}
		if db == nil {
			t.Fatal("For returned a nil pool and no error")
		}
		distinct[db] = true
	}
	if len(distinct) != 1 {
		t.Errorf("%d concurrent For() calls for one tenant produced %d distinct pools, want 1.\n\n"+
			"Only one is in tp.pools; the other %d are ORPHANED -- unreachable by Close(), "+
			"uncounted by any census, and each able to hold up to maxConns connections until "+
			"the process exits.", n, len(distinct), len(distinct)-1)
	}

	// THE SECOND OBSERVABLE, and it is the one that cannot pass by luck. A
	// single returned pool is consistent with a race that happened not to fire
	// on this run; one role lookup is not.
	if q := d.queries.Load(); q != 1 {
		t.Errorf("the role lookup ran %d times for one tenant, want 1.\n\n"+
			"Each extra lookup is a caller that went on to open its own pool. This is the "+
			"assertion that distinguishes single-flight from a race that did not fire.", q)
	}
}

// A failed open must not be cached, or one transient role-lookup error poisons
// the tenant for the lifetime of the process.
//
// The pre-cleat#1508 code had this property by accident -- it cached nothing on
// failure, so every call retried. Single-flight introduces a place to cache it,
// so the property now has to be kept deliberately.
func TestAFailedOpenIsNotCached(t *testing.T) {
	tp, d := newCountingPools(t)
	t.Cleanup(tp.Close)

	d.noRows.Store(true)
	if _, err := tp.For(context.Background(), "tenant-B"); err == nil {
		t.Fatal("For succeeded for a tenant with no provisioned role; it must fail closed (cleat#1307)")
	}
	if q := d.queries.Load(); q != 1 {
		t.Fatalf("first call made %d role lookups, want 1", q)
	}

	// The role is provisioned a moment later, as it would be by an operator.
	d.noRows.Store(false)
	db, err := tp.For(context.Background(), "tenant-B")
	if err != nil {
		t.Fatalf("the second call still failed: %v\n\n"+
			"A failed open was cached, so this tenant can never recover without a worker "+
			"restart -- a transient lookup error becomes permanent.", err)
	}
	if db == nil {
		t.Fatal("For returned a nil pool and no error")
	}
	if q := d.queries.Load(); q != 2 {
		t.Errorf("role lookups = %d, want 2 -- the second call must actually retry the "+
			"lookup rather than replay a cached failure", q)
	}
}

// A caller whose context is cancelled does not wait for another caller's open.
//
// The opener holds no lock, so a slow role lookup cannot block the map; but a
// waiter selecting only on the opener's completion would still be stuck behind
// a hung database with a cancelled context in its hand.
func TestAWaiterDoesNotOutliveItsContext(t *testing.T) {
	tp, d := newCountingPools(t)
	gate := make(chan struct{})
	d.setGate(gate)
	t.Cleanup(func() {
		close(gate)
		d.setGate(nil)
		tp.Close()
	})

	opened := make(chan struct{})
	go func() {
		defer close(opened)
		_, _ = tp.For(context.Background(), "tenant-C") // blocks in the gated lookup
	}()

	// Wait until the opener is actually inside the lookup, so this test is
	// about the waiter rather than about who got there first.
	deadline := time.Now().Add(2 * time.Second)
	for d.queries.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the opener never reached the role lookup")
		}
		time.Sleep(time.Millisecond)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := tp.For(ctx, "tenant-C")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("a waiter with a cancelled context got %v, want it to wrap context.Canceled.\n\n"+
			"Otherwise a caller that has given up is pinned to an unrelated caller's open, "+
			"which is exactly the case a hung database produces.", err)
	}
}
