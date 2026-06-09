// Package datadogexport behavioral tests — fake DB + in-memory store, no PostgreSQL.
package datadogexport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
)

// ---------------------------------------------------------------------------
// In-memory fake DB store
// ---------------------------------------------------------------------------

type fakeConfigRow struct {
	id            string
	tenantID      string
	name          string
	apiKey        string
	site          string
	metricsPrefix string
	enabled       bool
	createdAt     time.Time
	updatedAt     time.Time
}

type fakeWorkflowRow struct {
	status   string
	tenantID string
}

type fakeLeaseRow struct {
	name      string
	holder    string
	expiresAt time.Time
}

type fakeDBStore struct {
	mu                 sync.RWMutex
	nextID             int64
	configs            []fakeConfigRow
	workflowInstances  []fakeWorkflowRow
	apiKeys            map[string]string // key_hash_hex -> tenant_id
	leases             map[string]fakeLeaseRow // lease_name -> lease
	simulateErr        bool
}

func newFakeDBStore() *fakeDBStore {
	return &fakeDBStore{
		configs:           make([]fakeConfigRow, 0),
		workflowInstances: make([]fakeWorkflowRow, 0),
		apiKeys:           make(map[string]string),
		leases:            make(map[string]fakeLeaseRow),
	}
}

// ---------------------------------------------------------------------------
// Fake SQL driver
// ---------------------------------------------------------------------------

type fakeConnector struct {
	store *fakeDBStore
}

func (c *fakeConnector) Connect(_ context.Context) (driver.Conn, error) {
	return &fakeConn{store: c.store}, nil
}
func (c *fakeConnector) Driver() driver.Driver { return &fakeDrv{} }

type fakeDrv struct{}

func (*fakeDrv) Open(_ string) (driver.Conn, error) {
	return nil, fmt.Errorf("fakeDriver: use sql.OpenDB")
}

type fakeConn struct {
	store *fakeDBStore
}

func (*fakeConn) Prepare(_ string) (driver.Stmt, error) {
	return nil, fmt.Errorf("fakeConn: unexpected Prepare call")
}
func (*fakeConn) Close() error      { return nil }
func (*fakeConn) Begin() (driver.Tx, error) { return &fakeTx{}, nil }

type fakeTx struct{}
func (*fakeTx) Commit() error   { return nil }
func (*fakeTx) Rollback() error { return nil }

// --- ExecContext ---

func (c *fakeConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.store.mu.Lock()
	defer c.store.mu.Unlock()
	if c.store.simulateErr {
		return nil, fmt.Errorf("simulated db error")
	}

	switch {
	case strings.Contains(query, "INSERT INTO dd_config"):
		return c.execInsertConfig(args)
	case strings.Contains(query, "UPDATE dd_config"):
		return c.execUpdateConfig(query, args)
	case strings.Contains(query, "DELETE FROM dd_config"):
		return c.execDeleteConfig(args)
	case strings.Contains(query, "INSERT INTO plugin_lease"):
		return c.execInsertLease(args)
	case strings.Contains(query, "UPDATE plugin_lease"):
		return c.execUpdateLease(query, args)
	case strings.Contains(query, "DELETE FROM plugin_lease"):
		return c.execDeleteLease(args)
	default:
		return nil, fmt.Errorf("fakeConn: unexpected Exec query: %s", query)
	}
}

// --- QueryContext ---

func (c *fakeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if c.store.simulateErr {
		return nil, fmt.Errorf("simulated db error")
	}
	switch {
	case strings.Contains(query, "tenant_api_keys"):
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryTenantLookup(args)
	case strings.Contains(query, "SELECT id, name, api_key, site, metrics_prefix, enabled, created_at, updated_at"):
		if strings.Contains(query, "WHERE id = $") {
			c.store.mu.RLock()
			defer c.store.mu.RUnlock()
			return c.queryGetConfig(args)
		}
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryListConfigs(args)
	case strings.Contains(query, "SELECT id, tenant_id, api_key, site, metrics_prefix"):
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryEnabledConfigs(args)
	case strings.Contains(query, "SELECT status, COUNT(*) AS count"):
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryWorkflowStats(args)
	case strings.Contains(query, "SELECT id, name, api_key, site, metrics_prefix, enabled, created_at, updated_at"):
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryGetConfig(args)
	case strings.Contains(query, "SELECT 1 FROM plugin_lease"):
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryCheckLeaseLeader(args)
	default:
		return nil, fmt.Errorf("fakeConn: unexpected Query query: %s", query)
	}
}

// ---------------------------------------------------------------------------
// Exec implementations
// ---------------------------------------------------------------------------

