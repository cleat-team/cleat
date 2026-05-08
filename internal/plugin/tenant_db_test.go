package plugin

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"testing"
	"time"

	"github.com/google/uuid"
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
	tp := NewTenantPools(nil, "")
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
	tp := NewTenantPools(nil, "")
	tp.Close()
	// Closing again should also be safe.
	tp.Close()
}

func TestTenantPoolsCloseWithEntries(t *testing.T) {
	tp := NewTenantPools(nil, "")
	db := sql.OpenDB(&fakeConnector{})
	tp.pools[uuid.New()] = db
	// Close with one pool entry exercises the loop body.
	tp.Close()
	// The pool should have been removed from the map.
	if len(tp.pools) != 0 {
		t.Errorf("expected empty pools after Close, got %d", len(tp.pools))
	}
}

func TestEvictIdle(t *testing.T) {
	tp := NewTenantPools(nil, "")
	n := tp.EvictIdle(0)
	if n != 0 {
		t.Errorf("EvictIdle(0) = %d, want 0", n)
	}
	n = tp.EvictIdle(time.Hour)
	if n != 0 {
		t.Errorf("EvictIdle(1h) = %d, want 0", n)
	}
}
