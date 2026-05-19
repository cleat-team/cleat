// Package notifications tests the webhook delivery plugin with an in-memory fake
// database, avoiding any need for PostgreSQL.
package notifications

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
	"github.com/cleat-team/cleat/internal/auth"
	"github.com/cleat-team/cleat/internal/plugin"
	"github.com/cleat-team/cleat/internal/host"
)

// ---------------------------------------------------------------------------
// In-memory notification store (replaces PostgreSQL entirely for testing)
// ---------------------------------------------------------------------------

type testWebhookCfg struct {
	tenantID  uuid.UUID
	id        uuid.UUID
	url       string
	secret    string
	events    string // JSON array
	enabled   bool
	createdAt time.Time
	updatedAt time.Time
}

type testDelivery struct {
	id            uuid.UUID
	webhookID     uuid.UUID
	eventType     string
	payload       []byte
	status        string
	attemptCount  int
	lastAttemptAt *time.Time
	nextAttemptAt *time.Time
	deliveredAt   *time.Time
	responseCode  *int
	responseBody  *string
	createdAt     time.Time
}

type fakeNotifyStore struct {
	mu         sync.RWMutex
	configs    []*testWebhookCfg
	deliveries []*testDelivery
	apiKeys    map[string]string // key_hash_hex -> tenant_id string
}

func newFakeNotifyStore() *fakeNotifyStore {
	return &fakeNotifyStore{
		apiKeys: make(map[string]string),
	}
}

func findWebhookCfg(configs []*testWebhookCfg, id uuid.UUID) *testWebhookCfg {
	for _, c := range configs {
		if c.id == id {
			return c
		}
	}
	return nil
}

