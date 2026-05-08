// Package webhookingest behavioral tests — fake DB + in-memory store, no PostgreSQL.
package webhookingest

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
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
	"github.com/rcownie/cleat/internal/auth"
	"github.com/rcownie/cleat/internal/plugin"
)

// ---------------------------------------------------------------------------
// In-memory fake DB store
// ---------------------------------------------------------------------------

type webhookSourceRow struct {
	id              string
	tenantID        string
	name            string
	sourceType      string
	secret          string
	enabled         bool
	signalWorkflowID string
	signalName      string
	createdAt       time.Time
	updatedAt       time.Time
}

type webhookEventRow struct {
	id          string
	sourceID    string
	tenantID    string
	eventType   string
	headers     string
	payload     string
	receivedAt  time.Time
	processed   bool
	retryCount  int
	lastRetryAt *time.Time
	status      string
	errorMsg    *string
}

type fakeDBStore struct {
	mu      sync.RWMutex
	sources []webhookSourceRow
	events  []webhookEventRow
	apiKeys map[string]string // key_hash_hex -> tenant_id
}

func newFakeDBStore() *fakeDBStore {
	return &fakeDBStore{
		sources: make([]webhookSourceRow, 0),
		events:  make([]webhookEventRow, 0),
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
	case strings.Contains(query, "INSERT INTO webhook_sources"):
		return c.execInsertSource(args)
	case strings.Contains(query, "INSERT INTO webhook_events"):
		return c.execInsertEvent(args)
	case strings.Contains(query, "UPDATE webhook_events SET processed"):
		return c.execUpdateEventProcessed(args)
	case strings.Contains(query, "UPDATE webhook_events SET retry_count"):
		return c.execUpdateEventRetry(args)
	case strings.Contains(query, "UPDATE webhook_events"):
		return c.execUpdateEvent(args)
	case strings.Contains(query, "DELETE FROM webhook_sources"):
		return c.execDeleteSource(args)
	default:
		return nil, fmt.Errorf("fakeConn: unexpected Exec query: %s", query)
	}
}

// --- QueryContext ---

func (c *fakeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	switch {
	case strings.Contains(query, "SELECT tenant_id FROM tenant_api_keys"):
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryTenantLookup(args)
	case strings.Contains(query, "SELECT id, tenant_id, name, source_type, secret, enabled, signal_workflow_id, signal_name, created_at, updated_at"):
		if strings.Contains(query, "WHERE id = $1 AND") {
			c.store.mu.RLock()
			defer c.store.mu.RUnlock()
			return c.queryGetSource(args)
		}
		if strings.Contains(query, "WHERE id = $1") {
			c.store.mu.RLock()
			defer c.store.mu.RUnlock()
			return c.querySourceByID(args)
		}
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryListSources(args)
	case strings.Contains(query, "SELECT id, source_id, tenant_id, event_type, headers, payload, received_at, processed"):
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryListEvents(query, args)
	case strings.Contains(query, "SELECT id, event_type, payload, received_at"):
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryAwaitEvents(query, args)
	case strings.Contains(query, "SELECT e.id, e.source_id, e.event_type, e.payload, e.received_at"):
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryProcessBatch(args)
	default:
		return nil, fmt.Errorf("fakeConn: unexpected Query query: %s", query)
	}
}

// ---------------------------------------------------------------------------
// Exec implementations
// ---------------------------------------------------------------------------

func (c *fakeConn) execInsertSource(args []driver.NamedValue) (driver.Result, error) {
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
	sourceType, err := argString(args, 4)
	if err != nil {
		return nil, err
	}
	secret, err := argString(args, 5)
	if err != nil {
		return nil, err
	}
	nowVal, err := argTime(args, 6)
	if err != nil {
		return nil, err
	}

	var signalWorkflowID string
	if len(args) >= 7 {
		if v, err := argAny(args, 7); err == nil && v != nil {
			signalWorkflowID, _ = v.(string)
		}
	}

	signalName := "webhook_received"
	if len(args) >= 8 {
		if v, err := argString(args, 8); err == nil {
			signalName = v
		}
	}

	c.store.sources = append(c.store.sources, webhookSourceRow{
		id:               id,
		tenantID:         tenantID,
		name:             name,
		sourceType:       sourceType,
		secret:           secret,
		enabled:          true,
		signalWorkflowID: signalWorkflowID,
		signalName:       signalName,
		createdAt:        nowVal,
		updatedAt:        nowVal,
	})
	return &fakeResult{rowsAffected: 1}, nil
}

