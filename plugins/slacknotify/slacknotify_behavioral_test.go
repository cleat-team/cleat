// Package slacknotify behavioral tests — fake DB + in-memory store, no PostgreSQL.
package slacknotify

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
	"github.com/rcownie/cleat/internal/auth"
	"github.com/rcownie/cleat/internal/plugin"
)

// ---------------------------------------------------------------------------
// In-memory fake DB store
// ---------------------------------------------------------------------------

type slackConfigRow struct {
	id             string
	tenantID       string
	name           string
	webhookURL     string
	defaultChannel *string
	enabled        bool
	createdAt      time.Time
	updatedAt      time.Time
}

type fakeDBStore struct {
	mu          sync.RWMutex
	configs     []slackConfigRow
	apiKeys     map[string]string // key_hash_hex -> tenant_id
	simulateErr bool
}

func newFakeDBStore() *fakeDBStore {
	return &fakeDBStore{
		configs: make([]slackConfigRow, 0),
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
	if c.store.simulateErr {
		return nil, fmt.Errorf("simulated db error")
	}

	switch {
	case strings.Contains(query, "INSERT INTO slack_config"):
		return c.execInsertConfig(args)
	case strings.Contains(query, "UPDATE slack_config"):
		return c.execUpdateConfig(query, args)
	case strings.Contains(query, "DELETE FROM slack_config"):
		return c.execDeleteConfig(args)
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
	case strings.Contains(query, "SELECT tenant_id FROM tenant_api_keys"):
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryTenantLookup(args)
	case strings.Contains(query, "SELECT id, name, webhook_url, default_channel, enabled, created_at, updated_at"):
		if strings.Contains(query, "WHERE id = $1") {
			// Single config lookup (handleGetConfig or sendMessage)
			c.store.mu.RLock()
			defer c.store.mu.RUnlock()
			return c.queryGetConfig(args)
		}
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryListConfigs(args)
	case strings.Contains(query, "SELECT webhook_url, default_channel"):
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryWebhookConfig(args)
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
	webhookURL, err := argString(args, 4)
	if err != nil {
		return nil, err
	}

	// Args 5 is default_channel (nullable).
	var defaultChannel *string
	if v, err := argAny(args, 5); err == nil && v != nil {
		if s, ok := v.(string); ok {
			defaultChannel = &s
		}
	}

	// Args 6 is created_at (time.Time).
	nowVal, err := argTime(args, 6)
	if err != nil {
		return nil, err
	}

	c.store.configs = append(c.store.configs, slackConfigRow{
		id:             id,
		tenantID:       tenantID,
		name:           name,
		webhookURL:     webhookURL,
		defaultChannel: defaultChannel,
		enabled:        true,
		createdAt:      nowVal,
		updatedAt:      nowVal,
	})
	return &fakeResult{rowsAffected: 1}, nil
}

func (c *fakeConn) execUpdateConfig(query string, args []driver.NamedValue) (driver.Result, error) {
	// Find id and tenant_id. The WHERE clause has `id = $N AND tenant_id = $N+1`,
	// so the last two UUID-like args are: [..., id, tenant_id].
	var allUUIDs []string
	for _, a := range args {
		if a.Ordinal > 0 {
			if v, ok := a.Value.(string); ok {
				if _, err := uuid.Parse(v); err == nil {
					allUUIDs = append(allUUIDs, v)
				}
			}
		}
	}
	if len(allUUIDs) < 1 {
		return &fakeResult{rowsAffected: 0}, nil
	}
	// allUUIDs is in args order: [id, tenant_id]
	id := allUUIDs[0]
	tenantID := ""
	if len(allUUIDs) >= 2 {
		tenantID = allUUIDs[1]
	}

	for i, cfg := range c.store.configs {
		if (tenantID == "" || cfg.tenantID == tenantID) && cfg.id == id {
			// Apply SET clauses by checking which fields appear in the query.
			argIdx := 1
			if strings.Contains(query, "name = $") {
				if v, err := argString(args, argIdx); err == nil {
					cfg.name = v
				}
				argIdx++
			}
			if strings.Contains(query, "webhook_url = $") {
				if v, err := argString(args, argIdx); err == nil {
					cfg.webhookURL = v
				}
				argIdx++
			}
			if strings.Contains(query, "default_channel = $") {
				if v, err := argAny(args, argIdx); err == nil {
					if s, ok := v.(string); ok {
						cfg.defaultChannel = &s
					} else {
						cfg.defaultChannel = nil
					}
				}
				argIdx++
			}
			if strings.Contains(query, "enabled = $") {
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

	var results []slackConfigRow
	for _, cfg := range c.store.configs {
		if cfg.tenantID == tid {
			results = append(results, cfg)
		}
	}

	columns := []string{"id", "name", "webhook_url", "default_channel", "enabled", "created_at", "updated_at"}
	var data [][]driver.Value
	for _, cfg := range results {
		var dc driver.Value
		if cfg.defaultChannel != nil {
			dc = *cfg.defaultChannel
		}
		data = append(data, []driver.Value{
			cfg.id, cfg.name, cfg.webhookURL, dc,
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

	// Check if there's a "AND enabled = true" (sendMessage query)
	// We'll just return the config if found.
	for _, cfg := range c.store.configs {
		if cfg.id == id && (tid == "" || cfg.tenantID == tid) {
			var dc driver.Value
			if cfg.defaultChannel != nil {
				dc = *cfg.defaultChannel
			}
			return &fakeRows{
				columns: []string{"id", "name", "webhook_url", "default_channel", "enabled", "created_at", "updated_at"},
				data: [][]driver.Value{{
					cfg.id, cfg.name, cfg.webhookURL, dc,
					cfg.enabled, cfg.createdAt, cfg.updatedAt,
				}},
			}, nil
		}
	}
	return &fakeRows{
		columns: []string{"id", "name", "webhook_url", "default_channel", "enabled", "created_at", "updated_at"},
	}, nil
}

func (c *fakeConn) queryWebhookConfig(args []driver.NamedValue) (driver.Rows, error) {
	id, err := argString(args, 1)
	if err != nil {
		return nil, err
	}

	for _, cfg := range c.store.configs {
		if cfg.id == id && cfg.enabled {
			var dc driver.Value
			if cfg.defaultChannel != nil {
				dc = *cfg.defaultChannel
			}
			return &fakeRows{
				columns: []string{"webhook_url", "default_channel"},
				data:    [][]driver.Value{{cfg.webhookURL, dc}},
			}, nil
		}
	}

	// Need to also check tenant ID from the second arg.
	if len(args) >= 2 {
		tid, _ := argString(args, 2)
		for _, cfg := range c.store.configs {
			if cfg.id == id && cfg.tenantID == tid && cfg.enabled {
				var dc driver.Value
				if cfg.defaultChannel != nil {
					dc = *cfg.defaultChannel
				}
				return &fakeRows{
					columns: []string{"webhook_url", "default_channel"},
					data:    [][]driver.Value{{cfg.webhookURL, dc}},
				}, nil
			}
		}
	}

	return &fakeRows{columns: []string{"webhook_url", "default_channel"}}, nil
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
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

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

// TestConfigCreateAndGet verifies creating a Slack notification config and
// retrieving it by ID.
func TestConfigCreateAndGet(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	body := `{"name":"alerts","webhook_url":"https://hooks.slack.com/services/T00/B00/abc123"}`
	req := authedRequest("POST", "/slack/configs", bytes.NewReader([]byte(body)))
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
	if created["name"] != "alerts" {
		t.Errorf("expected name 'alerts', got %s", created["name"])
	}
	if created["webhook_url"] != "https://hooks.slack.com/services/T00/B00/abc123" {
		t.Errorf("unexpected webhook_url: %s", created["webhook_url"])
	}
	if created["enabled"] != true {
		t.Errorf("expected enabled=true, got %v", created["enabled"])
	}

	// Verify stored.
	store.mu.RLock()
	if len(store.configs) != 1 {
		t.Fatalf("expected 1 config in store, got %d", len(store.configs))
	}
	store.mu.RUnlock()

	// GET by ID.
	req = authedRequest("GET", "/slack/configs/"+id, nil)
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
	if fetched["name"] != "alerts" {
		t.Errorf("expected name 'alerts', got %s", fetched["name"])
	}
}

// TestConfigList verifies listing all Slack configs for a tenant.
func TestConfigList(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	// Create two configs.
	for _, name := range []string{"critical", "warning"} {
		body := fmt.Sprintf(`{"name":"%s","webhook_url":"https://hooks.slack.com/%s"}`, name, name)
		req := authedRequest("POST", "/slack/configs", bytes.NewReader([]byte(body)))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("POST %s: expected 201, got %d", name, rec.Code)
		}
	}

	// List.
	req := authedRequest("GET", "/slack/configs", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("LIST: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var configs []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &configs); err != nil {
		t.Fatalf("failed to decode list: %v", err)
	}
	if len(configs) != 2 {
		t.Fatalf("expected 2 configs, got %d", len(configs))
	}
}

// TestConfigUpdate verifies updating a Slack config.
func TestConfigUpdate(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	// Create.
	body := `{"name":"original","webhook_url":"https://hooks.slack.com/old"}`
	req := authedRequest("POST", "/slack/configs", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var created map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &created)
	id := created["id"].(string)

	// Update name only.
	updateBody := `{"name":"renamed"}`
	req = authedRequest("PUT", "/slack/configs/"+id, bytes.NewReader([]byte(updateBody)))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var updated map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("failed to decode update: %v", err)
	}
	if updated["name"] != "renamed" {
		t.Errorf("expected name 'renamed', got %s", updated["name"])
	}

	// Verify store.
	store.mu.RLock()
	cfg := store.configs[0]
	store.mu.RUnlock()
	if cfg.name != "renamed" {
		t.Errorf("store: expected name 'renamed', got %s", cfg.name)
	}
}

// TestConfigDelete verifies deleting a Slack config.
func TestConfigDelete(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	body := `{"name":"to-delete","webhook_url":"https://hooks.slack.com/del"}`
	req := authedRequest("POST", "/slack/configs", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST: expected 201, got %d", rec.Code)
	}

	var created map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &created)
	id := created["id"].(string)

	// Delete.
	req = authedRequest("DELETE", "/slack/configs/"+id, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE: expected 204, got %d", rec.Code)
	}

	store.mu.RLock()
	count := len(store.configs)
	store.mu.RUnlock()
	if count != 0 {
		t.Errorf("expected 0 configs after delete, got %d", count)
	}

	// GET -> 404.
	req = authedRequest("GET", "/slack/configs/"+id, nil)
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
	req := authedRequest("GET", "/slack/configs/"+badID, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for non-existent config, got %d", rec.Code)
	}

	req = authedRequest("PUT", "/slack/configs/"+badID, bytes.NewReader([]byte(`{"name":"nope"}`)))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for updating non-existent config, got %d", rec.Code)
	}
}

// TestCreateRejectsMissingFields verifies that creating a config without
// required fields returns 400.
func TestCreateRejectsMissingFields(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	// Missing name.
	body := `{"webhook_url":"https://hooks.slack.com/test"}`
	req := authedRequest("POST", "/slack/configs", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing name, got %d", rec.Code)
	}

	// Missing webhook_url.
	body = `{"name":"test"}`
	req = authedRequest("POST", "/slack/configs", bytes.NewReader([]byte(body)))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing webhook_url, got %d", rec.Code)
	}
}

// TestSendMessage verifies the send_message host function looks up the
// config and sends a webhook. We use a test server to capture the webhook
// payload.
func TestSendMessage(t *testing.T) {
	store := newFakeDBStore()

	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantStr

	cfgID := uuid.New()
	store.configs = append(store.configs, slackConfigRow{
		id:         cfgID.String(),
		tenantID:   testTenantStr,
		name:       "test",
		webhookURL: "https://hooks.slack.com/services/T00/B00/test",
		enabled:    true,
	})

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	// Create a test HTTP server for the Slack webhook.
	var capturedPayload []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPayload, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true,"ts":"12345.67890"}`))
	}))
	defer ts.Close()

	p := &Plugin{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}

	// Update the config's webhook URL to point to our test server.
	store.mu.Lock()
	store.configs[0].webhookURL = ts.URL
	store.mu.Unlock()

	// Build input JSON.
	input := map[string]interface{}{
		"config_id": cfgID.String(),
		"text":      "Hello from test!",
	}
	inputJSON, _ := json.Marshal(input)

	// Call sendMessage with a context containing tenant context.
	callCtx := &plugin.CallContext{TenantID: testTenantID, WorkflowID: "test-workflow"}
	ctx := plugin.WithCallContext(context.Background(), callCtx)

	output, err := p.sendMessage(ctx, string(inputJSON))
	if err != nil {
		t.Fatalf("sendMessage: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("failed to decode output: %v", err)
	}
	if result["success"] != true {
		t.Errorf("expected success=true, got %v", result["success"])
	}

	// Verify the webhook received the correct payload.
	if capturedPayload == nil {
		t.Fatal("expected captured payload")
	}
	var slackPayload map[string]interface{}
	json.Unmarshal(capturedPayload, &slackPayload)
	if slackPayload["text"] != "Hello from test!" {
		t.Errorf("expected text 'Hello from test!', got %s", slackPayload["text"])
	}
}

// ---------------------------------------------------------------------------
// Direct request helper (no auth middleware, adds tenant to context)
// ---------------------------------------------------------------------------

func slackRequest(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	ctx := auth.WithTenantID(req.Context(), testTenantID)
	return req.WithContext(ctx)
}

// ---------------------------------------------------------------------------
// Fake FuncRegistry for RegisterHostFunctions tests
// ---------------------------------------------------------------------------

type fakeFuncRegistry struct {
	funcs map[string]plugin.PluginFunc
}

func newFakeFuncRegistry() *fakeFuncRegistry {
	return &fakeFuncRegistry{funcs: make(map[string]plugin.PluginFunc)}
}

func (r *fakeFuncRegistry) Register(opts plugin.FuncOptions, fn plugin.PluginFunc) error {
	r.funcs[opts.Name] = fn
	return nil
}

func (r *fakeFuncRegistry) Has(name string) bool {
	_, ok := r.funcs[name]
	return ok
}

// ===========================================================================
// RegisterHostFunctions
// ===========================================================================

func TestSN_RegisterHostFunctions_NilRegistry(t *testing.T) {
	store := newFakeDBStore()
	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()
	p := &Plugin{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
	err := p.RegisterHostFunctions(nil)
	if err == nil || !strings.Contains(err.Error(), "nil function registry") {
		t.Fatalf("expected nil registry error, got: %v", err)
	}
}

func TestSN_RegisterHostFunctions_Valid(t *testing.T) {
	store := newFakeDBStore()
	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()
	p := &Plugin{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
	reg := newFakeFuncRegistry()
	if err := p.RegisterHostFunctions(reg); err != nil {
		t.Fatalf("RegisterHostFunctions: %v", err)
	}
	if !reg.Has("send_message") {
		t.Error("expected send_message to be registered")
	}
}

// ===========================================================================
// Migrations
// ===========================================================================

func TestSN_Migrations(t *testing.T) {
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

func TestSN_PluginInfo(t *testing.T) {
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

func TestSN_RegisterRoutes_NilMux(t *testing.T) {
	p, _, _ := setupTestPlugin(t)
	err := p.RegisterRoutes(nil)
	if err == nil || !strings.Contains(err.Error(), "nil mux") {
		t.Fatalf("expected nil mux error, got: %v", err)
	}
}

func TestSN_RegisterRoutes_Valid(t *testing.T) {
	p, _, _ := setupTestPlugin(t)
	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/slack/configs", nil)
	mux.ServeHTTP(rec, req)
	if rec.Code == 404 {
		t.Error("expected /slack/configs to be registered, got 404")
	}
}

// ===========================================================================
// Handler error paths — missing tenant returns 401 for all endpoints
// ===========================================================================

func TestSN_ErrorPaths_MissingTenant(t *testing.T) {
	p, _, _ := setupTestPlugin(t)
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	tests := []struct{ method, path, body string }{
		{"POST", "/slack/configs", `{"name":"t","webhook_url":"https://hook.example.com"}`},
		{"GET", "/slack/configs", ""},
		{"GET", "/slack/configs/00000000-0000-0000-0000-000000000001", ""},
		{"PUT", "/slack/configs/00000000-0000-0000-0000-000000000001", `{"name":"t"}`},
		{"DELETE", "/slack/configs/00000000-0000-0000-0000-000000000001", ""},
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

func TestSN_ErrorPaths_InvalidID(t *testing.T) {
	p, _, _ := setupTestPlugin(t)
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	tests := []struct{ method, path string }{
		{"GET", "/slack/configs/not-a-uuid"},
		{"PUT", "/slack/configs/not-a-uuid"},
		{"DELETE", "/slack/configs/not-a-uuid"},
	}

	for _, tc := range tests {
		rec := httptest.NewRecorder()
		req := slackRequest(tc.method, tc.path, nil)
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

func TestSN_ErrorPaths_InvalidJSON(t *testing.T) {
	p, _, _ := setupTestPlugin(t)
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	tests := []struct{ method, path string }{
		{"POST", "/slack/configs"},
		{"PUT", "/slack/configs/00000000-0000-0000-0000-000000000001"},
	}

	for _, tc := range tests {
		rec := httptest.NewRecorder()
		req := slackRequest(tc.method, tc.path, bytes.NewReader([]byte("not json")))
		mux.ServeHTTP(rec, req)
		if rec.Code != 400 {
			t.Errorf("%s %s: want 400, got %d: %s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}

// ===========================================================================
// Handler error path — PUT with no fields returns 400
// ===========================================================================

func TestSN_ErrorPaths_NoUpdateFields(t *testing.T) {
	p, _, _ := setupTestPlugin(t)
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	// Create a config first.
	rec := httptest.NewRecorder()
	req := slackRequest("POST", "/slack/configs", bytes.NewReader([]byte(`{"name":"test","webhook_url":"https://hook.example.com"}`)))
	mux.ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var created map[string]string
	json.Unmarshal(rec.Body.Bytes(), &created)
	id := created["id"]

	// Update with empty JSON — should fail with 400 (no fields to update).
	rec = httptest.NewRecorder()
	req = slackRequest("PUT", "/slack/configs/"+id, bytes.NewReader([]byte("{}")))
	mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("PUT with no fields: want 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// Handler error paths — simulated DB error returns 500
// ===========================================================================

func TestSN_ErrorPaths_DBError_Create(t *testing.T) {
	store := newFakeDBStore()
	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()
	store.simulateErr = true

	p := &Plugin{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	req := slackRequest("POST", "/slack/configs", bytes.NewReader([]byte(`{"name":"t","webhook_url":"https://hook.example.com"}`)))
	mux.ServeHTTP(rec, req)
	if rec.Code != 500 {
		t.Errorf("POST with DB error: want 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSN_ErrorPaths_DBError_List(t *testing.T) {
	store := newFakeDBStore()
	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	store.simulateErr = false
	store.configs = append(store.configs, slackConfigRow{
		id: uuid.New().String(), tenantID: testTenantStr,
		name: "test", webhookURL: "https://hook.example.com", enabled: true,
	})
	store.simulateErr = true

	p := &Plugin{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	req := slackRequest("GET", "/slack/configs", nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != 500 {
		t.Errorf("LIST with DB error: want 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSN_ErrorPaths_DBError_Get(t *testing.T) {
	store := newFakeDBStore()
	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	cfgID := uuid.New().String()
	store.configs = append(store.configs, slackConfigRow{
		id: cfgID, tenantID: testTenantStr,
		name: "test", webhookURL: "https://hook.example.com", enabled: true,
	})
	store.simulateErr = true

	p := &Plugin{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	req := slackRequest("GET", "/slack/configs/"+cfgID, nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != 500 {
		t.Errorf("GET with DB error: want 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSN_ErrorPaths_DBError_Update(t *testing.T) {
	store := newFakeDBStore()
	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	cfgID := uuid.New().String()
	store.configs = append(store.configs, slackConfigRow{
		id: cfgID, tenantID: testTenantStr,
		name: "test", webhookURL: "https://hook.example.com", enabled: true,
	})
	store.simulateErr = true

	p := &Plugin{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	req := slackRequest("PUT", "/slack/configs/"+cfgID, bytes.NewReader([]byte(`{"name":"updated"}`)))
	mux.ServeHTTP(rec, req)
	if rec.Code != 500 {
		t.Errorf("PUT with DB error: want 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSN_ErrorPaths_DBError_Delete(t *testing.T) {
	store := newFakeDBStore()
	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	cfgID := uuid.New().String()
	store.configs = append(store.configs, slackConfigRow{
		id: cfgID, tenantID: testTenantStr,
		name: "test", webhookURL: "https://hook.example.com", enabled: true,
	})
	store.simulateErr = true

	p := &Plugin{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	req := slackRequest("DELETE", "/slack/configs/"+cfgID, nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != 500 {
		t.Errorf("DELETE with DB error: want 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// sendMessage — error paths
// ===========================================================================

func TestSN_SendMessage_MissingTenant(t *testing.T) {
	store := newFakeDBStore()
	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}

	// No call context in background context -> error.
	_, err := p.sendMessage(context.Background(), `{"config_id":"00000000-0000-0000-0000-000000000001","text":"hello"}`)
	if err == nil || !strings.Contains(err.Error(), "no tenant context") {
		t.Fatalf("expected no tenant context error, got: %v", err)
	}
}

func TestSN_SendMessage_InvalidJSON(t *testing.T) {
	store := newFakeDBStore()
	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}

	ctx := plugin.WithCallContext(context.Background(), &plugin.CallContext{TenantID: testTenantID})
	_, err := p.sendMessage(ctx, `not json`)
	if err == nil || !strings.Contains(err.Error(), "invalid input") {
		t.Fatalf("expected invalid input error, got: %v", err)
	}
}

func TestSN_SendMessage_MissingConfigID(t *testing.T) {
	store := newFakeDBStore()
	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}

	ctx := plugin.WithCallContext(context.Background(), &plugin.CallContext{TenantID: testTenantID})
	_, err := p.sendMessage(ctx, `{"text":"hello"}`)
	if err == nil || !strings.Contains(err.Error(), "config_id is required") {
		t.Fatalf("expected config_id required error, got: %v", err)
	}
}

func TestSN_SendMessage_MissingText(t *testing.T) {
	store := newFakeDBStore()
	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}

	ctx := plugin.WithCallContext(context.Background(), &plugin.CallContext{TenantID: testTenantID})
	_, err := p.sendMessage(ctx, `{"config_id":"00000000-0000-0000-0000-000000000001","text":""}`)
	if err == nil || !strings.Contains(err.Error(), "text is required") {
		t.Fatalf("expected text required error, got: %v", err)
	}
}

func TestSN_SendMessage_ConfigNotFound(t *testing.T) {
	store := newFakeDBStore()
	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}

	ctx := plugin.WithCallContext(context.Background(), &plugin.CallContext{TenantID: testTenantID})
	// No config seeded -> config not found.
	_, err := p.sendMessage(ctx, `{"config_id":"00000000-0000-0000-0000-000000000001","text":"hello"}`)
	if err == nil || !strings.Contains(err.Error(), "config not found") {
		t.Fatalf("expected config not found error, got: %v", err)
	}
}

func TestSN_SendMessage_WebhookError(t *testing.T) {
	store := newFakeDBStore()

	cfgID := uuid.New()
	store.configs = append(store.configs, slackConfigRow{
		id: cfgID.String(), tenantID: testTenantStr,
		name: "test", webhookURL: "https://hooks.slack.com/fake", enabled: true,
	})

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	// Test server that returns an error status.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("server error"))
	}))
	defer ts.Close()

	store.mu.Lock()
	store.configs[0].webhookURL = ts.URL
	store.mu.Unlock()

	p := &Plugin{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}

	ctx := plugin.WithCallContext(context.Background(), &plugin.CallContext{TenantID: testTenantID})
	_, err := p.sendMessage(ctx, `{"config_id":"`+cfgID.String()+`","text":"hello"}`)
	if err == nil || !strings.Contains(err.Error(), "webhook returned") {
		t.Fatalf("expected webhook error, got: %v", err)
	}
}

func TestSN_SendMessage_NonJSONResponse(t *testing.T) {
	store := newFakeDBStore()

	cfgID := uuid.New()
	store.configs = append(store.configs, slackConfigRow{
		id: cfgID.String(), tenantID: testTenantStr,
		name: "test", webhookURL: "https://hooks.slack.com/fake", enabled: true,
	})

	db := sql.OpenDB(&fakeConnector{store: store})
	defer db.Close()

	// Test server that returns plain text (non-JSON) with a 2xx status.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer ts.Close()

	store.mu.Lock()
	store.configs[0].webhookURL = ts.URL
	store.mu.Unlock()

	p := &Plugin{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}

	ctx := plugin.WithCallContext(context.Background(), &plugin.CallContext{TenantID: testTenantID})
	out, err := p.sendMessage(ctx, `{"config_id":"`+cfgID.String()+`","text":"hello"}`)
	if err != nil {
		t.Fatalf("sendMessage: %v", err)
	}
	var result map[string]interface{}
	json.Unmarshal([]byte(out), &result)
	if result["success"] != true {
		t.Errorf("expected success=true, got %v", result["success"])
	}
}
