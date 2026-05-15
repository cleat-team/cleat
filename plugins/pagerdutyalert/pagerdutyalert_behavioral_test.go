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
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/cleat-team/cleat/internal/auth"
	"github.com/cleat-team/cleat/internal/host"
	"github.com/cleat-team/cleat/internal/plugin"
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
	failNextExec     bool                    // next ExecContext returns error
	failNextQuery    bool                    // next QueryContext returns error
	failNextRefetch  int32                   // atomic: next queryPDConfigByID returns error
	failNextScanOnList bool                  // next queryPDConfigList returns corrupt data
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

	if c.store.failNextExec {
		c.store.failNextExec = false
		return nil, fmt.Errorf("simulated exec error")
	}

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
	// Check fault flags under write lock before acquiring read lock for the query.
	c.store.mu.Lock()
	shouldFail := c.store.failNextQuery
	if shouldFail {
		c.store.failNextQuery = false
	}
	corruptList := c.store.failNextScanOnList
	if corruptList {
		c.store.failNextScanOnList = false
	}
	c.store.mu.Unlock()

	if shouldFail {
		return nil, fmt.Errorf("simulated query error")
	}

	c.store.mu.RLock()
	defer c.store.mu.RUnlock()

	q := strings.ReplaceAll(query, "\n", " ")
	switch {
	case strings.Contains(q, "SELECT tenant_id FROM tenant_api_keys"):
		return c.queryTenantByKeyHash(args)
	case strings.Contains(q, "SELECT routing_key FROM pd_config") || (strings.Contains(q, "routing_key") && strings.Contains(q, "FROM pd_config") && strings.Contains(q, "enabled = true")):
		return c.queryRoutingKey(args)
	case strings.Contains(q, "FROM pd_config") && strings.Contains(q, "ORDER BY"):
		return c.queryPDConfigList(args, corruptList)
	case strings.Contains(q, "FROM pd_config") && strings.Contains(q, "WHERE id ="):
		refetchFail := atomic.CompareAndSwapInt32(&c.store.failNextRefetch, 1, 0)
		return c.queryPDConfigByID(args, refetchFail)
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

func (c *fakeConn) queryPDConfigList(args []driver.NamedValue, corruptData bool) (driver.Rows, error) {
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
		if corruptData {
			data = append(data, []driver.Value{
				row.id, row.name, row.routingKey, "not-a-bool",
				row.createdAt, row.updatedAt,
			})
		} else {
			data = append(data, []driver.Value{
				row.id, row.name, row.routingKey, row.enabled,
				row.createdAt, row.updatedAt,
			})
		}
	}
	return &fakeRows{columns: columns, data: data}, nil
}