func (c *fakeConn) execInsertEvent(args []driver.NamedValue) (driver.Result, error) {
	id, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	sourceID, err := argString(args, 2)
	if err != nil {
		return nil, err
	}
	tenantID, err := argString(args, 3)
	if err != nil {
		return nil, err
	}
	eventType, err := argString(args, 4)
	if err != nil {
		return nil, err
	}
	headers, err := argString(args, 5)
	if err != nil {
		return nil, err
	}
	payload, err := argString(args, 6)
	if err != nil {
		return nil, err
	}
	nowVal, err := argTime(args, 7)
	if err != nil {
		return nil, err
	}

	c.store.events = append(c.store.events, webhookEventRow{
		id:         id,
		sourceID:   sourceID,
		tenantID:   tenantID,
		eventType:  eventType,
		headers:    headers,
		payload:    payload,
		receivedAt: nowVal,
		processed:  false,
		status:     "pending",
	})
	return &fakeResult{rowsAffected: 1}, nil
}

func (c *fakeConn) execUpdateEventProcessed(args []driver.NamedValue) (driver.Result, error) {
	id, err := argString(args, 1)
	if err != nil {
		return nil, err
	}

	for i, evt := range c.store.events {
		if evt.id == id {
			evt.processed = true
			evt.status = "completed"
			evt.errorMsg = nil
			c.store.events[i] = evt
			return &fakeResult{rowsAffected: 1}, nil
		}
	}
	return &fakeResult{rowsAffected: 0}, nil
}

func (c *fakeConn) execUpdateEventRetry(args []driver.NamedValue) (driver.Result, error) {
	id, err := argString(args, 1)
	if err != nil {
		return nil, err
	}

	for i, evt := range c.store.events {
		if evt.id == id {
			if len(args) >= 2 {
				if v, err := argInt64(args, 2); err == nil {
					evt.retryCount = int(v)
				}
			}
			if len(args) >= 3 {
				if v, err := argAny(args, 3); err == nil {
					if s, ok := v.(string); ok {
						evt.errorMsg = &s
					}
				}
			}
			now := time.Now()
			evt.lastRetryAt = &now

			// Determine status from the query.
			if strings.Contains(fmt.Sprintf("%v", args), "dead_letter") {
				evt.status = "dead_letter"
				evt.processed = true
			} else {
				evt.status = "pending"
			}
			c.store.events[i] = evt
			return &fakeResult{rowsAffected: 1}, nil
		}
	}
	return &fakeResult{rowsAffected: 0}, nil
}

func (c *fakeConn) execUpdateEvent(args []driver.NamedValue) (driver.Result, error) {
	return c.execUpdateEventProcessed(args)
}

