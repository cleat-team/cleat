// Package auditlog behavioral tests — fake DB + in-memory store, no PostgreSQL.
package auditlog

import (
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
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rcownie/cleat/internal/auth"
)

// ---------------------------------------------------------------------------
// In-memory fake DB store
// ---------------------------------------------------------------------------

type auditEventRow struct {
	id         uuid.UUID
	tenantID   uuid.UUID
	timestamp  time.Time
	method     string
	path       string
	statusCode int64
	userID     string
	ipAddress  string
	userAgent  string
	durationMs int64
	metadata   []byte
}

type fakeDBStore struct {
	mu      sync.RWMutex
	events  []auditEventRow
	apiKeys map[string]string // key_hash_hex -> tenant_id string
}

func newFakeDBStore() *fakeDBStore {
	return &fakeDBStore{
		events:  make([]auditEventRow, 0),
		apiKeys: make(map[string]string),
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

	switch {
	case strings.Contains(query, "INSERT INTO audit_events"):
		return c.execInsertAuditEvent(args)
	case strings.Contains(query, "DELETE FROM audit_events"):
		return c.execDeleteAuditEvents(args)
	default:
		return nil, fmt.Errorf("fakeConn: unexpected Exec query: %s", query)
	}
}

// --- QueryContext ---

func (c *fakeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	switch {
	case strings.Contains(query, "SELECT id, tenant_id, timestamp, method, path, status_code"):
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryAuditEvents(query, args)
	case strings.Contains(query, "SELECT tenant_id FROM tenant_api_keys"):
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryTenantLookup(args)
	default:
		return nil, fmt.Errorf("fakeConn: unexpected Query query: %s", query)
	}
}

// ---------------------------------------------------------------------------
// Exec implementations
// ---------------------------------------------------------------------------

func (c *fakeConn) execInsertAuditEvent(args []driver.NamedValue) (driver.Result, error) {
	tenantID, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	method, err := argString(args, 2)
	if err != nil {
		return nil, err
	}
	path, err := argString(args, 3)
	if err != nil {
		return nil, err
	}
	statusCode, err := argInt64(args, 4)
	if err != nil {
		return nil, err
	}
	userID, err := argString(args, 5)
	if err != nil {
		return nil, err
	}
	ipAddress, err := argString(args, 6)
	if err != nil {
		return nil, err
	}
	userAgent, err := argString(args, 7)
	if err != nil {
		return nil, err
	}
	durationMs, err := argInt64(args, 8)
	if err != nil {
		return nil, err
	}

	c.store.events = append(c.store.events, auditEventRow{
		id:         uuid.New(),
		tenantID:   uuid.MustParse(tenantID),
		timestamp:  time.Now(),
		method:     method,
		path:       path,
		statusCode: statusCode,
		userID:     userID,
		ipAddress:  ipAddress,
		userAgent:  userAgent,
		durationMs: durationMs,
		metadata:   []byte("{}"),
	})
	return &fakeResult{rowsAffected: 1}, nil
}

func (c *fakeConn) execDeleteAuditEvents(args []driver.NamedValue) (driver.Result, error) {
	cutoffVal, err := argAny(args, 1)
	if err != nil {
		return nil, err
	}
	cutoff, ok := cutoffVal.(time.Time)
	if !ok {
		return nil, fmt.Errorf("arg 1: want time.Time, got %T", cutoffVal)
	}

	var remaining []auditEventRow
	var deleted int64
	for _, e := range c.store.events {
		if e.timestamp.Before(cutoff) {
			deleted++
		} else {
			remaining = append(remaining, e)
		}
	}
	c.store.events = remaining
	return &fakeResult{rowsAffected: deleted}, nil
}

// ---------------------------------------------------------------------------
// Query implementations
// ---------------------------------------------------------------------------

// queryAuditEvents handles the SELECT ... FROM audit_events WHERE tenant_id = $1 ...
// with optional filters: method, path, status_code, timestamp >=, timestamp <=, LIMIT.
func (c *fakeConn) queryAuditEvents(query string, args []driver.NamedValue) (driver.Rows, error) {
	tenantID, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	tid := uuid.MustParse(tenantID)

	// Filter by tenant.
	var results []auditEventRow
	for _, e := range c.store.events {
		if e.tenantID == tid {
			results = append(results, e)
		}
	}

	// Apply filters sequentially from arg 2 onward.
	nextArg := 2

	// method filter
	if strings.Contains(query, "AND method = $") {
		if v, err := argAny(args, nextArg); err == nil {
			if s, ok := v.(string); ok && s != "" {
				var filtered []auditEventRow
				for _, e := range results {
					if e.method == s {
						filtered = append(filtered, e)
					}
				}
				results = filtered
			}
		}
		nextArg++
	}

	// path filter
	if strings.Contains(query, "AND path = $") {
		if v, err := argAny(args, nextArg); err == nil {
			if s, ok := v.(string); ok && s != "" {
				var filtered []auditEventRow
				for _, e := range results {
					if e.path == s {
						filtered = append(filtered, e)
					}
				}
				results = filtered
			}
		}
		nextArg++
	}

	// status_code filter
	if strings.Contains(query, "AND status_code = $") {
		if v, err := argAny(args, nextArg); err == nil {
			if code, ok := v.(int64); ok {
				var filtered []auditEventRow
				for _, e := range results {
					if e.statusCode == code {
						filtered = append(filtered, e)
					}
				}
				results = filtered
			}
		}
		nextArg++
	}

	// from (timestamp >=) filter
	if strings.Contains(query, "AND timestamp >=") {
		if v, err := argAny(args, nextArg); err == nil {
			if from, ok := v.(time.Time); ok {
				var filtered []auditEventRow
				for _, e := range results {
					if !e.timestamp.Before(from) {
						filtered = append(filtered, e)
					}
				}
				results = filtered
			}
		}
		nextArg++
	}

	// to (timestamp <=) filter
	if strings.Contains(query, "AND timestamp <=") {
		if v, err := argAny(args, nextArg); err == nil {
			if to, ok := v.(time.Time); ok {
				var filtered []auditEventRow
				for _, e := range results {
					if e.timestamp.Equal(to) || e.timestamp.Before(to) {
						filtered = append(filtered, e)
					}
				}
				results = filtered
			}
		}
		nextArg++
	}

	// Sort by timestamp DESC.
	sort.Slice(results, func(i, j int) bool {
		return results[i].timestamp.After(results[j].timestamp)
	})

	// LIMIT — always the last arg.
	for i := nextArg; i <= len(args); i++ {
		if v, err := argAny(args, i); err == nil {
			if lim, ok := v.(int64); ok && lim > 0 && int(lim) < len(results) {
				results = results[:lim]
				break
			}
			if f, ok := v.(float64); ok && int(f) < len(results) {
				results = results[:int(f)]
				break
			}
		}
	}

	rows, err := c.buildEventRows(results)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

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

func (c *fakeConn) buildEventRows(events []auditEventRow) (driver.Rows, error) {
	columns := []string{"id", "tenant_id", "timestamp", "method", "path", "status_code", "user_id", "ip_address", "user_agent", "duration_ms", "metadata"}
	var data [][]driver.Value
	for _, e := range events {
		data = append(data, []driver.Value{
			e.id.String(),
			e.tenantID.String(),
			e.timestamp,
			e.method,
			e.path,
			e.statusCode,
			e.userID,
			e.ipAddress,
			e.userAgent,
			e.durationMs,
			e.metadata,
		})
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

// setupTestPlugin creates a Plugin wired to an in-memory fake database.
// Middleware wraps the mux so all requests are recorded; auth middleware
// authenticates requests with "Authorization: Bearer test-api-key".
func setupTestPlugin(t *testing.T) (*Plugin, http.Handler, *fakeDBStore) {
	t.Helper()

	store := newFakeDBStore()

	// Pre-populate tenant API key so the auth middleware succeeds.
	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantStr

	db := sql.OpenDB(&fakeConnector{store: store})
	t.Cleanup(func() { db.Close() })

	p := &Plugin{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		config: Config{RetentionDays: 90},
	}

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	// Auth middleware -> Plugin middleware -> Mux.
	handler := auth.Middleware(db)(p.Middleware(mux))
	return p, handler, store
}

func authedRequest(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.Header.Set("Authorization", "Bearer test-api-key")
	return req
}

// waitForEventCount polls the store until at least n events exist or timeout.
func waitForEventCount(store *fakeDBStore, n int, timeout time.Duration) bool {
	deadline := time.After(timeout)
	for {
		store.mu.RLock()
		cnt := len(store.events)
		store.mu.RUnlock()
		if cnt >= n {
			return true
		}
		select {
		case <-deadline:
			return false
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// ---------------------------------------------------------------------------
// Behavioral tests
// ---------------------------------------------------------------------------

// TestMiddlewareRecordsEvent verifies that a request passing through the
// plugin middleware creates an audit event in the database.
func TestMiddlewareRecordsEvent(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	req := authedRequest("GET", "/audit/events", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !waitForEventCount(store, 1, time.Second) {
		t.Fatal("timed out waiting for audit event")
	}

	store.mu.RLock()
	event := store.events[0]
	store.mu.RUnlock()

	if event.method != "GET" {
		t.Errorf("expected method GET, got %s", event.method)
	}
	if event.path != "/audit/events" {
		t.Errorf("expected path /audit/events, got %s", event.path)
	}
	if event.tenantID != testTenantID {
		t.Errorf("expected tenant %s, got %s", testTenantStr, event.tenantID)
	}
}

// TestCreateAndQueryEvents creates events via middleware requests, then
// queries the audit log endpoint and verifies the response includes them.
func TestCreateAndQueryEvents(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	// Make two requests that will be recorded as audit events.
	for i := 0; i < 2; i++ {
		req := authedRequest("GET", "/audit/events", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /audit/events returned %d: %s", rec.Code, rec.Body.String())
		}
	}

	if !waitForEventCount(store, 2, time.Second) {
		t.Fatal("timed out waiting for 2 audit events")
	}

	// Now query via the route. Each event *about* querying also gets recorded,
	// so we have 4 total: events[0,1] from our two intentional requests,
	// and events[2,3] from the last two GET /audit/events calls.
	// The route returns all events for this tenant. Since we've made 4 total
	// requests through the middleware, we should see 4 events in the response.
	req := authedRequest("GET", "/audit/events", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /audit/events returned %d: %s", rec.Code, rec.Body.String())
	}

	// Wait for the last event to be recorded.
	if !waitForEventCount(store, 3, time.Second) {
		t.Fatal("timed out waiting for 3 audit events")
	}

	var events []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &events); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// We should have events in the response (at least 2 from our original requests).
	if len(events) < 2 {
		t.Fatalf("expected at least 2 events, got %d", len(events))
	}

	// Events should be ordered by timestamp DESC.
	for i := 1; i < len(events); i++ {
		ts1, _ := time.Parse(time.RFC3339Nano, events[i-1]["timestamp"].(string))
		ts2, _ := time.Parse(time.RFC3339Nano, events[i]["timestamp"].(string))
		if ts1.Before(ts2) {
			t.Errorf("events not in descending order at index %d", i)
		}
	}
}

// TestFilterByTenant verifies tenant isolation: events from different tenants
// are only visible to their respective tenant.
func TestFilterByTenant(t *testing.T) {
	store := newFakeDBStore()

	// Setup two tenants with API keys.
	tenantA := testTenantStr
	tenantB := uuid.MustParse("00000000-0000-0000-0000-000000000002").String()

	keyHashA := sha256.Sum256([]byte("key-a"))
	store.apiKeys[fmt.Sprintf("%x", keyHashA)] = tenantA

	keyHashB := sha256.Sum256([]byte("key-b"))
	store.apiKeys[fmt.Sprintf("%x", keyHashB)] = tenantB

	// Manually insert events for both tenants.
	store.events = append(store.events,
		auditEventRow{id: uuid.New(), tenantID: uuid.MustParse(tenantA), timestamp: time.Now(), method: "GET", path: "/tenant-a"},
		auditEventRow{id: uuid.New(), tenantID: uuid.MustParse(tenantB), timestamp: time.Now(), method: "POST", path: "/tenant-b"},
	)

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		config: Config{RetentionDays: 90},
	}

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	handler := auth.Middleware(db)(p.Middleware(mux))

	// Query as tenant A — should only see the GET /tenant-a event.
	req := httptest.NewRequest("GET", "/audit/events", nil)
	req.Header.Set("Authorization", "Bearer key-a")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var eventsA []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &eventsA); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if len(eventsA) != 1 {
		t.Fatalf("tenant A: expected 1 event, got %d", len(eventsA))
	}
	if eventsA[0]["method"] != "GET" {
		t.Errorf("tenant A: expected method GET, got %s", eventsA[0]["method"])
	}
	if eventsA[0]["path"] != "/tenant-a" {
		t.Errorf("tenant A: expected path /tenant-a, got %s", eventsA[0]["path"])
	}

	// Query as tenant B — should only see the POST /tenant-b event.
	req = httptest.NewRequest("GET", "/audit/events", nil)
	req.Header.Set("Authorization", "Bearer key-b")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var eventsB []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &eventsB); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if len(eventsB) != 1 {
		t.Fatalf("tenant B: expected 1 event, got %d", len(eventsB))
	}
	if eventsB[0]["method"] != "POST" {
		t.Errorf("tenant B: expected method POST, got %s", eventsB[0]["method"])
	}
}

// TestFilterByTimeRange creates events at different timestamps and verifies
// the from/to query parameters filter correctly.
func TestFilterByTimeRange(t *testing.T) {
	store := newFakeDBStore()

	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantStr

	now := time.Now()
	// Insert events with known timestamps.
	store.events = []auditEventRow{
		{id: uuid.New(), tenantID: testTenantID, timestamp: now.Add(-2 * time.Hour), method: "GET", path: "/old"},
		{id: uuid.New(), tenantID: testTenantID, timestamp: now, method: "GET", path: "/current"},
		{id: uuid.New(), tenantID: testTenantID, timestamp: now.Add(2 * time.Hour), method: "GET", path: "/future"},
	}

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		config: Config{RetentionDays: 90},
	}

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	handler := auth.Middleware(db)(mux)

	// Query with from=now-1h, to=now+1h — should only get /current.
	from := now.Add(-1 * time.Hour).Format(time.RFC3339)
	to := now.Add(1 * time.Hour).Format(time.RFC3339)

	req := authedRequest("GET", fmt.Sprintf("/audit/events?from=%s&to=%s", from, to), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var events []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &events); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event in time range, got %d: %+v", len(events), events)
	}
	if events[0]["path"] != "/current" {
		t.Errorf("expected path /current, got %s", events[0]["path"])
	}
}

// TestFilterByMethod verifies filtering audit events by HTTP method.
func TestFilterByMethod(t *testing.T) {
	store := newFakeDBStore()

	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantStr

	store.events = []auditEventRow{
		{id: uuid.New(), tenantID: testTenantID, timestamp: time.Now(), method: "GET", path: "/resource", statusCode: 200},
		{id: uuid.New(), tenantID: testTenantID, timestamp: time.Now(), method: "POST", path: "/resource", statusCode: 201},
		{id: uuid.New(), tenantID: testTenantID, timestamp: time.Now(), method: "DELETE", path: "/resource", statusCode: 204},
	}

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		config: Config{RetentionDays: 90},
	}

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	handler := auth.Middleware(db)(mux)

	req := authedRequest("GET", "/audit/events?method=POST", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var events []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &events); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event with method POST, got %d", len(events))
	}
	if events[0]["method"] != "POST" {
		t.Errorf("expected method POST, got %s", events[0]["method"])
	}
}

