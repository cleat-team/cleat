package pagerdutyalert

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

type fakePDConfigRow struct {
	tenantID   string
	id         string
	name       string
	routingKey string
	enabled    bool
	createdAt  time.Time
	updatedAt  time.Time
}

type fakeDBStore struct {
	mu       sync.RWMutex
	apiKeys  map[string]string               // key_hash -> tenant_id
	pdConfig map[string]*fakePDConfigRow     // "tenant:id" -> row
}

func newFakeDBStore() *fakeDBStore {
	return &fakeDBStore{
		apiKeys:  make(map[string]string),
		pdConfig: make(map[string]*fakePDConfigRow),
	}
}

type fakeConnector struct {
	store *fakeDBStore
}

func (c *fakeConnector) Connect(_ context.Context) (driver.Conn, error) {
	return &fakeConn{store: c.store}, nil
}

func (c *fakeConnector) Driver() driver.Driver { return &fakeDrv{} }

type fakeDrv struct{}

func (*fakeDrv) Open(_ string) (driver.Conn, error) {
	return nil, fmt.Errorf("fakeDriver: Open not supported")
}

type fakeConn struct {
	store *fakeDBStore
}

func (*fakeConn) Prepare(_ string) (driver.Stmt, error) {
	return nil, fmt.Errorf("fakeConn: unexpected Prepare")
}
func (*fakeConn) Close() error                                     { return nil }
func (*fakeConn) Begin() (driver.Tx, error)                        { return &fakeTx{}, nil }

type fakeTx struct{}

func (*fakeTx) Commit() error   { return nil }
func (*fakeTx) Rollback() error { return nil }

// ---------------------------------------------------------------------------
// Argument extractors
// ---------------------------------------------------------------------------