func findTestDelivery(deliveries []*testDelivery, id uuid.UUID) *testDelivery {
	for _, d := range deliveries {
		if d.id == id {
			return d
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Fake SQL driver
// ---------------------------------------------------------------------------

type fakeConnector struct {
	store *fakeNotifyStore
}

func (c *fakeConnector) Connect(_ context.Context) (driver.Conn, error) {
	return &fakeConn{store: c.store}, nil
}

func (c *fakeConnector) Driver() driver.Driver {
	return &fakeDrv{}
}

type fakeDrv struct{}

func (*fakeDrv) Open(_ string) (driver.Conn, error) {
	return nil, fmt.Errorf("fakeDriver: use sql.OpenDB")
}

type fakeConn struct {
	store *fakeNotifyStore
}

func (*fakeConn) Prepare(_ string) (driver.Stmt, error) {
	return nil, fmt.Errorf("fakeConn: unexpected Prepare call")
}

func (*fakeConn) Close() error { return nil }
func (*fakeConn) Begin() (driver.Tx, error) { return &fakeTx{}, nil }

type fakeTx struct{}

func (*fakeTx) Commit() error   { return nil }
func (*fakeTx) Rollback() error { return nil }

// --- ExecContext ---

func (c *fakeConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.store.mu.Lock()
	defer c.store.mu.Unlock()

	switch {
	case strings.Contains(query, "INSERT INTO webhook_config"):
		return c.execInsertWebhookConfig(args)
	case strings.Contains(query, "INSERT INTO webhook_delivery"):
		return c.execInsertWebhookDelivery(args)
	case strings.Contains(query, "UPDATE webhook_config"):
		return c.execUpdateWebhookConfig(args, query)
	case strings.Contains(query, "UPDATE webhook_delivery"):
		return c.execUpdateWebhookDelivery(args, query)
	case strings.Contains(query, "DELETE FROM webhook_config"):
		return c.execDeleteWebhookConfig(args)
	default:
		return nil, fmt.Errorf("fakeConn: unexpected Exec query: %s", query)
	}
}

// --- QueryContext ---

func (c *fakeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	switch {
	case strings.Contains(query, "tenant_api_keys"):
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryTenantLookup(args)
	case strings.Contains(query, "SELECT EXISTS(SELECT 1 FROM webhook_config"):
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryWebhookExists(args)
	case strings.Contains(query, "SELECT url, secret FROM webhook_config"):
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryWebhookConfigForDelivery(args)
	case strings.Contains(query, "d.status IN ('pending'"):
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryPendingDeliveries(args)
	case strings.Contains(query, "FROM webhook_delivery"):
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryDeliveries(args, query)
	case strings.Contains(query, "AND tenant_id = $2"):
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryGetWebhook(args)
	case strings.Contains(query, "FROM webhook_config"):
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryListWebhooks(args)
	default:
		return nil, fmt.Errorf("fakeConn: unexpected Query query: %s", query)
	}
}

// --- Exec implementations ---

func (c *fakeConn) execInsertWebhookConfig(args []driver.NamedValue) (driver.Result, error) {
	tidStr, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	tid, err := uuid.Parse(tidStr)
	if err != nil {
		return nil, err
	}
	idStr, err := argString(args, 2)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return nil, err
	}
	url, err := argString(args, 3)
	if err != nil {
		return nil, err
	}
	secret, err := argString(args, 4)
	if err != nil {
		return nil, err
	}
	eventsJSON, err := argString(args, 5)
	if err != nil {
		return nil, err
	}
	now, err := argTime(args, 6)
	if err != nil {
		return nil, err
	}

	c.store.configs = append(c.store.configs, &testWebhookCfg{
		tenantID:  tid,
		id:        id,
		url:       url,
		secret:    secret,
		events:    eventsJSON,
		enabled:   true,
		createdAt: now,
		updatedAt: now,
	})
	return &fakeResult{rowsAffected: 1}, nil
}

func (c *fakeConn) execInsertWebhookDelivery(args []driver.NamedValue) (driver.Result, error) {
	idStr, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return nil, err
	}
	webhookIDStr, err := argString(args, 2)
	if err != nil {
		return nil, err
	}
	webhookID, err := uuid.Parse(webhookIDStr)
	if err != nil {
		return nil, err
	}
	eventType, err := argString(args, 3)
	if err != nil {
		return nil, err
	}
	payload, err := argString(args, 4)
	if err != nil {
		return nil, err
	}

	var now time.Time
	for _, a := range args {
		if a.Ordinal == 5 {
			now, _ = a.Value.(time.Time)
		}
	}

	c.store.deliveries = append(c.store.deliveries, &testDelivery{
		id:            id,
		webhookID:     webhookID,
		eventType:     eventType,
		payload:       []byte(payload),
		status:        "pending",
		attemptCount:  0,
		nextAttemptAt: &now,
		createdAt:     now,
	})
	return &fakeResult{rowsAffected: 1}, nil
}

func (c *fakeConn) execUpdateWebhookConfig(args []driver.NamedValue, query string) (driver.Result, error) {
	// Find the highest ordinal (always the id ordinal); tenant is that + 1.
	lastOrdinal := 0
	for _, a := range args {
		if a.Ordinal > lastOrdinal {
			lastOrdinal = a.Ordinal
		}
	}
	tidStr, err := argString(args, lastOrdinal)
	if err != nil {
		return nil, err
	}
	tid := uuid.MustParse(tidStr)
	idStr, err := argString(args, lastOrdinal-1)
	if err != nil {
		return nil, err
	}
	id := uuid.MustParse(idStr)

	cfg := findWebhookCfg(c.store.configs, id)
	if cfg == nil || cfg.tenantID != tid {
		return &fakeResult{rowsAffected: 0}, nil
	}

	// Map ordinal positions to fields based on the SQL query.
	for ord := 1; ord <= lastOrdinal-2; ord++ {
		ordStr := fmt.Sprintf("%d", ord)
		switch {
		case strings.Contains(query, "url = $"+ordStr):
			if v, err := argString(args, ord); err == nil {
				cfg.url = v
			}
		case strings.Contains(query, "secret = $"+ordStr):
			if v, err := argString(args, ord); err == nil {
				cfg.secret = v
			}
		case strings.Contains(query, "events = $"+ordStr):
			if v, err := argString(args, ord); err == nil {
				cfg.events = v
			}
		case strings.Contains(query, "enabled = $"+ordStr):
			for _, a := range args {
				if a.Ordinal == ord {
					if v, ok := a.Value.(bool); ok {
						cfg.enabled = v
					}
				}
			}
		}
	}

	cfg.updatedAt = time.Now().UTC()
	return &fakeResult{rowsAffected: 1}, nil
}

func (c *fakeConn) execUpdateWebhookDelivery(args []driver.NamedValue, query string) (driver.Result, error) {
	// Find the delivery id (last ordinal).
	lastOrdinal := 0
	for _, a := range args {
		if a.Ordinal > lastOrdinal {
			lastOrdinal = a.Ordinal
		}
	}
	idStr, err := argString(args, lastOrdinal)
	if err != nil {
		return nil, err
	}
	id := uuid.MustParse(idStr)

	d := findTestDelivery(c.store.deliveries, id)
	if d == nil {
		return &fakeResult{rowsAffected: 0}, nil
	}

	now := time.Now().UTC()
	switch {
	case strings.Contains(query, "'delivered'"):
		d.status = "delivered"
		d.deliveredAt = &now
		// response_code = $2
		if code, err := argInt64(args, 2); err == nil {
			c := int(code)
			d.responseCode = &c
		}
		// response_body = $3
		if body, err := argString(args, 3); err == nil {
			d.responseBody = &body
		}
	case strings.Contains(query, "'retrying'"):
		d.status = "retrying"
	case strings.Contains(query, "'failed'"):
		d.status = "failed"
	}

	if attemptCount, err := argInt64(args, 1); err == nil {
		d.attemptCount = int(attemptCount)
	}
	d.lastAttemptAt = &now

	return &fakeResult{rowsAffected: 1}, nil
}

func (c *fakeConn) execDeleteWebhookConfig(args []driver.NamedValue) (driver.Result, error) {
	idStr, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return nil, err
	}
	tidStr, err := argString(args, 2)
	if err != nil {
		return nil, err
	}
	tid, err := uuid.Parse(tidStr)
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

// --- Query implementations ---

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

func (c *fakeConn) queryWebhookExists(args []driver.NamedValue) (driver.Rows, error) {
	idStr, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return nil, err
	}
	tidStr, err := argString(args, 2)
	if err != nil {
		return nil, err
	}
	tid, err := uuid.Parse(tidStr)
	if err != nil {
		return nil, err
	}

	exists := false
	for _, cfg := range c.store.configs {
		if cfg.id == id && cfg.tenantID == tid {
			exists = true
			break
		}
	}

	return &fakeRows{
		columns: []string{"exists"},
		data:    [][]driver.Value{{exists}},
	}, nil
}

