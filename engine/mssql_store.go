package engine

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	_ "github.com/microsoft/go-mssqldb"
)

// tenantSessionConnector wraps a driver.Connector to call
// sp_set_session_context on every new connection, enforcing
// SQL Server RLS at the connection level for the configured tenant.
type tenantSessionConnector struct {
	driver.Connector
	tenantID string
}

func (c *tenantSessionConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	if c.tenantID == "" {
		return conn, nil
	}
	// Validate tenantID format to prevent SQL injection.
	if _, parseErr := uuid.Parse(c.tenantID); parseErr != nil {
		conn.Close()
		return nil, fmt.Errorf("mssql: invalid tenant ID %q: %w", c.tenantID, parseErr)
	}

	// Use Prepare+Exec to set the session context, since go-mssqldb v1.10+
	// does not implement driver.ExecerContext on its connection type.
	// Explicit command text (no parameter markers) is safe because tenantID
	// was validated as a UUID above.
	query := "EXEC sp_set_session_context @key=N'tenant_id', @value=N'" + c.tenantID + "'"
	var stmt driver.Stmt
	if prepCtx, ok := conn.(driver.ConnPrepareContext); ok {
		stmt, err = prepCtx.PrepareContext(ctx, query)
	} else {
		stmt, err = conn.Prepare(query)
	}
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("mssql: prepare session context for tenant %s: %w", c.tenantID, err)
	}
	_, err = stmt.Exec(nil)
	stmt.Close()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("mssql: set session context for tenant %s: %w", c.tenantID, err)
	}
	return conn, nil
}

// ---------------------------------------------------------------------------
// MSSQLStore implements WorkflowStore using Microsoft SQL Server.
// ---------------------------------------------------------------------------

// MSSQLStore implements WorkflowStore using a Microsoft SQL Server database.
// Tenant isolation is enforced via SQL Server Row-Level Security (RLS).
// The connection pool's connector calls sp_set_session_context on every
// new connection, with per-transaction calls serving as defense-in-depth.
type MSSQLStore struct {
	db                *sql.DB
	taskQueues        []string
	tenantID          string
	dialect           Dialect
	idempotencyKeyTTL time.Duration
	notifyChannel     string // MSSQL has no LISTEN/NOTIFY; empty = disabled (forward compat)

	// Encryption at rest for sensitive event payloads.
	// NOTE: MSSQL does not yet support encryption at rest; these fields are
	// present for forward compatibility so that StreamEventHistory can
	// contain the same decryption guard as the Postgres variant.
	encryption               *PayloadEncryption
	encryptSensitivePayloads bool

	// disableReadRedaction when true bypasses RedactOnRead on the read path.
	// Set to true during replay to avoid the overhead of retroactive redaction.
	disableReadRedaction bool
}

// NewMSSQLStore creates an MSSQLStore scoped to the given task queues.
// The taskQueues slice specifies which task queues this worker pool should poll
// (e.g., "default", "gpu", "high-memory"). Defaults to ["default"].
// The tenantID defaults to the default tenant UUID from the tenant foundation migration.
func NewMSSQLStore(db *sql.DB, taskQueues ...string) *MSSQLStore {
	tqs := taskQueues
	if len(tqs) == 0 {
		tqs = []string{"default"}
	}
	return &MSSQLStore{
		db:                db,
		taskQueues:        tqs,
		tenantID:          "00000000-0000-0000-0000-000000000000",
		dialect:           DialectMSSQL,
		idempotencyKeyTTL: 720 * time.Hour,
	}
}

// WithIdempotencyKeyTTL returns a copy of the store with the given idempotency key TTL.
func (s *MSSQLStore) WithIdempotencyKeyTTL(ttl time.Duration) *MSSQLStore {
	cp := *s
	cp.idempotencyKeyTTL = ttl
	return &cp
}

// WithReadRedactionDisabled returns a copy of the store with redaction on
// the read path disabled. Used during replay to avoid overhead.
func (s *MSSQLStore) WithReadRedactionDisabled(disabled bool) *MSSQLStore {
	cp := *s
	cp.disableReadRedaction = disabled
	return &cp
}

// WithEncryption returns a copy of the store with encryption at rest enabled.
// NOTE: Encryption at rest is not yet supported on MSSQL backends. This method
// is present for forward compatibility so that StreamEventHistory can contain
// the same decryption guard as the Postgres variant.
func (s *MSSQLStore) WithEncryption(enc *PayloadEncryption, enabled bool) *MSSQLStore {
	cp := *s
	cp.encryption = enc
	cp.encryptSensitivePayloads = enabled
	return &cp
}

// WithTenant returns a copy of the store scoped to the given tenant ID.
// This is used in the dispatch loop to set the correct tenant context
// before executing a workflow. The returned store's methods will set
// the RLS session variable via sp_set_session_context.
func (s *MSSQLStore) WithTenant(tenantID string) *MSSQLStore {
	cp := *s
	cp.tenantID = tenantID
	return &cp
}

// setSessionContext sets the tenant_id session context for RLS policies.
// SQL Server equivalent of PostgreSQL's SET session_config.tenant_id.
func (s *MSSQLStore) setSessionContext(tx *sql.Tx) error {
	if s.tenantID == "" {
		return fmt.Errorf("setSessionContext: tenant ID must be set before setting session context for an RLS-scoped transaction")
	}
	_, err := tx.Exec(`
		EXEC sp_set_session_context @key=N'tenant_id', @value=@p1
	`, s.tenantID)
	return err
}

// beginTxWithContext begins a transaction and sets the RLS tenant context,
// ensuring all subsequent queries in the transaction are scoped to the
// current tenant. The caller must commit or rollback the returned tx.
func (s *MSSQLStore) beginTxWithContext(ctx context.Context) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("beginTxWithContext: begin tx: %w", err)
	}
	if err := s.setSessionContext(tx); err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("set session context: %w", err)
	}
	return tx, nil
}