func (c *fakeConn) execInsertConfig(args []driver.NamedValue) (driver.Result, error) {
	tenantID, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	id, err := argString(args, 2)
	if err != nil {
		return nil, err
	}
	name, err := argString(args, 3)
	if err != nil {
		return nil, err
	}
	apiKey, err := argString(args, 4)
	if err != nil {
		return nil, err
	}
	site, err := argString(args, 5)
	if err != nil {
		return nil, err
	}
	prefix, err := argString(args, 6)
	if err != nil {
		return nil, err
	}
	nowVal, err := argTime(args, 7)
	if err != nil {
		return nil, err
	}

	c.store.configs = append(c.store.configs, fakeConfigRow{
		id:            id,
		tenantID:      tenantID,
		name:          name,
		apiKey:        apiKey,
		site:          site,
		metricsPrefix: prefix,
		enabled:       true,
		createdAt:     nowVal,
		updatedAt:     nowVal,
	})
	c.store.nextID++
	return &fakeResult{rowsAffected: 1}, nil
}

func (c *fakeConn) execUpdateConfig(query string, args []driver.NamedValue) (driver.Result, error) {
	// Determine how many SET clauses there are by counting the number of
	// `= $` patterns in the SET clause (between "SET " and " WHERE").
	// Then the last two args ($N-1, $N) are id and tenant_id.
	// Find WHERE clause — might be preceded by whitespace/newlines.
	whereIdx := strings.Index(query, "WHERE")
	setClause := query
	if whereIdx >= 0 {
		setClause = query[:whereIdx]
	}
	// Count `= $` occurrences — each is a SET field.
	setCount := strings.Count(setClause, "= $")
	setArgs := setCount // number of SET values (excluding updated_at = now())

	// The positional args are: [set_values..., id, tenant_id]
	// setArgs is the number of SET value positionals.
	// So id is at position setArgs+1 and tenant_id at setArgs+2.
	idPos := setArgs + 1
	tidPos := setArgs + 2

	id := ""
	if v, err := argAny(args, idPos); err == nil {
		if s, ok := v.(string); ok {
			id = s
		}
	}
	tenantID := ""
	if v, err := argAny(args, tidPos); err == nil {
		if s, ok := v.(string); ok {
			tenantID = s
		}
	}

	if id == "" {
		return &fakeResult{rowsAffected: 0}, nil
	}

	for i, cfg := range c.store.configs {
		if (tenantID == "" || cfg.tenantID == tenantID) && cfg.id == id {
			// Apply SET clauses based on query patterns.
			argIdx := 1
			if strings.Contains(setClause, "name = $") {
				if v, err := argString(args, argIdx); err == nil {
					cfg.name = v
				}
				argIdx++
			}
			if strings.Contains(setClause, "api_key = $") {
				if v, err := argString(args, argIdx); err == nil {
					cfg.apiKey = v
				}
				argIdx++
			}
			if strings.Contains(setClause, "site = $") {
				if v, err := argString(args, argIdx); err == nil {
					cfg.site = v
				}
				argIdx++
			}
			if strings.Contains(setClause, "metrics_prefix = $") {
				if v, err := argString(args, argIdx); err == nil {
					cfg.metricsPrefix = v
				}
				argIdx++
			}
			if strings.Contains(setClause, "enabled = $") {
				if v, err := argAny(args, argIdx); err == nil {
					if b, ok := v.(bool); ok {
						cfg.enabled = b
					}
				}
				argIdx++
			}

			cfg.updatedAt = time.Now()
			c.store.configs[i] = cfg
			return &fakeResult{rowsAffected: 1}, nil
		}
	}
	return &fakeResult{rowsAffected: 0}, nil
}

func (c *fakeConn) execDeleteConfig(args []driver.NamedValue) (driver.Result, error) {
	id, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	tid, err := argString(args, 2)
	if err != nil {
		return nil, err
	}

	for i, cfg := range c.store.configs {
		if cfg.id == id && cfg.tenantID == tid {
			c.store.configs = append(c.store.configs[:i], c.store.configs[i+1:]...)
			return &fakeResult{rowsAffected: 1}, nil
		}
	}
	return &fakeResult{rowsAffected: 0}, nil
}

// --- Lease exec helpers ---

func (c *fakeConn) execInsertLease(args []driver.NamedValue) (driver.Result, error) {
	name, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	holder, err := argString(args, 2)
	if err != nil {
		return nil, err
	}
	if _, exists := c.store.leases[name]; exists {
		// Simulate PK violation.
		return nil, fmt.Errorf("fakeConn: lease %q already exists", name)
	}
	c.store.leases[name] = fakeLeaseRow{
		name:      name,
		holder:    holder,
		expiresAt: time.Now().Add(50 * time.Second),
	}
	return &fakeResult{rowsAffected: 1}, nil
}