func (c *fakeConn) queryPDConfigByID(args []driver.NamedValue, refetchFail bool) (driver.Rows, error) {
	if refetchFail {
		return nil, fmt.Errorf("simulated re-fetch error")
	}

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
		db:     &host.SQLDBAdapter{DB: db},
		logger: slog.Default(),
		httpClient: client,
	}

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	handler := auth.Middleware(host.NewPostgresStore(db), false)(mux)
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
	p.db.Exec(context.Background(), `
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

// ---------------------------------------------------------------------------
// Fault-injection HTTP transport
// ---------------------------------------------------------------------------

type failTransport struct {
	err error
}

func (t *failTransport) RoundTrip(_ *http.Request) (*http.Response, error) {
	return nil, t.err
}

// ---------------------------------------------------------------------------
// Mock FuncRegistry that returns an error on Register
// ---------------------------------------------------------------------------

type failRegistry struct {
	err error
}

func (r *failRegistry) Register(_ plugin.FuncOptions, _ plugin.PluginFunc) error {
	return r.err
}

// ---------------------------------------------------------------------------
// Additional route handler error-path tests
// ---------------------------------------------------------------------------

func TestPDCreateConfig_InvalidJSON(t *testing.T) {
	_, handler, _ := setupTestPlugin(t, nil)

	body := `{invalid json}`
	req := authedRequest("POST", "/pagerduty/configs", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid JSON, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPDCreateConfig_DBError(t *testing.T) {
	_, handler, store := setupTestPlugin(t, nil)
	store.failNextExec = true

	body := `{"name":"test","routing_key":"rk_test"}`
	req := authedRequest("POST", "/pagerduty/configs", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 for DB error, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPDCreateConfig_NoTenant(t *testing.T) {
	p, _, _ := setupTestPlugin(t, nil)

	body := `{"name":"test","routing_key":"rk_test"}`
	req := httptest.NewRequest("POST", "/pagerduty/configs", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	p.handleCreateConfig(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for missing tenant, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPDGetConfig_InvalidID(t *testing.T) {
	_, handler, _ := setupTestPlugin(t, nil)

	req := authedRequest("GET", "/pagerduty/configs/not-a-uuid", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid ID, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPDGetConfig_DBError(t *testing.T) {
	p, _, store := setupTestPlugin(t, nil)
	store.failNextQuery = true

	// Go through the mux (for PathValue) but inject tenant context.
	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(auth.WithTenantID(r.Context(), testTenantID))
		mux.ServeHTTP(w, r)
	})

	req := httptest.NewRequest("GET", "/pagerduty/configs/"+uuid.New().String(), nil)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 for DB error, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPDGetConfig_NoTenant(t *testing.T) {
	p, _, _ := setupTestPlugin(t, nil)

	req := httptest.NewRequest("GET", "/pagerduty/configs/"+uuid.New().String(), nil)
	rec := httptest.NewRecorder()
	p.handleGetConfig(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for missing tenant, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPDUpdateConfig_NoFields(t *testing.T) {
	_, handler, _ := setupTestPlugin(t, nil)

	// Create a valid config first using the same handler chain.
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

	// Update with empty body — no fields to update.
	updateBody := `{}`
	req = authedRequest("PUT", "/pagerduty/configs/"+configID, bytes.NewReader([]byte(updateBody)))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for no fields, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPDUpdateConfig_InvalidJSON(t *testing.T) {
	_, handler, _ := setupTestPlugin(t, nil)

	updateBody := `not json`
	req := authedRequest("PUT", "/pagerduty/configs/"+uuid.New().String(), bytes.NewReader([]byte(updateBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid JSON, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPDUpdateConfig_DBError(t *testing.T) {
	_, handler, store := setupTestPlugin(t, nil)
	store.failNextExec = true

	updateBody := `{"name":"updated"}`
	req := authedRequest("PUT", "/pagerduty/configs/"+uuid.New().String(), bytes.NewReader([]byte(updateBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 for DB error, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPDUpdateConfig_NoTenant(t *testing.T) {
	p, _, _ := setupTestPlugin(t, nil)

	body := `{"name":"updated"}`
	req := httptest.NewRequest("PUT", "/pagerduty/configs/"+uuid.New().String(), bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	p.handleUpdateConfig(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for missing tenant, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPDDeleteConfig_InvalidID(t *testing.T) {
	_, handler, _ := setupTestPlugin(t, nil)

	req := authedRequest("DELETE", "/pagerduty/configs/not-a-uuid", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid ID, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPDDeleteConfig_DBError(t *testing.T) {
	_, handler, store := setupTestPlugin(t, nil)
	store.failNextExec = true

	req := authedRequest("DELETE", "/pagerduty/configs/"+uuid.New().String(), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 for DB error, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPDDeleteConfig_NoTenant(t *testing.T) {
	p, _, _ := setupTestPlugin(t, nil)

	req := httptest.NewRequest("DELETE", "/pagerduty/configs/"+uuid.New().String(), nil)
	rec := httptest.NewRecorder()
	p.handleDeleteConfig(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for missing tenant, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPDListConfigs_DBError(t *testing.T) {
	p, _, store := setupTestPlugin(t, nil)
	store.failNextQuery = true

	req := httptest.NewRequest("GET", "/pagerduty/configs", nil)
	req = req.WithContext(auth.WithTenantID(context.Background(), testTenantID))
	rec := httptest.NewRecorder()
	p.handleListConfigs(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 for DB error, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPDListConfigs_NoTenant(t *testing.T) {
	p, _, _ := setupTestPlugin(t, nil)

	req := httptest.NewRequest("GET", "/pagerduty/configs", nil)
	rec := httptest.NewRecorder()
	p.handleListConfigs(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for missing tenant, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPDUpdateConfig_NotFound(t *testing.T) {
	_, handler, _ := setupTestPlugin(t, nil)

	body := `{"name":"nope"}`
	req := authedRequest("PUT", "/pagerduty/configs/"+uuid.New().String(), bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for not found, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Additional host function error-path tests
// ---------------------------------------------------------------------------

func TestRegisterHostFunctions_RegistryError(t *testing.T) {
	p := &Plugin{}
	scope := &failRegistry{err: fmt.Errorf("register declined")}
	err := p.RegisterHostFunctions(scope)
	if err == nil {
		t.Fatal("expected error from registry")
	}
	if !strings.Contains(err.Error(), "register declined") {
		t.Errorf("expected 'register declined', got: %v", err)
	}
}

func TestTriggerIncident_NoCallContext(t *testing.T) {
	p, _, _ := setupTestPlugin(t, nil)

	// No call context in the background context — triggerIncident should reject.
	_, err := p.triggerIncident(context.Background(), `{}`)
	if err == nil {
		t.Fatal("expected error for missing call context")
	}
	if !strings.Contains(err.Error(), "no tenant context") {
		t.Errorf("expected tenant context error, got: %v", err)
	}
}

func TestResolveIncident_NoCallContext(t *testing.T) {
	p, _, _ := setupTestPlugin(t, nil)

	_, err := p.resolveIncident(context.Background(), `{}`)
	if err == nil {
		t.Fatal("expected error for missing call context")
	}
	if !strings.Contains(err.Error(), "no tenant context") {
		t.Errorf("expected tenant context error, got: %v", err)
	}
}

func TestTriggerIncident_InvalidJSON(t *testing.T) {
	p, _, _ := setupTestPlugin(t, nil)

	_, err := p.triggerIncident(withCallContext(context.Background()), `not json`)
	if err == nil {
		t.Fatal("expected error for invalid JSON input")
	}
	if !strings.Contains(err.Error(), "invalid input") {
		t.Errorf("expected 'invalid input' error, got: %v", err)
	}
}

func TestResolveIncident_InvalidJSON(t *testing.T) {
	p, _, _ := setupTestPlugin(t, nil)

	_, err := p.resolveIncident(withCallContext(context.Background()), `not json`)
	if err == nil {
		t.Fatal("expected error for invalid JSON input")
	}
	if !strings.Contains(err.Error(), "invalid input") {
		t.Errorf("expected 'invalid input' error, got: %v", err)
	}
}

func TestPostToPagerDuty_HTTPError(t *testing.T) {
	p, _, _ := setupTestPlugin(t, &http.Client{
		Transport: &failTransport{err: fmt.Errorf("connection refused")},
		Timeout:   5 * time.Second,
	})

	_, err := p.postToPagerDuty(context.Background(), pdEventRequest{
		RoutingKey:  "rk_test",
		EventAction: "trigger",
	})
	if err == nil {
		t.Fatal("expected error from failing HTTP transport")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("expected transport error, got: %v", err)
	}
}

func TestPostToPagerDuty_Non2xx(t *testing.T) {
	pdMux := http.NewServeMux()
	pdMux.HandleFunc("/v2/enqueue", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"status":"error","message":"server error"}`))
	})
	pdSrv := httptest.NewServer(pdMux)
	defer pdSrv.Close()

	origTransport := &mockTransport{
		origURL: "https://events.pagerduty.com",
		mockURL: pdSrv.URL,
	}
	client := &http.Client{Transport: origTransport, Timeout: 5 * time.Second}

	p, _, _ := setupTestPlugin(t, client)

	_, err := p.postToPagerDuty(context.Background(), pdEventRequest{
		RoutingKey:  "rk_test",
		EventAction: "trigger",
	})
	if err == nil {
		t.Fatal("expected error from non-2xx API response")
	}
	if !strings.Contains(err.Error(), "500") && !strings.Contains(err.Error(), "API returned") {
		t.Errorf("expected API error, got: %v", err)
	}
}