func (c *fakeConn) queryWebhookConfigForDelivery(args []driver.NamedValue) (driver.Rows, error) {
	idStr, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return nil, err
	}

	cfg := findWebhookCfg(c.store.configs, id)
	if cfg == nil {
		return &fakeRows{columns: []string{"url", "secret"}}, nil
	}
	return &fakeRows{
		columns: []string{"url", "secret"},
		data:    [][]driver.Value{{cfg.url, cfg.secret}},
	}, nil
}

func (c *fakeConn) queryPendingDeliveries(args []driver.NamedValue) (driver.Rows, error) {
	columns := []string{"id", "webhook_id", "event_type", "payload", "attempt_count"}
	var data [][]driver.Value
	for _, d := range c.store.deliveries {
		if d.status == "pending" || d.status == "retrying" {
			// The payload must be []byte for driver.Value compatibility.
			data = append(data, []driver.Value{
				d.id.String(),
				d.webhookID.String(),
				d.eventType,
				d.payload,
				int64(d.attemptCount),
			})
		}
	}
	return &fakeRows{columns: columns, data: data}, nil
}

func (c *fakeConn) queryDeliveries(args []driver.NamedValue, query string) (driver.Rows, error) {
	webhookIDStr, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	webhookID, err := uuid.Parse(webhookIDStr)
	if err != nil {
		return nil, err
	}

	hasStatusFilter := strings.Contains(query, "AND status = $")
	var statusFilter string
	if hasStatusFilter && len(args) >= 2 {
		statusFilter, _ = argString(args, 2)
	}

	columns := []string{
		"id", "webhook_id", "event_type", "payload", "status",
		"attempt_count", "last_attempt_at", "next_attempt_at",
		"delivered_at", "response_code", "response_body", "created_at",
	}
	var data [][]driver.Value
	for _, d := range c.store.deliveries {
		if d.webhookID != webhookID {
			continue
		}
		if hasStatusFilter && d.status != statusFilter {
			continue
		}

		var lastAttemptAt driver.Value
		if d.lastAttemptAt != nil {
			lastAttemptAt = *d.lastAttemptAt
		}
		var nextAttemptAt driver.Value
		if d.nextAttemptAt != nil {
			nextAttemptAt = *d.nextAttemptAt
		}
		var deliveredAt driver.Value
		if d.deliveredAt != nil {
			deliveredAt = *d.deliveredAt
		}
		var responseCode driver.Value
		if d.responseCode != nil {
			responseCode = int64(*d.responseCode)
		}
		var responseBody driver.Value
		if d.responseBody != nil {
			responseBody = *d.responseBody
		}

		data = append(data, []driver.Value{
			d.id.String(),
			d.webhookID.String(),
			d.eventType,
			d.payload,
			d.status,
			int64(d.attemptCount),
			lastAttemptAt,
			nextAttemptAt,
			deliveredAt,
			responseCode,
			responseBody,
			d.createdAt,
		})
	}
	return &fakeRows{columns: columns, data: data}, nil
}