func (c *fakeConn) execUpdateLease(query string, args []driver.NamedValue) (driver.Result, error) {
	// Determine whether this is a "renew by holder" or "grab expired" UPDATE.
	// renewIfHolder: UPDATE ... SET expires_at = ... WHERE name = $1 AND holder = $2
	//   → args: [$1=name, $2=holder]
	// grabExpired: UPDATE ... SET holder = $1, expires_at = ... WHERE name = $2 AND expires_at < now()
	//   → args: [$1=new_holder, $2=name]
	// grabExpired has `SET holder` in the query, renewIfHolder does not.
	renewByHolder := !strings.Contains(query, "SET holder")

	if renewByHolder {
		name, err := argString(args, 1)
		if err != nil {
			return nil, err
		}
		holder, err := argString(args, 2)
		if err != nil {
			return nil, err
		}
		lease, exists := c.store.leases[name]
		if !exists || lease.holder != holder {
			return &fakeResult{rowsAffected: 0}, nil
		}
		lease.expiresAt = time.Now().Add(50 * time.Second)
		c.store.leases[name] = lease
		return &fakeResult{rowsAffected: 1}, nil
	}

	// grabExpired: UPDATE ... SET holder = $1, expires_at = ... WHERE name = $2 AND expires_at < now()
	newHolder, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	name, err := argString(args, 2)
	if err != nil {
		return nil, err
	}
	lease, exists := c.store.leases[name]
	if !exists || time.Now().Before(lease.expiresAt) {
		return &fakeResult{rowsAffected: 0}, nil
	}
	// Lease exists and is expired — take it over.
	c.store.leases[name] = fakeLeaseRow{
		name:      name,
		holder:    newHolder,
		expiresAt: time.Now().Add(50 * time.Second),
	}
	return &fakeResult{rowsAffected: 1}, nil
}

func (c *fakeConn) execDeleteLease(args []driver.NamedValue) (driver.Result, error) {
	name, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	delete(c.store.leases, name)
	return &fakeResult{rowsAffected: 1}, nil
}

func (c *fakeConn) queryCheckLeaseLeader(args []driver.NamedValue) (driver.Rows, error) {
	name, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	holder, err := argString(args, 2)
	if err != nil {
		return nil, err
	}
	lease, exists := c.store.leases[name]
	if !exists || lease.holder != holder || time.Now().After(lease.expiresAt) {
		// No match — return empty result.
		return &fakeRows{columns: []string{"1"}}, nil
	}
	return &fakeRows{
		columns: []string{"1"},
		data:    [][]driver.Value{{int64(1)}},
	}, nil
}

// ---------------------------------------------------------------------------
// Query implementations
// ---------------------------------------------------------------------------

func (c *fakeConn) queryTenantLookup(args []driver.NamedValue) (driver.Rows, error) {
	keyHash, err := argBytes(args, 1)
	if err != nil {
		return nil, err
	}
	hashHex := fmt.Sprintf("%x", keyHash)
	tid, ok := c.store.apiKeys[hashHex]
	if !ok {
		return &fakeRows{columns: []string{"tenant_id"}}, nil
	}
	return &fakeRows{
		columns: []string{"tenant_id"},
		data:    [][]driver.Value{{tid}},
	}, nil
}

func (c *fakeConn) queryListConfigs(args []driver.NamedValue) (driver.Rows, error) {
	tid, err := argString(args, 1)
	if err != nil {
		return nil, err
	}

	var results []fakeConfigRow
	for _, cfg := range c.store.configs {
		if cfg.tenantID == tid {
			results = append(results, cfg)
		}
	}

	columns := []string{"id", "name", "api_key", "site", "metrics_prefix", "enabled", "created_at", "updated_at"}
	var data [][]driver.Value
	for _, cfg := range results {
		data = append(data, []driver.Value{
			cfg.id, cfg.name, cfg.apiKey, cfg.site, cfg.metricsPrefix,
			cfg.enabled, cfg.createdAt, cfg.updatedAt,
		})
	}
	return &fakeRows{columns: columns, data: data}, nil
}

func (c *fakeConn) queryGetConfig(args []driver.NamedValue) (driver.Rows, error) {
	id, err := argString(args, 1)
	if err != nil {
		return nil, err
	}

	var tid string
	if len(args) >= 2 {
		tid, _ = argString(args, 2)
	}

	for _, cfg := range c.store.configs {
		if cfg.id == id && (tid == "" || cfg.tenantID == tid) {
			return &fakeRows{
				columns: []string{"id", "name", "api_key", "site", "metrics_prefix", "enabled", "created_at", "updated_at"},
				data: [][]driver.Value{{
					cfg.id, cfg.name, cfg.apiKey, cfg.site, cfg.metricsPrefix,
					cfg.enabled, cfg.createdAt, cfg.updatedAt,
				}},
			}, nil
		}
	}
	return &fakeRows{
		columns: []string{"id", "name", "api_key", "site", "metrics_prefix", "enabled", "created_at", "updated_at"},
	}, nil
}