// buildTaskQueueParam builds a comma-separated task queue string for STRING_SPLIT.
func (s *MSSQLStore) buildTaskQueueParam() string {
	if len(s.taskQueues) == 0 {
		return "default"
	}
	// If already a single string, return as-is (avoids double splitting).
	// The caller is expected to use STRING_SPLIT(@param, ',').
	return strings.Join(s.taskQueues, ",")
}

// ---------------------------------------------------------------------------
// Factory (C.2)
// ---------------------------------------------------------------------------

// nopCloser is a no-op io.Closer used by OpenStore.
type mssqlNopCloser struct{}

func (mssqlNopCloser) Close() error { return nil }

// MSSQLStoreFactory implements StoreFactory for Microsoft SQL Server.
// It manages per-tenant connection pools with sp_set_session_context
// baked into the connector, enforcing RLS at the connection level.
type MSSQLStoreFactory struct {
	mu                sync.RWMutex
	connStr           string             // connection string for SQL Server
	tenantDBs         map[string]*sql.DB // per-tenant connection pools with RLS context
	idempotencyKeyTTL  time.Duration
	tenantPoolMaxConns int
}

// NewMSSQLStoreFactory creates an MSSQLStoreFactory.
// connStr is the SQL Server connection string used to open per-tenant pools.
func NewMSSQLStoreFactory(connStr string, idempotencyKeyTTL ...time.Duration) *MSSQLStoreFactory {
	ttl := 720 * time.Hour
	if len(idempotencyKeyTTL) > 0 {
		ttl = idempotencyKeyTTL[0]
	}
	return &MSSQLStoreFactory{
		connStr:            connStr,
		tenantDBs:          make(map[string]*sql.DB),
		idempotencyKeyTTL:  ttl,
		tenantPoolMaxConns: 25,
	}
}

// WithTenantPoolMaxConns sets the max open connections per tenant pool.
func (f *MSSQLStoreFactory) WithTenantPoolMaxConns(n int) *MSSQLStoreFactory {
	if n > 0 {
		f.tenantPoolMaxConns = n
	}
	return f
}

// OpenStore creates an MSSQLStore scoped to the given tenant.
// Each tenant gets a dedicated connection pool with RLS session context
// baked into every connection.
//
// NOTE: Encryption at rest (--encrypt-sensitive-payloads) is not yet supported
// on MSSQL backends. See PostgresStore.WithEncryption for the reference implementation.
func (f *MSSQLStoreFactory) OpenStore(ctx context.Context, tenantID string, taskQueues ...string) (WorkflowStore, io.Closer, error) {
	tenantDB, err := f.getOrCreateTenantPool(ctx, tenantID)
	if err != nil {
		return nil, nil, fmt.Errorf("open store for tenant %s: %w", tenantID, err)
	}
	store := NewMSSQLStore(tenantDB, taskQueues...)
	store.tenantID = tenantID
	store = store.WithIdempotencyKeyTTL(f.idempotencyKeyTTL)
	return store, mssqlNopCloser{}, nil
}

// getOrCreateTenantPool returns a *sql.DB pool for the given tenant.
// The pool uses a wrapped connector that sets sp_set_session_context
// on every new connection, so RLS is enforced automatically.
func (f *MSSQLStoreFactory) getOrCreateTenantPool(ctx context.Context, tenantID string) (*sql.DB, error) {
	// Validate early to fail fast with a clear error, rather than failing
	// during the first connection attempt inside the connector.
	if _, err := uuid.Parse(tenantID); err != nil {
		return nil, fmt.Errorf("invalid tenant ID %q: %w", tenantID, err)
	}

	f.mu.RLock()
	db, ok := f.tenantDBs[tenantID]
	f.mu.RUnlock()
	if ok {
		return db, nil
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if db, ok := f.tenantDBs[tenantID]; ok {
		return db, nil
	}

	// Get the mssql driver as a Connector through the registered driver.
	baseDB, err := sql.Open("sqlserver", f.connStr)
	if err != nil {
		return nil, fmt.Errorf("open base mssql connection: %w", err)
	}
	d := baseDB.Driver()
	baseDB.Close()

	dc, ok := d.(driver.DriverContext)
	if !ok {
		return nil, fmt.Errorf("mssql driver does not implement DriverContext")
	}

	connector, err := dc.OpenConnector(f.connStr)
	if err != nil {
		return nil, fmt.Errorf("open mssql connector: %w", err)
	}

	wrapped := &tenantSessionConnector{
		Connector: connector,
		tenantID:  tenantID,
	}

	tenantDB := sql.OpenDB(wrapped)
	tenantDB.SetMaxOpenConns(f.tenantPoolMaxConns)
	tenantDB.SetMaxIdleConns(max(2, f.tenantPoolMaxConns/5))
	tenantDB.SetConnMaxLifetime(5 * time.Minute)

	if err := tenantDB.PingContext(ctx); err != nil {
		tenantDB.Close()
		return nil, fmt.Errorf("ping tenant pool for %s: %w", tenantID, err)
	}

	f.tenantDBs[tenantID] = tenantDB
	return tenantDB, nil
}

// Close closes all tenant connection pools.
func (f *MSSQLStoreFactory) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, db := range f.tenantDBs {
		db.Close()
		delete(f.tenantDBs, id)
	}
	return nil
}

// DriverName returns "mssql".
func (f *MSSQLStoreFactory) DriverName() string { return "mssql" }

// Dialect returns DialectMSSQL.
func (f *MSSQLStoreFactory) Dialect() Dialect { return DialectMSSQL }
