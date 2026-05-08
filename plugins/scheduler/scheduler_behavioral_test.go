package scheduler

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

type fakeScheduleRow struct {
	tenantID     string
	id           string
	name         string
	cron         string
	workflowName string
	input        []byte
	enabled      bool
	lastRunAt    *time.Time
	nextRunAt    *time.Time
	createdAt    time.Time
	updatedAt    time.Time
}

type fakeDBStore struct {
	mu        sync.RWMutex
	apiKeys   map[string]string               // key_hash -> tenant_id
	schedules map[string]*fakeScheduleRow     // "tenant:id" -> row
	now       func() time.Time
}

func newFakeDBStore() *fakeDBStore {
	return &fakeDBStore{
		apiKeys:   make(map[string]string),
		schedules: make(map[string]*fakeScheduleRow),
		now:       time.Now,
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
func (*fakeConn) Close() error              { return nil }
func (*fakeConn) Begin() (driver.Tx, error) { return &fakeTx{}, nil }

type fakeTx struct{}

func (*fakeTx) Commit() error   { return nil }
func (*fakeTx) Rollback() error { return nil }

// ---------------------------------------------------------------------------
// Argument extractors
// ---------------------------------------------------------------------------

func aStr(args []driver.NamedValue, ordinal int) (string, error) {
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

func aBytes(args []driver.NamedValue, ordinal int) ([]byte, error) {
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

func aBool(args []driver.NamedValue, ordinal int) (bool, error) {
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

func aAny(args []driver.NamedValue, ordinal int) (driver.Value, error) {
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
	case strings.Contains(query, "INSERT INTO schedules"):
		return c.execInsertSchedule(args)
	case strings.Contains(query, "UPDATE schedules"):
		return c.execUpdateSchedule(args)
	case strings.Contains(query, "DELETE FROM schedules"):
		return c.execDeleteSchedule(args)
	default:
		return nil, fmt.Errorf("fakeConn: unexpected Exec query: %s", query[:min(len(query), 80)])
	}
}

func (c *fakeConn) execInsertSchedule(args []driver.NamedValue) (driver.Result, error) {
	tid, err := aStr(args, 1)
	if err != nil {
		return nil, err
	}
	id, err := aStr(args, 2)
	if err != nil {
		return nil, err
	}
	name, err := aStr(args, 3)
	if err != nil {
		return nil, err
	}
	cron, err := aStr(args, 4)
	if err != nil {
		return nil, err
	}
	wfName, err := aStr(args, 5)
	if err != nil {
		return nil, err
	}
	input, err := aBytes(args, 6)
	if err != nil {
		return nil, err
	}
	enabled, err := aBool(args, 7)
	if err != nil {
		return nil, err
	}
	nextVal, err := aAny(args, 8)
	if err != nil {
		return nil, err
	}
	var nextRunAt *time.Time
	if t, ok := nextVal.(time.Time); ok {
		nextRunAt = &t
	}

	now := c.store.now()
	key := tid + ":" + id
	c.store.schedules[key] = &fakeScheduleRow{
		tenantID:     tid,
		id:           id,
		name:         name,
		cron:         cron,
		workflowName: wfName,
		input:        input,
		enabled:      enabled,
		nextRunAt:    nextRunAt,
		createdAt:    now,
		updatedAt:    now,
	}
	return &fakeResult{1}, nil
}

func (c *fakeConn) execUpdateSchedule(args []driver.NamedValue) (driver.Result, error) {
	// Find id and tenant_id from the end of the args.
	// The query is: UPDATE schedules SET updated_at = now(), ... WHERE id = $N AND tenant_id = $N+1
	n := len(args)
	if n < 2 {
		return &fakeResult{0}, nil
	}

	// Check if this is a background update (SET last_run_at = $1, next_run_at = $2 WHERE id = $3)
	// OR a route update (dynamic SET clauses).
	// Background query: "UPDATE schedules SET last_run_at = $1, next_run_at = $2, updated_at = now() WHERE id = $3"

	// For background update: 3 args [last_run_at, next_run_at, id]
	// For route update: variable args ending with [id, tenant_id]

	// For background update (runDueSchedules), we only have 3 args and no tenant_id in WHERE.
	// For route update, we have at least 2 args [id, tenant_id] at the end.
	if n == 3 {
		// Background update: last_run_at = $1, next_run_at = $2, WHERE id = $3
		id, err := aStr(args, 3)
		if err != nil {
			return nil, err
		}
		// Find the schedule by id across all tenants.
		for _, s := range c.store.schedules {
			if s.id == id {
				lrVal, _ := aAny(args, 1)
				if t, ok := lrVal.(time.Time); ok {
					s.lastRunAt = &t
				}
				nrVal, _ := aAny(args, 2)
				if t, ok := nrVal.(time.Time); ok {
					s.nextRunAt = &t
				} else {
					s.nextRunAt = nil
				}
				s.updatedAt = c.store.now()
				return &fakeResult{1}, nil
			}
		}
		return &fakeResult{0}, nil
	}

	// Route update: last args are id and tenant_id.
	id, err := aStr(args, n-1)
	if err != nil {
		return nil, err
	}
	tid, err := aStr(args, n)
	if err != nil {
		return nil, err
	}

	key := tid + ":" + id
	s, ok := c.store.schedules[key]
	if !ok {
		return &fakeResult{0}, nil
	}

	// Update enabled field if present (ordinal depends on how many fields)
	// For the test cases, we know the enabled field position.
	if n >= 3 {
		if v, err := aAny(args, 1); err == nil {
			if b, ok := v.(bool); ok {
				s.enabled = b
			}
		}
	}
	s.updatedAt = c.store.now()
	return &fakeResult{1}, nil
}

func (c *fakeConn) execDeleteSchedule(args []driver.NamedValue) (driver.Result, error) {
	id, err := aStr(args, 1)
	if err != nil {
		return nil, err
	}
	tid, err := aStr(args, 2)
	if err != nil {
		return nil, err
	}

	key := tid + ":" + id
	if _, ok := c.store.schedules[key]; !ok {
		return &fakeResult{0}, nil
	}
	delete(c.store.schedules, key)
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
	case strings.Contains(q, "next_run_at <= now()") || strings.Contains(q, "FOR UPDATE SKIP LOCKED"):
		return c.queryDueSchedules(args)
	case strings.Contains(q, "SELECT cron, enabled FROM schedules"):
		return c.queryScheduleForUpdate(args)
	case strings.Contains(q, "SELECT name, cron, workflow_name, input FROM schedules"):
		return c.queryScheduleForTrigger(args)
	case strings.Contains(q, "FROM schedules") && strings.Contains(q, "WHERE id ="):
		return c.queryScheduleByID(args)
	case strings.Contains(q, "FROM schedules") && strings.Contains(q, "ORDER BY"):
		return c.queryScheduleList(args)
	default:
		return nil, fmt.Errorf("fakeConn: unexpected Query query: %s", query[:min(len(query), 80)])
	}
}

func (c *fakeConn) queryTenantByKeyHash(args []driver.NamedValue) (driver.Rows, error) {
	keyHash, err := aBytes(args, 1)
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

func (c *fakeConn) queryScheduleByID(args []driver.NamedValue) (driver.Rows, error) {
	id, err := aStr(args, 1)
	if err != nil {
		return nil, err
	}
	tid, err := aStr(args, 2)
	if err != nil {
		return nil, err
	}

	key := tid + ":" + id
	s, ok := c.store.schedules[key]
	if !ok {
		return &fakeRows{
			columns: []string{"id", "name", "cron", "workflow_name", "input", "enabled", "last_run_at", "next_run_at", "created_at", "updated_at"},
		}, nil
	}

	var lr, nr driver.Value
	if s.lastRunAt != nil {
		lr = *s.lastRunAt
	}
	if s.nextRunAt != nil {
		nr = *s.nextRunAt
	}

	return &fakeRows{
		columns: []string{"id", "name", "cron", "workflow_name", "input", "enabled", "last_run_at", "next_run_at", "created_at", "updated_at"},
		data: [][]driver.Value{{
			s.id, s.name, s.cron, s.workflowName, s.input,
			s.enabled, lr, nr, s.createdAt, s.updatedAt,
		}},
	}, nil
}

func (c *fakeConn) queryScheduleList(args []driver.NamedValue) (driver.Rows, error) {
	tid, err := aStr(args, 1)
	if err != nil {
		return nil, err
	}

	columns := []string{"id", "name", "cron", "workflow_name", "input", "enabled", "last_run_at", "next_run_at", "created_at", "updated_at"}
	var data [][]driver.Value
	for _, s := range c.store.schedules {
		if s.tenantID != tid {
			continue
		}
		var lr, nr driver.Value
		if s.lastRunAt != nil {
			lr = *s.lastRunAt
		}
		if s.nextRunAt != nil {
			nr = *s.nextRunAt
		}
		data = append(data, []driver.Value{
			s.id, s.name, s.cron, s.workflowName, s.input,
			s.enabled, lr, nr, s.createdAt, s.updatedAt,
		})
	}
	return &fakeRows{columns: columns, data: data}, nil
}

func (c *fakeConn) queryScheduleForUpdate(args []driver.NamedValue) (driver.Rows, error) {
	id, err := aStr(args, 1)
	if err != nil {
		return nil, err
	}
	tid, err := aStr(args, 2)
	if err != nil {
		return nil, err
	}

	key := tid + ":" + id
	s, ok := c.store.schedules[key]
	if !ok {
		return &fakeRows{columns: []string{"cron", "enabled"}}, nil
	}

	return &fakeRows{
		columns: []string{"cron", "enabled"},
		data:    [][]driver.Value{{s.cron, s.enabled}},
	}, nil
}

func (c *fakeConn) queryScheduleForTrigger(args []driver.NamedValue) (driver.Rows, error) {
	id, err := aStr(args, 1)
	if err != nil {
		return nil, err
	}
	tid, err := aStr(args, 2)
	if err != nil {
		return nil, err
	}

	key := tid + ":" + id
	s, ok := c.store.schedules[key]
	if !ok {
		return &fakeRows{columns: []string{"name", "cron", "workflow_name", "input"}}, nil
	}

	return &fakeRows{
		columns: []string{"name", "cron", "workflow_name", "input"},
		data:    [][]driver.Value{{s.name, s.cron, s.workflowName, s.input}},
	}, nil
}

func (c *fakeConn) queryDueSchedules(args []driver.NamedValue) (driver.Rows, error) {
	// Find schedules where enabled=true AND next_run_at <= now()
	now := c.store.now()
	columns := []string{"id", "tenant_id", "name", "cron", "workflow_name", "input", "next_run_at"}
	var data [][]driver.Value
	for _, s := range c.store.schedules {
		if !s.enabled {
			continue
		}
		if s.nextRunAt == nil || s.nextRunAt.After(now) {
			continue
		}
		var nr driver.Value
		if s.nextRunAt != nil {
			nr = *s.nextRunAt
		}
		data = append(data, []driver.Value{
			s.id, s.tenantID, s.name, s.cron, s.workflowName, s.input, nr,
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

type controllableClock struct {
	mu  sync.Mutex
	now time.Time
}

func newControllableClock() *controllableClock {
	return &controllableClock{now: time.Date(2025, 6, 15, 10, 0, 0, 0, time.UTC)}
}

func (c *controllableClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *controllableClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

var testTenantID = uuid.MustParse("00000000-0000-0000-0000-000000000001")
var testTenantStr = testTenantID.String()

func setupTestPlugin(t *testing.T, clock *controllableClock) (*Plugin, http.Handler, *fakeDBStore) {
	t.Helper()

	store := newFakeDBStore()
	if clock != nil {
		store.now = clock.Now
	}

	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantStr

	db := sql.OpenDB(&fakeConnector{store: store})
	t.Cleanup(func() { db.Close() })

	p := &Plugin{
		db:     db,
		logger: slog.Default(),
		env: &plugin.Environment{
			DB: db,
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

// ---------------------------------------------------------------------------
// Tests: Schedule CRUD
// ---------------------------------------------------------------------------

func TestScheduleCreate(t *testing.T) {
	_, handler, _ := setupTestPlugin(t, nil)

	body := `{"name":"test-schedule","cron":"*/5 * * * *","workflow_name":"my-workflow"}`
	req := authedRequest("POST", "/schedules", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /schedules: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["name"] != "test-schedule" {
		t.Errorf("expected name 'test-schedule', got %q", resp["name"])
	}
	if resp["enabled"] != true {
		t.Error("expected enabled=true")
	}
}

func TestScheduleCreateValidation(t *testing.T) {
	_, handler, _ := setupTestPlugin(t, nil)

	tests := []struct {
		name string
		body string
		code int
	}{
		{"missing name", `{"cron":"* * * * *","workflow_name":"w"}`, 400},
		{"missing cron", `{"name":"n","workflow_name":"w"}`, 400},
		{"missing workflow", `{"name":"n","cron":"* * * * *"}`, 400},
		{"invalid cron", `{"name":"n","cron":"invalid","workflow_name":"w"}`, 400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := authedRequest("POST", "/schedules", bytes.NewReader([]byte(tt.body)))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tt.code {
				t.Errorf("expected %d, got %d: %s", tt.code, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestScheduleList(t *testing.T) {
	_, handler, _ := setupTestPlugin(t, nil)

	for _, name := range []string{"sched-a", "sched-b"} {
		body := `{"name":"` + name + `","cron":"*/5 * * * *","workflow_name":"wf"}`
		req := authedRequest("POST", "/schedules", bytes.NewReader([]byte(body)))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s: expected 201, got %d", name, rec.Code)
		}
	}

	req := authedRequest("GET", "/schedules", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /schedules: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var schedules []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &schedules); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(schedules) != 2 {
		t.Fatalf("expected 2 schedules, got %d", len(schedules))
	}
}

func TestScheduleGet(t *testing.T) {
	_, handler, _ := setupTestPlugin(t, nil)

	createBody := `{"name":"get-test","cron":"*/5 * * * *","workflow_name":"wf"}`
	req := authedRequest("POST", "/schedules", bytes.NewReader([]byte(createBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", rec.Code)
	}

	var created map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &created)
	schedID := created["id"].(string)

	req = authedRequest("GET", "/schedules/"+schedID, nil)
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

func TestScheduleGetNotFound(t *testing.T) {
	_, handler, _ := setupTestPlugin(t, nil)

	req := authedRequest("GET", "/schedules/"+uuid.New().String(), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestScheduleUpdate(t *testing.T) {
	_, handler, _ := setupTestPlugin(t, nil)

	createBody := `{"name":"orig","cron":"*/5 * * * *","workflow_name":"wf"}`
	req := authedRequest("POST", "/schedules", bytes.NewReader([]byte(createBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", rec.Code)
	}

	var created map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &created)
	schedID := created["id"].(string)

	updateBody := `{"name":"updated"}`
	req = authedRequest("PUT", "/schedules/"+schedID, bytes.NewReader([]byte(updateBody)))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestScheduleUpdateNotFound(t *testing.T) {
	_, handler, _ := setupTestPlugin(t, nil)

	body := `{"name":"nope"}`
	req := authedRequest("PUT", "/schedules/"+uuid.New().String(), bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestScheduleDelete(t *testing.T) {
	_, handler, _ := setupTestPlugin(t, nil)

	createBody := `{"name":"del-test","cron":"*/5 * * * *","workflow_name":"wf"}`
	req := authedRequest("POST", "/schedules", bytes.NewReader([]byte(createBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", rec.Code)
	}

	var created map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &created)
	schedID := created["id"].(string)

	req = authedRequest("DELETE", "/schedules/"+schedID, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE: expected 204, got %d", rec.Code)
	}

	req = authedRequest("DELETE", "/schedules/"+schedID, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("DELETE again: expected 404, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Tests: Schedule trigger
// ---------------------------------------------------------------------------

func TestScheduleTrigger(t *testing.T) {
	_, handler, _ := setupTestPlugin(t, nil)

	createBody := `{"name":"trigger-test","cron":"*/5 * * * *","workflow_name":"wf","input":{"key":"val"}}`
	req := authedRequest("POST", "/schedules", bytes.NewReader([]byte(createBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", rec.Code)
	}

	var created map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &created)
	schedID := created["id"].(string)

	req = authedRequest("POST", "/schedules/"+schedID+"/trigger", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("trigger: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var result map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &result)
	if result["status"] != "triggered" {
		t.Errorf("expected status 'triggered', got %q", result["status"])
	}
}

func TestScheduleTriggerNotFound(t *testing.T) {
	_, handler, _ := setupTestPlugin(t, nil)

	req := authedRequest("POST", "/schedules/"+uuid.New().String()+"/trigger", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Tests: Schedule enable/disable
// ---------------------------------------------------------------------------

func TestScheduleEnableDisable(t *testing.T) {
	_, handler, _ := setupTestPlugin(t, nil)

	// Create disabled schedule.
	createBody := `{"name":"toggle-test","cron":"*/5 * * * *","workflow_name":"wf","enabled":false}`
	req := authedRequest("POST", "/schedules", bytes.NewReader([]byte(createBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", rec.Code)
	}

	var created map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &created)
	schedID := created["id"].(string)

	// Enable it.
	enableBody := `{"enabled":true}`
	req = authedRequest("PUT", "/schedules/"+schedID, bytes.NewReader([]byte(enableBody)))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify by getting the schedule.
	req = authedRequest("GET", "/schedules/"+schedID, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: expected 200, got %d", rec.Code)
	}

	var sched map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &sched)
	if sched["enabled"] != true {
		t.Error("expected enabled=true after update")
	}
}

// ---------------------------------------------------------------------------
// Tests: Background scheduler loop
// ---------------------------------------------------------------------------

func TestScheduleNextRunCalculation(t *testing.T) {
	p, handler, store := setupTestPlugin(t, nil)

	clock := newControllableClock()
	store.now = clock.Now
	p.env = &plugin.Environment{
		DB: p.db,
	}

	// Create a schedule that fires every minute.
	createBody := `{"name":"frequent","cron":"* * * * *","workflow_name":"wf-freq"}`
	req := authedRequest("POST", "/schedules", bytes.NewReader([]byte(createBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", rec.Code)
	}

	var created map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &created)
	schedID := created["id"].(string)

	// Verify the next_run_at is set.
	req = authedRequest("GET", "/schedules/"+schedID, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: expected 200, got %d", rec.Code)
	}

	var sched map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &sched)
	if sched["next_run_at"] == nil {
		t.Error("expected next_run_at to be set")
	}
	if sched["enabled"] != true {
		t.Error("expected enabled=true")
	}
}

func TestRunDueSchedules(t *testing.T) {
	clock := newControllableClock()
	clock.now = time.Date(2025, 6, 15, 10, 0, 0, 0, time.UTC)

	p, _, store := setupTestPlugin(t, clock)

	startWorkflowCalls := 0
	p.env = &plugin.Environment{
		DB: p.db,
		StartWorkflow: func(ctx context.Context, defName string, input json.RawMessage) (string, error) {
			startWorkflowCalls++
			return uuid.New().String(), nil
		},
	}

	// Create a schedule that fires every 5 minutes and is due immediately.
	// The next_run_at should be at 10:00 (current time), so it's due.
	// Use POST to create it, but we need to deal with the fact that handleCreate
	// computes next_run_at based on time.Now() not the controllable clock.
	// We'll insert directly into the store to control the next_run_at.
	schedID := uuid.New().String()
	nextRunAt := time.Date(2025, 6, 15, 10, 0, 0, 0, time.UTC) // exactly now = due

	store.mu.Lock()
	store.schedules[testTenantStr+":"+schedID] = &fakeScheduleRow{
		tenantID:     testTenantStr,
		id:           schedID,
		name:         "due-schedule",
		cron:         "*/5 * * * *",
		workflowName: "wf-due",
		input:        []byte(`{"key":"val"}`),
		enabled:      true,
		nextRunAt:    &nextRunAt,
		createdAt:    clock.now,
		updatedAt:    clock.now,
	}
	store.mu.Unlock()

	// Run due schedules.
	schedulesDue, workflowsStarted, workflowsFailed := p.runDueSchedules(context.Background())
	if schedulesDue != 1 {
		t.Errorf("expected 1 due schedule, got %d", schedulesDue)
	}
	if workflowsStarted != 1 {
		t.Errorf("expected 1 workflow started, got %d", workflowsStarted)
	}
	if workflowsFailed != 0 {
		t.Errorf("expected 0 workflows failed, got %d", workflowsFailed)
	}
	if startWorkflowCalls != 1 {
		t.Errorf("expected 1 StartWorkflow call, got %d", startWorkflowCalls)
	}

	// Verify the schedule was updated with new next_run_at.
	store.mu.RLock()
	s := store.schedules[testTenantStr+":"+schedID]
	store.mu.RUnlock()
	if s == nil {
		t.Fatal("schedule should still exist")
	}
	if s.lastRunAt == nil {
		t.Error("expected last_run_at to be set")
	}
	if s.nextRunAt == nil {
		t.Error("expected next_run_at to be updated")
	} else if s.nextRunAt.Equal(nextRunAt) {
		t.Error("expected next_run_at to advance, not stay the same")
	}
}

func TestRunDueSchedulesNoneDue(t *testing.T) {
	clock := newControllableClock()
	p, _, store := setupTestPlugin(t, clock)

	p.env = &plugin.Environment{DB: p.db}

	// Create a schedule with future next_run_at (not due).
	schedID := uuid.New().String()
	future := clock.Now().Add(1 * time.Hour)

	store.mu.Lock()
	store.schedules[testTenantStr+":"+schedID] = &fakeScheduleRow{
		tenantID:     testTenantStr,
		id:           schedID,
		name:         "future-schedule",
		cron:         "0 * * * *",
		workflowName: "wf-future",
		input:        []byte("{}"),
		enabled:      true,
		nextRunAt:    &future,
		createdAt:    clock.now,
		updatedAt:    clock.now,
	}
	store.mu.Unlock()

	schedulesDue, workflowsStarted, workflowsFailed := p.runDueSchedules(context.Background())
	if schedulesDue != 0 {
		t.Errorf("expected 0 due schedules, got %d", schedulesDue)
	}
	if workflowsStarted != 0 {
		t.Errorf("expected 0 workflows started, got %d", workflowsStarted)
	}
	if workflowsFailed != 0 {
		t.Errorf("expected 0 workflows failed, got %d", workflowsFailed)
	}
}

func TestRunDueSchedulesDisabled(t *testing.T) {
	clock := newControllableClock()
	p, _, store := setupTestPlugin(t, clock)
	p.env = &plugin.Environment{DB: p.db}

	schedID := uuid.New().String()
	past := clock.Now().Add(-1 * time.Hour)

	store.mu.Lock()
	store.schedules[testTenantStr+":"+schedID] = &fakeScheduleRow{
		tenantID:     testTenantStr,
		id:           schedID,
		name:         "disabled-schedule",
		cron:         "*/5 * * * *",
		workflowName: "wf-disabled",
		input:        []byte("{}"),
		enabled:      false, // disabled
		nextRunAt:    &past,
		createdAt:    clock.now,
		updatedAt:    clock.now,
	}
	store.mu.Unlock()

	schedulesDue, workflowsStarted, workflowsFailed := p.runDueSchedules(context.Background())
	if schedulesDue != 0 {
		t.Errorf("expected 0 due schedules for disabled, got %d", schedulesDue)
	}
	if workflowsStarted != 0 {
		t.Errorf("expected 0 workflows started, got %d", workflowsStarted)
	}
	if workflowsFailed != 0 {
		t.Errorf("expected 0 workflows failed, got %d", workflowsFailed)
	}
}

// ---------------------------------------------------------------------------
// Tests: Host functions (RegisterHostFunctions)
// ---------------------------------------------------------------------------

func TestRegisterHostFunctions_NilRegistry(t *testing.T) {
	p := &Plugin{}
	err := p.RegisterHostFunctions(nil)
	if err == nil {
		t.Error("expected error for nil FuncRegistry")
	}
	if !strings.Contains(err.Error(), "nil FuncRegistry") {
		t.Errorf("expected error about nil FuncRegistry, got: %v", err)
	}
}

func TestRegisterHostFunctions_Valid(t *testing.T) {
	p := &Plugin{}
	mockReg := &mockFuncRegistry{}
	err := p.RegisterHostFunctions(mockReg)
	if err != nil {
		t.Fatalf("RegisterHostFunctions with valid registry: %v", err)
	}
}

// mockFuncRegistry implements plugin.FuncRegistry for testing.
type mockFuncRegistry struct{}

func (m *mockFuncRegistry) Register(_ plugin.FuncOptions, _ plugin.PluginFunc) error {
	return nil
}

// ---------------------------------------------------------------------------
// Tests: Migrations
// ---------------------------------------------------------------------------

func TestMigrations(t *testing.T) {
	p := &Plugin{}
	migrations := p.Migrations()

	if len(migrations) == 0 {
		t.Fatal("expected at least one migration")
	}

	for i, m := range migrations {
		if m.Version <= 0 {
			t.Errorf("migration %d: expected Version > 0, got %d", i, m.Version)
		}
		if m.Up == "" {
			t.Errorf("migration %d: expected non-empty Up SQL", i)
		}
	}
}

// ---------------------------------------------------------------------------
// Tests: RegisterRoutes nil mux
// ---------------------------------------------------------------------------

func TestRegisterRoutesNilMux(t *testing.T) {
	p := &Plugin{}
	err := p.RegisterRoutes(nil)
	if err == nil {
		t.Fatal("expected error for nil mux")
	}
	if !strings.Contains(err.Error(), "nil mux") {
		t.Errorf("expected error mentioning nil mux, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Tests: Unauthorized (missing tenant) for all endpoints
// ---------------------------------------------------------------------------

func TestScheduleCreateUnauthorized(t *testing.T) {
	mux := http.NewServeMux()
	p := &Plugin{logger: slog.Default()}
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}
	// No auth middleware -> no tenant in context
	req := httptest.NewRequest("POST", "/schedules", bytes.NewReader([]byte(`{"name":"n","cron":"* * * * *","workflow_name":"w"}`)))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestScheduleListUnauthorized(t *testing.T) {
	mux := http.NewServeMux()
	p := &Plugin{logger: slog.Default()}
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}
	req := httptest.NewRequest("GET", "/schedules", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestScheduleGetUnauthorized(t *testing.T) {
	mux := http.NewServeMux()
	p := &Plugin{logger: slog.Default()}
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}
	req := httptest.NewRequest("GET", "/schedules/"+uuid.New().String(), nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestScheduleUpdateUnauthorized(t *testing.T) {
	mux := http.NewServeMux()
	p := &Plugin{logger: slog.Default()}
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}
	body := `{"name":"updated"}`
	req := httptest.NewRequest("PUT", "/schedules/"+uuid.New().String(), bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestScheduleDeleteUnauthorized(t *testing.T) {
	mux := http.NewServeMux()
	p := &Plugin{logger: slog.Default()}
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}
	req := httptest.NewRequest("DELETE", "/schedules/"+uuid.New().String(), nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestScheduleTriggerUnauthorized(t *testing.T) {
	mux := http.NewServeMux()
	p := &Plugin{logger: slog.Default()}
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}
	req := httptest.NewRequest("POST", "/schedules/"+uuid.New().String()+"/trigger", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Tests: Invalid schedule ID (non-UUID)
// ---------------------------------------------------------------------------

func TestScheduleGetInvalidID(t *testing.T) {
	_, handler, _ := setupTestPlugin(t, nil)

	req := authedRequest("GET", "/schedules/not-a-uuid", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestScheduleUpdateInvalidID(t *testing.T) {
	_, handler, _ := setupTestPlugin(t, nil)

	body := `{"name":"updated"}`
	req := authedRequest("PUT", "/schedules/not-a-uuid", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestScheduleDeleteInvalidID(t *testing.T) {
	_, handler, _ := setupTestPlugin(t, nil)

	req := authedRequest("DELETE", "/schedules/not-a-uuid", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestScheduleTriggerInvalidID(t *testing.T) {
	_, handler, _ := setupTestPlugin(t, nil)

	req := authedRequest("POST", "/schedules/not-a-uuid/trigger", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Tests: Cron edge cases - DST, boundaries
// ---------------------------------------------------------------------------

func TestCronDSTTransition(t *testing.T) {
	// Simulate a "spring forward" DST transition.
	// In US/Eastern, clocks spring forward at 2:00 AM on the second Sunday
	// of March. On March 9, 2025, 2:00 AM becomes 3:00 AM.
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("timezone data not available:", err)
	}

	// 1:59 AM just before the DST transition - this time does not exist
	// in US/Eastern on that date (2:00 AM jumps to 3:00 AM).
	beforeDST := time.Date(2025, 3, 9, 1, 59, 0, 0, loc)

	// A cron that fires every 30 minutes should find the next match.
	next := nextRun("*/30 * * * *", beforeDST)
	if next.IsZero() {
		t.Fatal("expected non-zero nextRun after DST transition")
	}
	t.Logf("before DST: %v, next: %v", beforeDST, next)
}

func TestCronYearBoundary(t *testing.T) {
	// Last hour of the year: next run should flip to January 1 of next year.
	base := time.Date(2025, 12, 31, 23, 30, 0, 0, time.UTC)
	next := nextRun("0 0 1 1 *", base) // Jan 1 midnight
	expected := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("year boundary: expected %v, got %v", expected, next)
	}
}

func TestCronMonthBoundary(t *testing.T) {
	// Last day of January: next run on Feb 1.
	base := time.Date(2025, 1, 31, 23, 30, 0, 0, time.UTC)
	next := nextRun("0 0 1 * *", base) // 1st of each month at midnight
	expected := time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("month boundary: expected %v, got %v", expected, next)
	}
}

func TestCronEndOfMonthFeb(t *testing.T) {
	// Feb 28 of non-leap year, next run on March 1.
	base := time.Date(2025, 2, 28, 12, 0, 0, 0, time.UTC)
	next := nextRun("0 0 1 * *", base)
	expected := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("feb end: expected %v, got %v", expected, next)
	}
}

// ---------------------------------------------------------------------------
// Tests: Parse cron error paths
// ---------------------------------------------------------------------------

func TestParseCronErrorPaths(t *testing.T) {
	tests := []struct {
		name    string
		expr    string
		wantErr bool
	}{
		{"empty", "", true},
		{"3 fields", "* * *", true},
		{"6 fields", "* * * * * *", true},
		{"invalid minute", "60 * * * *", true},
		{"invalid hour", "* 24 * * *", true},
		{"invalid dayOfMonth", "* * 0 * *", true},
		{"invalid month", "* * * 13 *", true},
		{"invalid dayOfWeek", "* * * * 7", true},
		{"negative step", "*/0 * * * *", true},
		{"invalid range step", "1-10/0 * * * *", true},
		{"invalid range end", "10-5 * * * *", true},
		{"valid standard", "*/5 * * * *", false},
		{"valid specific", "30 9 * * 1-5", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseCron(tt.expr)
			if tt.wantErr && err == nil {
				t.Errorf("parseCron(%q): expected error, got nil", tt.expr)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("parseCron(%q): unexpected error: %v", tt.expr, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Tests: Run() with nil db
// ---------------------------------------------------------------------------

func TestRunWithNilDB(t *testing.T) {
	p := &Plugin{
		logger: slog.Default(),
		// db is nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately so Run returns quickly

	err := p.Run(ctx)
	if err != nil {
		t.Fatalf("Run() with nil db returned error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Tests: Background loop - workflow start failure
// ---------------------------------------------------------------------------

func TestRunDueSchedules_WorkflowFailure(t *testing.T) {
	clock := newControllableClock()
	clock.now = time.Date(2025, 6, 15, 10, 0, 0, 0, time.UTC)

	p, _, store := setupTestPlugin(t, clock)

	startWorkflowCalls := 0
	p.env = &plugin.Environment{
		DB: p.db,
		StartWorkflow: func(ctx context.Context, defName string, input json.RawMessage) (string, error) {
			startWorkflowCalls++
			return "", fmt.Errorf("workflow deployment not found")
		},
	}

	schedID := uuid.New().String()
	nextRunAt := time.Date(2025, 6, 15, 10, 0, 0, 0, time.UTC) // due now

	store.mu.Lock()
	store.schedules[testTenantStr+":"+schedID] = &fakeScheduleRow{
		tenantID:     testTenantStr,
		id:           schedID,
		name:         "failing-schedule",
		cron:         "*/5 * * * *",
		workflowName: "wf-fail",
		input:        []byte(`{}`),
		enabled:      true,
		nextRunAt:    &nextRunAt,
		createdAt:    clock.now,
		updatedAt:    clock.now,
	}
	store.mu.Unlock()

	schedulesDue, workflowsStarted, workflowsFailed := p.runDueSchedules(context.Background())
	if schedulesDue != 1 {
		t.Errorf("expected 1 due schedule, got %d", schedulesDue)
	}
	if workflowsStarted != 0 {
		t.Errorf("expected 0 workflows started, got %d", workflowsStarted)
	}
	if workflowsFailed != 1 {
		t.Errorf("expected 1 workflow failure, got %d", workflowsFailed)
	}
	if startWorkflowCalls != 1 {
		t.Errorf("expected 1 StartWorkflow call, got %d", startWorkflowCalls)
	}
}

// ---------------------------------------------------------------------------
// Tests: Create schedule with explicit input and disabled
// ---------------------------------------------------------------------------

func TestScheduleCreateWithInput(t *testing.T) {
	_, handler, _ := setupTestPlugin(t, nil)

	body := `{"name":"input-test","cron":"0 9 * * *","workflow_name":"wf","input":{"key":"value"},"enabled":false}`
	req := authedRequest("POST", "/schedules", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /schedules: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["enabled"] != false {
		t.Error("expected enabled=false")
	}
	if resp["name"] != "input-test" {
		t.Errorf("expected name 'input-test', got %q", resp["name"])
	}
}

// ---------------------------------------------------------------------------
// Tests: Create schedule with bad JSON body
// ---------------------------------------------------------------------------

func TestScheduleCreateBadJSON(t *testing.T) {
	_, handler, _ := setupTestPlugin(t, nil)

	req := authedRequest("POST", "/schedules", bytes.NewReader([]byte(`{invalid`)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad JSON, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestScheduleUpdateBadJSON(t *testing.T) {
	_, handler, _ := setupTestPlugin(t, nil)

	req := authedRequest("PUT", "/schedules/"+uuid.New().String(), bytes.NewReader([]byte(`{invalid`)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad JSON, got %d: %s", rec.Code, rec.Body.String())
	}
}