func (c *fakeConn) queryEnabledConfigs(_ []driver.NamedValue) (driver.Rows, error) {
	var results []fakeConfigRow
	for _, cfg := range c.store.configs {
		if cfg.enabled {
			results = append(results, cfg)
		}
	}

	columns := []string{"id", "tenant_id", "api_key", "site", "metrics_prefix"}
	var data [][]driver.Value
	for _, cfg := range results {
		data = append(data, []driver.Value{
			cfg.id, cfg.tenantID, cfg.apiKey, cfg.site, cfg.metricsPrefix,
		})
	}
	return &fakeRows{columns: columns, data: data}, nil
}

func (c *fakeConn) queryWorkflowStats(args []driver.NamedValue) (driver.Rows, error) {
	tid, err := argString(args, 1)
	if err != nil {
		return nil, err
	}

	counts := make(map[string]int)
	for _, w := range c.store.workflowInstances {
		if w.tenantID == tid {
			counts[w.status]++
		}
	}

	columns := []string{"status", "count"}
	var data [][]driver.Value
	for status, count := range counts {
		data = append(data, []driver.Value{status, int64(count)})
	}
	return &fakeRows{columns: columns, data: data}, nil
}

// ---------------------------------------------------------------------------
// Argument extractors
// ---------------------------------------------------------------------------

func argString(args []driver.NamedValue, ordinal int) (string, error) {
	for _, a := range args {
		if a.Ordinal == ordinal {
			switch v := a.Value.(type) {
			case string:
				return v, nil
			case []byte:
				return string(v), nil
			default:
				return "", fmt.Errorf("arg %d: want string, got %T", ordinal, a.Value)
			}
		}
	}
	return "", fmt.Errorf("arg %d not found", ordinal)
}

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

func argInt64(args []driver.NamedValue, ordinal int) (int64, error) {
	for _, a := range args {
		if a.Ordinal == ordinal {
			switch v := a.Value.(type) {
			case int64:
				return v, nil
			case float64:
				return int64(v), nil
			default:
				return 0, fmt.Errorf("arg %d: want int64, got %T", ordinal, a.Value)
			}
		}
	}
	return 0, fmt.Errorf("arg %d not found", ordinal)
}