func (c *fakeConn) execDeleteSource(args []driver.NamedValue) (driver.Result, error) {
	id, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	tid, err := argString(args, 2)
	if err != nil {
		return nil, err
	}

	for i, src := range c.store.sources {
		if src.id == id && src.tenantID == tid {
			c.store.sources = append(c.store.sources[:i], c.store.sources[i+1:]...)
			return &fakeResult{rowsAffected: 1}, nil
		}
	}
	return &fakeResult{rowsAffected: 0}, nil
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

func (c *fakeConn) queryListSources(args []driver.NamedValue) (driver.Rows, error) {
	tid, err := argString(args, 1)
	if err != nil {
		return nil, err
	}

	var results []webhookSourceRow
	for _, s := range c.store.sources {
		if s.tenantID == tid {
			results = append(results, s)
		}
	}

	columns := []string{"id", "tenant_id", "name", "source_type", "secret", "enabled", "signal_workflow_id", "signal_name", "created_at", "updated_at"}
	var data [][]driver.Value
	for _, s := range results {
		data = append(data, []driver.Value{
			s.id, s.tenantID, s.name, s.sourceType, s.secret,
			s.enabled, s.signalWorkflowID, s.signalName,
			s.createdAt, s.updatedAt,
		})
	}
	return &fakeRows{columns: columns, data: data}, nil
}

func (c *fakeConn) querySourceByID(args []driver.NamedValue) (driver.Rows, error) {
	id, err := argString(args, 1)
	if err != nil {
		return nil, err
	}

	for _, s := range c.store.sources {
		if s.id == id {
			return &fakeRows{
				columns: []string{"id", "tenant_id", "name", "source_type", "secret", "enabled", "signal_workflow_id", "signal_name", "created_at", "updated_at"},
				data: [][]driver.Value{{
					s.id, s.tenantID, s.name, s.sourceType, s.secret,
					s.enabled, s.signalWorkflowID, s.signalName,
					s.createdAt, s.updatedAt,
				}},
			}, nil
		}
	}
	return &fakeRows{columns: []string{"id", "tenant_id", "name", "source_type", "secret", "enabled", "signal_workflow_id", "signal_name", "created_at", "updated_at"}}, nil
}

func (c *fakeConn) queryGetSource(args []driver.NamedValue) (driver.Rows, error) {
	id, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	tid, err := argString(args, 2)
	if err != nil {
		return nil, err
	}

	for _, s := range c.store.sources {
		if s.id == id && s.tenantID == tid {
			return &fakeRows{
				columns: []string{"id", "tenant_id", "name", "source_type", "secret", "enabled", "signal_workflow_id", "signal_name", "created_at", "updated_at"},
				data: [][]driver.Value{{
					s.id, s.tenantID, s.name, s.sourceType, s.secret,
					s.enabled, s.signalWorkflowID, s.signalName,
					s.createdAt, s.updatedAt,
				}},
			}, nil
		}
	}
	return &fakeRows{columns: []string{"id", "tenant_id", "name", "source_type", "secret", "enabled", "signal_workflow_id", "signal_name", "created_at", "updated_at"}}, nil
}

func (c *fakeConn) queryListEvents(query string, args []driver.NamedValue) (driver.Rows, error) {
	tid, err := argString(args, 1)
	if err != nil {
		return nil, err
	}

	var results []webhookEventRow
	for _, e := range c.store.events {
		if e.tenantID == tid {
			results = append(results, e)
		}
	}

	// Apply optional filters.
	nextArg := 2

	// source_id filter
	if strings.Contains(query, "AND source_id = $") {
		if v, err := argAny(args, nextArg); err == nil {
			if sid, ok := v.(string); ok {
				var filtered []webhookEventRow
				for _, e := range results {
					if e.sourceID == sid {
						filtered = append(filtered, e)
					}
				}
				results = filtered
			}
		}
		nextArg++
	}

	// event_type filter
	if strings.Contains(query, "AND event_type = $") {
		if v, err := argAny(args, nextArg); err == nil {
			if et, ok := v.(string); ok {
				var filtered []webhookEventRow
				for _, e := range results {
					if e.eventType == et {
						filtered = append(filtered, e)
					}
				}
				results = filtered
			}
		}
		nextArg++
	}

	// processed filter
	if strings.Contains(query, "AND processed = $") {
		if v, err := argAny(args, nextArg); err == nil {
			if p, ok := v.(bool); ok {
				var filtered []webhookEventRow
				for _, e := range results {
					if e.processed == p {
						filtered = append(filtered, e)
					}
				}
				results = filtered
			}
		}
		nextArg++
	}

	// Sort by received_at DESC
	sorted := make([]webhookEventRow, len(results))
	copy(sorted, results)
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j].receivedAt.After(sorted[i].receivedAt) {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	results = sorted

	// Apply LIMIT
	for i := nextArg; i <= len(args); i++ {
		if v, err := argAny(args, i); err == nil {
			if lim, ok := v.(int64); ok && lim > 0 && int(lim) < len(results) {
				results = results[:lim]
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

func (c *fakeConn) queryAwaitEvents(query string, args []driver.NamedValue) (driver.Rows, error) {
	tid, err := argString(args, 1)
	if err != nil {
		return nil, err
	}

	var results []webhookEventRow
	for _, e := range c.store.events {
		if e.tenantID == tid && !e.processed {
			results = append(results, e)
		}
	}

	nextArg := 2

	// source_id filter
	if strings.Contains(query, "AND source_id = $") {
		if v, err := argAny(args, nextArg); err == nil {
			if sid, ok := v.(string); ok {
				var filtered []webhookEventRow
				for _, e := range results {
					if e.sourceID == sid {
						filtered = append(filtered, e)
					}
				}
				results = filtered
			}
		}
		nextArg++
	}

	// event_type filter
	if strings.Contains(query, "AND event_type = $") {
		if v, err := argAny(args, nextArg); err == nil {
			if et, ok := v.(string); ok {
				var filtered []webhookEventRow
				for _, e := range results {
					if e.eventType == et {
						filtered = append(filtered, e)
					}
				}
				results = filtered
			}
		}
		nextArg++
	}

	// Sort by received_at DESC, take first
	if len(results) > 0 {
		best := results[0]
		for _, e := range results[1:] {
			if e.receivedAt.After(best.receivedAt) {
				best = e
			}
		}
		results = []webhookEventRow{best}
	}

	columns := []string{"id", "event_type", "payload", "received_at"}
	var data [][]driver.Value
	for _, e := range results {
		data = append(data, []driver.Value{
			e.id, e.eventType, []byte(e.payload), e.receivedAt,
		})
	}
	return &fakeRows{columns: columns, data: data}, nil
}

func (c *fakeConn) queryProcessBatch(_ []driver.NamedValue) (driver.Rows, error) {
	// Find unprocessed events older than ~10 seconds.
	var results []webhookEventRow
	cutoff := time.Now().Add(-10 * time.Second)

	for _, e := range c.store.events {
		if !e.processed && (e.status == "pending" || e.status == "") && e.receivedAt.Before(cutoff) {
			results = append(results, e)
		}
	}

	// Sort by received_at ASC
	for i := 0; i < len(results); i++ {
		for j := i + 1; j < len(results); j++ {
			if results[j].receivedAt.Before(results[i].receivedAt) {
				results[i], results[j] = results[j], results[i]
			}
		}
	}

	if len(results) > 100 {
		results = results[:100]
	}

	columns := []string{"id", "source_id", "event_type", "payload", "received_at", "signal_workflow_id", "signal_name", "retry_count"}
	var data [][]driver.Value
	for _, e := range results {
		// Find the source for this event.
		signalWorkflowID := ""
		signalName := "webhook_received"
		for _, s := range c.store.sources {
			if s.id == e.sourceID {
				signalWorkflowID = s.signalWorkflowID
				signalName = s.signalName
				if signalName == "" {
					signalName = "webhook_received"
				}
				break
			}
		}

		data = append(data, []driver.Value{
			e.id, e.sourceID, e.eventType, []byte(e.payload), e.receivedAt,
			signalWorkflowID, signalName, int64(e.retryCount),
		})
	}
	return &fakeRows{columns: columns, data: data}, nil
}

func (c *fakeConn) buildEventRows(events []webhookEventRow) (driver.Rows, error) {
	columns := []string{"id", "source_id", "tenant_id", "event_type", "headers", "payload", "received_at", "processed"}
	var data [][]driver.Value
	for _, e := range events {
		data = append(data, []driver.Value{
			e.id, e.sourceID, e.tenantID, e.eventType,
			[]byte(e.headers), []byte(e.payload), e.receivedAt, e.processed,
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
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	// Auth middleware for management routes; ingest route works without auth.
	handler := auth.Middleware(db)(mux)
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

// TestCreateSource verifies creating a webhook ingestion source and
// reading it back.
func TestCreateSource(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	body := `{"name":"github-webhook","source_type":"github","secret":"my-secret"}`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var created map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	id := created["id"].(string)
	if created["name"] != "github-webhook" {
		t.Errorf("expected name 'github-webhook', got %s", created["name"])
	}
	if created["source_type"] != "github" {
		t.Errorf("expected source_type 'github', got %s", created["source_type"])
	}
	if created["enabled"] != true {
		t.Errorf("expected enabled=true, got %v", created["enabled"])
	}
	if endpoint, ok := created["endpoint_url"].(string); !ok || endpoint == "" {
		t.Errorf("expected non-empty endpoint_url, got %q", endpoint)
	}

	// Verify in store.
	store.mu.RLock()
	sourceCount := len(store.sources)
	store.mu.RUnlock()
	if sourceCount != 1 {
		t.Fatalf("expected 1 source in store, got %d", sourceCount)
	}

	// GET by ID.
	req = authedRequest("GET", "/ingest/sources/"+id, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var fetched map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &fetched)
	if fetched["name"] != "github-webhook" {
		t.Errorf("expected name 'github-webhook', got %s", fetched["name"])
	}
}

// TestCreateSourceDefaults verifies default values when creating a source
// without source_type.
func TestCreateSourceDefaults(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	body := `{"name":"generic-source"}`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var created map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &created)
	if created["source_type"] != "generic" {
		t.Errorf("expected default source_type 'generic', got %s", created["source_type"])
	}
}

// TestIngestWebhookPayload verifies the full flow: create source → POST
// payload to ingest endpoint → event stored.
func TestIngestWebhookPayload(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	// Create a source.
	createBody := `{"name":"test-source","source_type":"generic"}`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(createBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create source: expected 201, got %d", rec.Code)
	}

	var created map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &created)
	sourceID := created["id"].(string)

	// Ingest a webhook payload (no auth required on ingest endpoint).
	payload := `{"action":"opened","issue":{"number":1}}`
	req = httptest.NewRequest("POST", "/ingest/"+sourceID, bytes.NewReader([]byte(payload)))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("ingest: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var ingestResp map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &ingestResp)
	if ingestResp["received"] != true {
		t.Errorf("expected received=true, got %v", ingestResp["received"])
	}
	eventID := ingestResp["id"].(string)

	// Verify event in store.
	store.mu.RLock()
	eventCount := len(store.events)
	store.mu.RUnlock()
	if eventCount != 1 {
		t.Fatalf("expected 1 event in store, got %d", eventCount)
	}

	store.mu.RLock()
	event := store.events[0]
	store.mu.RUnlock()
	if event.id != eventID {
		t.Errorf("expected event id %s, got %s", eventID, event.id)
	}
	if event.sourceID != sourceID {
		t.Errorf("expected source_id %s, got %s", sourceID, event.sourceID)
	}
	if event.processed {
		t.Errorf("expected processed=false")
	}

	// Verify the event is listed via GET /ingest/events.
	req = authedRequest("GET", "/ingest/events", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list events: expected 200, got %d", rec.Code)
	}

	var events []map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &events)
	if len(events) != 1 {
		t.Fatalf("expected 1 event in list, got %d", len(events))
	}
	if events[0]["id"] != eventID {
		t.Errorf("expected event id %s, got %s", eventID, events[0]["id"])
	}
}

// TestIngestWithHMACVerification verifies HMAC signature validation.
func TestIngestWithHMACVerification(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	// Create source with a secret.
	createBody := `{"name":"signed-source","source_type":"github","secret":"my-hmac-secret"}`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(createBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create source: expected 201, got %d", rec.Code)
	}

	var created map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &created)
	sourceID := created["id"].(string)

	// Ingest with valid HMAC signature.
	payload := `{"action":"opened"}`
	mac := hmac.New(sha256.New, []byte("my-hmac-secret"))
	mac.Write([]byte(payload))
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	req = httptest.NewRequest("POST", "/ingest/"+sourceID, bytes.NewReader([]byte(payload)))
	req.Header.Set("X-Hub-Signature-256", sig)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("valid sig: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestIngestInvalidHMAC verifies that an invalid HMAC signature is rejected.
func TestIngestInvalidHMAC(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	createBody := `{"name":"signed-source","source_type":"github","secret":"real-secret"}`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(createBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create source: expected 201, got %d", rec.Code)
	}

	var created map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &created)
	sourceID := created["id"].(string)

	// Ingest with WRONG HMAC signature.
	payload := `{"action":"opened"}`
	req = httptest.NewRequest("POST", "/ingest/"+sourceID, bytes.NewReader([]byte(payload)))
	req.Header.Set("X-Hub-Signature-256", "sha256=invalid")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid sig: expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestIngestMissingHMAC verifies that missing HMAC signature is rejected
// when the source has a secret.
func TestIngestMissingHMAC(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	createBody := `{"name":"signed-source","source_type":"github","secret":"some-secret"}`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(createBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create source: expected 201, got %d", rec.Code)
	}

	var created map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &created)
	sourceID := created["id"].(string)

	// Ingest without HMAC signature.
	payload := `{"action":"opened"}`
	req = httptest.NewRequest("POST", "/ingest/"+sourceID, bytes.NewReader([]byte(payload)))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing sig: expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestIngestDisabledSource verifies that a disabled source returns 403.
func TestIngestDisabledSource(t *testing.T) {
	store := newFakeDBStore()
	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantStr

	// Directly insert a disabled source.
	sourceID := uuid.New()
	store.sources = append(store.sources, webhookSourceRow{
		id:         sourceID.String(),
		tenantID:   testTenantStr,
		name:       "disabled",
		sourceType: "generic",
		enabled:    false,
	})

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	handler := auth.Middleware(db)(mux)

	req := httptest.NewRequest("POST", "/ingest/"+sourceID.String(), bytes.NewReader([]byte(`{"test":true}`)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for disabled source, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestBackgroundRetryWorker verifies processBatch retries unprocessed events.
func TestBackgroundRetryWorker(t *testing.T) {
	store := newFakeDBStore()

	sourceID := uuid.New()
	store.sources = append(store.sources, webhookSourceRow{
		id:               sourceID.String(),
		tenantID:         testTenantStr,
		name:             "test",
		sourceType:       "generic",
		enabled:          true,
		signalWorkflowID: "wf-123",
		signalName:       "webhook_received",
	})

	// Add an unprocessed event older than 10 seconds.
	eventID := uuid.New()
	store.events = append(store.events, webhookEventRow{
		id:         eventID.String(),
		sourceID:   sourceID.String(),
		tenantID:   testTenantStr,
		eventType:  "webhook",
		payload:    `{"hello":"world"}`,
		receivedAt: time.Now().Add(-30 * time.Second),
		processed:  false,
		status:     "pending",
	})

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	signalChan := make(chan string, 1)
	p := &Plugin{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		env: &plugin.Environment{
			SignalWorkflow: func(ctx context.Context, workflowID, signalName, payload string) error {
				signalChan <- payload
				return nil
			},
		},
	}

	// Call processBatch directly (what the background worker calls).
	p.processBatch(context.Background())

	// Verify the signal was sent.
	select {
	case payload := <-signalChan:
		if !strings.Contains(payload, "hello") {
			t.Errorf("expected signal payload to contain 'hello', got %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for signal delivery")
	}

	// Verify event was marked processed.
	store.mu.RLock()
	evt := store.events[0]
	store.mu.RUnlock()
	if !evt.processed {
		t.Errorf("expected event to be marked processed")
	}
	if evt.status != "completed" {
		t.Errorf("expected status 'completed', got %s", evt.status)
	}
}

// TestAwaitWebhookHostFunction verifies the await_webhook host function
// finds and claims unprocessed events.
func TestAwaitWebhookHostFunction(t *testing.T) {
	store := newFakeDBStore()

	sourceID := uuid.New()
	store.sources = append(store.sources, webhookSourceRow{
		id:         sourceID.String(),
		tenantID:   testTenantStr,
		name:       "test",
		sourceType: "generic",
		enabled:    true,
	})

	// Add an unprocessed event.
	eventID := uuid.New()
	store.events = append(store.events, webhookEventRow{
		id:         eventID.String(),
		sourceID:   sourceID.String(),
		tenantID:   testTenantStr,
		eventType:  "push",
		payload:    `{"ref":"main"}`,
		receivedAt: time.Now(),
		processed:  false,
		status:     "pending",
	})

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// Call awaitWebhook with context containing tenant info.
	callCtx := &plugin.CallContext{TenantID: testTenantID, WorkflowID: "test-wf"}
	ctx := plugin.WithCallContext(context.Background(), callCtx)

	input, _ := json.Marshal(map[string]interface{}{
		"source_id":  sourceID.String(),
		"event_type": "push",
	})
	output, err := p.awaitWebhook(ctx, string(input))
	if err != nil {
		t.Fatalf("awaitWebhook: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("failed to decode output: %v", err)
	}
	if result["found"] != true {
		t.Errorf("expected found=true, got %v", result["found"])
	}
	if result["event_type"] != "push" {
		t.Errorf("expected event_type 'push', got %s", result["event_type"])
	}

	// Verify event was marked processed.
	store.mu.RLock()
	evt := store.events[0]
	store.mu.RUnlock()
	if !evt.processed {
		t.Errorf("expected event to be marked processed after consumption")
	}
}

// TestAwaitWebhookNoEvents verifies that awaitWebhook returns found=false
// when no matching events exist.
func TestAwaitWebhookNoEvents(t *testing.T) {
	store := newFakeDBStore()

	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantStr

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	callCtx := &plugin.CallContext{TenantID: testTenantID, WorkflowID: "test-wf"}
	ctx := plugin.WithCallContext(context.Background(), callCtx)

	input, _ := json.Marshal(map[string]interface{}{
		"source_id": uuid.New().String(),
	})
	output, err := p.awaitWebhook(ctx, string(input))
	if err != nil {
		t.Fatalf("awaitWebhook: %v", err)
	}

	var result map[string]interface{}
	json.Unmarshal([]byte(output), &result)
	if result["found"] != false {
		t.Errorf("expected found=false, got %v", result["found"])
	}
}

// TestSourceDelete verifies deleting a webhook source.
func TestSourceDelete(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	createBody := `{"name":"to-delete","source_type":"generic"}`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(createBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", rec.Code)
	}

	var created map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &created)
	id := created["id"].(string)

	req = authedRequest("DELETE", "/ingest/sources/"+id, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE: expected 204, got %d", rec.Code)
	}

	store.mu.RLock()
	count := len(store.sources)
	store.mu.RUnlock()
	if count != 0 {
		t.Errorf("expected 0 sources after delete, got %d", count)
	}
}