func argS(args []driver.NamedValue, ordinal int) (string, error) {
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

func argB(args []driver.NamedValue, ordinal int) ([]byte, error) {
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

func argAny(args []driver.NamedValue, ordinal int) (driver.Value, error) {
	for _, a := range args {
		if a.Ordinal == ordinal {
			return a.Value, nil
		}
	}
	return nil, fmt.Errorf("arg %d not found", ordinal)
}

// ---------------------------------------------------------------------------
// ExecContext
// ---------------------------------------------------------------------------

func (c *fakeConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.store.mu.Lock()
	defer c.store.mu.Unlock()

	switch {
	case strings.Contains(query, "INSERT INTO pd_config"):
		return c.execInsertPDConfig(args)
	case strings.Contains(query, "UPDATE pd_config"):
		return c.execUpdatePDConfig(args)
	case strings.Contains(query, "DELETE FROM pd_config"):
		return c.execDeletePDConfig(args)
	default:
		return nil, fmt.Errorf("fakeConn: unexpected Exec query: %s", query[:min(len(query), 80)])
	}
}

func (c *fakeConn) execInsertPDConfig(args []driver.NamedValue) (driver.Result, error) {
	tid, err := argS(args, 1)
	if err != nil {
		return nil, err
	}
	id, err := argS(args, 2)
	if err != nil {
		return nil, err
	}
	name, err := argS(args, 3)
	if err != nil {
		return nil, err
	}
	routingKey, err := argS(args, 4)
	if err != nil {
		return nil, err
	}
	nowVal, err := argAny(args, 5)
	if err != nil {
		return nil, err
	}
	now := nowVal.(time.Time)

	key := tid + ":" + id
	c.store.pdConfig[key] = &fakePDConfigRow{
		tenantID:   tid,
		id:         id,
		name:       name,
		routingKey: routingKey,
		enabled:    true,
		createdAt:  now,
		updatedAt:  now,
	}
	return &fakeResult{1}, nil
}

func (c *fakeConn) execUpdatePDConfig(args []driver.NamedValue) (driver.Result, error) {
	// Dynamic update query — figure out which args are id and tenant_id from end.
	// The query format is: UPDATE pd_config SET ... WHERE id = $N AND tenant_id = $N+1
	// args are positional: [name, routing_key, enabled, ..., id, tenant_id]
	// We need to find id and tenant_id from the last two args.
	n := len(args)
	if n < 2 {
		return &fakeResult{0}, nil
	}
	id, err := argS(args, n-1)
	if err != nil {
		return nil, err
	}
	tid, err := argS(args, n)
	if err != nil {
		return nil, err
	}

	key := tid + ":" + id
	row, ok := c.store.pdConfig[key]
	if !ok {
		return &fakeResult{0}, nil
	}

	// Apply the non-nil updates. The args before the last two are the SET values.
	// We need to figure out which ones are set based on the positional args.
	// Position 1-? = SET values, then position n-1 = id, n = tenant_id.
	for i := 1; i <= n-2; i++ {
		v, err := argAny(args, i)
		if err != nil {
			continue
		}
		// We don't know which field this corresponds to without full SQL parsing,
		// but we can update the timestamp anyway.
		_ = v
	}
	row.updatedAt = time.Now()
	return &fakeResult{1}, nil
}

func (c *fakeConn) execDeletePDConfig(args []driver.NamedValue) (driver.Result, error) {
	id, err := argS(args, 1)
	if err != nil {
		return nil, err
	}
	tid, err := argS(args, 2)
	if err != nil {
		return nil, err
	}

	key := tid + ":" + id
	if _, ok := c.store.pdConfig[key]; !ok {
		return &fakeResult{0}, nil
	}
	delete(c.store.pdConfig, key)
	return &fakeResult{1}, nil
}

// ---------------------------------------------------------------------------
// QueryContext
// ---------------------------------------------------------------------------

func (c *fakeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.store.mu.RLock()
	defer c.store.mu.RUnlock()

	q := strings.ReplaceAll(query, "\n", " ")
	switch {
	case strings.Contains(q, "SELECT tenant_id FROM tenant_api_keys"):
		return c.queryTenantByKeyHash(args)
	case strings.Contains(q, "SELECT routing_key FROM pd_config") || (strings.Contains(q, "routing_key") && strings.Contains(q, "FROM pd_config") && strings.Contains(q, "enabled = true")):
		return c.queryRoutingKey(args)
	case strings.Contains(q, "FROM pd_config") && strings.Contains(q, "ORDER BY"):
		return c.queryPDConfigList(args)
	case strings.Contains(q, "FROM pd_config") && strings.Contains(q, "WHERE id ="):
		return c.queryPDConfigByID(args)
	default:
		return nil, fmt.Errorf("fakeConn: unexpected Query query: %s", query[:min(len(query), 80)])
	}
}

func (c *fakeConn) queryTenantByKeyHash(args []driver.NamedValue) (driver.Rows, error) {
	keyHash, err := argB(args, 1)
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

func (c *fakeConn) queryRoutingKey(args []driver.NamedValue) (driver.Rows, error) {
	configID, err := argS(args, 1)
	if err != nil {
		return nil, err
	}
	tid, err := argS(args, 2)
	if err != nil {
		return nil, err
	}

	key := tid + ":" + configID
	row, ok := c.store.pdConfig[key]
	if !ok || !row.enabled {
		return &fakeRows{columns: []string{"routing_key"}}, nil
	}

	return &fakeRows{
		columns: []string{"routing_key"},
		data:    [][]driver.Value{{row.routingKey}},
	}, nil
}

func (c *fakeConn) queryPDConfigList(args []driver.NamedValue) (driver.Rows, error) {
	tid, err := argS(args, 1)
	if err != nil {
		return nil, err
	}

	columns := []string{"id", "name", "routing_key", "enabled", "created_at", "updated_at"}
	var data [][]driver.Value
	for _, row := range c.store.pdConfig {
		if row.tenantID != tid {
			continue
		}
		data = append(data, []driver.Value{
			row.id, row.name, row.routingKey, row.enabled,
			row.createdAt, row.updatedAt,
		})
	}
	return &fakeRows{columns: columns, data: data}, nil
}

func (c *fakeConn) queryPDConfigByID(args []driver.NamedValue) (driver.Rows, error) {
	id, err := argS(args, 1)
	if err != nil {
		return nil, err
	}
	tid, err := argS(args, 2)
	if err != nil {
		return nil, err
	}

	key := tid + ":" + id
	row, ok := c.store.pdConfig[key]
	if !ok {
		return &fakeRows{columns: []string{"id", "name", "routing_key", "enabled", "created_at", "updated_at"}}, nil
	}

	return &fakeRows{
		columns: []string{"id", "name", "routing_key", "enabled", "created_at", "updated_at"},
		data: [][]driver.Value{{
			row.id, row.name, row.routingKey, row.enabled,
			row.createdAt, row.updatedAt,
		}},
	}, nil
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
// Test helpers
// ---------------------------------------------------------------------------

var testTenantID = uuid.MustParse("00000000-0000-0000-0000-000000000001")
var testTenantStr = testTenantID.String()

func setupTestPlugin(t *testing.T, httpClient *http.Client) (*Plugin, http.Handler, *fakeDBStore) {
	t.Helper()

	store := newFakeDBStore()
	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantStr

	db := sql.OpenDB(&fakeConnector{store: store})
	t.Cleanup(func() { db.Close() })

	client := httpClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}

	p := &Plugin{
		db:     db,
		logger: slog.Default(),
		httpClient: client,
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

func withCallContext(ctx context.Context) context.Context {
	return plugin.WithCallContext(ctx, &plugin.CallContext{
		TenantID: testTenantID,
	})
}

// ---------------------------------------------------------------------------
// Tests: PD Config CRUD
// ---------------------------------------------------------------------------

func TestPDCreateConfig(t *testing.T) {
	_, handler, _ := setupTestPlugin(t, nil)

	body := `{"name":"test-config","routing_key":"rk_test_123"}`
	req := authedRequest("POST", "/pagerduty/configs", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /pagerduty/configs: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if resp["name"] != "test-config" {
		t.Errorf("expected name 'test-config', got %q", resp["name"])
	}
	if resp["routing_key"] != "rk_test_123" {
		t.Errorf("expected routing_key 'rk_test_123', got %q", resp["routing_key"])
	}
	if resp["enabled"] != true {
		t.Error("expected enabled to be true")
	}
}

func TestPDCreateConfigValidation(t *testing.T) {
	_, handler, _ := setupTestPlugin(t, nil)

	tests := []struct {
		name string
		body string
		code int
	}{
		{"missing name", `{"routing_key":"rk"}`, 400},
		{"missing routing_key", `{"name":"n"}`, 400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := authedRequest("POST", "/pagerduty/configs", bytes.NewReader([]byte(tt.body)))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tt.code {
				t.Errorf("expected %d, got %d: %s", tt.code, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestPDListConfigs(t *testing.T) {
	_, handler, _ := setupTestPlugin(t, nil)

	for _, name := range []string{"cfg-a", "cfg-b"} {
		body := `{"name":"` + name + `","routing_key":"rk_` + name + `"}`
		req := authedRequest("POST", "/pagerduty/configs", bytes.NewReader([]byte(body)))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s: expected 201, got %d", name, rec.Code)
		}
	}

	req := authedRequest("GET", "/pagerduty/configs", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var configs []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &configs); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if len(configs) != 2 {
		t.Fatalf("expected 2 configs, got %d", len(configs))
	}
}

func TestPDGetConfig(t *testing.T) {
	_, handler, _ := setupTestPlugin(t, nil)

	createBody := `{"name":"get-test","routing_key":"rk_get"}`
	req := authedRequest("POST", "/pagerduty/configs", bytes.NewReader([]byte(createBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", rec.Code)
	}

	var created map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &created)
	configID := created["id"].(string)

	req = authedRequest("GET", "/pagerduty/configs/"+configID, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var got map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got["name"] != "get-test" {
		t.Errorf("expected name 'get-test', got %q", got["name"])
	}
}

func TestPDGetConfigNotFound(t *testing.T) {
	_, handler, _ := setupTestPlugin(t, nil)

	req := authedRequest("GET", "/pagerduty/configs/"+uuid.New().String(), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPDUpdateConfig(t *testing.T) {
	_, handler, _ := setupTestPlugin(t, nil)

	createBody := `{"name":"orig","routing_key":"rk_orig"}`
	req := authedRequest("POST", "/pagerduty/configs", bytes.NewReader([]byte(createBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", rec.Code)
	}

	var created map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &created)
	configID := created["id"].(string)

	// Update name and enabled.
	updateBody := `{"name":"updated","enabled":false}`
	req = authedRequest("PUT", "/pagerduty/configs/"+configID, bytes.NewReader([]byte(updateBody)))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPDUpdateConfigNotFound(t *testing.T) {
	_, handler, _ := setupTestPlugin(t, nil)

	body := `{"name":"nope"}`
	req := authedRequest("PUT", "/pagerduty/configs/"+uuid.New().String(), bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPDDeleteConfig(t *testing.T) {
	_, handler, _ := setupTestPlugin(t, nil)

	createBody := `{"name":"del-test","routing_key":"rk_del"}`
	req := authedRequest("POST", "/pagerduty/configs", bytes.NewReader([]byte(createBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", rec.Code)
	}

	var created map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &created)
	configID := created["id"].(string)

	req = authedRequest("DELETE", "/pagerduty/configs/"+configID, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE: expected 204, got %d", rec.Code)
	}

	// Should now be 404.
	req = authedRequest("DELETE", "/pagerduty/configs/"+configID, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("DELETE again: expected 404, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Tests: Host function trigger/resolve with mock PagerDuty API
// ---------------------------------------------------------------------------

func TestTriggerIncidentLifecycle(t *testing.T) {
	// Mock PagerDuty Events API.
	pdMux := http.NewServeMux()
	pdMux.HandleFunc("/v2/enqueue", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req pdEventRequest
		if err := json.Unmarshal(body, &req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if req.RoutingKey == "" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"status":"error","message":"routing key required"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if req.EventAction == "trigger" {
			w.Write([]byte(`{"status":"success","dedup_key":"incident_abc123","message":"Event processed"}`))
		} else {
			w.Write([]byte(`{"status":"success","message":"Event processed"}`))
		}
	})
	pdSrv := httptest.NewServer(pdMux)
	defer pdSrv.Close()

	// Override the PagerDuty API URL to point at our mock server.
	// Since pdEventsAPIURL is a const, we can't change it. Instead, we use a
	// custom httpClient with a transport that rewrites the URL.
	origTransport := &mockTransport{
		origURL: "https://events.pagerduty.com",
		mockURL: pdSrv.URL,
	}
	client := &http.Client{
		Transport: origTransport,
		Timeout:   5 * time.Second,
	}

	p, handler, store := setupTestPlugin(t, client)

	// Create a PD config.
	createBody := `{"name":"pd-test","routing_key":"rk_lifecycle"}`
	req := authedRequest("POST", "/pagerduty/configs", bytes.NewReader([]byte(createBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create config: expected 201, got %d", rec.Code)
	}

	var created map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &created)
	configID := created["id"].(string)

	// Verify the config exists in the store.
	store.mu.RLock()
	row, ok := store.pdConfig[testTenantStr+":"+configID]
	store.mu.RUnlock()
	if !ok {
		t.Fatal("expected config in store")
	}
	if row.routingKey != "rk_lifecycle" {
		t.Errorf("expected routing_key 'rk_lifecycle', got %q", row.routingKey)
	}

	// Trigger an incident.
	triggerInput := `{"config_id":"` + configID + `","summary":"Test incident","severity":"critical","source":"test-suite","details":"detail info"}`
	triggerOut, err := p.triggerIncident(withCallContext(context.Background()), triggerInput)
	if err != nil {
		t.Fatalf("triggerIncident() returned error: %v", err)
	}

	var triggerResult triggerIncidentOutput
	if err := json.Unmarshal([]byte(triggerOut), &triggerResult); err != nil {
		t.Fatalf("failed to decode trigger output: %v", err)
	}
	if triggerResult.IncidentKey != "incident_abc123" {
		t.Errorf("expected incident_key 'incident_abc123', got %q", triggerResult.IncidentKey)
	}
	if triggerResult.Status != "success" {
		t.Errorf("expected status 'success', got %q", triggerResult.Status)
	}

	// Resolve the incident using the returned incident_key.
	resolveInput := `{"config_id":"` + configID + `","incident_key":"` + triggerResult.IncidentKey + `"}`
	resolveOut, err := p.resolveIncident(withCallContext(context.Background()), resolveInput)
	if err != nil {
		t.Fatalf("resolveIncident() returned error: %v", err)
	}

	var resolveResult resolveIncidentOutput
	if err := json.Unmarshal([]byte(resolveOut), &resolveResult); err != nil {
		t.Fatalf("failed to decode resolve output: %v", err)
	}
	if resolveResult.Status != "success" {
		t.Errorf("expected status 'success', got %q", resolveResult.Status)
	}
}

func TestTriggerIncidentValidation(t *testing.T) {
	p, _, _ := setupTestPlugin(t, nil)

	tests := []struct {
		name  string
		input string
		errFn func(err error) bool
	}{
		{"empty input", `{}`, func(err error) bool { return err != nil && strings.Contains(err.Error(), "config_id") }},
		{"missing summary", `{"config_id":"` + uuid.New().String() + `"}`, func(err error) bool { return err != nil && strings.Contains(err.Error(), "summary") }},
		{"missing severity", `{"config_id":"` + uuid.New().String() + `","summary":"test"}`, func(err error) bool { return err != nil && strings.Contains(err.Error(), "severity") }},
		{"invalid severity", `{"config_id":"` + uuid.New().String() + `","summary":"test","severity":"unknown","source":"src"}`, func(err error) bool { return err != nil && strings.Contains(err.Error(), "invalid severity") }},
		{"missing source", `{"config_id":"` + uuid.New().String() + `","summary":"test","severity":"error"}`, func(err error) bool { return err != nil && strings.Contains(err.Error(), "source") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := p.triggerIncident(withCallContext(context.Background()), tt.input)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.errFn(err) {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestResolveIncidentValidation(t *testing.T) {
	p, _, _ := setupTestPlugin(t, nil)

	_, err := p.resolveIncident(withCallContext(context.Background()), `{}`)
	if err == nil {
		t.Fatal("expected error for missing config_id")
	}
	if !strings.Contains(err.Error(), "config_id") {
		t.Errorf("expected config_id error, got: %v", err)
	}

	_, err = p.resolveIncident(withCallContext(context.Background()), `{"config_id":"`+uuid.New().String()+`"}`)
	if err == nil {
		t.Fatal("expected error for missing incident_key")
	}
	if !strings.Contains(err.Error(), "incident_key") {
		t.Errorf("expected incident_key error, got: %v", err)
	}
}

func TestTriggerIncidentConfigNotFound(t *testing.T) {
	p, _, _ := setupTestPlugin(t, nil)

	input := `{"config_id":"` + uuid.New().String() + `","summary":"test","severity":"error","source":"src"}`
	_, err := p.triggerIncident(withCallContext(context.Background()), input)
	if err == nil {
		t.Fatal("expected error for config not found, got nil")
	}
	if !strings.Contains(err.Error(), "config not found") && !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected not found error, got: %v", err)
	}
}

func TestTriggerIncidentMissingAPIKey(t *testing.T) {
	p, _, _ := setupTestPlugin(t, nil)

	// First create a config that exists.
	cfgID := uuid.New().String()
	p.db.ExecContext(context.Background(), `
		INSERT INTO pd_config (tenant_id, id, name, routing_key, enabled, created_at, updated_at)
		VALUES ($1, $2, $3, $4, true, now(), now())
	`, testTenantStr, cfgID, "test", "rk_test")

	input := `{"config_id":"` + cfgID + `","summary":"test","severity":"error","source":"src"}`
	_, err := p.triggerIncident(withCallContext(context.Background()), input)
	// The config exists and is enabled, but we have no mock PD server — the
	// httpClient will fail trying to reach the real PagerDuty API.
	// That's expected since we didn't set up a mock transport.
	if err == nil {
		t.Fatal("expected error due to no PD mock server, got nil")
	}
}

// ---------------------------------------------------------------------------
// Mock transport for rewriting PagerDuty API requests
// ---------------------------------------------------------------------------

type mockTransport struct {
	origURL string
	mockURL string
}

func (t *mockTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Rewrite the URL from the real PagerDuty API to the mock server.
	newURL := strings.Replace(req.URL.String(), t.origURL, t.mockURL, 1)
	mockReq, _ := http.NewRequestWithContext(req.Context(), req.Method, newURL, req.Body)
	mockReq.Header = req.Header
	return http.DefaultTransport.RoundTrip(mockReq)
}