func argTime(args []driver.NamedValue, ordinal int) (time.Time, error) {
	for _, a := range args {
		if a.Ordinal == ordinal {
			t, ok := a.Value.(time.Time)
			if !ok {
				return time.Time{}, fmt.Errorf("arg %d: want time.Time, got %T", ordinal, a.Value)
			}
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("arg %d not found", ordinal)
}

func argAny(args []driver.NamedValue, ordinal int) (driver.Value, error) {
	for _, a := range args {
		if a.Ordinal == ordinal {
			return a.Value, nil
		}
	}
	return nil, fmt.Errorf("arg %d not found", ordinal)
}

// ---------------------------------------------------------------------------
// driver.Result / driver.Rows stubs
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

// ---------------------------------------------------------------------------
// Test setup
// ---------------------------------------------------------------------------

var testTenantID = uuid.MustParse("00000000-0000-0000-0000-000000000001")
var testTenantStr = testTenantID.String()

func setupTestPlugin(t *testing.T) (*Plugin, http.Handler, *fakeDBStore) {
	t.Helper()

	store := newFakeDBStore()

	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantStr

	db := sql.OpenDB(&fakeConnector{store: store})
	t.Cleanup(func() { db.Close() })

	p := &Plugin{
		db:     &engine.SQLDBAdapter{DB: db},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	handler := auth.Middleware(engine.NewPostgresStore(db), false)(mux)
	return p, handler, store
}

func authedRequest(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.Header.Set("Authorization", "Bearer test-api-key")
	return req
}

// ---------------------------------------------------------------------------
// Behavioral tests
// ---------------------------------------------------------------------------

// TestConfigCreateAndGet verifies creating a Datadog export config and
// retrieving it by ID.
func TestConfigCreateAndGet(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	body := `{"name":"test-config","api_key":"dd-api-key-123","site":"datadoghq.eu"}`
	req := authedRequest("POST", "/datadog/configs", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var created map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("failed to decode create response: %v", err)
	}
	id := created["id"].(string)
	if created["name"] != "test-config" {
		t.Errorf("expected name 'test-config', got %s", created["name"])
	}
	if created["site"] != "datadoghq.eu" {
		t.Errorf("expected site 'datadoghq.eu', got %s", created["site"])
	}
	if created["enabled"] != true {
		t.Errorf("expected enabled=true, got %v", created["enabled"])
	}

	// Verify it's in the store.
	store.mu.RLock()
	configCount := len(store.configs)
	store.mu.RUnlock()
	if configCount != 1 {
		t.Errorf("expected 1 config in store, got %d", configCount)
	}

	// GET by ID.
	req = authedRequest("GET", "/datadog/configs/"+id, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var fetched map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &fetched); err != nil {
		t.Fatalf("failed to decode get response: %v", err)
	}
	if fetched["id"] != id {
		t.Errorf("expected id %s, got %s", id, fetched["id"])
	}
	if fetched["name"] != "test-config" {
		t.Errorf("expected name 'test-config', got %s", fetched["name"])
	}
}

// TestConfigList verifies listing all configs for a tenant.
func TestConfigList(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	// Create two configs.
	for _, name := range []string{"config-a", "config-b"} {
		body := fmt.Sprintf(`{"name":"%s","api_key":"key-%s"}`, name, name)
		req := authedRequest("POST", "/datadog/configs", bytes.NewReader([]byte(body)))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("POST %s: expected 201, got %d", name, rec.Code)
		}
	}

	// List all configs.
	req := authedRequest("GET", "/datadog/configs", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("LIST: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var configs []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &configs); err != nil {
		t.Fatalf("failed to decode list response: %v", err)
	}
	if len(configs) != 2 {
		t.Fatalf("expected 2 configs, got %d", len(configs))
	}
}

// TestConfigUpdate verifies updating a Datadog config.
func TestConfigUpdate(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	// Create.
	body := `{"name":"original","api_key":"key-1","site":"datadoghq.com"}`
	req := authedRequest("POST", "/datadog/configs", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var created map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &created)
	id := created["id"].(string)

	// Update.
	updateBody := `{"name":"updated","site":"datadoghq.eu","enabled":false}`
	req = authedRequest("PUT", "/datadog/configs/"+id, bytes.NewReader([]byte(updateBody)))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var updated map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("failed to decode update response: %v", err)
	}
	if updated["name"] != "updated" {
		t.Errorf("expected name 'updated', got %s", updated["name"])
	}

	// Verify store has updated data.
	store.mu.RLock()
	cfg := store.configs[0]
	store.mu.RUnlock()
	if cfg.name != "updated" {
		t.Errorf("store: expected name 'updated', got %s", cfg.name)
	}
}

// TestConfigDelete verifies deleting a config.
func TestConfigDelete(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	// Create.
	body := `{"name":"to-delete","api_key":"key-del"}`
	req := authedRequest("POST", "/datadog/configs", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST: expected 201, got %d", rec.Code)
	}

	var created map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &created)
	id := created["id"].(string)

	// Delete.
	req = authedRequest("DELETE", "/datadog/configs/"+id, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE: expected 204, got %d", rec.Code)
	}

	// Verify store is empty.
	store.mu.RLock()
	count := len(store.configs)
	store.mu.RUnlock()
	if count != 0 {
		t.Errorf("expected 0 configs after delete, got %d", count)
	}

	// GET should be 404.
	req = authedRequest("GET", "/datadog/configs/"+id, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET after delete: expected 404, got %d", rec.Code)
	}
}

// TestConfigNotFound verifies 404 for non-existent config.
func TestConfigNotFound(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	badID := "00000000-0000-0000-0000-000000009999"
	req := authedRequest("GET", "/datadog/configs/"+badID, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for non-existent config, got %d", rec.Code)
	}

	req = authedRequest("PUT", "/datadog/configs/"+badID, bytes.NewReader([]byte(`{"name":"nope"}`)))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for updating non-existent config, got %d", rec.Code)
	}
}

