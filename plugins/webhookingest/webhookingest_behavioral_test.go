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
	"github.com/rcownie/cleat/internal/host"
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
	failNextQuery    bool
	failNextExec     bool
	querySkip        int
	execSkip         int
	corruptNextScan  bool
	corruptScanSkip  int
	failNextRowsErr  bool
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

	if c.store.failNextExec {
		if c.store.execSkip > 0 {
			c.store.execSkip--
		} else {
			c.store.failNextExec = false
			return nil, fmt.Errorf("simulated exec error")
		}
	}

	switch {
	case strings.Contains(query, "INSERT INTO webhook_sources"):
		return c.execInsertSource(args)
	case strings.Contains(query, "INSERT INTO webhook_events"):
		return c.execInsertEvent(args)
	case strings.Contains(query, "SET retry_count"):
		return c.execUpdateEventRetry(query, args)
	case strings.Contains(query, "SET processed"):
		return c.execUpdateEventProcessed(args)
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
	// Check fail flag under write lock, then proceed.
	c.store.mu.Lock()
	failed := c.store.failNextQuery
	if failed && c.store.querySkip > 0 {
		c.store.querySkip--
		failed = false
	}
	if failed {
		c.store.failNextQuery = false
	}
	corrupt := c.store.corruptNextScan
	if corrupt && c.store.corruptScanSkip > 0 {
		c.store.corruptScanSkip--
		corrupt = false
	}
	if corrupt {
		c.store.corruptNextScan = false
	}
	failRowsErr := c.store.failNextRowsErr
	if failRowsErr {
		c.store.failNextRowsErr = false
	}
	c.store.mu.Unlock()

	if failed {
		return nil, fmt.Errorf("simulated query error")
	}

	if failRowsErr {
		return &rowsErrFakeRows{}, nil
	}

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
		return c.queryListSources(args, corrupt)
	case strings.Contains(query, "SELECT id, source_id, tenant_id, event_type, headers, payload, received_at, processed"):
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryListEvents(query, args, corrupt)
	case strings.Contains(query, "SELECT id, event_type, payload, received_at"):
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryAwaitEvents(query, args)
	case strings.Contains(query, "SELECT e.id, e.source_id, e.event_type, e.payload, e.received_at"):
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryProcessBatch(args, corrupt)
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

func (c *fakeConn) execUpdateEventRetry(query string, args []driver.NamedValue) (driver.Result, error) {
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
			if strings.Contains(query, "dead_letter") {
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

func (c *fakeConn) queryListSources(args []driver.NamedValue, corrupt bool) (driver.Rows, error) {
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
	for i, s := range results {
		enabled := driver.Value(s.enabled)
		if corrupt && i == 0 {
			enabled = "not-a-bool"
		}
		data = append(data, []driver.Value{
			s.id, s.tenantID, s.name, s.sourceType, s.secret,
			enabled, s.signalWorkflowID, s.signalName,
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

func (c *fakeConn) queryListEvents(query string, args []driver.NamedValue, corrupt bool) (driver.Rows, error) {
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

	rows, err := c.buildEventRows(results, corrupt)
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

func (c *fakeConn) queryProcessBatch(_ []driver.NamedValue, corrupt bool) (driver.Rows, error) {
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
	for i, e := range results {
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

		retryCountVal := driver.Value(int64(e.retryCount))
		if corrupt && i == 0 {
			retryCountVal = "not-an-int"
		}
		data = append(data, []driver.Value{
			e.id, e.sourceID, e.eventType, []byte(e.payload), e.receivedAt,
			signalWorkflowID, signalName, retryCountVal,
		})
	}
	return &fakeRows{columns: columns, data: data}, nil
}

func (c *fakeConn) buildEventRows(events []webhookEventRow, corrupt bool) (driver.Rows, error) {
	columns := []string{"id", "source_id", "tenant_id", "event_type", "headers", "payload", "received_at", "processed"}
	var data [][]driver.Value
	for i, e := range events {
		processed := driver.Value(e.processed)
		if corrupt && i == 0 {
			processed = "not-a-bool"
		}
		data = append(data, []driver.Value{
			e.id, e.sourceID, e.tenantID, e.eventType,
			[]byte(e.headers), []byte(e.payload), e.receivedAt, processed,
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
		db:     &host.SQLDBAdapter{DB: db},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	// Auth middleware for management routes; ingest route works without auth.
	handler := auth.Middleware(host.NewPostgresStore(db))(mux)
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

// ---------------------------------------------------------------------------
// Fake function registry
// ---------------------------------------------------------------------------

type fakeFuncRegistry struct {
	funcs map[string]plugin.PluginFunc
}

func newFakeFuncRegistry() *fakeFuncRegistry {
	return &fakeFuncRegistry{funcs: map[string]plugin.PluginFunc{}}
}

func (r *fakeFuncRegistry) Register(opts plugin.FuncOptions, fn plugin.PluginFunc) error {
	r.funcs[opts.Name] = fn
	return nil
}

func (r *fakeFuncRegistry) Has(name string) bool {
	_, ok := r.funcs[name]
	return ok
}

// errReadCloser simulates an io.ReadCloser that fails on Read.
type errReadCloser struct{}

func (*errReadCloser) Read(_ []byte) (int, error) { return 0, fmt.Errorf("simulated read error") }
func (*errReadCloser) Close() error               { return nil }

// rowsErrFakeRows returns a non-EOF error from Next() and non-nil from Err().
type rowsErrFakeRows struct{ pos int }

func (*rowsErrFakeRows) Columns() []string { return []string{"id"} }
func (*rowsErrFakeRows) Close() error      { return nil }
func (r *rowsErrFakeRows) Next(_ []driver.Value) error {
	if r.pos > 0 {
		return io.EOF
	}
	r.pos++
	return fmt.Errorf("simulated rows iteration error")
}
func (*rowsErrFakeRows) Err() error { return fmt.Errorf("simulated rows iteration error") }

type rowsErrConnector struct{ store *fakeDBStore }

func (c *rowsErrConnector) Connect(_ context.Context) (driver.Conn, error) { return &rowsErrConn{store: c.store}, nil }
func (c *rowsErrConnector) Driver() driver.Driver { return &rowsErrDrv{} }

type rowsErrConn struct{ store *fakeDBStore }

func (*rowsErrConn) Prepare(_ string) (driver.Stmt, error) { return nil, fmt.Errorf("rowsErrConn: unexpected Prepare") }
func (*rowsErrConn) Close() error                           { return nil }
func (*rowsErrConn) Begin() (driver.Tx, error)              { return nil, fmt.Errorf("rowsErrConn: no tx") }
func (c *rowsErrConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	// For processBatch query, return rows that fail with rows.Err().
	if strings.Contains(query, "SELECT e.id, e.source_id, e.event_type") {
		return &rowsErrFakeRows{}, nil
	}
	return nil, fmt.Errorf("rowsErrConn: unexpected query: %s", query)
}

type rowsErrDrv struct{}

func (*rowsErrDrv) Open(_ string) (driver.Conn, error) { return nil, fmt.Errorf("use sql.OpenDB") }

// ===========================================================================
// RegisterHostFunctions
// ===========================================================================

func TestRegisterHostFunctions_NilRegistry(t *testing.T) {
	p, _, _ := setupTestPlugin(t)
	err := p.RegisterHostFunctions(nil)
	if err == nil || !strings.Contains(err.Error(), "nil function registry") {
		t.Fatalf("expected nil registry error, got: %v", err)
	}
}

func TestRegisterHostFunctions_Valid(t *testing.T) {
	p, _, _ := setupTestPlugin(t)
	reg := newFakeFuncRegistry()
	if err := p.RegisterHostFunctions(reg); err != nil {
		t.Fatalf("RegisterHostFunctions: %v", err)
	}
	if !reg.Has("await_webhook") {
		t.Error("expected await_webhook to be registered")
	}
}

// ===========================================================================
// Migrations
// ===========================================================================

func TestMigrations(t *testing.T) {
	p, _, _ := setupTestPlugin(t)
	migrations := p.Migrations()
	if len(migrations) == 0 {
		t.Error("expected at least one migration")
	}
	for i, m := range migrations {
		if m.Version == 0 {
			t.Errorf("migration %d: version must be non-zero", i)
		}
		if m.Up == "" {
			t.Errorf("migration %d: Up SQL is empty", i)
		}
	}
}

// ===========================================================================
// RegisterRoutes
// ===========================================================================

func TestRegisterRoutes_NilMux(t *testing.T) {
	p, _, _ := setupTestPlugin(t)
	err := p.RegisterRoutes(nil)
	if err == nil || !strings.Contains(err.Error(), "nil mux") {
		t.Fatalf("expected nil mux error, got: %v", err)
	}
}

func TestRegisterRoutes_Valid(t *testing.T) {
	p, _, _ := setupTestPlugin(t)
	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}
	// Verify each expected route is registered (does not return 404).
	routes := []struct{ method, path string }{
		{"POST", "/ingest/11111111-1111-1111-1111-111111111111"},
		{"GET", "/ingest/sources"},
		{"POST", "/ingest/sources"},
		{"GET", "/ingest/sources/11111111-1111-1111-1111-111111111111"},
		{"DELETE", "/ingest/sources/11111111-1111-1111-1111-111111111111"},
		{"GET", "/ingest/events"},
	}
	for _, r := range routes {
		req := httptest.NewRequest(r.method, r.path, nil)
		_, pattern := mux.Handler(req)
		if pattern == "" {
			t.Errorf("no handler matched %s %s", r.method, r.path)
		}
	}
}

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
		db:     &host.SQLDBAdapter{DB: db},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	handler := auth.Middleware(host.NewPostgresStore(db))(mux)

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
		db:     &host.SQLDBAdapter{DB: db},
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
		db:     &host.SQLDBAdapter{DB: db},
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
		db:     &host.SQLDBAdapter{DB: db},
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

// ===========================================================================
// HandleGetSource not found
// ===========================================================================

func TestGetSourceNotFound(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	req := authedRequest("GET", "/ingest/sources/"+uuid.New().String(), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for non-existent source, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// CreateSource missing required fields
// ===========================================================================

func TestCreateSourceMissingName(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	body := `{"source_type":"github"}`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing name, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["error"] == nil {
		t.Error("expected error message in response")
	}
}

func TestCreateSourceInvalidBody(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	body := `not valid json`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid body, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// CreateSource with signal binding
// ===========================================================================

func TestCreateSourceWithSignalBinding(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	body := `{"name":"signal-source","source_type":"generic","signal_workflow_id":"wf-001","signal_name":"custom_signal"}`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var created map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &created)
	if created["signal_workflow_id"] != "wf-001" {
		t.Errorf("expected signal_workflow_id 'wf-001', got %v", created["signal_workflow_id"])
	}
	if created["signal_name"] != "custom_signal" {
		t.Errorf("expected signal_name 'custom_signal', got %v", created["signal_name"])
	}
}

// ===========================================================================
// List events with and without filters
// ===========================================================================

func TestListEventsWithFilters(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	// Create two sources.
	createBody1 := `{"name":"source-1","source_type":"github"}`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(createBody1)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create source 1: expected 201, got %d", rec.Code)
	}
	var src1 map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &src1)
	sourceID1 := src1["id"].(string)

	createBody2 := `{"name":"source-2","source_type":"stripe"}`
	req = authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(createBody2)))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create source 2: expected 201, got %d", rec.Code)
	}
	var src2 map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &src2)
	sourceID2 := src2["id"].(string)

	// Ingest an event for source 1.
	payload1 := `{"event":"push"}`
	req = httptest.NewRequest("POST", "/ingest/"+sourceID1, bytes.NewReader([]byte(payload1)))
	req.Header.Set("X-Github-Event", "push")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("ingest 1: expected 201, got %d", rec.Code)
	}

	// Ingest an event for source 2.
	payload2 := `{"event":"charge"}`
	req = httptest.NewRequest("POST", "/ingest/"+sourceID2, bytes.NewReader([]byte(payload2)))
	req.Header.Set("X-Event-Type", "payment")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("ingest 2: expected 201, got %d", rec.Code)
	}

	// List all events.
	req = authedRequest("GET", "/ingest/events", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list events: expected 200, got %d", rec.Code)
	}
	var events []map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &events)
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}

	// List events filtered by source_id.
	req = authedRequest("GET", "/ingest/events?source_id="+sourceID1, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list events by source: expected 200, got %d", rec.Code)
	}
	json.Unmarshal(rec.Body.Bytes(), &events)
	if len(events) != 1 {
		t.Fatalf("expected 1 event for source 1 filter, got %d", len(events))
	}

	// List events filtered by event_type.
	req = authedRequest("GET", "/ingest/events?event_type=push", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list events by type: expected 200, got %d", rec.Code)
	}
	json.Unmarshal(rec.Body.Bytes(), &events)
	if len(events) != 1 {
		t.Fatalf("expected 1 event for event_type filter, got %d", len(events))
	}

	// List events filtered by processed=false.
	req = authedRequest("GET", "/ingest/events?processed=false", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list events by processed: expected 200, got %d", rec.Code)
	}
	json.Unmarshal(rec.Body.Bytes(), &events)
	if len(events) != 2 {
		t.Fatalf("expected 2 unprocessed events, got %d", len(events))
	}

	_ = store // store used implicitly through handler
}

// ===========================================================================
// Ingest with non-JSON body
// ===========================================================================

func TestIngestNonJSONBody(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	// Create source.
	createBody := `{"name":"nonjson-test","source_type":"generic"}`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(createBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create source: expected 201, got %d", rec.Code)
	}

	var created map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &created)
	sourceID := created["id"].(string)

	// Ingest a non-JSON plain text payload.
	rawText := "plain text webhook body"
	req = httptest.NewRequest("POST", "/ingest/"+sourceID, bytes.NewReader([]byte(rawText)))
	req.Header.Set("Content-Type", "text/plain")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("ingest: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify the payload was stored as a JSON string.
	store.mu.RLock()
	if len(store.events) != 1 {
		store.mu.RUnlock()
		t.Fatalf("expected 1 event, got %d", len(store.events))
	}
	payload := store.events[0].payload
	store.mu.RUnlock()

	var decodedPayload string
	if err := json.Unmarshal([]byte(payload), &decodedPayload); err != nil {
		t.Fatalf("payload should be a JSON string, got: %s (parse error: %v)", payload, err)
	}
	if decodedPayload != rawText {
		t.Errorf("expected payload %q, got %q", rawText, decodedPayload)
	}
}

// ===========================================================================
// Ingest with signal delivery
// ===========================================================================

func TestIngestWithSignalDelivery(t *testing.T) {
	store := newFakeDBStore()
	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantStr

	sourceID := uuid.New()
	store.sources = append(store.sources, webhookSourceRow{
		id:               sourceID.String(),
		tenantID:         testTenantStr,
		name:             "signal-source",
		sourceType:       "generic",
		secret:           "",
		enabled:          true,
		signalWorkflowID: "wf-signal-test",
		signalName:       "my_signal",
	})

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	signalChan := make(chan string, 1)
	p := &Plugin{
		db:     &host.SQLDBAdapter{DB: db},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		env: &plugin.Environment{
			SignalWorkflow: func(ctx context.Context, workflowID, signalName, payload string) error {
				signalChan <- signalName
				return nil
			},
		},
	}

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}
	handler := auth.Middleware(host.NewPostgresStore(db))(mux)

	// Ingest a payload.
	payload := `{"action":"triggered"}`
	req := httptest.NewRequest("POST", "/ingest/"+sourceID.String(), bytes.NewReader([]byte(payload)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("ingest: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify signal was delivered.
	select {
	case signalName := <-signalChan:
		if signalName != "my_signal" {
			t.Errorf("expected signal name 'my_signal', got %s", signalName)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for signal delivery")
	}

	// Verify event was marked processed.
	store.mu.RLock()
	evt := store.events[0]
	store.mu.RUnlock()
	if !evt.processed {
		t.Error("expected event to be marked processed after signal delivery")
	}
	if evt.status != "completed" {
		t.Errorf("expected status 'completed', got %s", evt.status)
	}
}

// ===========================================================================
// Ingest with invalid source ID (not a UUID)
// ===========================================================================

func TestIngestInvalidSourceID(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	req := httptest.NewRequest("POST", "/ingest/not-a-uuid", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid source ID, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// Ingest with source not found (valid UUID but doesn't exist)
// ===========================================================================

func TestIngestSourceNotFound(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	req := httptest.NewRequest("POST", "/ingest/"+uuid.New().String(), bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for non-existent source, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// List sources handler
// ===========================================================================

func TestListSourcesHandler(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	// Empty list initially.
	req := authedRequest("GET", "/ingest/sources", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for list sources, got %d: %s", rec.Code, rec.Body.String())
	}
	var sources []webhookSourceJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &sources); err != nil {
		t.Fatalf("unmarshal sources: %v", err)
	}
	if len(sources) != 0 {
		t.Errorf("expected empty list, got %d sources", len(sources))
	}

	// Create two sources.
	for i := 0; i < 2; i++ {
		name := fmt.Sprintf("source-%d", i)
		body := fmt.Sprintf(`{"name":"%s"}`, name)
		req := authedRequest("POST", "/ingest/sources", strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create source %d: expected 201, got %d", i, rec.Code)
		}
	}

	// List should now return 2 sources.
	req = authedRequest("GET", "/ingest/sources", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for list sources, got %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sources); err != nil {
		t.Fatalf("unmarshal sources: %v", err)
	}
	if len(sources) != 2 {
		t.Errorf("expected 2 sources, got %d", len(sources))
	}

	// Verify they are stored in the fake DB store.
	store.mu.RLock()
	n := len(store.sources)
	store.mu.RUnlock()
	if n != 2 {
		t.Errorf("expected 2 sources in store, got %d", n)
	}
}

func TestListSourcesNoAuth(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	req := httptest.NewRequest("GET", "/ingest/sources", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without auth, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// markRetryFailed edge cases
// ===========================================================================

func TestMarkRetryFailed(t *testing.T) {
	store := newFakeDBStore()

	eventID := uuid.New()
	store.events = append(store.events, webhookEventRow{
		id:        eventID.String(),
		tenantID:  testTenantStr,
		eventType: "webhook",
		payload:   `{"test":true}`,
		processed: false,
		status:    "pending",
	})

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:     &host.SQLDBAdapter{DB: db},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// First failure: should stay pending (retry 0 -> 1, below max of 3).
	p.markRetryFailed(context.Background(), eventID, 0, "signal delivery failed: timeout")

	store.mu.RLock()
	evt := store.events[0]
	store.mu.RUnlock()
	if evt.retryCount != 1 {
		t.Errorf("expected retryCount 1, got %d", evt.retryCount)
	}
	if evt.status != "pending" {
		t.Errorf("expected status 'pending', got %s", evt.status)
	}
	if evt.errorMsg == nil || *evt.errorMsg != "signal delivery failed: timeout" {
		t.Errorf("expected error message to be set, got %v", evt.errorMsg)
	}
	if evt.lastRetryAt == nil {
		t.Error("expected lastRetryAt to be set")
	}

	// Third failure (retry count 2 -> 3, >= max of 3): should go to dead_letter.
	p.markRetryFailed(context.Background(), eventID, 2, "signal delivery failed: timeout x3")

	store.mu.RLock()
	evt = store.events[0]
	store.mu.RUnlock()
	if evt.retryCount != 3 {
		t.Errorf("expected retryCount 3, got %d", evt.retryCount)
	}
	if evt.status != "dead_letter" {
		t.Errorf("expected status 'dead_letter', got %s", evt.status)
	}
	if !evt.processed {
		t.Error("expected processed=true after dead_letter")
	}
}

// ===========================================================================
// retryEvent edge cases
// ===========================================================================

func TestRetryEventNoSignalWorkflow(t *testing.T) {
	store := newFakeDBStore()

	eventID := uuid.New()
	store.events = append(store.events, webhookEventRow{
		id:        eventID.String(),
		tenantID:  testTenantStr,
		eventType: "webhook",
		payload:   `{"test":true}`,
		processed: false,
		status:    "pending",
	})

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:     &host.SQLDBAdapter{DB: db},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// Event with no signal_workflow_id: should complete.
	p.retryEvent(context.Background(), eventID, uuid.New(), "webhook", []byte(`{"test":true}`),
		time.Now(), "", "webhook_received", 0)

	store.mu.RLock()
	evt := store.events[0]
	store.mu.RUnlock()
	if !evt.processed {
		t.Error("expected event to be marked processed")
	}
	if evt.status != "completed" {
		t.Errorf("expected status 'completed', got %s", evt.status)
	}
}

func TestRetryEventSignalDeliveryFailure(t *testing.T) {
	store := newFakeDBStore()

	eventID := uuid.New()
	store.events = append(store.events, webhookEventRow{
		id:        eventID.String(),
		tenantID:  testTenantStr,
		eventType: "webhook",
		payload:   `{"test":true}`,
		receivedAt: time.Now().Add(-30 * time.Second),
		processed: false,
		status:    "pending",
	})

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	signalCalled := false
	p := &Plugin{
		db:     &host.SQLDBAdapter{DB: db},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		env: &plugin.Environment{
			SignalWorkflow: func(ctx context.Context, workflowID, signalName, payload string) error {
				signalCalled = true
				return fmt.Errorf("signal delivery failed")
			},
		},
	}

	p.retryEvent(context.Background(), eventID, uuid.New(), "webhook", []byte(`{"test":true}`),
		time.Now().Add(-30*time.Second), "wf-signal", "webhook_received", 0)

	if !signalCalled {
		t.Error("expected SignalWorkflow to be called")
	}

	store.mu.RLock()
	evt := store.events[0]
	store.mu.RUnlock()
	if evt.processed {
		t.Error("expected event to NOT be processed after failed signal")
	}
	if evt.status != "pending" {
		t.Errorf("expected status 'pending' after first retry, got %s", evt.status)
	}
}

// ===========================================================================
// Run with nil DB
// ===========================================================================

func TestRunWebhookBackgroundNilDB(t *testing.T) {
	p := &Plugin{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := p.Run(ctx)
	if err != nil {
		t.Errorf("Run() with nil DB returned error: %v", err)
	}
}

// ===========================================================================
// Get source with no auth
// ===========================================================================

func TestGetSourceNoAuth(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	req := httptest.NewRequest("GET", "/ingest/sources/"+uuid.New().String(), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without auth, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// Init with nil logger
// ===========================================================================

func TestWH_Init_NilLogger(t *testing.T) {
	p := &Plugin{}
	ctx := context.Background()
	env := &plugin.Environment{
		DB:  &host.SQLDBAdapter{DB: sql.OpenDB(&fakeConnector{store: newFakeDBStore()})},
		Mux: http.NewServeMux(),
	}
	err := p.Init(ctx, env)
	if err != nil {
		t.Fatalf("Init with nil logger: %v", err)
	}
	if p.logger == nil {
		t.Error("expected logger to be set even when nil is provided")
	}
}

// ===========================================================================
// CreateSource no auth
// ===========================================================================

func TestWH_CreateSource_NoTenant(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	req := httptest.NewRequest("POST", "/ingest/sources", bytes.NewReader([]byte(`{"name":"test"}`)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// CreateSource DB exec error
// ===========================================================================

func TestWH_CreateSource_ExecError(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	store.mu.Lock()
	store.failNextExec = true
	store.mu.Unlock()

	body := `{"name":"test-source","source_type":"github"}`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 from exec error, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// GetSource invalid ID
// ===========================================================================

func TestWH_GetSource_InvalidID(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	req := authedRequest("GET", "/ingest/sources/not-a-uuid", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid UUID, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// GetSource DB query error
// ===========================================================================

func TestWH_GetSource_QueryError(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	// Create a source first so we have a valid ID.
	createBody := `{"name":"test-source","source_type":"github"}`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(createBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create source: expected 201, got %d", rec.Code)
	}
	var created map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &created)
	sourceID := created["id"].(string)

	// Set fail flag with skip=1 for auth middleware query.
	store.mu.Lock()
	store.failNextQuery = true
	store.querySkip = 1
	store.mu.Unlock()

	req = authedRequest("GET", "/ingest/sources/"+sourceID, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 from query error, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// DeleteSource no auth
// ===========================================================================

func TestWH_DeleteSource_NoTenant(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	req := httptest.NewRequest("DELETE", "/ingest/sources/"+uuid.New().String(), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// DeleteSource invalid ID
// ===========================================================================

func TestWH_DeleteSource_InvalidID(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	req := authedRequest("DELETE", "/ingest/sources/not-a-uuid", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid UUID, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// DeleteSource not found
// ===========================================================================

func TestWH_DeleteSource_NotFound(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	// Delete a non-existent source (valid UUID but doesn't exist).
	req := authedRequest("DELETE", "/ingest/sources/"+uuid.New().String(), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// DeleteSource DB exec error
// ===========================================================================

func TestWH_DeleteSource_ExecError(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	// Create a source first.
	createBody := `{"name":"test-source"}`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(createBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create source: expected 201, got %d", rec.Code)
	}
	var created map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &created)
	sourceID := created["id"].(string)

	// Set exec fail flag.
	store.mu.Lock()
	store.failNextExec = true
	store.mu.Unlock()

	req = authedRequest("DELETE", "/ingest/sources/"+sourceID, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 from exec error, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// ListSources DB query error
// ===========================================================================

func TestWH_ListSources_QueryError(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	store.mu.Lock()
	store.failNextQuery = true
	store.querySkip = 1
	store.mu.Unlock()

	req := authedRequest("GET", "/ingest/sources", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 from query error, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// ListSources tenant isolation
// ===========================================================================

func TestWH_ListSources_TenantIsolation(t *testing.T) {
	store := newFakeDBStore()
	keyHash1 := sha256.Sum256([]byte("key-tenant-1"))
	keyHash2 := sha256.Sum256([]byte("key-tenant-2"))
	tenant1Str := "00000000-0000-0000-0000-000000000001"
	tenant2Str := "00000000-0000-0000-0000-000000000002"
	store.apiKeys[fmt.Sprintf("%x", keyHash1)] = tenant1Str
	store.apiKeys[fmt.Sprintf("%x", keyHash2)] = tenant2Str

	// Add a source for each tenant.
	store.sources = append(store.sources, webhookSourceRow{
		id:       uuid.New().String(),
		tenantID: tenant1Str,
		name:     "tenant-1-source",
	})
	store.sources = append(store.sources, webhookSourceRow{
		id:       uuid.New().String(),
		tenantID: tenant2Str,
		name:     "tenant-2-source",
	})

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:     &host.SQLDBAdapter{DB: db},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}
	handler := auth.Middleware(host.NewPostgresStore(db))(mux)

	// Tenant 1 lists sources.
	req := httptest.NewRequest("GET", "/ingest/sources", nil)
	req.Header.Set("Authorization", "Bearer key-tenant-1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("tenant 1 list: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var sources []webhookSourceJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &sources); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(sources) != 1 {
		t.Fatalf("expected 1 source for tenant 1, got %d", len(sources))
	}
	if sources[0].Name != "tenant-1-source" {
		t.Errorf("expected 'tenant-1-source', got %s", sources[0].Name)
	}

	// Tenant 2 lists sources.
	req = httptest.NewRequest("GET", "/ingest/sources", nil)
	req.Header.Set("Authorization", "Bearer key-tenant-2")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("tenant 2 list: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sources); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(sources) != 1 {
		t.Fatalf("expected 1 source for tenant 2, got %d", len(sources))
	}
	if sources[0].Name != "tenant-2-source" {
		t.Errorf("expected 'tenant-2-source', got %s", sources[0].Name)
	}
}

// ===========================================================================
// ListEvents no auth
// ===========================================================================

func TestWH_ListEvents_NoTenant(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	req := httptest.NewRequest("GET", "/ingest/events", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// ListEvents DB query error
// ===========================================================================

func TestWH_ListEvents_QueryError(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	store.mu.Lock()
	store.failNextQuery = true
	store.querySkip = 1
	store.mu.Unlock()

	req := authedRequest("GET", "/ingest/events", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 from query error, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// Ingest source lookup DB error
// ===========================================================================

func TestWH_Ingest_SourceLookupError(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	// Create a source so we have a valid source ID to use.
	createBody := `{"name":"test-source"}`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(createBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create source: expected 201, got %d", rec.Code)
	}
	var created map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &created)
	sourceID := created["id"].(string)

	// Set flag to fail the next query (ingest endpoint has no auth middleware query).
	store.mu.Lock()
	store.failNextQuery = true
	store.mu.Unlock()

	// Ingest request should fail during source lookup.
	payload := `{"action":"opened"}`
	req = httptest.NewRequest("POST", "/ingest/"+sourceID, bytes.NewReader([]byte(payload)))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 from source lookup error, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// Ingest event insert DB error
// ===========================================================================

func TestWH_Ingest_EventInsertError(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	// Create a source so we have a valid source ID to use.
	createBody := `{"name":"test-source"}`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(createBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create source: expected 201, got %d", rec.Code)
	}
	var created map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &created)
	sourceID := created["id"].(string)

	// Set flag to fail the next exec (event insert after source lookup).
	store.mu.Lock()
	store.failNextExec = true
	store.mu.Unlock()

	// Ingest request should fail during event insert.
	payload := `{"action":"opened"}`
	req = httptest.NewRequest("POST", "/ingest/"+sourceID, bytes.NewReader([]byte(payload)))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 from event insert error, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// Run with valid DB (immediate cancel)
// ===========================================================================

func TestWH_Run_WithDB(t *testing.T) {
	store := newFakeDBStore()
	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:     &host.SQLDBAdapter{DB: db},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	err := p.Run(ctx)
	if err != nil {
		t.Errorf("Run() with valid DB returned error: %v", err)
	}
}

// ===========================================================================
// processBatch query error
// ===========================================================================

func TestWH_ProcessBatch_QueryError(t *testing.T) {
	store := newFakeDBStore()
	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:     &host.SQLDBAdapter{DB: db},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// Set fail flag so the query in processBatch fails.
	store.mu.Lock()
	store.failNextQuery = true
	store.mu.Unlock()

	// Should not panic, just log the error.
	p.processBatch(context.Background())
}

// ===========================================================================
// processBatch scan error (row with mismatched types)
// ===========================================================================

func TestWH_ProcessBatch_ScanError(t *testing.T) {
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

	// Add an unprocessed event that will be picked up by processBatch.
	eventID := uuid.New()
	store.events = append(store.events, webhookEventRow{
		id:         eventID.String(),
		sourceID:   sourceID.String(),
		tenantID:   testTenantStr,
		eventType:  "webhook",
		payload:    `{"test":true}`,
		receivedAt: time.Now().Add(-30 * time.Second),
		processed:  false,
		status:     "pending",
	})

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:     &host.SQLDBAdapter{DB: db},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// Should not panic, should process the event via processBatch.
	p.processBatch(context.Background())

	// Verify the event was processed.
	store.mu.RLock()
	evt := store.events[0]
	store.mu.RUnlock()
	if !evt.processed {
		t.Error("expected event to be marked processed after processBatch scan")
	}
}

// ===========================================================================
// AwaitWebhook no tenant context
// ===========================================================================

func TestWH_AwaitWebhook_NoTenant(t *testing.T) {
	store := newFakeDBStore()
	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantStr

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:     &host.SQLDBAdapter{DB: db},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// Call awaitWebhook with a context that has no CallContext.
	_, err := p.awaitWebhook(context.Background(), `{"source_id":"`+uuid.New().String()+`"}`)
	if err == nil {
		t.Fatal("expected error for missing tenant context, got nil")
	}
	if !strings.Contains(err.Error(), "no tenant context") {
		t.Errorf("expected 'no tenant context' error, got: %v", err)
	}
}

// ===========================================================================
// AwaitWebhook invalid source ID
// ===========================================================================

func TestWH_AwaitWebhook_InvalidSourceID(t *testing.T) {
	store := newFakeDBStore()
	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantStr

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:     &host.SQLDBAdapter{DB: db},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	callCtx := &plugin.CallContext{TenantID: testTenantID, WorkflowID: "test-wf"}
	ctx := plugin.WithCallContext(context.Background(), callCtx)

	// Call with invalid source_id.
	_, err := p.awaitWebhook(ctx, `{"source_id":"not-a-uuid"}`)
	if err == nil {
		t.Fatal("expected error for invalid source_id, got nil")
	}
	if !strings.Contains(err.Error(), "invalid source_id") {
		t.Errorf("expected 'invalid source_id' error, got: %v", err)
	}
}

// ===========================================================================
// AwaitWebhook invalid input JSON
// ===========================================================================

func TestWH_AwaitWebhook_InvalidJSON(t *testing.T) {
	store := newFakeDBStore()
	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantStr

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:     &host.SQLDBAdapter{DB: db},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	callCtx := &plugin.CallContext{TenantID: testTenantID, WorkflowID: "test-wf"}
	ctx := plugin.WithCallContext(context.Background(), callCtx)

	// Call with invalid JSON input.
	_, err := p.awaitWebhook(ctx, `not valid json`)
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
	if !strings.Contains(err.Error(), "invalid input") {
		t.Errorf("expected 'invalid input' error, got: %v", err)
	}
}

// ===========================================================================
// RegisterHostFunctions register error
// ===========================================================================

type errFuncRegistry struct{}

func (r *errFuncRegistry) Register(_ plugin.FuncOptions, _ plugin.PluginFunc) error {
	return fmt.Errorf("simulated register error")
}

func TestWH_RegisterHostFunctions_RegisterError(t *testing.T) {
	p, _, _ := setupTestPlugin(t)
	err := p.RegisterHostFunctions(&errFuncRegistry{})
	if err == nil {
		t.Fatal("expected error from Register, got nil")
	}
	if !strings.Contains(err.Error(), "simulated register error") {
		t.Errorf("expected 'simulated register error', got: %v", err)
	}
}

// ===========================================================================
// Ingest body read error
// ===========================================================================

func TestWH_Ingest_BodyReadError(t *testing.T) {
	store := newFakeDBStore()
	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantStr

	sourceID := uuid.New()
	store.sources = append(store.sources, webhookSourceRow{
		id:         sourceID.String(),
		tenantID:   testTenantStr,
		name:       "test",
		sourceType: "generic",
		enabled:    true,
	})

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:     &host.SQLDBAdapter{DB: db},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}
	handler := auth.Middleware(host.NewPostgresStore(db))(mux)

	// Send request with a body that fails on Read.
	req := httptest.NewRequest("POST", "/ingest/"+sourceID.String(), &errReadCloser{})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for body read error, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// CreateSource body read error
// ===========================================================================

func TestWH_CreateSource_BodyReadError(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	// Send request with a body that fails on Read.
	req := authedRequest("POST", "/ingest/sources", &errReadCloser{})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for body read error, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// Ingest with SignalWorkflow error
// ===========================================================================

func TestWH_Ingest_SignalWorkflowError(t *testing.T) {
	store := newFakeDBStore()
	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantStr

	sourceID := uuid.New()
	store.sources = append(store.sources, webhookSourceRow{
		id:               sourceID.String(),
		tenantID:         testTenantStr,
		name:             "signal-source",
		sourceType:       "generic",
		enabled:          true,
		signalWorkflowID: "wf-signal-error",
		signalName:       "my_signal",
	})

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:     &host.SQLDBAdapter{DB: db},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		env: &plugin.Environment{
			SignalWorkflow: func(ctx context.Context, workflowID, signalName, payload string) error {
				return fmt.Errorf("simulated signal failure")
			},
		},
	}

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}
	handler := auth.Middleware(host.NewPostgresStore(db))(mux)

	// Ingest a payload.
	payload := `{"action":"test"}`
	req := httptest.NewRequest("POST", "/ingest/"+sourceID.String(), bytes.NewReader([]byte(payload)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	// Should still return 201 even though signal delivery fails.
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 even with signal error, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// Ingest with empty signal name (should default to webhook_received)
// ===========================================================================

func TestWH_Ingest_EmptySignalName(t *testing.T) {
	store := newFakeDBStore()
	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantStr

	sourceID := uuid.New()
	store.sources = append(store.sources, webhookSourceRow{
		id:               sourceID.String(),
		tenantID:         testTenantStr,
		name:             "empty-signal-source",
		sourceType:       "generic",
		enabled:          true,
		signalWorkflowID: "wf-empty-signal",
		signalName:       "",
	})

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	signalNameCh := make(chan string, 1)
	p := &Plugin{
		db:     &host.SQLDBAdapter{DB: db},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		env: &plugin.Environment{
			SignalWorkflow: func(ctx context.Context, workflowID, signalName, payload string) error {
				signalNameCh <- signalName
				return nil
			},
		},
	}

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}
	handler := auth.Middleware(host.NewPostgresStore(db))(mux)

	// Ingest a payload.
	payload := `{"action":"test"}`
	req := httptest.NewRequest("POST", "/ingest/"+sourceID.String(), bytes.NewReader([]byte(payload)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify signal name defaulted to "webhook_received".
	select {
	case signalName := <-signalNameCh:
		if signalName != "webhook_received" {
			t.Errorf("expected signal name 'webhook_received', got %s", signalName)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for signal delivery")
	}
}

// ===========================================================================
// Ingest with non-JSON body and signal delivery
// ===========================================================================

func TestWH_Ingest_NonJSONBodyWithSignal(t *testing.T) {
	store := newFakeDBStore()
	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantStr

	sourceID := uuid.New()
	store.sources = append(store.sources, webhookSourceRow{
		id:               sourceID.String(),
		tenantID:         testTenantStr,
		name:             "nonjson-signal-source",
		sourceType:       "generic",
		enabled:          true,
		signalWorkflowID: "wf-nonjson",
		signalName:       "my_signal",
	})

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	signalPayloadCh := make(chan string, 1)
	p := &Plugin{
		db:     &host.SQLDBAdapter{DB: db},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		env: &plugin.Environment{
			SignalWorkflow: func(ctx context.Context, workflowID, signalName, payload string) error {
				signalPayloadCh <- signalName + "|" + payload
				return nil
			},
		},
	}

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}
	handler := auth.Middleware(host.NewPostgresStore(db))(mux)

	// Ingest a non-JSON plain text body.
	rawText := "plain text body"
	req := httptest.NewRequest("POST", "/ingest/"+sourceID.String(), bytes.NewReader([]byte(rawText)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify signal was delivered.
	select {
	case payload := <-signalPayloadCh:
		if !strings.Contains(payload, "plain text body") {
			t.Errorf("expected signal to contain body text, got: %s", payload)
		}
		// The signal name should be "my_signal"
		if !strings.HasPrefix(payload, "my_signal|") {
			t.Errorf("expected signal name 'my_signal', got prefix: %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for signal delivery")
	}
}

// ===========================================================================
// AwaitWebhook query DB error
// ===========================================================================

func TestWH_AwaitWebhook_QueryError(t *testing.T) {
	store := newFakeDBStore()
	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantStr

	// Add an event so the query doesn't return ErrNoRows.
	eventID := uuid.New()
	store.events = append(store.events, webhookEventRow{
		id:        eventID.String(),
		tenantID:  testTenantStr,
		eventType: "push",
		payload:   `{"ref":"main"}`,
		processed: false,
	})

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	// Set fail flag for the next query (the events query in awaitWebhook).
	store.mu.Lock()
	store.failNextQuery = true
	store.mu.Unlock()

	p := &Plugin{
		db:     &host.SQLDBAdapter{DB: db},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	callCtx := &plugin.CallContext{TenantID: testTenantID, WorkflowID: "test-wf"}
	ctx := plugin.WithCallContext(context.Background(), callCtx)

	input, _ := json.Marshal(map[string]interface{}{
		"source_id": uuid.New().String(),
	})
	_, err := p.awaitWebhook(ctx, string(input))
	if err == nil {
		t.Fatal("expected error from query failure, got nil")
	}
	if !strings.Contains(err.Error(), "query events") {
		t.Errorf("expected error mentioning 'query events', got: %v", err)
	}
}

// ===========================================================================
// AwaitWebhook exec error (mark processed)
// ===========================================================================

func TestWH_AwaitWebhook_ExecError(t *testing.T) {
	store := newFakeDBStore()
	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantStr

	// Add an event that awaitWebhook will find.
	eventID := uuid.New()
	store.events = append(store.events, webhookEventRow{
		id:        eventID.String(),
		tenantID:  testTenantStr,
		eventType: "push",
		payload:   `{"ref":"main"}`,
		processed: false,
	})

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	// Set fail flag for the exec (mark processed) after the query succeeds.
	store.mu.Lock()
	store.failNextExec = true
	store.mu.Unlock()

	p := &Plugin{
		db:     &host.SQLDBAdapter{DB: db},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	callCtx := &plugin.CallContext{TenantID: testTenantID, WorkflowID: "test-wf"}
	ctx := plugin.WithCallContext(context.Background(), callCtx)

	input, _ := json.Marshal(map[string]interface{}{
		"event_type": "push",
	})
	output, err := p.awaitWebhook(ctx, string(input))
	if err != nil {
		t.Fatalf("expected no error even when exec fails, got: %v", err)
	}
	// The event should still be returned even if marking as processed fails.
	var result map[string]interface{}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("failed to decode output: %v", err)
	}
	if result["found"] != true {
		t.Errorf("expected found=true, got %v", result["found"])
	}
}

// ===========================================================================
// ListEvents empty list (nil to empty slice)
// ===========================================================================

func TestWH_ListEvents_EmptyList(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	// List events with no events in the store — should return [] not null.
	req := authedRequest("GET", "/ingest/events", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var events []json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &events); err != nil {
		t.Fatalf("unmarshal events: %v", err)
	}
	if events == nil {
		t.Error("expected non-nil empty slice, got null")
	}
}

// ===========================================================================
// ListSources scan error (corrupt data)
// ===========================================================================

func TestWH_ListSources_ScanError(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	// Create a source so there's data to scan.
	createBody := `{"name":"test-source"}`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(createBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create source: expected 201, got %d", rec.Code)
	}

	// Set corrupt flag to cause scan error on the next list query.
	store.mu.Lock()
	store.corruptNextScan = true
	store.corruptScanSkip = 1 // skip the auth middleware query (tenant lookup)
	store.mu.Unlock()

	req = authedRequest("GET", "/ingest/sources", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	// Should still return 200 — corrupt row is skipped and logged.
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 despite scan error, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// ListEvents scan error (corrupt data)
// ===========================================================================

func TestWH_ListEvents_ScanError(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	// Create a source to accept webhook events.
	createBody := `{"name":"test-source","source_type":"github"}`
	req := authedRequest("POST", "/ingest/sources", bytes.NewReader([]byte(createBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create source: expected 201, got %d", rec.Code)
	}
	var created map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &created)
	sourceID := created["id"].(string)

	// Ingest a webhook payload to create an event.
	payload := `{"event":"test"}`
	req = httptest.NewRequest("POST", "/ingest/"+sourceID, bytes.NewReader([]byte(payload)))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("ingest: expected 201, got %d", rec.Code)
	}

	// Set corrupt flag for the events list query.
	store.mu.Lock()
	store.corruptNextScan = true
	store.corruptScanSkip = 1 // skip auth middleware query
	store.mu.Unlock()

	req = authedRequest("GET", "/ingest/events", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	// Should still return 200 — corrupt row is skipped and logged.
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 despite scan error, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// ProcessBatch scan error
// ===========================================================================

func TestWH_ProcessBatch_RowsErr(t *testing.T) {
	store := newFakeDBStore()

	db := sql.OpenDB(&rowsErrConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:     &host.SQLDBAdapter{DB: db},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// Should not panic — rows.Err() is logged but not returned.
	p.processBatch(context.Background())
}

// ===========================================================================
// ProcessBatch corrupt scan error
// ===========================================================================

func TestWH_ProcessBatch_CorruptScan(t *testing.T) {
	store := newFakeDBStore()

	sourceID := uuid.New()
	store.sources = append(store.sources, webhookSourceRow{
		id:               sourceID.String(),
		tenantID:         testTenantStr,
		name:             "test",
		sourceType:       "generic",
		enabled:          true,
		signalWorkflowID: "wf-scan-err",
	})

	// Add an unprocessed event older than 10 seconds.
	store.events = append(store.events, webhookEventRow{
		id:         uuid.New().String(),
		sourceID:   sourceID.String(),
		tenantID:   testTenantStr,
		eventType:  "webhook",
		payload:    `{"test":true}`,
		receivedAt: time.Now().Add(-30 * time.Second),
		processed:  false,
		status:     "pending",
	})

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	// Set corrupt flag so the processBatch query returns corrupt data.
	store.mu.Lock()
	store.corruptNextScan = true
	store.mu.Unlock()

	p := &Plugin{
		db:     &host.SQLDBAdapter{DB: db},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// Should not panic — corrupt row is skipped and logged.
	p.processBatch(context.Background())
}