func (c *fakeConn) queryGetWebhook(args []driver.NamedValue) (driver.Rows, error) {
	idStr, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return nil, err
	}
	tidStr, err := argString(args, 2)
	if err != nil {
		return nil, err
	}
	tid, err := uuid.Parse(tidStr)
	if err != nil {
		return nil, err
	}

	columns := []string{"id", "url", "secret", "events", "enabled", "created_at", "updated_at"}
	for _, cfg := range c.store.configs {
		if cfg.id == id && cfg.tenantID == tid {
			return &fakeRows{
				columns: columns,
				data: [][]driver.Value{{
					cfg.id.String(),
					cfg.url,
					cfg.secret,
					[]byte(cfg.events),
					cfg.enabled,
					cfg.createdAt,
					cfg.updatedAt,
				}},
			}, nil
		}
	}
	return &fakeRows{columns: columns}, nil
}

func (c *fakeConn) queryListWebhooks(args []driver.NamedValue) (driver.Rows, error) {
	tidStr, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	tid, err := uuid.Parse(tidStr)
	if err != nil {
		return nil, err
	}

	columns := []string{"id", "url", "secret", "events", "enabled", "created_at", "updated_at"}
	var data [][]driver.Value
	for _, cfg := range c.store.configs {
		if cfg.tenantID == tid {
			data = append(data, []driver.Value{
				cfg.id.String(),
				cfg.url,
				cfg.secret,
				[]byte(cfg.events),
				cfg.enabled,
				cfg.createdAt,
				cfg.updatedAt,
			})
		}
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
				return "", fmt.Errorf("arg %d: want string/[]byte, got %T", ordinal, a.Value)
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
			v, ok := a.Value.(int64)
			if !ok {
				return 0, fmt.Errorf("arg %d: want int64, got %T", ordinal, a.Value)
			}
			return v, nil
		}
	}
	return 0, fmt.Errorf("arg %d not found", ordinal)
}

func argTime(args []driver.NamedValue, ordinal int) (time.Time, error) {
	for _, a := range args {
		if a.Ordinal == ordinal {
			v, ok := a.Value.(time.Time)
			if !ok {
				return time.Time{}, fmt.Errorf("arg %d: want time.Time, got %T", ordinal, a.Value)
			}
			return v, nil
		}
	}
	return time.Time{}, fmt.Errorf("arg %d not found", ordinal)
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

// ---------------------------------------------------------------------------
// Test setup helpers
// ---------------------------------------------------------------------------

var testTenantID = uuid.MustParse("00000000-0000-0000-0000-000000000001")

const testAPIKey = "test-api-key"

func setupTestPlugin(t *testing.T) (*Plugin, *fakeNotifyStore) {
	t.Helper()

	store := newFakeNotifyStore()

	// Pre-populate tenant API key so auth middleware succeeds.
	keyHash := sha256.Sum256([]byte(testAPIKey))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantID.String()

	db := sql.OpenDB(&fakeConnector{store: store})
	t.Cleanup(func() { db.Close() })

	p := &Plugin{
		db:     &host.SQLDBAdapter{DB: db},
		logger: slog.Default(),
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
		config: Config{},
	}

	return p, store
}

func authedRequest(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	return req
}

// createTestWebhook is a helper that creates a webhook via the route handler
// and returns its ID.
func createTestWebhook(t *testing.T, handler http.Handler, url, secret string) uuid.UUID {
	t.Helper()

	body := fmt.Sprintf(`{"url":%q,"secret":%q,"events":["test.event"]}`, url, secret)
	req := authedRequest("POST", "/webhooks", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("createTestWebhook: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("createTestWebhook: failed to decode: %v", err)
	}
	idStr, _ := resp["id"].(string)
	id := uuid.MustParse(idStr)
	return id
}

// buildHandler creates an http.Handler with routes and auth middleware.
func buildHandler(t *testing.T, p *Plugin, store *fakeNotifyStore) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}
	db := sql.OpenDB(&fakeConnector{store: store})
	t.Cleanup(func() { db.Close() })
	return auth.Middleware(host.NewPostgresStore(db), false)(mux)
}

// ---------------------------------------------------------------------------
// Existing tests
// ---------------------------------------------------------------------------

func TestInfo(t *testing.T) {
	p := &Plugin{}
	info := p.Info()
	if info.Name != "notifications" {
		t.Errorf("expected Name 'notifications', got %q", info.Name)
	}
	if info.Version != "0.1.0" {
		t.Errorf("expected Version '0.1.0', got %q", info.Version)
	}
	if info.Description == "" {
		t.Error("expected non-empty Description")
	}
}