// TestCreateRejectsEmptyAPIKey verifies that creating a config without an
// API key returns 400.
func TestCreateRejectsEmptyAPIKey(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	body := `{"name":"no-key"}`
	req := authedRequest("POST", "/datadog/configs", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing api_key, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestMetricDataFormatting verifies the metric payload structure built by
// exportForConfig is correctly formatted.
func TestMetricDataFormatting(t *testing.T) {
	store := newFakeDBStore()

	// Add an enabled config.
	cfgID := uuid.New().String()
	store.configs = append(store.configs, fakeConfigRow{
		id:            cfgID,
		tenantID:      testTenantStr,
		name:          "test",
		apiKey:        "dd-api-key",
		site:          "datadoghq.com",
		metricsPrefix: "cleat",
		enabled:       true,
		createdAt:     time.Now(),
		updatedAt:     time.Now(),
	})

	// Add workflow instances with different statuses.
	for _, status := range []string{"running", "completed", "failed"} {
		store.workflowInstances = append(store.workflowInstances, fakeWorkflowRow{
			status:   status,
			tenantID: testTenantStr,
		})
	}
	// Add a second completed instance.
	store.workflowInstances = append(store.workflowInstances, fakeWorkflowRow{
		status:   "completed",
		tenantID: testTenantStr,
	})

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	// Use a custom RoundTripper that captures the request payload and returns OK.
	var capturedPayload []byte
	p := &Plugin{
		db:     &engine.SQLDBAdapter{DB: db},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				capturedPayload, _ = io.ReadAll(req.Body)
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader([]byte(`{"status":"ok"}`))),
				}, nil
			}),
			Timeout: 5 * time.Second,
		},
	}

	cfg := ddConfigRow{
		ID:            uuid.MustParse(cfgID),
		TenantID:      testTenantID,
		APIKey:        "dd-api-key",
		Site:          "datadoghq.com",
		MetricsPrefix: "cleat",
	}

	err := p.exportForConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("exportForConfig: %v", err)
	}

	if capturedPayload == nil {
		t.Fatal("expected captured payload, got nil")
	}

	var payload ddSeriesPayload
	if err := json.Unmarshal(capturedPayload, &payload); err != nil {
		t.Fatalf("failed to decode payload: %v", err)
	}

	// Verify series count: 3 statuses + 1 total = 4.
	if len(payload.Series) != 4 {
		t.Fatalf("expected 4 series, got %d", len(payload.Series))
	}

	// Check metric names.
	metricNames := make(map[string]bool)
	for _, s := range payload.Series {
		metricNames[s.Metric] = true
		if s.Type != "gauge" {
			t.Errorf("expected type 'gauge', got %s", s.Type)
		}
		if len(s.Points) != 1 {
			t.Errorf("expected 1 point, got %d", len(s.Points))
		}
	}

	if !metricNames["cleat.workflows.running"] {
		t.Error("expected metric cleat.workflows.running")
	}
	if !metricNames["cleat.workflows.completed"] {
		t.Error("expected metric cleat.workflows.completed")
	}
	if !metricNames["cleat.workflows.failed"] {
		t.Error("expected metric cleat.workflows.failed")
	}
	if !metricNames["cleat.workflows.total"] {
		t.Error("expected metric cleat.workflows.total")
	}
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// TestBackgroundExportWorker verifies that the background worker queries
// enabled configs and attempts to export metrics.
func TestBackgroundExportWorker(t *testing.T) {
	store := newFakeDBStore()

	// Add two enabled configs.
	for i := 0; i < 2; i++ {
		store.configs = append(store.configs, fakeConfigRow{
			id:            uuid.New().String(),
			tenantID:      testTenantStr,
			name:          fmt.Sprintf("cfg-%d", i),
			apiKey:        fmt.Sprintf("key-%d", i),
			site:          "datadoghq.com",
			metricsPrefix: "cleat",
			enabled:       true,
		})
	}

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:     &engine.SQLDBAdapter{DB: db},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader([]byte(`{}`))),
				}, nil
			}),
			Timeout: 5 * time.Second,
		},
	}

	// Call exportMetrics directly (what the background worker calls).
	err := p.exportMetrics(context.Background())
	if err != nil {
		t.Fatalf("exportMetrics: %v", err)
	}
	// No assertion needed beyond no error — the worker ran successfully.
}

// TestCreateDefaults verifies default values when creating a config.
func TestCreateDefaults(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	body := `{"name":"defaults-test","api_key":"key-defaults"}`
	req := authedRequest("POST", "/datadog/configs", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var created map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &created)

	if created["site"] != "datadoghq.com" {
		t.Errorf("expected default site 'datadoghq.com', got %s", created["site"])
	}
	if created["metrics_prefix"] != "cleat" {
		t.Errorf("expected default metrics_prefix 'cleat', got %s", created["metrics_prefix"])
	}
}

// ---------------------------------------------------------------------------
// errorReader for simulating body read failures
// ---------------------------------------------------------------------------

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) {
	return 0, fmt.Errorf("simulated read error")
}

// ---------------------------------------------------------------------------
// Direct request helper (no auth middleware, adds tenant to context)
// ---------------------------------------------------------------------------

func ddRequest(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	ctx := auth.WithTenantID(req.Context(), testTenantID)
	return req.WithContext(ctx)
}

// ===========================================================================
// Migrations
// ===========================================================================