func TestPostToPagerDuty_ParseError(t *testing.T) {
	pdMux := http.NewServeMux()
	pdMux.HandleFunc("/v2/enqueue", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`not valid json at all`))
	})
	pdSrv := httptest.NewServer(pdMux)
	defer pdSrv.Close()

	origTransport := &mockTransport{
		origURL: "https://events.pagerduty.com",
		mockURL: pdSrv.URL,
	}
	client := &http.Client{Transport: origTransport, Timeout: 5 * time.Second}

	p, _, _ := setupTestPlugin(t, client)

	_, err := p.postToPagerDuty(context.Background(), pdEventRequest{
		RoutingKey:  "rk_test",
		EventAction: "trigger",
	})
	if err == nil {
		t.Fatal("expected error from invalid response JSON")
	}
	if !strings.Contains(err.Error(), "parse response") {
		t.Errorf("expected 'parse response' error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// TriggerIncident config disabled test
// ---------------------------------------------------------------------------

func TestTriggerIncident_DisabledConfig(t *testing.T) {
	p, _, store := setupTestPlugin(t, nil)

	// Create a config then disable it.
	cfgID := uuid.New().String()
	store.mu.Lock()
	store.pdConfig[testTenantStr+":"+cfgID] = &fakePDConfigRow{
		tenantID:   testTenantStr,
		id:         cfgID,
		name:       "disabled",
		routingKey: "rk_disabled",
		enabled:    false,
		createdAt:  time.Now(),
		updatedAt:  time.Now(),
	}
	store.mu.Unlock()

	input := `{"config_id":"` + cfgID + `","summary":"test","severity":"error","source":"src"}`
	_, err := p.triggerIncident(withCallContext(context.Background()), input)
	if err == nil {
		t.Fatal("expected error for disabled config")
	}
	if !strings.Contains(err.Error(), "config not found or disabled") {
		t.Errorf("expected 'disabled' error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// TriggerIncident with structured details
// ---------------------------------------------------------------------------

func TestTriggerIncident_WithStructuredDetails(t *testing.T) {
	pdMux := http.NewServeMux()
	pdMux.HandleFunc("/v2/enqueue", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req pdEventRequest
		if err := json.Unmarshal(body, &req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if req.Payload == nil || req.Payload.CustomDetails == nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"status":"error","message":"missing details"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"success","dedup_key":"details_abc"}`))
	})
	pdSrv := httptest.NewServer(pdMux)
	defer pdSrv.Close()

	origTransport := &mockTransport{
		origURL: "https://events.pagerduty.com",
		mockURL: pdSrv.URL,
	}
	client := &http.Client{Transport: origTransport, Timeout: 5 * time.Second}

	p, handler, _ := setupTestPlugin(t, client)

	createBody := `{"name":"details-test","routing_key":"rk_details"}`
	req := authedRequest("POST", "/pagerduty/configs", bytes.NewReader([]byte(createBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create config: expected 201, got %d", rec.Code)
	}
	var created map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &created)
	configID := created["id"].(string)

	// Trigger with JSON details that should be parsed as structured data.
	input := `{"config_id":"` + configID + `","summary":"details test","severity":"error","source":"test","details":"{\"key\":\"value\",\"count\":42}"}`
	out, err := p.triggerIncident(withCallContext(context.Background()), input)
	if err != nil {
		t.Fatalf("triggerIncident returned error: %v", err)
	}
	var result triggerIncidentOutput
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if result.Status != "success" {
		t.Errorf("expected success, got %q", result.Status)
	}
	if result.IncidentKey != "details_abc" {
		t.Errorf("expected dedup_key 'details_abc', got %q", result.IncidentKey)
	}
}

// ---------------------------------------------------------------------------
// ResolveIncident config not found
// ---------------------------------------------------------------------------

func TestResolveIncident_ConfigNotFound(t *testing.T) {
	p, _, _ := setupTestPlugin(t, nil)

	input := `{"config_id":"` + uuid.New().String() + `","incident_key":"inc_123"}`
	_, err := p.resolveIncident(withCallContext(context.Background()), input)
	if err == nil {
		t.Fatal("expected error for config not found")
	}
	if !strings.Contains(err.Error(), "config not found") && !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Additional edge-case tests for remaining uncovered code paths
// ---------------------------------------------------------------------------

// errReadCloser simulates a body read error for testing error paths.
type errReadCloser struct{}

func (*errReadCloser) Read(_ []byte) (int, error) { return 0, fmt.Errorf("simulated read error") }
func (*errReadCloser) Close() error                { return nil }

func TestPDCreateConfig_BodyReadError(t *testing.T) {
	_, handler, _ := setupTestPlugin(t, nil)

	req := authedRequest("POST", "/pagerduty/configs", &errReadCloser{})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 for body read error, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "read body") {
		t.Errorf("expected 'read body' error, got: %s", rec.Body.String())
	}
}

func TestPDListConfigs_Empty(t *testing.T) {
	_, handler, _ := setupTestPlugin(t, nil)

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
	if len(configs) != 0 {
		t.Errorf("expected 0 configs, got %d", len(configs))
	}
}

func TestPDUpdateConfig_InvalidID(t *testing.T) {
	_, handler, _ := setupTestPlugin(t, nil)

	body := `{"name":"updated"}`
	req := authedRequest("PUT", "/pagerduty/configs/not-a-uuid", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid ID, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPDUpdateConfig_WithRoutingKey(t *testing.T) {
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

	updateBody := `{"routing_key":"rk_updated"}`
	req = authedRequest("PUT", "/pagerduty/configs/"+configID, bytes.NewReader([]byte(updateBody)))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT with routing_key: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Fault injection for re-fetch error after update
// ---------------------------------------------------------------------------

func (s *fakeDBStore) triggerRefetchError() {
	atomic.StoreInt32(&s.failNextRefetch, 1)
}


func TestPDUpdateConfig_RefetchError(t *testing.T) {
	_, handler, store := setupTestPlugin(t, nil)

	createBody := `{"name":"refetch-test","routing_key":"rk_refetch"}`
	req := authedRequest("POST", "/pagerduty/configs", bytes.NewReader([]byte(createBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", rec.Code)
	}
	var created map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &created)
	configID := created["id"].(string)

	store.triggerRefetchError()

	updateBody := `{"name":"refetch-updated"}`
	req = authedRequest("PUT", "/pagerduty/configs/"+configID, bytes.NewReader([]byte(updateBody)))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 for re-fetch error, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "retrieve updated config") {
		t.Errorf("expected 'retrieve updated config' error, got: %s", rec.Body.String())
	}
}



func (s *fakeDBStore) triggerListScanError() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNextScanOnList = true
}

func TestPDListConfigs_ScanError(t *testing.T) {
	_, handler, store := setupTestPlugin(t, nil)

	createBody := `{"name":"scan-test","routing_key":"rk_scan"}`
	req := authedRequest("POST", "/pagerduty/configs", bytes.NewReader([]byte(createBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", rec.Code)
	}
	var created map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &created)
	configID := created["id"].(string)

	createBody2 := `{"name":"scan-test-2","routing_key":"rk_scan2"}`
	req2 := authedRequest("POST", "/pagerduty/configs", bytes.NewReader([]byte(createBody2)))
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusCreated {
		t.Fatalf("create 2: expected 201, got %d", rec2.Code)
	}
	_ = configID

	store.triggerListScanError()

	req = authedRequest("GET", "/pagerduty/configs", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 even with corrupt row, got %d: %s", rec.Code, rec.Body.String())
	}
}
