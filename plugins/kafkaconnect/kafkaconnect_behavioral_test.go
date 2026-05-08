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
	"github.com/rcownie/cleat/internal/plugin"
)

// ---------------------------------------------------------------------------
// In-memory fake DB store
// ---------------------------------------------------------------------------

type fakeKafkaConfigRow struct {
	tenantID      string
	id            string
	name          string
	brokers       string
	topic         string
	consumerGroup string
	eventType     string
	enabled       bool
	createdAt     time.Time
	updatedAt     time.Time
}

type fakeDBStore struct {
	mu        sync.RWMutex
	apiKeys   map[string]string                     // key_hash -> tenant_id
	kafkaCfgs map[string]*fakeKafkaConfigRow        // "tenant:id" -> row
	now       func() time.Time
}

func newFakeDBStore() *fakeDBStore {
	return &fakeDBStore{
		apiKeys:   make(map[string]string),
		kafkaCfgs: make(map[string]*fakeKafkaConfigRow),
		now:       time.Now,
	}
}

type fakeConnector struct {
	store *fakeDBStore
}

func (c *fakeConnector) Connect(_ context.Context) (driver.Conn, error) {
	return &fakeConn{store: c.store}, nil
}

func (c *fakeConnector) Driver() driver.Driver {
	return &fakeDrv{}
}

type fakeDrv struct{}

func (*fakeDrv) Open(_ string) (driver.Conn, error) {
	return nil, fmt.Errorf("fakeDriver: Open not supported; use sql.OpenDB")
}

type fakeConn struct {
	store *fakeDBStore
}

func (*fakeConn) Prepare(_ string) (driver.Stmt, error) {
	return nil, fmt.Errorf("fakeConn: unexpected Prepare call")
}

func (*fakeConn) Close() error { return nil }
func (*fakeConn) Begin() (driver.Tx, error) { return &fakeTx{}, nil }

type fakeTx struct{}

func (*fakeTx) Commit() error   { return nil }
func (*fakeTx) Rollback() error { return nil }

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
			v, ok := a.Value.(int64)
			if !ok {
				return 0, fmt.Errorf("arg %d: want int64, got %T", ordinal, a.Value)
			}
			return v, nil
		}
	}
	return 0, fmt.Errorf("arg %d not found", ordinal)
}