func TestDD_Migrations(t *testing.T) {
	p, _, _ := setupTestPlugin(t)
	migrations := p.Migrations()
	if len(migrations) == 0 {
		t.Fatal("expected at least one migration")
	}
	for i, m := range migrations {
		if m.Version == 0 {
			t.Errorf("migration %d: version must be non-zero", i)
		}
		if m.Up == "" {
			t.Errorf("migration %d: Up SQL is empty", i)
		}
		if m.Down == "" {
			t.Errorf("migration %d: Down SQL is empty", i)
		}
	}
}

// ===========================================================================
// Plugin Info
// ===========================================================================

func TestDD_PluginInfo(t *testing.T) {
	p, _, _ := setupTestPlugin(t)
	info := p.Info()
	if info.Name == "" {
		t.Error("expected non-empty Name")
	}
	if info.Version == "" {
		t.Error("expected non-empty Version")
	}
}

// ===========================================================================
// RegisterRoutes
// ===========================================================================

func TestDD_RegisterRoutes_NilMux(t *testing.T) {
	p, _, _ := setupTestPlugin(t)
	err := p.RegisterRoutes(nil)
	if err == nil || !strings.Contains(err.Error(), "nil mux") {
		t.Fatalf("expected nil mux error, got: %v", err)
	}
}

func TestDD_RegisterRoutes_Valid(t *testing.T) {
	p, _, _ := setupTestPlugin(t)
	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}
	// Verify at least one route is registered (should not 404).
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/datadog/configs", nil)
	mux.ServeHTTP(rec, req)
	if rec.Code == 404 {
		t.Error("expected /datadog/configs to be registered, got 404")
	}
}

// ===========================================================================
// Run — nil db and cancellation paths
// ===========================================================================

