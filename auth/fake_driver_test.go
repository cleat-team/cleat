package auth

import (
	"context"
	"database/sql/driver"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// In-memory fake database store
// ---------------------------------------------------------------------------

type fakeTenantRow struct {
	tenantID    string
	name        string
	displayName string
}

type fakeAPIKeyRow struct {
	keyID       string
	tenantID    string
	keyHashHex  string
	description string
	revokedAt   *time.Time
}

// fakeDBStore holds the in-memory data used by the fake SQL driver.
type fakeDBStore struct {
	mu      sync.RWMutex
	tenants []fakeTenantRow
	apiKeys map[string]fakeAPIKeyRow // key_hash_hex -> row
	nextID  int
}

func newFakeDBStore() *fakeDBStore {
	return &fakeDBStore{
		apiKeys: make(map[string]fakeAPIKeyRow),
	}
}

// ---------------------------------------------------------------------------
// sql/driver.Connector and driver.Driver
// ---------------------------------------------------------------------------

type fakeConnector struct {
	store *fakeDBStore
}

func (c *fakeConnector) Connect(_ context.Context) (driver.Conn, error) {
	return &fakeConn{store: c.store}, nil
}

func (c *fakeConnector) Driver() driver.Driver {
	return &fakeDriver{}
}

type fakeDriver struct{}

func (*fakeDriver) Open(_ string) (driver.Conn, error) {
	return nil, fmt.Errorf("fakeDriver: use sql.OpenDB")
}

// ---------------------------------------------------------------------------
// driver.Conn with QueryerContext / ExecerContext
// ---------------------------------------------------------------------------

type fakeConn struct {
	store *fakeDBStore
}

func (*fakeConn) Prepare(_ string) (driver.Stmt, error) {
	return nil, fmt.Errorf("fakeConn: unexpected Prepare call")
}

func (*fakeConn) Close() error { return nil }

func (*fakeConn) Begin() (driver.Tx, error) { return &fakeTx{}, nil }

type fakeTx struct{}

func (*fakeTx) Commit() error   { return nil }
func (*fakeTx) Rollback() error { return nil }

// QueryContext implements driver.QueryerContext.
//
// SELECT queries hold a read lock; INSERT … RETURNING queries hold a write lock
// (they mutate the store).
func (c *fakeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	switch {
	case strings.Contains(query, "SELECT tenant_id FROM admin.tenant_api_keys") ||
		strings.Contains(query, "SELECT tenant_id FROM tenant_api_keys"):
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryTenantLookup(args)
	case strings.Contains(query, "INSERT INTO admin.tenants") ||
		strings.Contains(query, "INSERT INTO tenants"):
		c.store.mu.Lock()
		defer c.store.mu.Unlock()
		return c.execInsertTenant(args)
	default:
		return nil, fmt.Errorf("fakeConn: unexpected Query: %s", query)
	}
}

// ExecContext implements driver.ExecerContext.
func (c *fakeConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.store.mu.Lock()
	defer c.store.mu.Unlock()

	switch {
	case strings.Contains(query, "INSERT INTO admin.tenant_api_keys") ||
		strings.Contains(query, "INSERT INTO tenant_api_keys"):
		return c.execInsertAPIKey(args)
	case strings.Contains(query, "UPDATE admin.tenant_api_keys SET revoked_at") ||
		strings.Contains(query, "UPDATE tenant_api_keys SET revoked_at"):
		return c.execRevokeAPIKey(args)
	default:
		return nil, fmt.Errorf("fakeConn: unexpected Exec: %s", query)
	}
}

// ---------------------------------------------------------------------------
// Query implementations
// ---------------------------------------------------------------------------

// queryTenantLookup handles:
//
//	SELECT tenant_id FROM tenant_api_keys WHERE key_hash = $1 AND revoked_at IS NULL
func (c *fakeConn) queryTenantLookup(args []driver.NamedValue) (driver.Rows, error) {
	keyHash, err := argBytes(args, 1)
	if err != nil {
		return nil, err
	}
	hashHex := fmt.Sprintf("%x", keyHash)
	key, ok := c.store.apiKeys[hashHex]
	if !ok || key.revokedAt != nil {
		// Return empty rows → Next() returns io.EOF → sql.ErrNoRows
		return &fakeRows{columns: []string{"tenant_id"}}, nil
	}
	return &fakeRows{
		columns: []string{"tenant_id"},
		data:    [][]driver.Value{{key.tenantID}},
	}, nil
}

// execInsertTenant handles:
//
//	INSERT INTO tenants (name, display_name) VALUES ($1, $2) RETURNING tenant_id
func (c *fakeConn) execInsertTenant(args []driver.NamedValue) (driver.Rows, error) {
	name, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	displayName, err := argString(args, 2)
	if err != nil {
		return nil, err
	}

	// Simulate UNIQUE constraint on name.
	for _, t := range c.store.tenants {
		if t.name == name {
			return nil, fmt.Errorf(`pq: duplicate key value violates unique constraint "tenants_name_key"`)
		}
	}

	c.store.nextID++
	tid := fmt.Sprintf("00000000-0000-0000-0000-%012d", c.store.nextID)
	c.store.tenants = append(c.store.tenants, fakeTenantRow{
		tenantID:    tid,
		name:        name,
		displayName: displayName,
	})
	return &fakeRows{
		columns: []string{"tenant_id"},
		data:    [][]driver.Value{{tid}},
	}, nil
}

// execInsertAPIKey handles:
//
//	INSERT INTO tenant_api_keys (tenant_id, key_hash, description) VALUES ($1, $2, $3)
func (c *fakeConn) execInsertAPIKey(args []driver.NamedValue) (driver.Result, error) {
	tenantID, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	keyHash, err := argBytes(args, 2)
	if err != nil {
		return nil, err
	}
	description, err := argString(args, 3)
	if err != nil {
		return nil, err
	}

	hashHex := fmt.Sprintf("%x", keyHash)
	keyID := fmt.Sprintf("00000000-0000-0000-0000-%012d", len(c.store.apiKeys)+1)
	c.store.apiKeys[hashHex] = fakeAPIKeyRow{
		keyID:       keyID,
		tenantID:    tenantID,
		keyHashHex:  hashHex,
		description: description,
	}
	return &fakeResult{rowsAffected: 1}, nil
}

// execRevokeAPIKey handles:
//
//	UPDATE tenant_api_keys SET revoked_at = now() WHERE key_id = $1 AND revoked_at IS NULL
func (c *fakeConn) execRevokeAPIKey(args []driver.NamedValue) (driver.Result, error) {
	keyID, err := argString(args, 1)
	if err != nil {
		return nil, err
	}

	for hashHex, key := range c.store.apiKeys {
		if key.keyID == keyID && key.revokedAt == nil {
			now := time.Now()
			key.revokedAt = &now
			c.store.apiKeys[hashHex] = key
			return &fakeResult{rowsAffected: 1}, nil
		}
	}
	return &fakeResult{rowsAffected: 0}, nil
}

// ---------------------------------------------------------------------------
// Argument extractors
// ---------------------------------------------------------------------------

func argBytes(args []driver.NamedValue, ordinal int) ([]byte, error) {
	for _, a := range args {
		if a.Ordinal == ordinal {
			b, ok := a.Value.([]byte)
			if !ok {
				return nil, fmt.Errorf("arg %d: want []byte, got %T", ordinal, a.Value)
			}
			return b, nil
		}
	}
	return nil, fmt.Errorf("arg %d not found", ordinal)
}

func argString(args []driver.NamedValue, ordinal int) (string, error) {
	for _, a := range args {
		if a.Ordinal == ordinal {
			s, ok := a.Value.(string)
			if !ok {
				return "", fmt.Errorf("arg %d: want string, got %T", ordinal, a.Value)
			}
			return s, nil
		}
	}
	return "", fmt.Errorf("arg %d not found", ordinal)
}

// ---------------------------------------------------------------------------
// driver.Result and driver.Rows stubs
// ---------------------------------------------------------------------------

type fakeResult struct {
	rowsAffected int64
}

func (r *fakeResult) LastInsertId() (int64, error) { return 0, nil }
func (r *fakeResult) RowsAffected() (int64, error) { return r.rowsAffected, nil }

type fakeRows struct {
	columns []string
	data    [][]driver.Value
	pos     int
}

func (r *fakeRows) Columns() []string { return r.columns }
func (r *fakeRows) Close() error      { return nil }

func (r *fakeRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.pos])
	r.pos++
	return nil
}
