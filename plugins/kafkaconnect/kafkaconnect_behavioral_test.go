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
	"github.com/cleat-team/cleat/internal/auth"
	"github.com/cleat-team/cleat/internal/host"
	"github.com/cleat-team/cleat/internal/plugin"
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
	failNextExec  bool                              // if true, next ExecContext returns error
	failNextQuery bool                              // if true, next QueryContext returns error
	querySkip     int                               // number of queries to let succeed before failNextQuery takes effect
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

	if c.store.failNextExec {
		c.store.failNextExec = false
		return nil, fmt.Errorf("simulated exec error")
	}

	switch {
	case strings.Contains(query, "INSERT INTO kafka_config"):
		return c.execInsertKafkaConfig(args)
	case strings.Contains(query, "INSERT INTO ingested_events"):
		return &fakeResult{rowsAffected: 1}, nil
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
	// Check fail flag briefly under write lock, then proceed with read lock.
	c.store.mu.Lock()
	failed := c.store.failNextQuery
	if failed && c.store.querySkip > 0 {
		c.store.querySkip--
		failed = false
	}
	if failed {
		c.store.failNextQuery = false
	}
	c.store.mu.Unlock()

	if failed {
		return nil, fmt.Errorf("simulated query error")
	}

	c.store.mu.RLock()
	defer c.store.mu.RUnlock()

	switch {
	case strings.Contains(query, "SELECT tenant_id FROM tenant_api_keys"):
		return c.queryTenantByKeyHash(args)
	case strings.Contains(query, "COALESCE(event_type"):
		return c.queryPollConfigs()
	case strings.Contains(query, "FROM kafka_config") && strings.Contains(query, "enabled = true"):
		return c.queryKafkaConfigByID(args)
	case strings.Contains(query, "FROM kafka_config") && strings.Contains(query, "ORDER BY"):
		return c.queryKafkaConfigList(args)
	case strings.Contains(query, "FROM event_subscriptions"):
		return &fakeRows{
			columns: []string{"id", "tenant_id", "event_type", "def_name", "entry_point", "input_template", "filter_expr", "enabled", "created_at", "max_retries"},
		}, nil
	case strings.Contains(query, "FROM event_awaiters"):
		return &fakeRows{
			columns: []string{"workflow_id"},
		}, nil
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

// queryPollConfigs handles the pollConfigs query:
//
//	SELECT id, tenant_id, name, brokers, topic, consumer_group, COALESCE(event_type, topic)
//	FROM kafka_config
//	WHERE enabled = true
//	ORDER BY tenant_id, created_at DESC
func (c *fakeConn) queryPollConfigs() (driver.Rows, error) {
	columns := []string{"id", "tenant_id", "name", "brokers", "topic", "consumer_group", "event_type"}
	var data [][]driver.Value
	for _, row := range c.store.kafkaCfgs {
		if !row.enabled {
			continue
		}
		eventType := row.eventType
		if eventType == "" {
			eventType = row.topic
		}
		data = append(data, []driver.Value{
			row.id,
			row.tenantID,
			row.name,
			row.brokers,
			row.topic,
			row.consumerGroup,
			eventType,
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
		db:     &host.SQLDBAdapter{DB: db},
		logger: slog.Default(),
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
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
		db:     &host.SQLDBAdapter{DB: db},
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

// ===========================================================================
// Config defaults
// ===========================================================================

func TestKafkaCreateConfigDefaults(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	body := `{"name":"defaults-test","brokers":"broker:9092","topic":"my-topic"}`
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
	// Default consumer_group should be "cleat-consumer".
	if resp["consumer_group"] != "cleat-consumer" {
		t.Errorf("expected default consumer_group 'cleat-consumer', got %q", resp["consumer_group"])
	}
	// Default event_type should mirror the topic.
	if resp["event_type"] != "my-topic" {
		t.Errorf("expected default event_type 'my-topic', got %q", resp["event_type"])
	}
}

func TestKafkaCreateConfigCustomValues(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	body := `{"name":"custom-test","brokers":"broker:9092","topic":"ctopic","consumer_group":"my-group","event_type":"my-event"}`
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
	if resp["consumer_group"] != "my-group" {
		t.Errorf("expected consumer_group 'my-group', got %q", resp["consumer_group"])
	}
	if resp["event_type"] != "my-event" {
		t.Errorf("expected event_type 'my-event', got %q", resp["event_type"])
	}
}

// ===========================================================================
// Empty list
// ===========================================================================

func TestKafkaListConfigsEmpty(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	req := authedRequest("GET", "/kafka/configs", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := strings.TrimSpace(rec.Body.String())
	if body != "[]" {
		t.Errorf("empty list: expected [], got %q", body)
	}
}

// ===========================================================================
// Invalid JSON body
// ===========================================================================

func TestKafkaCreateConfigInvalidJSON(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	req := authedRequest("POST", "/kafka/configs", bytes.NewReader([]byte(`not json`)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid JSON, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// Delete nonexistent config
// ===========================================================================

func TestKafkaDeleteConfigNotFound(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	nonexistentID := "00000000-0000-0000-0000-000000000099"
	req := authedRequest("DELETE", "/kafka/configs/"+nonexistentID, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for nonexistent config, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestKafkaDeleteConfigInvalidID(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	req := authedRequest("DELETE", "/kafka/configs/not-a-uuid", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid UUID, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// Tenant isolation
// ===========================================================================

func TestKafkaConfigTenantIsolation(t *testing.T) {
	// This test verifies that configs created by tenant A are not visible to
	// tenant B. We use two different API keys that map to different tenants.
	store := newFakeDBStore()

	// Register two tenants with different API keys.
	tenantA := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	tenantB := uuid.MustParse("00000000-0000-0000-0000-000000000002")

	keyHashA := sha256.Sum256([]byte("tenant-a-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHashA)] = tenantA.String()
	keyHashB := sha256.Sum256([]byte("tenant-b-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHashB)] = tenantB.String()

	db := sql.OpenDB(&fakeConnector{store: store})
	t.Cleanup(func() { db.Close() })

	p := &Plugin{
		db:     &host.SQLDBAdapter{DB: db},
		logger: slog.Default(),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}
	handler := auth.Middleware(host.NewPostgresStore(db), false)(mux)

	// Tenant A creates a config.
	body := `{"name":"tenant-a-config","brokers":"a:9092","topic":"a-topic"}`
	req := httptest.NewRequest("POST", "/kafka/configs", bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer tenant-a-key")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("tenant A create: expected 201, got %d", rec.Code)
	}

	// Tenant B lists configs — should be empty.
	req = httptest.NewRequest("GET", "/kafka/configs", nil)
	req.Header.Set("Authorization", "Bearer tenant-b-key")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("tenant B list: expected 200, got %d", rec.Code)
	}
	bodyStr := strings.TrimSpace(rec.Body.String())
	if bodyStr != "[]" {
		t.Errorf("tenant B should see empty list, got %q", bodyStr)
	}
}

// ===========================================================================
// Produce — invalid input JSON
// ===========================================================================

func TestKafkaProduceInvalidInputJSON(t *testing.T) {
	p, _, _ := setupTestPlugin(t)

	_, err := p.produce(withCallContext(context.Background()), `not json`)
	if err == nil {
		t.Fatal("expected error for invalid JSON input, got nil")
	}
	if !strings.Contains(err.Error(), "invalid input") {
		t.Errorf("expected 'invalid input' error, got: %v", err)
	}
}

// ===========================================================================
// Produce — empty config_id
// ===========================================================================

func TestKafkaProduceEmptyConfigID(t *testing.T) {
	p, _, _ := setupTestPlugin(t)

	_, err := p.produce(withCallContext(context.Background()), `{"value":"hello"}`)
	if err == nil {
		t.Fatal("expected error for empty config_id, got nil")
	}
	if !strings.Contains(err.Error(), "config_id is required") {
		t.Errorf("expected 'config_id is required' error, got: %v", err)
	}
}

// ===========================================================================
// Produce — REST proxy unreachable (fast timeout)
// ===========================================================================

func TestKafkaProduceRestProxyUnreachable(t *testing.T) {
	p, _, store := setupTestPlugin(t)

	cfgID := uuid.New().String()
	store.mu.Lock()
	store.kafkaCfgs[testTenantStr+":"+cfgID] = &fakeKafkaConfigRow{
		tenantID:      testTenantStr,
		id:            cfgID,
		brokers:       "broker:9092",
		topic:         "timeout-topic",
		enabled:       true,
		consumerGroup: "cleat-consumer",
		eventType:     "timeout-topic",
		createdAt:     time.Now(),
		updatedAt:     time.Now(),
	}
	store.mu.Unlock()

	// Point REST proxy to an address that will be refused (fast).
	p.config.RestProxyURL = "http://127.0.0.1:1"
	p.httpClient = &http.Client{Timeout: 100 * time.Millisecond}

	input := `{"config_id":"` + cfgID + `","value":"test"}`
	_, err := p.produce(withCallContext(context.Background()), input)
	if err == nil {
		t.Fatal("expected error for unreachable REST proxy, got nil")
	}
	if !strings.Contains(err.Error(), "rest proxy") {
		t.Errorf("expected error mentioning rest proxy, got: %v", err)
	}
}

// ===========================================================================
// Migrations — basic validation
// ===========================================================================

func TestKafkaMigrations(t *testing.T) {
	p := &Plugin{}
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
// RegisterHostFunctions — edge cases
// ===========================================================================

func TestKafkaRegisterHostFunctions_Valid(t *testing.T) {
	p := &Plugin{}
	reg := &fakeFuncRegistry{funcs: map[string]plugin.PluginFunc{}}
	if err := p.RegisterHostFunctions(reg); err != nil {
		t.Fatalf("RegisterHostFunctions: %v", err)
	}
	if _, ok := reg.funcs["produce"]; !ok {
		t.Error("expected 'produce' to be registered")
	}
}

func TestKafkaRegisterHostFunctions_NilRegistry(t *testing.T) {
	p := &Plugin{}
	err := p.RegisterHostFunctions(nil)
	if err == nil || !strings.Contains(err.Error(), "nil function registry") {
		t.Fatalf("expected nil registry error, got: %v", err)
	}
}

// fakeFuncRegistry is a test stub for plugin.FuncRegistry.
type fakeFuncRegistry struct {
	funcs       map[string]plugin.PluginFunc
	registerErr error // if set, Register returns this error
}

func (r *fakeFuncRegistry) Register(opts plugin.FuncOptions, fn plugin.PluginFunc) error {
	if r.registerErr != nil {
		return r.registerErr
	}
	r.funcs[opts.Name] = fn
	return nil
}

func TestKafkaRegisterHostFunctions_RegistryError(t *testing.T) {
	p := &Plugin{}
	reg := &fakeFuncRegistry{
		funcs:       map[string]plugin.PluginFunc{},
		registerErr: fmt.Errorf("registry full"),
	}
	err := p.RegisterHostFunctions(reg)
	if err == nil {
		t.Fatal("expected error from registry, got nil")
	}
	if !strings.Contains(err.Error(), "registry full") {
		t.Errorf("expected error containing 'registry full', got: %v", err)
	}
}

// ===========================================================================
// Init — with Logger preset
// ===========================================================================

func TestKafkaInitWithLogger(t *testing.T) {
	p := &Plugin{}
	customLogger := slog.New(slog.NewTextHandler(io.Discard, nil))
	env := &plugin.Environment{
		Logger: customLogger,
	}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if p.logger != customLogger {
		t.Error("expected logger to be preserved from environment")
	}
}

// ===========================================================================
// Background Run — context cancellation
// ===========================================================================

func TestKafkaRunContextCancelled(t *testing.T) {
	p, _, _ := setupTestPlugin(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := p.Run(ctx)
	if err != nil {
		t.Fatalf("Run() should return nil on context cancellation, got: %v", err)
	}
}

// ===========================================================================
// pollConfigs with enabled config and no REST proxy
// ===========================================================================

func TestKafkaPollConfigsNoProxy(t *testing.T) {
	p, _, store := setupTestPlugin(t)

	// Insert an enabled config.
	cfgID := uuid.New().String()
	store.mu.Lock()
	store.kafkaCfgs[testTenantStr+":"+cfgID] = &fakeKafkaConfigRow{
		tenantID:      testTenantStr,
		id:            cfgID,
		name:          "poll-test-config",
		brokers:       "broker:9092",
		topic:         "poll-test-topic",
		consumerGroup: "cleat-consumer",
		eventType:     "poll-test-topic",
		enabled:       true,
		createdAt:     time.Now(),
		updatedAt:     time.Now(),
	}
	store.mu.Unlock()

	// pollConfigs should query enabled configs and skip consumption since no REST proxy is configured.
	err := p.pollConfigs(context.Background())
	if err != nil {
		t.Fatalf("pollConfigs: %v", err)
	}
}

// ===========================================================================
// consumeViaRestProxy — full chain with mock HTTP
// ===========================================================================

func TestKafkaConsumeViaRestProxy(t *testing.T) {
	var proxySrv *httptest.Server
	proxySrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && strings.Contains(r.URL.Path, "/consumers/"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{
				"instance_id": "test-instance",
				"base_uri":    proxySrv.URL + "/consumers/test-instance",
			})
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/subscription"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/records"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode([]map[string]interface{}{
				{"topic": "test-topic", "key": nil, "value": "hello", "partition": 0, "offset": int64(1)},
			})
		case r.Method == "DELETE":
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer proxySrv.Close()

	p := &Plugin{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpClient: &http.Client{Timeout: 5 * time.Second},
		config: Config{RestProxyURL: proxySrv.URL},
	}

	c := configRow{
		ID:            uuid.New(),
		TenantID:      testTenantID,
		Name:          "test-config",
		Brokers:       "broker:9092",
		Topic:         "test-topic",
		ConsumerGroup: "cleat-consumer",
		EventType:     "test-topic",
	}

	records, err := p.consumeViaRestProxy(context.Background(), c)
	if err != nil {
		t.Fatalf("consumeViaRestProxy: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	if records[0].Topic != "test-topic" {
		t.Errorf("expected topic 'test-topic', got %q", records[0].Topic)
	}
	if records[0].Value != "hello" {
		t.Errorf("expected value 'hello', got %v", records[0].Value)
	}
}

// ===========================================================================
// produceViaRestProxy — non-success HTTP response
// ===========================================================================

func TestKafkaProduceViaRestProxyNonSuccess(t *testing.T) {
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error_code":500,"message":"Internal Server Error"}`))
	}))
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
		brokers:       "broker:9092",
		topic:         "error-topic",
		consumerGroup: "cleat-consumer",
		eventType:     "error-topic",
		enabled:       true,
	}
	store.mu.Unlock()

	p := &Plugin{
		db:     &host.SQLDBAdapter{DB: db},
		logger: slog.Default(),
		httpClient: &http.Client{Timeout: 5 * time.Second},
		config: Config{RestProxyURL: proxySrv.URL},
	}

	input := `{"config_id":"` + cfgID.String() + `","value":"test"}`
	_, err := p.produce(withCallContext(context.Background()), input)
	if err == nil {
		t.Fatal("expected error for REST proxy non-200, got nil")
	}
	if !strings.Contains(err.Error(), "rest proxy returned") {
		t.Errorf("expected error containing 'rest proxy returned', got: %v", err)
	}
}

// ===========================================================================
// Route handler — DB exec error on create
// ===========================================================================

func TestKafkaCreateConfigExecError(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	store.failNextExec = true

	body := `{"name":"test","brokers":"broker:9092","topic":"test-topic"}`
	req := authedRequest("POST", "/kafka/configs", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for DB exec error, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// Route handler — DB query error on list
// ===========================================================================

func TestKafkaListConfigsQueryError(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	// Auth middleware does one query; skip it, then fail the handler query.
	store.failNextQuery = true
	store.querySkip = 1

	req := authedRequest("GET", "/kafka/configs", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for DB query error, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// Route handler — DB exec error on delete
// ===========================================================================

func TestKafkaDeleteConfigExecError(t *testing.T) {
	_, handler, store := setupTestPlugin(t)

	store.failNextExec = true

	req := authedRequest("DELETE", "/kafka/configs/00000000-0000-0000-0000-000000000001", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for DB exec error, got %d: %s", rec.Code, rec.Body.String())
	}
}


// ===========================================================================
// publishRecord — coverage for the 0% function
// ===========================================================================

func TestKafkaPublishRecord(t *testing.T) {
		p, _, _ := setupTestPlugin(t)

		cfg := configRow{
			ID:            uuid.New(),
			TenantID:      testTenantID,
			Name:          "test-publish",
			Brokers:       "broker:9092",
			Topic:         "test-topic",
			ConsumerGroup: "cleat-consumer",
			EventType:     "test-event",
		}

		record := kafkaRecord{
			Topic:     "test-topic",
			Key:       "my-key",
			Value:     "hello",
			Partition: 0,
			Offset:    1,
		}

		err := p.publishRecord(context.Background(), cfg, record)
		if err != nil {
			t.Fatalf("publishRecord: %v", err)
		}
}

// ===========================================================================
// pollConfig — with REST Proxy URL set but consume failing
// ===========================================================================

func TestKafkaPollConfigConsumeError(t *testing.T) {
		p, _, store := setupTestPlugin(t)

		// Set RestProxyURL to an unreachable address so consumeViaRestProxy fails.
		p.config.RestProxyURL = "http://127.0.0.1:1"
		p.httpClient = &http.Client{Timeout: 100 * time.Millisecond}

		// Insert an enabled config.
		cfgID := uuid.New().String()
		store.mu.Lock()
		store.kafkaCfgs[testTenantStr+":"+cfgID] = &fakeKafkaConfigRow{
			tenantID:      testTenantStr,
			id:            cfgID,
			name:          "consume-error",
			brokers:       "broker:9092",
			topic:         "error-topic",
			consumerGroup: "cleat-consumer",
			eventType:     "error-topic",
			enabled:       true,
			createdAt:     time.Now(),
			updatedAt:     time.Now(),
		}
		store.mu.Unlock()

		// pollConfigs should query enabled configs and fail at consumeViaRestProxy.
		// The error is logged but pollConfigs should not return an error.
		err := p.pollConfigs(context.Background())
		if err != nil {
			t.Fatalf("pollConfigs: %v", err)
		}
}

// ===========================================================================
// pollConfig — RestProxyURL set, consume succeeds, publishRecord called
// ===========================================================================

func TestKafkaPollConfigConsumeAndPublish(t *testing.T) {
		var proxySrv *httptest.Server
		proxySrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == "POST" && strings.Contains(r.URL.Path, "/consumers/"):
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(map[string]string{
					"instance_id": "test-instance",
					"base_uri":    proxySrv.URL + "/consumers/test-instance",
				})
			case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/subscription"):
				w.WriteHeader(http.StatusNoContent)
			case r.Method == "GET" && strings.Contains(r.URL.Path, "/records"):
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode([]map[string]interface{}{
					{"topic": "publish-topic", "key": nil, "value": "hello-world", "partition": 0, "offset": int64(42)},
				})
			case r.Method == "DELETE":
				w.WriteHeader(http.StatusNoContent)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer proxySrv.Close()

		p, _, store := setupTestPlugin(t)
		p.config.RestProxyURL = proxySrv.URL
		p.httpClient = &http.Client{Timeout: 5 * time.Second}

		// Insert an enabled config.
		cfgID := uuid.New().String()
		store.mu.Lock()
		store.kafkaCfgs[testTenantStr+":"+cfgID] = &fakeKafkaConfigRow{
			tenantID:      testTenantStr,
			id:            cfgID,
			name:          "consume-publish",
			brokers:       "broker:9092",
			topic:         "publish-topic",
			consumerGroup: "cleat-consumer",
			eventType:     "publish-event",
			enabled:       true,
			createdAt:     time.Now(),
			updatedAt:     time.Now(),
		}
		store.mu.Unlock()

		err := p.pollConfigs(context.Background())
		if err != nil {
			t.Fatalf("pollConfigs: %v", err)
		}
}