// TestFilterByStatus verifies filtering audit events by status code.
func TestFilterByStatus(t *testing.T) {
	store := newFakeDBStore()

	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantStr

	store.events = []auditEventRow{
		{id: uuid.New(), tenantID: testTenantID, timestamp: time.Now(), method: "GET", path: "/ok", statusCode: 200},
		{id: uuid.New(), tenantID: testTenantID, timestamp: time.Now(), method: "GET", path: "/not-found", statusCode: 404},
		{id: uuid.New(), tenantID: testTenantID, timestamp: time.Now(), method: "GET", path: "/error", statusCode: 500},
	}

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		config: Config{RetentionDays: 90},
	}

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	handler := auth.Middleware(db)(mux)

	req := authedRequest("GET", "/audit/events?status=404", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var events []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &events); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event with status 404, got %d", len(events))
	}
	if int(events[0]["status_code"].(float64)) != 404 {
		t.Errorf("expected status_code 404, got %v", events[0]["status_code"])
	}
}

// TestRetentionCleanup verifies the background retention cleanup deletes
// old events beyond the configured retention period.
func TestRetentionCleanup(t *testing.T) {
	store := newFakeDBStore()

	// Add some old events (beyond 90-day retention).
	oldTime := time.Now().Add(-100 * 24 * time.Hour)
	store.events = append(store.events,
		auditEventRow{id: uuid.New(), tenantID: testTenantID, timestamp: oldTime, method: "GET", path: "/old1"},
		auditEventRow{id: uuid.New(), tenantID: testTenantID, timestamp: oldTime.Add(-1 * time.Hour), method: "GET", path: "/old2"},
	)

	// Add a recent event (within retention).
	store.events = append(store.events,
		auditEventRow{id: uuid.New(), tenantID: testTenantID, timestamp: time.Now(), method: "GET", path: "/recent"},
	)

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		config: Config{RetentionDays: 90},
	}

	// Run cleanup directly.
	affected, err := p.cleanupRetention(context.Background())
	if err != nil {
		t.Fatalf("cleanupRetention: %v", err)
	}
	if affected != 2 {
		t.Errorf("expected 2 deleted events, got %d", affected)
	}

	// Verify only the recent event remains.
	store.mu.RLock()
	remaining := len(store.events)
	store.mu.RUnlock()
	if remaining != 1 {
		t.Fatalf("expected 1 remaining event, got %d", remaining)
	}

	store.mu.RLock()
	lastEvent := store.events[0]
	store.mu.RUnlock()
	if lastEvent.path != "/recent" {
		t.Errorf("expected remaining event path /recent, got %s", lastEvent.path)
	}
}

// TestQueryEventsLimit verifies the limit parameter.
func TestQueryEventsLimit(t *testing.T) {
	store := newFakeDBStore()

	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantStr

	// Insert 5 events.
	for i := 0; i < 5; i++ {
		store.events = append(store.events, auditEventRow{
			id: uuid.New(), tenantID: testTenantID, timestamp: time.Now(),
			method: "GET", path: fmt.Sprintf("/item-%d", i),
		})
	}

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		config: Config{RetentionDays: 90},
	}

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	handler := auth.Middleware(db)(mux)

	req := authedRequest("GET", "/audit/events?limit=2", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var events []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &events); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if len(events) > 2 {
		t.Fatalf("expected at most 2 events with limit=2, got %d", len(events))
	}
}
