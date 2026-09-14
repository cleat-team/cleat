package plugin

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"strings"
	"testing"
)

// fakeConnector provides a valid driver.Connector that returns an error
// on Connect — sufficient to exercise Close() without a real database.
type fakeConnector struct{}

func (c *fakeConnector) Connect(ctx context.Context) (driver.Conn, error) {
	return nil, driver.ErrBadConn
}

func (c *fakeConnector) Driver() driver.Driver {
	return &fakeConnectorDriver{}
}

type fakeConnectorDriver struct{}

func (d *fakeConnectorDriver) Open(name string) (driver.Conn, error) {
	return nil, driver.ErrBadConn
}

func TestNewTenantPools(t *testing.T) {
	tp := NewTenantPools(nil, "", 5, make([]byte, TenantRoleSecretMinBytes))
	if tp == nil {
		t.Fatal("NewTenantPools returned nil")
	}
	if tp.OwnerDB != nil {
		t.Error("NewTenantPools with nil OwnerDB should store nil")
	}
	if tp.pools == nil {
		t.Error("pools map should be initialized")
	}
	if len(tp.pools) != 0 {
		t.Errorf("expected empty pools, got %d entries", len(tp.pools))
	}
}

func TestTenantPoolsClose(t *testing.T) {
	// Close on empty pools should not panic.
	tp := NewTenantPools(nil, "", 5, make([]byte, TenantRoleSecretMinBytes))
	tp.Close()
	// Closing again should also be safe.
	tp.Close()
}

func TestTenantPoolsCloseWithEntries(t *testing.T) {
	tp := NewTenantPools(nil, "", 5, make([]byte, TenantRoleSecretMinBytes))
	db := sql.OpenDB(&fakeConnector{})
	// The map holds an entry rather than a bare *sql.DB since cleat#1508, so
	// that a caller arriving mid-open has something to wait on. A test that
	// plants one has to plant it already-ready.
	ready := make(chan struct{})
	close(ready)
	tp.pools["550e8400-e29b-41d4-a716-446655440000"] = &tenantPool{ready: ready, db: db}
	// Close with one pool entry exercises the loop body.
	tp.Close()
	// The pool should have been removed from the map.
	if len(tp.pools) != 0 {
		t.Errorf("expected empty pools after Close, got %d", len(tp.pools))
	}
}

// EvictIdle's real behaviour is in a_tenant_pool_records_when_it_was_last_used_test.go.
//
// What stood here asserted EvictIdle(0) == 0 and EvictIdle(1h) == 0 against a
// TenantPools holding NO POOLS -- it never called For() and never wrote
// tp.pools. Every possible implementation satisfies that, including the
// `return 0` stub it was written alongside and the working one that replaced
// it: both were green on it, which is the definition of a test that cannot
// fail. It read as coverage of the function and was coverage of an empty map.
//
// Removed rather than extended, because the cases worth having need a pool, a
// clock and a driver, and they live with the other tenant-pool tests.

// ---------------------------------------------------------------------------
// A driver that answers the role lookup with NO ROWS.
//
// Hand-rolled rather than sqlmock, matching auth/fake_driver_test.go: this
// module has no SQL-mocking dependency and adding one for two assertions is the
// wrong trade. All it has to do is make QueryRow().Scan() return
// sql.ErrNoRows, which an empty result set does.
// ---------------------------------------------------------------------------

type noRowsDriver struct{}

func (noRowsDriver) Open(string) (driver.Conn, error) { return noRowsConn{}, nil }

type noRowsConn struct{}

func (noRowsConn) Prepare(query string) (driver.Stmt, error) { return noRowsStmt{}, nil }
func (noRowsConn) Close() error                              { return nil }
func (noRowsConn) Begin() (driver.Tx, error)                 { return nil, driver.ErrSkip }

type noRowsStmt struct{}

func (noRowsStmt) Close() error                                 { return nil }
func (noRowsStmt) NumInput() int                                { return -1 }
func (noRowsStmt) Exec(_ []driver.Value) (driver.Result, error) { return nil, driver.ErrSkip }
func (noRowsStmt) Query(_ []driver.Value) (driver.Rows, error)  { return &noRowsRows{}, nil }

type noRowsRows struct{}

func (r *noRowsRows) Columns() []string           { return []string{"role_name"} }
func (r *noRowsRows) Close() error                { return nil }
func (r *noRowsRows) Next(_ []driver.Value) error { return io.EOF }

func init() { sql.Register("cleat-plugin-norows", noRowsDriver{}) }

// TestForRefusesATenantWithNoProvisionedRole is the fail-closed assertion.
// cleat#1307.
//
// For() used to fall back to tp.OwnerDB when admin.tenant_roles had no row,
// with a log line calling it "single-tenant mode". Harmless while TenantPools
// could not be constructed; a privilege escalation the moment it IS the
// isolation mechanism, because the owner connection sees every tenant's rows.
func TestForRefusesATenantWithNoProvisionedRole(t *testing.T) {
	owner, err := sql.Open("cleat-plugin-norows", "")
	if err != nil {
		t.Fatalf("open the no-rows driver: %v", err)
	}
	defer owner.Close()

	tp := NewTenantPools(owner, "host=x dbname=y", 5, make([]byte, TenantRoleSecretMinBytes))
	got, err := tp.For(context.Background(), "aaaaaaaa-0000-0000-0000-000000000001")

	if err == nil {
		t.Fatal("For() returned no error for a tenant with no provisioned role.\n\n" +
			"Falling back to the owner pool hands that tenant a connection that sees " +
			"every other tenant's rows (cleat#1307).")
	}
	if got != nil {
		t.Error("For() returned a database handle alongside its error")
	}
	if !strings.Contains(err.Error(), "no provisioned role") {
		t.Errorf("the error does not name the cause: %v", err)
	}
}

// The negative control: an empty tenant id is still the documented
// single-tenant path and must still return the owner pool.
func TestForStillReturnsTheOwnerPoolForTheEmptyTenant(t *testing.T) {
	owner := sql.OpenDB(&fakeConnector{})
	defer owner.Close()
	tp := NewTenantPools(owner, "", 5, make([]byte, TenantRoleSecretMinBytes))

	got, err := tp.For(context.Background(), "")
	if err != nil {
		t.Fatalf("For(\"\") errored: %v", err)
	}
	if got != owner {
		t.Error("For(\"\") did not return the owner pool; single-tenant deployments " +
			"depend on that path")
	}
}