func TestInit(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{}
	err := p.Init(context.Background(), env)
	if err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if p.httpClient == nil {
		t.Error("expected httpClient to be set")
	}
	if p.logger == nil {
		t.Error("expected logger to be set")
	}
}

func TestInitWithConfig(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		Config: []byte(`{}`),
	}
	err := p.Init(context.Background(), env)
	if err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
}

func TestInitInvalidConfig(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		Config: []byte(`not valid json`),
	}
	err := p.Init(context.Background(), env)
	if err == nil {
		t.Fatal("expected error for invalid config, got nil")
	}
}

func TestRegisterRoutes(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	err := p.RegisterRoutes(mux)
	if err != nil {
		t.Fatalf("RegisterRoutes() returned error: %v", err)
	}

	tests := []struct {
		method string
		path   string
	}{
		{"POST", "/webhooks"},
		{"GET", "/webhooks"},
		{"GET", "/webhooks/11111111-1111-1111-1111-111111111111"},
		{"PUT", "/webhooks/11111111-1111-1111-1111-111111111111"},
		{"DELETE", "/webhooks/11111111-1111-1111-1111-111111111111"},
		{"GET", "/webhooks/11111111-1111-1111-1111-111111111111/deliveries"},
	}
	for _, tt := range tests {
		req := httptest.NewRequest(tt.method, tt.path, nil)
		_, pattern := mux.Handler(req)
		if pattern == "" {
			t.Errorf("no handler matched %s %s", tt.method, tt.path)
		}
	}
}

// ---------------------------------------------------------------------------
// Behavioral tests
// ---------------------------------------------------------------------------

// TestCreateAndGetWebhook creates a webhook via the HTTP route and then
// retrieves it, verifying all fields are preserved.
func TestCreateAndGetWebhook(t *testing.T) {
	p, store := setupTestPlugin(t)
	handler := buildHandler(t, p, store)

	// Create webhook.
	id := createTestWebhook(t, handler, "https://example.com/hook", "my-secret")

	// GET /webhooks/{id}
	req := authedRequest("GET", "/webhooks/"+id.String(), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET webhook: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("GET webhook: failed to decode: %v", err)
	}
	if resp["url"] != "https://example.com/hook" {
		t.Errorf("expected url %q, got %q", "https://example.com/hook", resp["url"])
	}
	if resp["secret"] != "my-secret" {
		t.Errorf("expected secret %q, got %q", "my-secret", resp["secret"])
	}
	if resp["enabled"] != true {
		t.Errorf("expected enabled=true, got %v", resp["enabled"])
	}
}

