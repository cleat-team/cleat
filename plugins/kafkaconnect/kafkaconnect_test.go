package kafkaconnect

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
	"github.com/rcownie/cleat/internal/host"
	"github.com/rcownie/cleat/internal/plugin"
)

func TestInfo(t *testing.T) {
	p := &Plugin{}
	info := p.Info()
	if info.Name != "kafka-connect" {
		t.Errorf("expected Name 'kafka-connect', got %q", info.Name)
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

func TestInitWithRestProxyConfig(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		Config: []byte(`{"rest_proxy_url": "http://localhost:8082"}`),
	}
	err := p.Init(context.Background(), env)
	if err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if p.config.RestProxyURL != "http://localhost:8082" {
		t.Errorf("expected RestProxyURL 'http://localhost:8082', got %q", p.config.RestProxyURL)
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
	env := &plugin.Environment{}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}

	mux := http.NewServeMux()
	err := p.RegisterRoutes(mux)
	if err != nil {
		t.Fatalf("RegisterRoutes() returned error: %v", err)
	}

	// Verify routes are registered by making requests.
	tests := []struct {
		method string
		path   string
	}{
		{"POST", "/kafka/configs"},
		{"GET", "/kafka/configs"},
		{"DELETE", "/kafka/configs/11111111-1111-1111-1111-111111111111"},
	}
	for _, tt := range tests {
		req := httptest.NewRequest(tt.method, tt.path, nil)
		_, pattern := mux.Handler(req)
		if pattern == "" {
			t.Errorf("no handler matched %s %s", tt.method, tt.path)
		}
	}
}

func TestRegisterRoutesNilMux(t *testing.T) {
	p := &Plugin{}
	err := p.RegisterRoutes(nil)
	if err == nil {
		t.Fatal("expected error for nil mux, got nil")
	}
}

func TestRegisterHostFunctions(t *testing.T) {
	p := &Plugin{}
	scope := &mockRegistry{}
	err := p.RegisterHostFunctions(scope)
	if err != nil {
		t.Fatalf("RegisterHostFunctions() returned error: %v", err)
	}
	if scope.name != "produce" {
		t.Errorf("expected function name 'produce', got %q", scope.name)
	}
	if scope.fn == nil {
		t.Error("expected function to be registered")
	}
}

func TestRegisterHostFunctionsNilScope(t *testing.T) {
	p := &Plugin{}
	err := p.RegisterHostFunctions(nil)
	if err == nil {
		t.Fatal("expected error for nil scope, got nil")
	}
}

func TestProduceValidateInput(t *testing.T) {
	p := &Plugin{}
	_, err := p.produce(context.Background(), `{}`)
	if err == nil {
		t.Fatal("expected error for empty config_id, got nil")
	}
}

func TestMigrations(t *testing.T) {
	p := &Plugin{}
	migrations := p.Migrations()
	if len(migrations) != 2 {
		t.Fatalf("expected 2 migrations, got %d", len(migrations))
	}
	if migrations[0].Version != 1 {
		t.Errorf("expected Version 1, got %d", migrations[0].Version)
	}
	if migrations[0].Up == "" {
		t.Error("expected non-empty Up SQL for migration 1")
	}
	if migrations[0].Down == "" {
		t.Error("expected non-empty Down SQL for migration 1")
	}
	if migrations[1].Version != 2 {
		t.Errorf("expected Version 2, got %d", migrations[1].Version)
	}
	if migrations[1].Up == "" {
		t.Error("expected non-empty Up SQL for migration 2")
	}
	if migrations[1].Down == "" {
		t.Error("expected non-empty Down SQL for migration 2")
	}
}

// mockRegistry implements plugin.FuncRegistry for testing.
type mockRegistry struct {
	name string
	fn   plugin.PluginFunc
}

func (m *mockRegistry) Register(opts plugin.FuncOptions, fn plugin.PluginFunc) error {
	m.name = opts.Name
	m.fn = fn
	return nil
}

// ---------------------------------------------------------------------------
// Fake Kafka config store for HTTP handler tests
// ---------------------------------------------------------------------------

type fakeKafkaConfig struct {
	id            uuid.UUID
	tenantID      uuid.UUID
	name          string
	brokers       string
	topic         string
	consumerGroup string
	eventType     string
	enabled       bool
	createdAt     time.Time
	updatedAt     time.Time
}

type fakeKafkaStore struct {
	mu      sync.Mutex
	configs []*fakeKafkaConfig
	apiKeys map[string]string // key_hash_hex -> tenant_id string
}

func newFakeKafkaStore() *fakeKafkaStore {
	return &fakeKafkaStore{apiKeys: make(map[string]string)}
}

type fakeKafkaConnector struct {
	store *fakeKafkaStore
}

func (c *fakeKafkaConnector) Connect(_ context.Context) (driver.Conn, error) {
	return &fakeKafkaConn{store: c.store}, nil
}

func (c *fakeKafkaConnector) Driver() driver.Driver {
	return &fakeKafkaDriver{}
}

type fakeKafkaDriver struct{}

func (*fakeKafkaDriver) Open(_ string) (driver.Conn, error) {
	return nil, fmt.Errorf("fakeKafkaDriver: use sql.OpenDB")
}

type fakeKafkaConn struct {
	store *fakeKafkaStore
}

func (*fakeKafkaConn) Prepare(_ string) (driver.Stmt, error) {
	return nil, fmt.Errorf("fakeKafkaConn: unexpected Prepare call")
}

func (*fakeKafkaConn) Close() error    { return nil }
func (*fakeKafkaConn) Begin() (driver.Tx, error) { return &fakeKafkaTx{}, nil }

type fakeKafkaTx struct{}

func (*fakeKafkaTx) Commit() error   { return nil }
func (*fakeKafkaTx) Rollback() error { return nil }

func (c *fakeKafkaConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.store.mu.Lock()
	defer c.store.mu.Unlock()

	if strings.Contains(query, "INSERT INTO kafka_config") {
		return c.execInsert(args)
	}
	if strings.Contains(query, "DELETE FROM kafka_config") {
		return c.execDelete(args)
	}
	return nil, fmt.Errorf("fakeKafkaConn: unexpected Exec query: %s", query)
}

func (c *fakeKafkaConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.store.mu.Lock()
	defer c.store.mu.Unlock()

	if strings.Contains(query, "tenant_api_keys") {
		return c.queryTenantLookup(args)
	}
	if strings.Contains(query, "FROM kafka_config") {
		return c.queryList(args)
	}
	return nil, fmt.Errorf("fakeKafkaConn: unexpected Query query: %s", query)
}

func argKafkaString(args []driver.NamedValue, ordinal int) (string, error) {
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

func argKafkaTime(args []driver.NamedValue, ordinal int) (time.Time, error) {
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

func (c *fakeKafkaConn) execInsert(args []driver.NamedValue) (driver.Result, error) {
	tidStr, err := argKafkaString(args, 1)
	if err != nil {
		return nil, err
	}
	tid, _ := uuid.Parse(tidStr)
	idStr, err := argKafkaString(args, 2)
	if err != nil {
		return nil, err
	}
	id, _ := uuid.Parse(idStr)
	name, _ := argKafkaString(args, 3)
	brokers, _ := argKafkaString(args, 4)
	topic, _ := argKafkaString(args, 5)
	consumerGroup, _ := argKafkaString(args, 6)
	eventType, _ := argKafkaString(args, 7)
	now, _ := argKafkaTime(args, 8)

	cfg := &fakeKafkaConfig{
		id:            id,
		tenantID:      tid,
		name:          name,
		brokers:       brokers,
		topic:         topic,
		consumerGroup: consumerGroup,
		eventType:     eventType,
		enabled:       true,
		createdAt:     now,
		updatedAt:     now,
	}
	c.store.configs = append(c.store.configs, cfg)
	return &fakeKafkaResult{rowsAffected: 1}, nil
}

func (c *fakeKafkaConn) execDelete(args []driver.NamedValue) (driver.Result, error) {
	idStr, err := argKafkaString(args, 1)
	if err != nil {
		return nil, err
	}
	id, _ := uuid.Parse(idStr)
	tidStr, err := argKafkaString(args, 2)
	if err != nil {
		return nil, err
	}
	tid, _ := uuid.Parse(tidStr)

	for i, cfg := range c.store.configs {
		if cfg.id == id && cfg.tenantID == tid {
			c.store.configs = append(c.store.configs[:i], c.store.configs[i+1:]...)
			return &fakeKafkaResult{rowsAffected: 1}, nil
		}
	}
	return &fakeKafkaResult{rowsAffected: 0}, nil
}

func (c *fakeKafkaConn) queryTenantLookup(args []driver.NamedValue) (driver.Rows, error) {
	keyHash, err := argKafkaBytes(args, 1)
	if err != nil {
		return nil, err
	}
	hashHex := fmt.Sprintf("%x", keyHash)
	tid, ok := c.store.apiKeys[hashHex]
	if !ok {
		return &fakeKafkaRows{cols: []string{"tenant_id"}}, nil
	}
	return &fakeKafkaRows{
		cols: []string{"tenant_id"},
		data: [][]driver.Value{{tid}},
	}, nil
}

func (c *fakeKafkaConn) queryList(args []driver.NamedValue) (driver.Rows, error) {
	tidStr, err := argKafkaString(args, 1)
	if err != nil {
		return nil, err
	}
	tid, _ := uuid.Parse(tidStr)

	cols := []string{"id", "name", "brokers", "topic", "consumer_group", "event_type", "enabled", "created_at", "updated_at"}
	var data [][]driver.Value
	for _, cfg := range c.store.configs {
		if cfg.tenantID == tid {
			data = append(data, []driver.Value{
				cfg.id.String(),
				cfg.name,
				cfg.brokers,
				cfg.topic,
				cfg.consumerGroup,
				cfg.eventType,
				cfg.enabled,
				cfg.createdAt,
				cfg.updatedAt,
			})
		}
	}
	return &fakeKafkaRows{cols: cols, data: data}, nil
}

func argKafkaBytes(args []driver.NamedValue, ordinal int) ([]byte, error) {
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

type fakeKafkaResult struct {
	rowsAffected int64
}

func (r *fakeKafkaResult) LastInsertId() (int64, error) { return 0, nil }
func (r *fakeKafkaResult) RowsAffected() (int64, error) { return r.rowsAffected, nil }

type fakeKafkaRows struct {
	cols []string
	data [][]driver.Value
	pos  int
}

func (r *fakeKafkaRows) Columns() []string { return r.cols }
func (r *fakeKafkaRows) Close() error      { return nil }
func (r *fakeKafkaRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.pos])
	r.pos++
	return nil
}

// ---- Test setup for HTTP handler tests ----

func setupKafkaHandler(t *testing.T) (*Plugin, http.Handler, *fakeKafkaStore) {
	t.Helper()

	store := newFakeKafkaStore()
	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantID.String()

	db := sql.OpenDB(&fakeKafkaConnector{store: store})
	t.Cleanup(func() { db.Close() })

	p := &Plugin{
		db:     &host.SQLDBAdapter{DB: db},
		logger: slog.Default(),
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
		config: Config{},
	}

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	handler := auth.Middleware(host.NewPostgresStore(db), false)(mux)
	return p, handler, store
}

// ---- HTTP handler tests ----

func TestHandleCreateConfig(t *testing.T) {
	_, handler, _ := setupKafkaHandler(t)

	body := `{"name":"test-config","brokers":"localhost:9092","topic":"test-topic"}`
	req := authedRequest("POST", "/kafka/configs", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if resp["name"] != "test-config" {
		t.Errorf("expected name 'test-config', got %q", resp["name"])
	}
	if resp["brokers"] != "localhost:9092" {
		t.Errorf("expected brokers 'localhost:9092', got %q", resp["brokers"])
	}
	if resp["topic"] != "test-topic" {
		t.Errorf("expected topic 'test-topic', got %q", resp["topic"])
	}
}

func TestHandleCreateConfigValidation(t *testing.T) {
	_, handler, _ := setupKafkaHandler(t)

	// Missing name.
	body := `{"brokers":"localhost:9092","topic":"test-topic"}`
	req := authedRequest("POST", "/kafka/configs", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing name, got %d: %s", rec.Code, rec.Body.String())
	}

	// Missing brokers.
	body = `{"name":"test","topic":"test-topic"}`
	req = authedRequest("POST", "/kafka/configs", bytes.NewReader([]byte(body)))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing brokers, got %d: %s", rec.Code, rec.Body.String())
	}

	// Missing topic.
	body = `{"name":"test","brokers":"localhost:9092"}`
	req = authedRequest("POST", "/kafka/configs", bytes.NewReader([]byte(body)))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing topic, got %d: %s", rec.Code, rec.Body.String())
	}

	// Invalid JSON.
	body = `not json`
	req = authedRequest("POST", "/kafka/configs", bytes.NewReader([]byte(body)))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid JSON, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleCreateConfigDefaults(t *testing.T) {
	_, handler, _ := setupKafkaHandler(t)

	// Without consumer_group and event_type, defaults should apply.
	body := `{"name":"defaults-test","brokers":"localhost:9092","topic":"my-topic"}`
	req := authedRequest("POST", "/kafka/configs", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["consumer_group"] != "cleat-consumer" {
		t.Errorf("expected default consumer_group 'cleat-consumer', got %q", resp["consumer_group"])
	}
	if resp["event_type"] != "my-topic" {
		t.Errorf("expected default event_type 'my-topic', got %q", resp["event_type"])
	}
}

func TestHandleListConfigs(t *testing.T) {
	_, handler, _ := setupKafkaHandler(t)

	// Create two configs.
	for _, name := range []string{"config-a", "config-b"} {
		body := fmt.Sprintf(`{"name":%q,"brokers":"localhost:9092","topic":"topic-%s"}`, name, name)
		req := authedRequest("POST", "/kafka/configs", bytes.NewReader([]byte(body)))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("expected 201 for %s, got %d", name, rec.Code)
		}
	}

	req := authedRequest("GET", "/kafka/configs", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var results []interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &results); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("expected 2 configs, got %d", len(results))
	}
}

func TestHandleDeleteConfig(t *testing.T) {
	_, handler, _ := setupKafkaHandler(t)

	// Create a config first.
	body := `{"name":"delete-me","brokers":"localhost:9092","topic":"del-topic"}`
	req := authedRequest("POST", "/kafka/configs", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	id := resp["id"].(string)

	// Delete it.
	req = authedRequest("DELETE", "/kafka/configs/"+id, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rec.Code)
	}
}

func TestHandleDeleteConfigNotFound(t *testing.T) {
	_, handler, _ := setupKafkaHandler(t)

	req := authedRequest("DELETE", "/kafka/configs/"+uuid.New().String(), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleDeleteConfigInvalidID(t *testing.T) {
	_, handler, _ := setupKafkaHandler(t)

	req := authedRequest("DELETE", "/kafka/configs/not-a-uuid", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}