func argBool(args []driver.NamedValue, ordinal int) (bool, error) {
	for _, a := range args {
		if a.Ordinal == ordinal {
			v, ok := a.Value.(bool)
			if !ok {
				return false, fmt.Errorf("arg %d: want bool, got %T", ordinal, a.Value)
			}
			return v, nil
		}
	}
	return false, fmt.Errorf("arg %d not found", ordinal)
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
// ExecContext implementation
// ---------------------------------------------------------------------------

func (c *fakeConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.store.mu.Lock()
	defer c.store.mu.Unlock()

	switch {
	case strings.Contains(query, "INSERT INTO kafka_config"):
		return c.execInsertKafkaConfig(args)
	case strings.Contains(query, "DELETE FROM kafka_config"):
		return c.execDeleteKafkaConfig(args)
	default:
		return nil, fmt.Errorf("fakeConn: unexpected Exec query: %s", query[:min(len(query), 80)])
	}
}

func (c *fakeConn) execInsertKafkaConfig(args []driver.NamedValue) (driver.Result, error) {
	tid, err := argString(args, 1)
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
	brokers, err := argString(args, 4)
	if err != nil {
		return nil, err
	}
	topic, err := argString(args, 5)
	if err != nil {
		return nil, err
	}
	consumerGroup, err := argString(args, 6)
	if err != nil {
		return nil, err
	}
	eventType, err := argString(args, 7)
	if err != nil {
		return nil, err
	}
	nowVal, err := argAny(args, 8)
	if err != nil {
		return nil, err
	}
	now := nowVal.(time.Time)

	key := tid + ":" + id
	c.store.kafkaCfgs[key] = &fakeKafkaConfigRow{
		tenantID:      tid,
		id:            id,
		name:          name,
		brokers:       brokers,
		topic:         topic,
		consumerGroup: consumerGroup,
		eventType:     eventType,
		enabled:       true,
		createdAt:     now,
		updatedAt:     now,
	}
	return &fakeResult{rowsAffected: 1}, nil
}

func (c *fakeConn) execDeleteKafkaConfig(args []driver.NamedValue) (driver.Result, error) {
	id, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	tid, err := argString(args, 2)
	if err != nil {
		return nil, err
	}

	key := tid + ":" + id
	if _, ok := c.store.kafkaCfgs[key]; !ok {
		return &fakeResult{rowsAffected: 0}, nil
	}
	delete(c.store.kafkaCfgs, key)
	return &fakeResult{rowsAffected: 1}, nil
}

// ---------------------------------------------------------------------------
// QueryContext implementation
// ---------------------------------------------------------------------------

func (c *fakeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.store.mu.RLock()
	defer c.store.mu.RUnlock()

	switch {
	case strings.Contains(query, "SELECT tenant_id FROM tenant_api_keys"):
		return c.queryTenantByKeyHash(args)
	case strings.Contains(query, "FROM kafka_config") && strings.Contains(query, "enabled = true"):
		return c.queryKafkaConfigByID(args)
	case strings.Contains(query, "FROM kafka_config") && strings.Contains(query, "ORDER BY"):
		return c.queryKafkaConfigList(args)
	default:
		return nil, fmt.Errorf("fakeConn: unexpected Query query: %s", query[:min(len(query), 80)])
	}
}

func (c *fakeConn) queryTenantByKeyHash(args []driver.NamedValue) (driver.Rows, error) {
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

func (c *fakeConn) queryKafkaConfigByID(args []driver.NamedValue) (driver.Rows, error) {
	configID, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	tid, err := argString(args, 2)
	if err != nil {
		return nil, err
	}

	key := tid + ":" + configID
	row, ok := c.store.kafkaCfgs[key]
	if !ok || !row.enabled {
		return &fakeRows{columns: []string{"brokers", "topic"}}, nil
	}

	return &fakeRows{
		columns: []string{"brokers", "topic"},
		data:    [][]driver.Value{{row.brokers, row.topic}},
	}, nil
}

func (c *fakeConn) queryKafkaConfigList(args []driver.NamedValue) (driver.Rows, error) {
	tid, err := argString(args, 1)
	if err != nil {
		return nil, err
	}

	columns := []string{"id", "name", "brokers", "topic", "consumer_group", "event_type", "enabled", "created_at", "updated_at"}
	var data [][]driver.Value
	for _, row := range c.store.kafkaCfgs {
		if row.tenantID != tid {
			continue
		}
		data = append(data, []driver.Value{
			row.id, row.name, row.brokers, row.topic,
			row.consumerGroup, row.eventType, row.enabled,
			row.createdAt, row.updatedAt,
		})
	}

	return &fakeRows{columns: columns, data: data}, nil
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

func setupTestPlugin(t *testing.T) (*Plugin, http.Handler, *fakeDBStore) {
	t.Helper()

	store := newFakeDBStore()

	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantStr

	db := sql.OpenDB(&fakeConnector{store: store})
	t.Cleanup(func() { db.Close() })

	p := &Plugin{
		db:     db,
		logger: slog.Default(),
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
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

// withCallContext returns a context with the test tenant's CallContext injected.
func withCallContext(ctx context.Context) context.Context {
	return plugin.WithCallContext(ctx, &plugin.CallContext{
		TenantID: testTenantID,
	})
}

// ---------------------------------------------------------------------------
// Tests: Kafka Config CRUD
// ---------------------------------------------------------------------------

func TestKafkaCreateConfig(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	body := `{"name":"test-config","brokers":"localhost:9092","topic":"test-topic"}`
	req := authedRequest("POST", "/kafka/configs", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /kafka/configs: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp["name"] != "test-config" {
		t.Errorf("expected name 'test-config', got %q", resp["name"])
	}
	if resp["topic"] != "test-topic" {
		t.Errorf("expected topic 'test-topic', got %q", resp["topic"])
	}
	if resp["enabled"] != true {
		t.Error("expected enabled to be true")
	}
}

func TestKafkaCreateConfigValidation(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	tests := []struct {
		name     string
		body     string
		wantCode int
	}{
		{"missing name", `{"brokers":"b","topic":"t"}`, 400},
		{"missing brokers", `{"name":"n","topic":"t"}`, 400},
		{"missing topic", `{"name":"n","brokers":"b"}`, 400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := authedRequest("POST", "/kafka/configs", bytes.NewReader([]byte(tt.body)))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tt.wantCode {
				t.Errorf("expected %d, got %d: %s", tt.wantCode, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestKafkaListConfigs(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	// Create two configs.
	for _, name := range []string{"config-a", "config-b"} {
		body := `{"name":"` + name + `","brokers":"localhost:9092","topic":"` + name + `-topic"}`
		req := authedRequest("POST", "/kafka/configs", bytes.NewReader([]byte(body)))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s: expected 201, got %d", name, rec.Code)
		}
	}

	req := authedRequest("GET", "/kafka/configs", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /kafka/configs: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var configs []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &configs); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if len(configs) != 2 {
		t.Fatalf("expected 2 configs, got %d", len(configs))
	}
}

func TestKafkaDeleteConfig(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	// Create a config first.
	createBody := `{"name":"del-config","brokers":"localhost:9092","topic":"del-topic"}`
	req := authedRequest("POST", "/kafka/configs", bytes.NewReader([]byte(createBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", rec.Code)
	}

	var created map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &created)
	configID := created["id"].(string)

	// Delete it.
	req = authedRequest("DELETE", "/kafka/configs/"+configID, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE: expected 204, got %d", rec.Code)
	}

	// Delete same config again → 404.
	req = authedRequest("DELETE", "/kafka/configs/"+configID, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("DELETE again: expected 404, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Tests: Host function produce
// ---------------------------------------------------------------------------

func TestProduceHostFunction(t *testing.T) {
	p, _, store := setupTestPlugin(t)

	// Insert a config directly into the store.
	cfgID := uuid.New().String()
	store.mu.Lock()
	store.kafkaCfgs[testTenantStr+":"+cfgID] = &fakeKafkaConfigRow{
		tenantID:      testTenantStr,
		id:            cfgID,
		name:          "test-produce",
		brokers:       "localhost:9092",
		topic:         "test-topic",
		consumerGroup: "cleat-consumer",
		eventType:     "test-topic",
		enabled:       true,
		createdAt:     time.Now(),
		updatedAt:     time.Now(),
	}
	store.mu.Unlock()

	input := `{"config_id":"` + cfgID + `","key":"my-key","value":"hello world","headers":{"source":"test"}}`
	output, err := p.produce(withCallContext(context.Background()), input)
	if err != nil {
		t.Fatalf("produce() returned error: %v", err)
	}

	var result produceOutput
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("failed to decode output: %v", err)
	}
	if !result.Success {
		t.Error("expected success=true")
	}
}

func TestProduceToNonExistentConfig(t *testing.T) {
	p, _, _ := setupTestPlugin(t)

	badID := uuid.New().String()
	input := `{"config_id":"` + badID + `","value":"test"}`
	_, err := p.produce(withCallContext(context.Background()), input)
	if err == nil {
		t.Fatal("expected error for non-existent config, got nil")
	}
	if !strings.Contains(err.Error(), "config not found") {
		t.Errorf("expected 'config not found' error, got: %v", err)
	}
}

func TestProduceToDisabledConfig(t *testing.T) {
	p, _, store := setupTestPlugin(t)

	cfgID := uuid.New().String()
	store.mu.Lock()
	store.kafkaCfgs[testTenantStr+":"+cfgID] = &fakeKafkaConfigRow{
		tenantID:      testTenantStr,
		id:            cfgID,
		brokers:       "localhost:9092",
		topic:         "disabled-topic",
		enabled:       false,
		consumerGroup: "cleat-consumer",
		eventType:     "disabled-topic",
		createdAt:     time.Now(),
		updatedAt:     time.Now(),
	}
	store.mu.Unlock()

	input := `{"config_id":"` + cfgID + `","value":"test"}`
	_, err := p.produce(withCallContext(context.Background()), input)
	if err == nil {
		t.Fatal("expected error for disabled config, got nil")
	}
	if !strings.Contains(err.Error(), "config not found") {
		t.Errorf("expected 'config not found' error, got: %v", err)
	}
}

func TestProduceEmptyValue(t *testing.T) {
	p, _, _ := setupTestPlugin(t)

	// Empty value should fail validation.
	input := `{"config_id":"` + uuid.New().String() + `","value":""}`
	_, err := p.produce(withCallContext(context.Background()), input)
	if err == nil {
		t.Fatal("expected error for empty value, got nil")
	}
}

func TestProduceMissingTenantContext(t *testing.T) {
	p, _, _ := setupTestPlugin(t)

	input := `{"config_id":"` + uuid.New().String() + `","value":"test"}`
	_, err := p.produce(context.Background(), input)
	if err == nil {
		t.Fatal("expected error for missing tenant context, got nil")
	}
	if !strings.Contains(err.Error(), "no tenant context") {
		t.Errorf("expected 'no tenant context' error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Tests: Auth
// ---------------------------------------------------------------------------

func TestKafkaUnauthenticated(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	req := httptest.NewRequest("POST", "/kafka/configs", bytes.NewReader([]byte(`{"name":"n","brokers":"b","topic":"t"}`)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Tests: Produce with REST proxy mock
// ---------------------------------------------------------------------------

func TestProduceViaRestProxy(t *testing.T) {
	// Create a mock REST proxy server.
	mux := http.NewServeMux()
	mux.HandleFunc("/topics/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.kafka.v2+json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"offsets":[{"partition":0,"offset":1}]}`))
	})
	proxySrv := httptest.NewServer(mux)
	defer proxySrv.Close()

	store := newFakeDBStore()
	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantStr

	db := sql.OpenDB(&fakeConnector{store: store})
	t.Cleanup(func() { db.Close() })

	cfgID := uuid.New()
	store.mu.Lock()
	store.kafkaCfgs[testTenantStr+":"+cfgID.String()] = &fakeKafkaConfigRow{
		tenantID:      testTenantStr,
		id:            cfgID.String(),
		brokers:       "localhost:9092",
		topic:         "proxy-topic",
		consumerGroup: "cleat-consumer",
		eventType:     "proxy-topic",
		enabled:       true,
	}
	store.mu.Unlock()

	p := &Plugin{
		db:     db,
		logger: slog.Default(),
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
		config: Config{RestProxyURL: proxySrv.URL},
	}

	input := `{"config_id":"` + cfgID.String() + `","key":"proxy-key","value":"proxy-value"}`
	output, err := p.produce(withCallContext(context.Background()), input)
	if err != nil {
		t.Fatalf("produce() returned error: %v", err)
	}

	var result produceOutput
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("failed to decode output: %v", err)
	}
	if !result.Success {
		t.Error("expected success=true")
	}
}