// TestListWebhooks creates multiple webhooks and verifies the list endpoint.
func TestListWebhooks(t *testing.T) {
	p, store := setupTestPlugin(t)
	handler := buildHandler(t, p, store)

	createTestWebhook(t, handler, "https://a.example.com", "sec-a")
	createTestWebhook(t, handler, "https://b.example.com", "sec-b")

	req := authedRequest("GET", "/webhooks", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("LIST webhooks: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var list []interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("LIST: failed to decode: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("LIST: expected 2 webhooks, got %d", len(list))
	}
}

// TestCreateWebhookValidation verifies that creating a webhook without a url
// returns 400.
func TestCreateWebhookValidation(t *testing.T) {
	p, store := setupTestPlugin(t)
	handler := buildHandler(t, p, store)

	req := authedRequest("POST", "/webhooks", bytes.NewReader([]byte(`{"events":["test"]}`)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing url, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestUpdateWebhook creates a webhook, updates it, and verifies the change.
func TestUpdateWebhook(t *testing.T) {
	p, store := setupTestPlugin(t)
	handler := buildHandler(t, p, store)

	id := createTestWebhook(t, handler, "https://original.example.com", "orig-secret")

	updateBody := `{"url":"https://updated.example.com"}`
	req := authedRequest("PUT", "/webhooks/"+id.String(), bytes.NewReader([]byte(updateBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("UPDATE webhook: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["url"] != "https://updated.example.com" {
		t.Errorf("expected updated url %q, got %q", "https://updated.example.com", resp["url"])
	}
}

// TestDeleteWebhook creates a webhook, deletes it, and verifies it's gone.
func TestDeleteWebhook(t *testing.T) {
	p, store := setupTestPlugin(t)
	handler := buildHandler(t, p, store)

	id := createTestWebhook(t, handler, "https://delete.example.com", "secret")

	req := authedRequest("DELETE", "/webhooks/"+id.String(), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE webhook: expected 204, got %d", rec.Code)
	}

	// GET -> 404
	req = authedRequest("GET", "/webhooks/"+id.String(), nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET deleted webhook: expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestHostFunctionSendWebhook tests the sendWebhook host function: it creates
// a webhook config in the store, calls sendWebhook with a valid webhook_id,
// and verifies that a delivery row is created in pending status.
func TestHostFunctionSendWebhook(t *testing.T) {
	p, store := setupTestPlugin(t)

	// Create a webhook config directly in the store.
	webhookID := uuid.New()
	now := time.Now().UTC()
	store.mu.Lock()
	store.configs = append(store.configs, &testWebhookCfg{
		tenantID:  testTenantID,
		id:        webhookID,
		url:       "https://example.com/hook",
		secret:    "test-secret",
		events:    `["test.event"]`,
		enabled:   true,
		createdAt: now,
		updatedAt: now,
	})
	store.mu.Unlock()

	cc := &plugin.CallContext{
		TenantID:   testTenantID.String(),
		WorkflowID: "wf-test",
		DB:         p.db.(*host.SQLDBAdapter).DB,
	}
	ctx := plugin.WithCallContext(context.Background(), cc)

	input := fmt.Sprintf(`{"webhook_id":"%s","event_type":"test.event","payload":{"key":"val"}}`, webhookID.String())
	output, err := p.sendWebhook(ctx, input)
	if err != nil {
		t.Fatalf("sendWebhook: %v", err)
	}

	var out map[string]interface{}
	if err := json.Unmarshal([]byte(output), &out); err != nil {
		t.Fatalf("sendWebhook output: failed to decode: %v", err)
	}
	deliveryIDStr, ok := out["delivery_id"].(string)
	if !ok || deliveryIDStr == "" {
		t.Fatalf("sendWebhook: expected delivery_id in output, got %+v", out)
	}

	// Verify a delivery row was created.
	store.mu.RLock()
	deliveryID := uuid.MustParse(deliveryIDStr)
	d := findTestDelivery(store.deliveries, deliveryID)
	store.mu.RUnlock()
	if d == nil {
		t.Fatal("sendWebhook: delivery not found in store")
	}
	if d.status != "pending" {
		t.Errorf("sendWebhook: expected status 'pending', got %q", d.status)
	}
	if d.webhookID != webhookID {
		t.Errorf("sendWebhook: expected webhook_id %s, got %s", webhookID, d.webhookID)
	}
	if d.eventType != "test.event" {
		t.Errorf("sendWebhook: expected event_type 'test.event', got %q", d.eventType)
	}
}

// TestHostFunctionInvalidWebhook tests that sendWebhook returns an error
// when called with a non-existent webhook_id.
func TestHostFunctionInvalidWebhook(t *testing.T) {
	p, _ := setupTestPlugin(t)

	cc := &plugin.CallContext{
		TenantID:   testTenantID.String(),
		WorkflowID: "wf-test",
		DB:         p.db.(*host.SQLDBAdapter).DB,
	}
	ctx := plugin.WithCallContext(context.Background(), cc)

	input := fmt.Sprintf(`{"webhook_id":"%s","event_type":"test.event"}`, uuid.New().String())
	_, err := p.sendWebhook(ctx, input)
	if err == nil {
		t.Fatal("sendWebhook: expected error for invalid webhook_id, got nil")
	}
	if !strings.Contains(err.Error(), "webhook not found") {
		t.Errorf("sendWebhook: expected error containing 'webhook not found', got %q", err)
	}
}

// TestHostFunctionMissingWebhookID tests that sendWebhook returns an error
// when the webhook_id is missing.
func TestHostFunctionMissingWebhookID(t *testing.T) {
	p, _ := setupTestPlugin(t)

	cc := &plugin.CallContext{
		TenantID:   testTenantID.String(),
		WorkflowID: "wf-test",
		DB:         p.db.(*host.SQLDBAdapter).DB,
	}
	ctx := plugin.WithCallContext(context.Background(), cc)

	_, err := p.sendWebhook(ctx, `{"event_type":"test.event"}`)
	if err == nil {
		t.Fatal("sendWebhook: expected error for missing webhook_id, got nil")
	}
}

// TestHostFunctionMissingEventType tests that sendWebhook returns an error
// when the event_type is missing.
func TestHostFunctionMissingEventType(t *testing.T) {
	p, _ := setupTestPlugin(t)

	cc := &plugin.CallContext{
		TenantID:   testTenantID.String(),
		WorkflowID: "wf-test",
		DB:         p.db.(*host.SQLDBAdapter).DB,
	}
	ctx := plugin.WithCallContext(context.Background(), cc)

	input := fmt.Sprintf(`{"webhook_id":"%s"}`, uuid.New().String())
	_, err := p.sendWebhook(ctx, input)
	if err == nil {
		t.Fatal("sendWebhook: expected error for missing event_type, got nil")
	}
}

// TestWebhookDelivery tests the background delivery processing. It creates
// a webhook config pointing to a local test server, creates a delivery in
// pending status, and calls processDeliveries to verify the delivery is
// successfully sent over HTTP.
func TestWebhookDelivery(t *testing.T) {
	p, store := setupTestPlugin(t)

	// Start a mock HTTP server to receive the webhook.
	var received bool
	var receivedPayload []byte
	var receivedEventType string
	var receivedSig string
	mockMux := http.NewServeMux()
	mockMux.HandleFunc("/hook", func(w http.ResponseWriter, r *http.Request) {
		received = true
		receivedEventType = r.Header.Get("X-Webhook-Event")
		receivedSig = r.Header.Get("X-Webhook-Signature")
		receivedPayload, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})
	mockServer := httptest.NewServer(mockMux)
	defer mockServer.Close()

	// Create webhook config pointing to the mock server.
	webhookID := uuid.New()
	now := time.Now().UTC()
	store.mu.Lock()
	store.configs = append(store.configs, &testWebhookCfg{
		tenantID:  testTenantID,
		id:        webhookID,
		url:       mockServer.URL + "/hook",
		secret:    "test-hmac-secret",
		events:    `["test.event"]`,
		enabled:   true,
		createdAt: now,
		updatedAt: now,
	})

	// Create a pending delivery with next_attempt_at in the past.
	past := now.Add(-1 * time.Hour)
	deliveryID := uuid.New()
	store.deliveries = append(store.deliveries, &testDelivery{
		id:           deliveryID,
		webhookID:    webhookID,
		eventType:    "test.event",
		payload:      []byte(`{"msg":"hello"}`),
		status:       "pending",
		attemptCount: 0,
		nextAttemptAt: &past,
		createdAt:    past,
	})
	store.mu.Unlock()

	// Call processDeliveries.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	attempted, succeeded, failed, err := p.processDeliveries(ctx)
	if err != nil {
		t.Fatalf("processDeliveries: %v", err)
	}
	if attempted != 1 {
		t.Errorf("expected 1 attempted delivery, got %d", attempted)
	}
	if succeeded != 1 {
		t.Errorf("expected 1 succeeded delivery, got %d", succeeded)
	}
	if failed != 0 {
		t.Errorf("expected 0 failed deliveries, got %d", failed)
	}

	// Verify the HTTP server received the request.
	if !received {
		t.Fatal("webhook HTTP server did not receive the request")
	}
	if receivedEventType != "test.event" {
		t.Errorf("expected event type 'test.event', got %q", receivedEventType)
	}
	if !strings.Contains(string(receivedPayload), "hello") {
		t.Errorf("expected payload to contain 'hello', got %s", string(receivedPayload))
	}
	if !strings.HasPrefix(receivedSig, "sha256=") {
		t.Errorf("expected signature to start with 'sha256=', got %q", receivedSig)
	}

	// Verify the delivery was marked as delivered.
	store.mu.RLock()
	d := findTestDelivery(store.deliveries, deliveryID)
	store.mu.RUnlock()
	if d == nil {
		t.Fatal("delivery not found after processing")
	}
	if d.status != "delivered" {
		t.Errorf("expected status 'delivered', got %q", d.status)
	}
	if d.responseCode == nil || *d.responseCode != http.StatusOK {
		t.Errorf("expected response_code 200, got %v", d.responseCode)
	}
}

// TestListDeliveries creates a webhook, creates a delivery directly in the
// store, then lists deliveries via the HTTP endpoint.
func TestListDeliveries(t *testing.T) {
	p, store := setupTestPlugin(t)
	handler := buildHandler(t, p, store)

	// Create webhook via route.
	id := createTestWebhook(t, handler, "https://example.com/hook", "secret")

	// Create a delivery directly in the store.
	now := time.Now().UTC()
	deliveryID := uuid.New()
	store.mu.Lock()
	store.deliveries = append(store.deliveries, &testDelivery{
		id:           deliveryID,
		webhookID:    id,
		eventType:    "test.event",
		payload:      []byte(`{"key":"val"}`),
		status:       "pending",
		attemptCount: 0,
		createdAt:    now,
	})
	store.mu.Unlock()

	// GET /webhooks/{id}/deliveries
	req := authedRequest("GET", "/webhooks/"+id.String()+"/deliveries", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("LIST deliveries: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var deliveries []interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &deliveries); err != nil {
		t.Fatalf("LIST deliveries: failed to decode: %v", err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("LIST deliveries: expected 1 delivery, got %d", len(deliveries))
	}
}

// TestListDeliveriesNonexistentWebhook verifies that listing deliveries for
// a non-existent webhook returns 404.
func TestListDeliveriesNonexistentWebhook(t *testing.T) {
	p, store := setupTestPlugin(t)
	handler := buildHandler(t, p, store)

	req := authedRequest("GET", "/webhooks/"+uuid.New().String()+"/deliveries", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for nonexistent webhook, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestDeleteNonexistentWebhook verifies that deleting a non-existent webhook
// returns 404.
func TestDeleteNonexistentWebhook(t *testing.T) {
	p, store := setupTestPlugin(t)
	handler := buildHandler(t, p, store)

	req := authedRequest("DELETE", "/webhooks/"+uuid.New().String(), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("DELETE nonexistent: expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestCreateWebhookInvalidJSON verifies that creating a webhook with invalid
// JSON returns 400.
func TestCreateWebhookInvalidJSON(t *testing.T) {
	p, store := setupTestPlugin(t)
	handler := buildHandler(t, p, store)

	req := authedRequest("POST", "/webhooks", bytes.NewReader([]byte(`not json`)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid JSON, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ---- Exported interface tests ----

func TestRegisterHostFunctions(t *testing.T) {
	p := &Plugin{}
	scope := &testFuncRegistry{}
	err := p.RegisterHostFunctions(scope)
	if err != nil {
		t.Fatalf("RegisterHostFunctions() returned error: %v", err)
	}
	if _, ok := scope.funcs["send_webhook"]; !ok {
		t.Error("expected 'send_webhook' function to be registered")
	}
}

func TestRegisterHostFunctionsNilScope(t *testing.T) {
	p := &Plugin{}
	err := p.RegisterHostFunctions(nil)
	if err == nil {
		t.Fatal("expected error for nil scope, got nil")
	}
}

// testFuncRegistry implements plugin.FuncRegistry for testing.
type testFuncRegistry struct {
	funcs map[string]plugin.FuncOptions
}

func (r *testFuncRegistry) Register(opts plugin.FuncOptions, fn plugin.PluginFunc) error {
	if r.funcs == nil {
		r.funcs = make(map[string]plugin.FuncOptions)
	}
	r.funcs[opts.Name] = opts
	return nil
}


// ---- joinSetClauses tests ----

func TestJoinSetClauses(t *testing.T) {
	tests := []struct {
		input []string
		want  string
	}{
		{nil, ""},
		{[]string{}, ""},
		{[]string{"url = $1"}, "url = $1"},
		{[]string{"url = $1", "secret = $2"}, "url = $1, secret = $2"},
		{[]string{"a = $1", "b = $2", "c = $3"}, "a = $1, b = $2, c = $3"},
	}
	for _, tt := range tests {
		got := joinSetClauses(tt.input)
		if got != tt.want {
			t.Errorf("joinSetClauses(%v) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// ---- sendWebhook host function edge cases ----

func TestSendWebhookNoTenant(t *testing.T) {
	p := &Plugin{logger: slog.Default(), db: nil}
	_, err := p.sendWebhook(context.Background(), `{"webhook_id":"`+uuid.New().String()+`","event_type":"test"}`)
	if err == nil {
		t.Fatal("expected error for missing tenant")
	}
	if !strings.Contains(err.Error(), "no tenant context") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSendWebhookInvalidJSON(t *testing.T) {
	p := &Plugin{logger: slog.Default()}
	cc := &plugin.CallContext{TenantID: testTenantID.String(), WorkflowID: "wf-1"}
	ctx := plugin.WithCallContext(context.Background(), cc)
	_, err := p.sendWebhook(ctx, `not json`)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestSendWebhookNilPayload(t *testing.T) {
	p, store := setupTestPlugin(t)
	webhookID := uuid.New()
	now := time.Now().UTC()
	store.mu.Lock()
	store.configs = append(store.configs, &testWebhookCfg{
		tenantID:  testTenantID,
		id:        webhookID,
		url:       "https://example.com/hook",
		secret:    "",
		events:    `["test.event"]`,
		enabled:   true,
		createdAt: now,
		updatedAt: now,
	})
	store.mu.Unlock()

	cc := &plugin.CallContext{TenantID: testTenantID.String(), WorkflowID: "wf-test"}
	ctx := plugin.WithCallContext(context.Background(), cc)

	input := fmt.Sprintf(`{"webhook_id":"%s","event_type":"test.event"}`, webhookID.String())
	_, err := p.sendWebhook(ctx, input)
	if err != nil {
		t.Fatalf("sendWebhook: %v", err)
	}
}