func TestDD_Run_NilDB(t *testing.T) {
	p := &Plugin{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := p.Run(ctx)
	if err != nil {
		t.Errorf("Run with nil db: expected nil, got %v", err)
	}
}

func TestDD_Run_Cancellation(t *testing.T) {
	store := newFakeDBStore()
	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()
	p := &Plugin{
		db:     &engine.SQLDBAdapter{DB: db},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	err := p.Run(ctx)
	if err != nil {
		t.Errorf("Run: expected nil, got %v", err)
	}
}

// ===========================================================================
// Handler error paths — missing tenant returns 401 for all endpoints
// ===========================================================================

func TestDD_ErrorPaths_MissingTenant(t *testing.T) {
	p, _, _ := setupTestPlugin(t)
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	tests := []struct{ method, path, body string }{
		{"POST", "/datadog/configs", `{"name":"t","api_key":"k"}`},
		{"GET", "/datadog/configs", ""},
		{"GET", "/datadog/configs/00000000-0000-0000-0000-000000000001", ""},
		{"PUT", "/datadog/configs/00000000-0000-0000-0000-000000000001", `{"name":"t"}`},
		{"DELETE", "/datadog/configs/00000000-0000-0000-0000-000000000001", ""},
	}

	for _, tc := range tests {
		var body io.Reader
		if tc.body != "" {
			body = bytes.NewReader([]byte(tc.body))
		}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, body)
		mux.ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Errorf("%s %s: want 401, got %d: %s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}

// ===========================================================================
// Handler error paths — invalid UUID returns 400
// ===========================================================================

func TestDD_ErrorPaths_InvalidID(t *testing.T) {
	p, _, _ := setupTestPlugin(t)
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	tests := []struct{ method, path string }{
		{"GET", "/datadog/configs/not-a-uuid"},
		{"PUT", "/datadog/configs/not-a-uuid"},
		{"DELETE", "/datadog/configs/not-a-uuid"},
	}

	for _, tc := range tests {
		rec := httptest.NewRecorder()
		req := ddRequest(tc.method, tc.path, nil)
		mux.ServeHTTP(rec, req)
		if rec.Code != 400 {
			t.Errorf("%s %s: want 400, got %d: %s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
		var body map[string]string
		json.Unmarshal(rec.Body.Bytes(), &body)
		if body["error"] == "" {
			t.Errorf("%s %s: expected error message in response", tc.method, tc.path)
		}
	}
}

// ===========================================================================
// Handler error paths — invalid JSON body returns 400
// ===========================================================================

func TestDD_ErrorPaths_InvalidJSON(t *testing.T) {
	p, _, _ := setupTestPlugin(t)
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	tests := []struct{ method, path string }{
		{"POST", "/datadog/configs"},
		{"PUT", "/datadog/configs/00000000-0000-0000-0000-000000000001"},
	}

	for _, tc := range tests {
		rec := httptest.NewRecorder()
		req := ddRequest(tc.method, tc.path, bytes.NewReader([]byte("not json")))
		mux.ServeHTTP(rec, req)
		if rec.Code != 400 {
			t.Errorf("%s %s: want 400, got %d: %s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}

// ===========================================================================
// Handler error path — PUT with no fields returns 400
// ===========================================================================

func TestDD_ErrorPaths_NoUpdateFields(t *testing.T) {
	p, _, _ := setupTestPlugin(t)
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	// Create a config first.
	rec := httptest.NewRecorder()
	req := ddRequest("POST", "/datadog/configs", bytes.NewReader([]byte(`{"name":"test","api_key":"k"}`)))
	mux.ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var created map[string]string
	json.Unmarshal(rec.Body.Bytes(), &created)
	id := created["id"]

	// Update with empty JSON — should fail with 400 (no fields to update).
	rec = httptest.NewRecorder()
	req = ddRequest("PUT", "/datadog/configs/"+id, bytes.NewReader([]byte("{}")))
	mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("PUT with no fields: want 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// Handler error paths — simulated DB error returns 500
// ===========================================================================

func TestDD_ErrorPaths_DBError_Create(t *testing.T) {
	store := newFakeDBStore()
	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()
	store.simulateErr = true

	p := &Plugin{
		db:     &engine.SQLDBAdapter{DB: db},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	req := ddRequest("POST", "/datadog/configs", bytes.NewReader([]byte(`{"name":"t","api_key":"k"}`)))
	mux.ServeHTTP(rec, req)
	if rec.Code != 500 {
		t.Errorf("POST with DB error: want 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDD_ErrorPaths_DBError_List(t *testing.T) {
	store := newFakeDBStore()
	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	store.simulateErr = false
	store.configs = append(store.configs, fakeConfigRow{
		id: uuid.New().String(), tenantID: testTenantStr,
		name: "test", apiKey: "k", enabled: true,
	})
	store.simulateErr = true

	p := &Plugin{
		db:     &engine.SQLDBAdapter{DB: db},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	req := ddRequest("GET", "/datadog/configs", nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != 500 {
		t.Errorf("LIST with DB error: want 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDD_ErrorPaths_DBError_Get(t *testing.T) {
	store := newFakeDBStore()
	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	cfgID := uuid.New().String()
	store.configs = append(store.configs, fakeConfigRow{
		id: cfgID, tenantID: testTenantStr,
		name: "test", apiKey: "k", enabled: true,
	})
	store.simulateErr = true

	p := &Plugin{
		db:     &engine.SQLDBAdapter{DB: db},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	req := ddRequest("GET", "/datadog/configs/"+cfgID, nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != 500 {
		t.Errorf("GET with DB error: want 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDD_ErrorPaths_DBError_Update(t *testing.T) {
	store := newFakeDBStore()
	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	cfgID := uuid.New().String()
	store.configs = append(store.configs, fakeConfigRow{
		id: cfgID, tenantID: testTenantStr,
		name: "test", apiKey: "k", enabled: true,
	})
	store.simulateErr = true

	p := &Plugin{
		db:     &engine.SQLDBAdapter{DB: db},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	req := ddRequest("PUT", "/datadog/configs/"+cfgID, bytes.NewReader([]byte(`{"name":"updated"}`)))
	mux.ServeHTTP(rec, req)
	if rec.Code != 500 {
		t.Errorf("PUT with DB error: want 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDD_ErrorPaths_DBError_Delete(t *testing.T) {
	store := newFakeDBStore()
	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	cfgID := uuid.New().String()
	store.configs = append(store.configs, fakeConfigRow{
		id: cfgID, tenantID: testTenantStr,
		name: "test", apiKey: "k", enabled: true,
	})
	store.simulateErr = true

	p := &Plugin{
		db:     &engine.SQLDBAdapter{DB: db},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	req := ddRequest("DELETE", "/datadog/configs/"+cfgID, nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != 500 {
		t.Errorf("DELETE with DB error: want 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// exportMetrics error path — DB query error
// ===========================================================================

func TestDD_ExportMetrics_QueryError(t *testing.T) {
	store := newFakeDBStore()
	store.simulateErr = true
	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:     &engine.SQLDBAdapter{DB: db},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
	err := p.exportMetrics(context.Background())
	if err == nil {
		t.Error("expected error from exportMetrics with simulated db error")
	}
}

// ===========================================================================
// exportForConfig error path — DB query error
// ===========================================================================

func TestDD_ExportForConfig_QueryError(t *testing.T) {
	store := newFakeDBStore()
	store.simulateErr = true
	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:     &engine.SQLDBAdapter{DB: db},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
	cfg := ddConfigRow{
		ID:       uuid.New(),
		TenantID: testTenantID,
		APIKey:   "test",
		Site:     "datadoghq.com",
	}
	err := p.exportForConfig(context.Background(), cfg)
	if err == nil {
		t.Error("expected error from exportForConfig with simulated db error")
	}
}
