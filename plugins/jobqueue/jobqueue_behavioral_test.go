// Package jobqueue behavioral tests — fake SQL driver + in-memory store pattern.
package jobqueue

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
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rcownie/cleat/internal/auth"
	"github.com/rcownie/cleat/internal/plugin"
)

// ---------------------------------------------------------------------------
// In-memory store (replaces PostgreSQL's task_queue table)
// ---------------------------------------------------------------------------

// jqRow represents a single row in the task_queue table.
type jqRow struct {
	tenantID    string
	queueName   string
	jobID       string
	payload     []byte
	status      string
	createdAt   time.Time
	startedAt   *time.Time
	completedAt *time.Time
	defName     *string
	input       []byte
	runID       *string
}

// fakeJobQueueStore is a goroutine-safe in-memory store for the task_queue
// table and tenant API keys.
type fakeJobQueueStore struct {
	mu      sync.RWMutex
	rows    map[string]*jqRow // "tenantID:queueName:jobID" -> row
	apiKeys map[string]string // hex(key_hash) -> tenant_id string
	now     func() time.Time
	failNextExec  bool // if true, next ExecContext returns error (cleared after use)
	failNextQuery bool // if true, next QueryContext returns error (cleared after use)
	querySkip     int  // number of queries to let succeed before failNextQuery takes effect
	execSkip      int  // number of execs to let succeed before failNextExec takes effect
}

func newFakeJobQueueStore() *fakeJobQueueStore {
	return &fakeJobQueueStore{
		rows:    make(map[string]*jqRow),
		apiKeys: make(map[string]string),
		now:     time.Now,
	}
}

// rowKey builds the map key for a task_queue row.
func rowKey(tid, queueName, jobID string) string {
	return tid + ":" + queueName + ":" + jobID
}

// ---------------------------------------------------------------------------
// Fake SQL driver (replaces PostgreSQL entirely)
// ---------------------------------------------------------------------------

type fakeConnector struct {
	store *fakeJobQueueStore
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
	store *fakeJobQueueStore
}

func (*fakeConn) Prepare(_ string) (driver.Stmt, error) {
	return nil, fmt.Errorf("fakeConn: unexpected Prepare call")
}

func (*fakeConn) Close() error { return nil }

func (*fakeConn) Begin() (driver.Tx, error) { return &fakeTx{}, nil }

type fakeTx struct{}

func (*fakeTx) Commit() error   { return nil }
func (*fakeTx) Rollback() error { return nil }

// ---- ExecContext (INSERT / UPDATE) ----

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
	case strings.Contains(query, "INSERT INTO task_queue"):
		return c.execInsert(args)
	case strings.Contains(query, "SET status = 'running'"):
		return c.execClaim(args)
	case strings.Contains(query, "SET status = 'failed'") && strings.Contains(query, "AND status = 'pending'"):
		return c.execCancel(args)
	case strings.Contains(query, "SET status = 'failed'"):
		return c.execMarkFailed(args)
	case strings.Contains(query, "SET status = 'completed'") && strings.Contains(query, "run_id"):
		return c.execMarkCompleted(args, true)
	case strings.Contains(query, "SET status = 'completed'"):
		return c.execMarkCompleted(args, false)
	default:
		return nil, fmt.Errorf("fakeConn: unexpected Exec query: %s", query)
	}
}

// ---- QueryContext (SELECT) ----

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

	// Determine lock type based on query.
	// SELECT ... FROM task_queue WHERE status = 'pending' needs read lock.
	// SELECT ... tenant_api_keys needs read lock.
	switch {
	case strings.Contains(query, "tenant_api_keys"):
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryTenantLookup(args)
	case strings.Contains(query, "def_name, input") && strings.Contains(query, "WHERE status = 'pending'"):
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryPendingJobs(args)
	case strings.Contains(query, "AND job_id"):
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryJobByID(args)
	case strings.Contains(query, "tenant_id = $1 AND queue_name = $2"):
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryListJobs(query, args)
	default:
		return nil, fmt.Errorf("fakeConn: unexpected Query query: %s", query)
	}
}

// ---------------------------------------------------------------------------
// Argument extractors
// ---------------------------------------------------------------------------

func argString(args []driver.NamedValue, ordinal int) (string, error) {
	for _, a := range args {
		if a.Ordinal == ordinal {
			s, ok := a.Value.(string)
			if !ok {
				return "", fmt.Errorf("arg %d: want string, got %T", ordinal, a.Value)
			}
			return s, nil
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

// argOptionalString returns nil when the value is nil (SQL NULL).
func argOptionalString(args []driver.NamedValue, ordinal int) (*string, error) {
	for _, a := range args {
		if a.Ordinal == ordinal {
			if a.Value == nil {
				return nil, nil
			}
			s, ok := a.Value.(string)
			if !ok {
				return nil, fmt.Errorf("arg %d: want string or nil, got %T", ordinal, a.Value)
			}
			return &s, nil
		}
	}
	return nil, fmt.Errorf("arg %d not found", ordinal)
}

// argOptionalBytes returns nil when the value is nil (SQL NULL).
func argOptionalBytes(args []driver.NamedValue, ordinal int) ([]byte, error) {
	for _, a := range args {
		if a.Ordinal == ordinal {
			if a.Value == nil {
				return nil, nil
			}
			b, ok := a.Value.([]byte)
			if !ok {
				return nil, fmt.Errorf("arg %d: want []byte or nil, got %T", ordinal, a.Value)
			}
			return b, nil
		}
	}
	return nil, fmt.Errorf("arg %d not found", ordinal)
}

// argAny returns the raw driver.Value for an ordinal without type conversion.
func argAny(args []driver.NamedValue, ordinal int) (driver.Value, error) {
	for _, a := range args {
		if a.Ordinal == ordinal {
			return a.Value, nil
		}
	}
	return nil, fmt.Errorf("arg %d not found", ordinal)
}

// ---------------------------------------------------------------------------
// Exec implementations
// ---------------------------------------------------------------------------

// execInsert handles:
//
//	INSERT INTO task_queue (tenant_id, queue_name, job_id, payload, def_name, input)
//	VALUES ($1, $2, $3, $4, $5, $6)
func (c *fakeConn) execInsert(args []driver.NamedValue) (driver.Result, error) {
	tid, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	queueName, err := argString(args, 2)
	if err != nil {
		return nil, err
	}
	jobID, err := argString(args, 3)
	if err != nil {
		return nil, err
	}
	payload, err := argOptionalBytes(args, 4)
	if err != nil {
		return nil, err
	}
	defName, err := argOptionalString(args, 5)
	if err != nil {
		return nil, err
	}
	input, err := argOptionalBytes(args, 6)
	if err != nil {
		return nil, err
	}

	now := c.store.now()
	key := rowKey(tid, queueName, jobID)
	c.store.rows[key] = &jqRow{
		tenantID:  tid,
		queueName: queueName,
		jobID:     jobID,
		payload:   payload,
		status:    "pending",
		createdAt: now,
		defName:   defName,
		input:     input,
	}
	return &fakeResult{rowsAffected: 1}, nil
}

// execClaim handles:
//
//	UPDATE task_queue SET status = 'running', started_at = now()
//	WHERE job_id = $1 AND tenant_id = $2 AND queue_name = $3 AND status = 'pending'
func (c *fakeConn) execClaim(args []driver.NamedValue) (driver.Result, error) {
	jobID, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	tid, err := argString(args, 2)
	if err != nil {
		return nil, err
	}
	queueName, err := argString(args, 3)
	if err != nil {
		return nil, err
	}

	key := rowKey(tid, queueName, jobID)
	row, ok := c.store.rows[key]
	if !ok || row.status != "pending" {
		return &fakeResult{rowsAffected: 0}, nil
	}

	now := c.store.now()
	row.status = "running"
	row.startedAt = &now
	return &fakeResult{rowsAffected: 1}, nil
}

// execCancel handles the cancel-job endpoint:
//
//	UPDATE task_queue SET status = 'failed', completed_at = now()
//	WHERE tenant_id = $1 AND queue_name = $2 AND job_id = $3 AND status = 'pending'
func (c *fakeConn) execCancel(args []driver.NamedValue) (driver.Result, error) {
	tid, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	queueName, err := argString(args, 2)
	if err != nil {
		return nil, err
	}
	jobID, err := argString(args, 3)
	if err != nil {
		return nil, err
	}

	key := rowKey(tid, queueName, jobID)
	row, ok := c.store.rows[key]
	if !ok || row.status != "pending" {
		return &fakeResult{rowsAffected: 0}, nil
	}

	now := c.store.now()
	row.status = "failed"
	row.completedAt = &now
	return &fakeResult{rowsAffected: 1}, nil
}

// execMarkFailed handles marking a job failed after dispatch failure:
//
//	UPDATE task_queue SET status = 'failed', completed_at = now()
//	WHERE job_id = $1 AND tenant_id = $2 AND queue_name = $3
func (c *fakeConn) execMarkFailed(args []driver.NamedValue) (driver.Result, error) {
	jobID, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	tid, err := argString(args, 2)
	if err != nil {
		return nil, err
	}
	queueName, err := argString(args, 3)
	if err != nil {
		return nil, err
	}

	key := rowKey(tid, queueName, jobID)
	row, ok := c.store.rows[key]
	if !ok {
		return &fakeResult{rowsAffected: 0}, nil
	}

	now := c.store.now()
	row.status = "failed"
	row.completedAt = &now
	return &fakeResult{rowsAffected: 1}, nil
}

// execMarkCompleted handles:
//
//	With run_id:    UPDATE ... SET status = 'completed', completed_at = now(), run_id = $4 WHERE ...
//	Without run_id: UPDATE ... SET status = 'completed', completed_at = now() WHERE ...
func (c *fakeConn) execMarkCompleted(args []driver.NamedValue, hasRunID bool) (driver.Result, error) {
	jobID, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	tid, err := argString(args, 2)
	if err != nil {
		return nil, err
	}
	queueName, err := argString(args, 3)
	if err != nil {
		return nil, err
	}

	key := rowKey(tid, queueName, jobID)
	row, ok := c.store.rows[key]
	if !ok {
		return &fakeResult{rowsAffected: 0}, nil
	}

	now := c.store.now()
	row.status = "completed"
	row.completedAt = &now
	if hasRunID {
		runID, err := argString(args, 4)
		if err != nil {
			return nil, err
		}
		row.runID = &runID
	}
	return &fakeResult{rowsAffected: 1}, nil
}

// ---------------------------------------------------------------------------
// Query implementations
// ---------------------------------------------------------------------------

// queryTenantLookup handles:
//
//	SELECT tenant_id FROM tenant_api_keys WHERE key_hash = $1 AND revoked_at IS NULL
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

// queryPendingJobs handles:
//
//	SELECT tenant_id, queue_name, job_id, payload, def_name, input
//	FROM task_queue
//	WHERE status = 'pending'
//	ORDER BY created_at ASC
//	LIMIT 10
func (c *fakeConn) queryPendingJobs(_ []driver.NamedValue) (driver.Rows, error) {
	var results []*jqRow
	for _, row := range c.store.rows {
		if row.status == "pending" {
			results = append(results, row)
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].createdAt.Before(results[j].createdAt)
	})

	// Limit to 10
	if len(results) > 10 {
		results = results[:10]
	}

	columns := []string{"tenant_id", "queue_name", "job_id", "payload", "def_name", "input"}
	var data [][]driver.Value
	for _, row := range results {
		var defNameVal driver.Value
		if row.defName != nil {
			defNameVal = *row.defName
		}
		var inputVal driver.Value = []byte("{}")
		if row.input != nil {
			inputVal = row.input
		}
		data = append(data, []driver.Value{
			row.tenantID,
			row.queueName,
			row.jobID,
			row.payload,
			defNameVal,
			inputVal,
		})
	}
	return &fakeRows{columns: columns, data: data}, nil
}

// queryJobByID handles:
//
//	SELECT job_id, queue_name, status, payload, created_at, started_at, completed_at
//	FROM task_queue
//	WHERE tenant_id = $1 AND queue_name = $2 AND job_id = $3
func (c *fakeConn) queryJobByID(args []driver.NamedValue) (driver.Rows, error) {
	tid, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	queueName, err := argString(args, 2)
	if err != nil {
		return nil, err
	}
	jobID, err := argString(args, 3)
	if err != nil {
		return nil, err
	}

	key := rowKey(tid, queueName, jobID)
	row, ok := c.store.rows[key]
	if !ok {
		return &fakeRows{
			columns: []string{"job_id", "queue_name", "status", "payload", "created_at", "started_at", "completed_at"},
		}, nil
	}

	var startedAtVal, completedAtVal driver.Value
	if row.startedAt != nil {
		startedAtVal = *row.startedAt
	}
	if row.completedAt != nil {
		completedAtVal = *row.completedAt
	}

	return &fakeRows{
		columns: []string{"job_id", "queue_name", "status", "payload", "created_at", "started_at", "completed_at"},
		data: [][]driver.Value{{
			row.jobID,
			row.queueName,
			row.status,
			row.payload,
			row.createdAt,
			startedAtVal,
			completedAtVal,
		}},
	}, nil
}

// queryListJobs handles:
//
//	SELECT job_id, queue_name, status, payload, created_at, started_at, completed_at
//	FROM task_queue
//	WHERE tenant_id = $1 AND queue_name = $2
//	[AND status = $3]
//	ORDER BY created_at DESC
//	LIMIT $n
func (c *fakeConn) queryListJobs(query string, args []driver.NamedValue) (driver.Rows, error) {
	tid, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	queueName, err := argString(args, 2)
	if err != nil {
		return nil, err
	}

	hasStatusFilter := strings.Contains(query, "AND status =")

	var statusFilter string
	var limit int64 = 50
	if hasStatusFilter {
		statusFilter, err = argString(args, 3)
		if err != nil {
			return nil, err
		}
		limit, err = argInt64(args, 4)
		if err != nil {
			return nil, err
		}
	} else {
		limit, err = argInt64(args, 3)
		if err != nil {
			return nil, err
		}
	}

	var results []*jqRow
	for _, row := range c.store.rows {
		if row.tenantID != tid || row.queueName != queueName {
			continue
		}
		if hasStatusFilter && row.status != statusFilter {
			continue
		}
		results = append(results, row)
	}

	// Sort by created_at DESC
	sort.Slice(results, func(i, j int) bool {
		return results[i].createdAt.After(results[j].createdAt)
	})

	// Apply limit
	if int64(len(results)) > limit {
		results = results[:limit]
	}

	columns := []string{"job_id", "queue_name", "status", "payload", "created_at", "started_at", "completed_at"}
	var data [][]driver.Value
	for _, row := range results {
		var startedAtVal, completedAtVal driver.Value
		if row.startedAt != nil {
			startedAtVal = *row.startedAt
		}
		if row.completedAt != nil {
			completedAtVal = *row.completedAt
		}
		data = append(data, []driver.Value{
			row.jobID,
			row.queueName,
			row.status,
			row.payload,
			row.createdAt,
			startedAtVal,
			completedAtVal,
		})
	}
	return &fakeRows{columns: columns, data: data}, nil
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
// Controllable clock
// ---------------------------------------------------------------------------

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Now()}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// ---------------------------------------------------------------------------
// Fake Environment (tracks StartWorkflow calls)
// ---------------------------------------------------------------------------

type startWorkflowCall struct {
	ctx     context.Context
	defName string
	input   json.RawMessage
}

type fakeEnvironment struct {
	plugin.Environment
	mu      sync.Mutex
	wfCalls []startWorkflowCall
	wfError error // if set, StartWorkflow returns this error
}

func newFakeEnvironment() *fakeEnvironment {
	fe := &fakeEnvironment{}
	fe.Environment.StartWorkflow = func(ctx context.Context, defName string, input json.RawMessage) (string, error) {
		fe.mu.Lock()
		fe.wfCalls = append(fe.wfCalls, startWorkflowCall{ctx: ctx, defName: defName, input: input})
		count := len(fe.wfCalls)
		fe.mu.Unlock()
		if fe.wfError != nil {
			return "", fe.wfError
		}
		// Return a deterministic run ID so the test can verify it was stored.
		return fmt.Sprintf("run-%s-%d", defName, count), nil
	}
	return fe
}

// ---------------------------------------------------------------------------
// Test setup helpers
// ---------------------------------------------------------------------------

var testTenantID = uuid.MustParse("00000000-0000-0000-0000-000000000001")

// setupTestPlugin creates a Plugin wired to an in-memory fake database and fake
// environment. The returned http.Handler includes the auth middleware so that
// requests carrying "Authorization: Bearer test-api-key" are authenticated.
func setupTestPlugin(t *testing.T) (*Plugin, http.Handler, *fakeJobQueueStore, *fakeClock, *fakeEnvironment) {
	t.Helper()

	clock := newFakeClock()
	store := newFakeJobQueueStore()
	store.now = clock.Now

	// Pre-populate a tenant API key so the auth middleware succeeds.
	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantID.String()

	db := sql.OpenDB(&fakeConnector{store: store})
	t.Cleanup(func() { db.Close() })

	fakeEnv := newFakeEnvironment()

	p := &Plugin{
		db:     db,
		mux:    http.NewServeMux(),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		env:    &fakeEnv.Environment,
	}

	if err := p.RegisterRoutes(p.mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	handler := auth.Middleware(db)(p.mux)
	return p, handler, store, clock, fakeEnv
}

// authedRequest creates a request authenticated with the test API key.
func authedRequest(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.Header.Set("Authorization", "Bearer test-api-key")
	return req
}

// ---------------------------------------------------------------------------
// Pending job insert helper (bypasses HTTP, inserts directly into store)
// ---------------------------------------------------------------------------

func insertPendingJob(store *fakeJobQueueStore, queueName, jobID string, payload []byte, defName *string, input []byte) {
	store.mu.Lock()
	defer store.mu.Unlock()
	now := store.now()
	key := rowKey(testTenantID.String(), queueName, jobID)
	store.rows[key] = &jqRow{
		tenantID:  testTenantID.String(),
		queueName: queueName,
		jobID:     jobID,
		payload:   payload,
		status:    "pending",
		createdAt: now,
		defName:   defName,
		input:     input,
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestEnqueueJob verifies that POST /jobqueue/{queue_name}/jobs creates a job
// with the correct fields in the task_queue store.
func TestEnqueueJob(t *testing.T) {
	_, handler, store, _, _ := setupTestPlugin(t)

	body := `{"payload":{"key":"value"},"def_name":"test-workflow","input":{"foo":"bar"}}`
	req := authedRequest("POST", "/jobqueue/test-queue/jobs", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp["status"] != "pending" {
		t.Errorf("expected status 'pending', got %q", resp["status"])
	}
	if resp["queue_name"] != "test-queue" {
		t.Errorf("expected queue_name 'test-queue', got %q", resp["queue_name"])
	}
	jobIDStr, ok := resp["job_id"].(string)
	if !ok || jobIDStr == "" {
		t.Fatal("expected non-empty job_id")
	}

	// Verify store state.
	store.mu.RLock()
	defer store.mu.RUnlock()

	key := rowKey(testTenantID.String(), "test-queue", jobIDStr)
	row, ok := store.rows[key]
	if !ok {
		t.Fatal("expected job to exist in store")
	}
	if row.status != "pending" {
		t.Errorf("expected status 'pending', got %q", row.status)
	}
	if row.queueName != "test-queue" {
		t.Errorf("expected queue_name 'test-queue', got %q", row.queueName)
	}
	if string(row.payload) != `{"key":"value"}` {
		t.Errorf("expected payload %q, got %q", `{"key":"value"}`, string(row.payload))
	}
	if row.defName == nil || *row.defName != "test-workflow" {
		t.Errorf("expected def_name 'test-workflow', got %v", row.defName)
	}
	if string(row.input) != `{"foo":"bar"}` {
		t.Errorf("expected input %q, got %q", `{"foo":"bar"}`, string(row.input))
	}
}

// TestClaimAndCompleteJob inserts a pending job (with no def_name) and calls
// pollPending. The job should be claimed (pending->running) and then
// auto-completed since there is no workflow to dispatch.
func TestClaimAndCompleteJob(t *testing.T) {
	p, _, store, _, _ := setupTestPlugin(t)

	jobID := uuid.New().String()
	insertPendingJob(store, "q", jobID, []byte(`{}`), nil, nil)

	claimed, dispatched, failed, err := p.pollPending(context.Background())
	if err != nil {
		t.Fatalf("pollPending: %v", err)
	}
	if claimed != 1 {
		t.Errorf("expected 1 claimed, got %d", claimed)
	}
	if dispatched != 0 {
		t.Errorf("expected 0 dispatched (no def_name), got %d", dispatched)
	}
	if failed != 0 {
		t.Errorf("expected 0 failed, got %d", failed)
	}

	store.mu.RLock()
	defer store.mu.RUnlock()
	key := rowKey(testTenantID.String(), "q", jobID)
	row, ok := store.rows[key]
	if !ok {
		t.Fatal("expected job to exist")
	}
	if row.status != "completed" {
		t.Errorf("expected status 'completed', got %q", row.status)
	}
	if row.startedAt == nil {
		t.Error("expected started_at to be set (job was claimed)")
	}
	if row.completedAt == nil {
		t.Error("expected completed_at to be set (job was completed)")
	}
}

// TestListJobs enqueues multiple jobs via POST and verifies the GET list
// endpoint returns all of them.
func TestListJobs(t *testing.T) {
	_, handler, store, _, _ := setupTestPlugin(t)

	// Enqueue 3 jobs.
	for i := 0; i < 3; i++ {
		payload := fmt.Sprintf(`{"i":%d}`, i)
		body := fmt.Sprintf(`{"payload":%s}`, payload)
		req := authedRequest("POST", "/jobqueue/myqueue/jobs", bytes.NewReader([]byte(body)))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("enqueue %d: expected 201, got %d: %s", i, rec.Code, rec.Body.String())
		}
	}

	// GET /jobqueue/myqueue/jobs
	req := authedRequest("GET", "/jobqueue/myqueue/jobs", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var jobs []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &jobs); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(jobs) != 3 {
		t.Fatalf("expected 3 jobs, got %d", len(jobs))
	}

	// Verify store also has 3 rows.
	store.mu.RLock()
	rowCount := 0
	for _, row := range store.rows {
		if row.tenantID == testTenantID.String() && row.queueName == "myqueue" {
			rowCount++
		}
	}
	store.mu.RUnlock()
	if rowCount != 3 {
		t.Errorf("expected 3 rows in store, got %d", rowCount)
	}
}

// TestGetSingleJob enqueues a job and then retrieves it by job_id.
func TestGetSingleJob(t *testing.T) {
	_, handler, _, _, _ := setupTestPlugin(t)

	// Enqueue a job.
	body := `{"payload":"single"}`
	req := authedRequest("POST", "/jobqueue/myqueue/jobs", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("enqueue: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var enqueueResp map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &enqueueResp)
	jobID := enqueueResp["job_id"].(string)

	// GET /jobqueue/myqueue/jobs/{job_id}
	req = authedRequest("GET", "/jobqueue/myqueue/jobs/"+jobID, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var jobResp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &jobResp); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if jobResp["job_id"] != jobID {
		t.Errorf("expected job_id %q, got %q", jobID, jobResp["job_id"])
	}
	if jobResp["queue_name"] != "myqueue" {
		t.Errorf("expected queue_name 'myqueue', got %q", jobResp["queue_name"])
	}
	if jobResp["status"] != "pending" {
		t.Errorf("expected status 'pending', got %q", jobResp["status"])
	}
}

// TestCancelJob enqueues a pending job and cancels it via DELETE.
// The handler sets status to 'failed' and sets completed_at.
func TestCancelJob(t *testing.T) {
	_, handler, store, _, _ := setupTestPlugin(t)

	// Enqueue a job.
	body := `{"payload":"cancel-me"}`
	req := authedRequest("POST", "/jobqueue/myqueue/jobs", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("enqueue: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var enqueueResp map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &enqueueResp)
	jobID := enqueueResp["job_id"].(string)

	// DELETE /jobqueue/myqueue/jobs/{job_id}
	req = authedRequest("DELETE", "/jobqueue/myqueue/jobs/"+jobID, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204 NoContent, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify store state — the handler sets status='failed'.
	store.mu.RLock()
	defer store.mu.RUnlock()
	key := rowKey(testTenantID.String(), "myqueue", jobID)
	row, ok := store.rows[key]
	if !ok {
		t.Fatal("expected job to exist in store")
	}
	if row.status != "failed" {
		t.Errorf("expected status 'failed', got %q", row.status)
	}
	if row.completedAt == nil {
		t.Error("expected completed_at to be set")
	}
}

// TestJobDispatchFailure verifies that when pollPending tries to dispatch a
// workflow and StartWorkflow fails, the job is marked as 'failed'.
func TestJobDispatchFailure(t *testing.T) {
	p, _, store, _, fakeEnv := setupTestPlugin(t)

	// Make StartWorkflow return an error.
	fakeEnv.wfError = fmt.Errorf("simulated dispatch failure")

	// Insert a pending job with a def_name.
	jobID := uuid.New().String()
	defName := "failing-workflow"
	insertPendingJob(store, "q", jobID, []byte(`{"data":"test"}`), &defName, []byte(`{"input":"val"}`))

	// Call pollPending — the job should be claimed, then fail to dispatch.
	claimed, dispatched, failed, err := p.pollPending(context.Background())
	if err != nil {
		t.Fatalf("pollPending: %v", err)
	}
	if claimed != 1 {
		t.Errorf("expected 1 claimed, got %d", claimed)
	}
	if dispatched != 0 {
		t.Errorf("expected 0 dispatched, got %d", dispatched)
	}
	if failed != 1 {
		t.Errorf("expected 1 failed, got %d", failed)
	}

	// Verify the job was tracked by StartWorkflow.
	fakeEnv.mu.Lock()
	calls := len(fakeEnv.wfCalls)
	fakeEnv.mu.Unlock()
	if calls != 1 {
		t.Errorf("expected 1 StartWorkflow call, got %d", calls)
	}

	// Verify the job is in 'failed' state.
	store.mu.RLock()
	defer store.mu.RUnlock()
	key := rowKey(testTenantID.String(), "q", jobID)
	row, ok := store.rows[key]
	if !ok {
		t.Fatal("expected job to exist")
	}
	if row.status != "failed" {
		t.Errorf("expected status 'failed', got %q", row.status)
	}
	if row.startedAt == nil {
		t.Error("expected started_at to be set (job was claimed)")
	}
	if row.completedAt == nil {
		t.Error("expected completed_at to be set (job was marked failed)")
	}
}

// TestPollSkipsNonPending verifies that pollPending only processes jobs with
// status='pending' and ignores running/completed/failed jobs.
func TestPollSkipsNonPending(t *testing.T) {
	p, _, store, _, _ := setupTestPlugin(t)

	now := store.now()
	pendingID := uuid.New().String()
	runningID := uuid.New().String()
	completedID := uuid.New().String()
	failedID := uuid.New().String()
	
	store.mu.Lock()
	store.rows[rowKey(testTenantID.String(), "q", pendingID)] = &jqRow{tenantID: testTenantID.String(), queueName: "q", jobID: pendingID, status: "pending", createdAt: now}
	store.rows[rowKey(testTenantID.String(), "q", runningID)] = &jqRow{tenantID: testTenantID.String(), queueName: "q", jobID: runningID, status: "running", createdAt: now, startedAt: &now}
	store.rows[rowKey(testTenantID.String(), "q", completedID)] = &jqRow{tenantID: testTenantID.String(), queueName: "q", jobID: completedID, status: "completed", createdAt: now, startedAt: &now, completedAt: &now}
	store.rows[rowKey(testTenantID.String(), "q", failedID)] = &jqRow{tenantID: testTenantID.String(), queueName: "q", jobID: failedID, status: "failed", createdAt: now, completedAt: &now}
	store.mu.Unlock()

	claimed, dispatched, failed, err := p.pollPending(context.Background())
	if err != nil {
		t.Fatalf("pollPending: %v", err)
	}
	if claimed != 1 {
		t.Errorf("expected 1 claimed (only pending job), got %d", claimed)
	}
	if dispatched != 0 {
		t.Errorf("expected 0 dispatched, got %d", dispatched)
	}
	if failed != 0 {
		t.Errorf("expected 0 failed, got %d", failed)
	}

	store.mu.RLock()
	pRow := store.rows[rowKey(testTenantID.String(), "q", pendingID)]
	store.mu.RUnlock()
	if pRow == nil || pRow.status != "completed" {
		t.Errorf("expected pending job to be completed, got %v", pRow)
	}
}

// TestEmptyJobList verifies that an empty queue returns an empty JSON array.
func TestEmptyJobList(t *testing.T) {
	_, handler, _, _, _ := setupTestPlugin(t)

	req := authedRequest("GET", "/jobqueue/empty-queue/jobs", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var jobs []interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &jobs); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("expected empty array, got %d elements: %+v", len(jobs), jobs)
	}
}

// TestListWithStatusFilter enqueues jobs with different statuses and verifies
// the GET list endpoint correctly filters by status.
func TestListWithStatusFilter(t *testing.T) {
	_, handler, _, _, _ := setupTestPlugin(t)

	// Enqueue 3 pending jobs.
	jobIDs := make([]string, 3)
	for i := 0; i < 3; i++ {
		body := fmt.Sprintf(`{"payload":"job-%d"}`, i)
		req := authedRequest("POST", "/jobqueue/filter-queue/jobs", bytes.NewReader([]byte(body)))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("enqueue %d: expected 201, got %d", i, rec.Code)
		}
		var resp map[string]interface{}
		json.Unmarshal(rec.Body.Bytes(), &resp)
		jobIDs[i] = resp["job_id"].(string)
	}

	// Cancel one job so it has status 'failed'.
	req := authedRequest("DELETE", "/jobqueue/filter-queue/jobs/"+jobIDs[0], nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("cancel: expected 204, got %d", rec.Code)
	}

	// List with status=pending — should return 2 jobs.
	req = authedRequest("GET", "/jobqueue/filter-queue/jobs?status=pending", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list pending: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var pendingJobs []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &pendingJobs); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if len(pendingJobs) != 2 {
		t.Errorf("expected 2 pending jobs, got %d: %+v", len(pendingJobs), pendingJobs)
	}
	for _, j := range pendingJobs {
		if j["status"] != "pending" {
			t.Errorf("expected status 'pending', got %q", j["status"])
		}
	}

	// List with status=failed — should return 1 job.
	req = authedRequest("GET", "/jobqueue/filter-queue/jobs?status=failed", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list failed: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var failedJobs []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &failedJobs); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if len(failedJobs) != 1 {
		t.Errorf("expected 1 failed job, got %d: %+v", len(failedJobs), failedJobs)
	}
	if failedJobs[0]["status"] != "failed" {
		t.Errorf("expected status 'failed', got %q", failedJobs[0]["status"])
	}

	// List with status=running — should return empty array.
	req = authedRequest("GET", "/jobqueue/filter-queue/jobs?status=running", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list running: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var runningJobs []interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &runningJobs); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if len(runningJobs) != 0 {
		t.Errorf("expected empty array for running status, got %d", len(runningJobs))
	}
}

// TestPollCallsStartWorkflow verifies that pollPending dispatches jobs with
// def_name via StartWorkflow and marks them completed on success, storing the
// run_id.
func TestPollCallsStartWorkflow(t *testing.T) {
	p, _, store, _, fakeEnv := setupTestPlugin(t)

	// Insert a pending job with a def_name and input.
	jobID := uuid.New().String()
	defName := "my-workflow"
	insertPendingJob(store, "wf-queue", jobID, []byte(`{"source":"test"}`), &defName, []byte(`{"x":1}`))

	// Call pollPending — job should be dispatched and completed.
	claimed, dispatched, failed, err := p.pollPending(context.Background())
	if err != nil {
		t.Fatalf("pollPending: %v", err)
	}
	if claimed != 1 {
		t.Errorf("expected 1 claimed, got %d", claimed)
	}
	if dispatched != 1 {
		t.Errorf("expected 1 dispatched, got %d", dispatched)
	}
	if failed != 0 {
		t.Errorf("expected 0 failed, got %d", failed)
	}

	// Verify StartWorkflow was called exactly once with correct args.
	fakeEnv.mu.Lock()
	calls := make([]startWorkflowCall, len(fakeEnv.wfCalls))
	copy(calls, fakeEnv.wfCalls)
	fakeEnv.mu.Unlock()

	if len(calls) != 1 {
		t.Fatalf("expected 1 StartWorkflow call, got %d", len(calls))
	}
	if calls[0].defName != "my-workflow" {
		t.Errorf("expected defName 'my-workflow', got %q", calls[0].defName)
	}
	if string(calls[0].input) != `{"x":1}` {
		t.Errorf("expected input %q, got %q", `{"x":1}`, string(calls[0].input))
	}

	// Verify job is completed with run_id stored.
	store.mu.RLock()
	defer store.mu.RUnlock()
	key := rowKey(testTenantID.String(), "wf-queue", jobID)
	row, ok := store.rows[key]
	if !ok {
		t.Fatal("expected job to exist")
	}
	if row.status != "completed" {
		t.Errorf("expected status 'completed', got %q", row.status)
	}
	if row.startedAt == nil {
		t.Error("expected started_at to be set")
	}
	if row.completedAt == nil {
		t.Error("expected completed_at to be set")
	}
	if row.runID == nil || *row.runID == "" {
		t.Error("expected run_id to be set")
	} else {
		expectedRunID := "run-my-workflow-1"
		if *row.runID != expectedRunID {
			t.Errorf("expected run_id %q, got %q", expectedRunID, *row.runID)
		}
	}
}

// TestRunStartsAndStops verifies that Run() starts and exits cleanly on
// context cancellation without error.
func TestRunStartsAndStops(t *testing.T) {
	p, _, _, _, _ := setupTestPlugin(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // immediate cancellation

	err := p.Run(ctx)
	if err != nil {
		t.Fatalf("Run() should return nil on context cancellation, got: %v", err)
	}
}

// TestEnqueueWithEmptyPayload verifies that a POST with only payload as a
// JSON value (no def_name) works and defaults to no def_name.
func TestEnqueueWithEmptyPayload(t *testing.T) {
	_, handler, store, _, _ := setupTestPlugin(t)

	body := `{"payload":42}`
	req := authedRequest("POST", "/jobqueue/q/jobs", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	jobID := resp["job_id"].(string)

	store.mu.RLock()
	defer store.mu.RUnlock()
	key := rowKey(testTenantID.String(), "q", jobID)
	row, ok := store.rows[key]
	if !ok {
		t.Fatal("expected job to exist")
	}
	if row.defName != nil {
		t.Errorf("expected def_name to be nil for empty-payload job, got %q", *row.defName)
	}
	if string(row.payload) != "42" {
		t.Errorf("expected payload '42', got %q", string(row.payload))
	}
}

// TestGetNonExistentJob verifies that GET for a non-existent job_id returns 404.
func TestGetNonExistentJob(t *testing.T) {
	_, handler, _, _, _ := setupTestPlugin(t)

	req := authedRequest("GET", "/jobqueue/q/jobs/00000000-0000-0000-0000-000000000999", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestCancelNonExistentJob verifies that cancelling a non-existent job returns 404.
func TestCancelNonExistentJob(t *testing.T) {
	_, handler, _, _, _ := setupTestPlugin(t)

	req := authedRequest("DELETE", "/jobqueue/q/jobs/00000000-0000-0000-0000-000000000999", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestUnauthenticatedRequest verifies that a request without a valid API key
// gets a 401 response.
func TestUnauthenticatedRequest(t *testing.T) {
	_, handler, _, _, _ := setupTestPlugin(t)

	body := `{"payload":"test"}`
	req := httptest.NewRequest("POST", "/jobqueue/q/jobs", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated request, got %d", rec.Code)
	}
}

// ===========================================================================
// Migrations
// ===========================================================================

func TestJQMigrations(t *testing.T) {
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
// RegisterCommands
// ===========================================================================

func TestJQRegisterCommands(t *testing.T) {
	p := &Plugin{}
	cmds := p.RegisterCommands()
	if len(cmds) == 0 {
		t.Fatal("expected at least one command")
	}
	if cmds[0].Name != "jobqueue-enqueue" {
		t.Errorf("expected Name 'jobqueue-enqueue', got %q", cmds[0].Name)
	}
	if cmds[0].Description == "" {
		t.Error("expected non-empty Description")
	}
	if cmds[0].Run == nil {
		t.Error("expected Run function to be non-nil")
	}
}

func TestJQRegisterCommandsRunNoDSN(t *testing.T) {
	p := &Plugin{}
	cmds := p.RegisterCommands()
	if len(cmds) == 0 {
		t.Fatal("no commands registered")
	}

	// Valid flags but no DSN should produce a "database URL required" error.
	err := cmds[0].Run([]string{
		"--tenant=00000000-0000-0000-0000-000000000001",
		"--queue=test-queue",
		"--payload={}",
	})
	if err == nil {
		t.Fatal("expected error for missing DSN, got nil")
	}
	if !strings.Contains(err.Error(), "database URL required") {
		t.Errorf("expected 'database URL required' error, got: %v", err)
	}
}

// ===========================================================================
// RegisterRoutes — nil mux
// ===========================================================================

func TestJQRegisterRoutesNilMux(t *testing.T) {
	p := &Plugin{}
	err := p.RegisterRoutes(nil)
	if err == nil {
		t.Fatal("expected error for nil mux, got nil")
	}
	if !strings.Contains(err.Error(), "nil mux") {
		t.Errorf("expected error containing 'nil mux', got: %v", err)
	}
}

// ===========================================================================
// handleEnqueue — error paths
// ===========================================================================

func TestJQEnqueueInvalidJSON(t *testing.T) {
	_, handler, _, _, _ := setupTestPlugin(t)

	req := authedRequest("POST", "/jobqueue/q/jobs", bytes.NewReader([]byte(`not json`)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid JSON, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestJQEnqueueMissingQueueName(t *testing.T) {
	p, _, _, _, _ := setupTestPlugin(t)

	body := `{"payload":"test"}`
	req := httptest.NewRequest("POST", "/jobqueue//jobs", bytes.NewReader([]byte(body)))
	req = req.WithContext(auth.WithTenantID(req.Context(), testTenantID))
	rec := httptest.NewRecorder()

	// Call handler directly (bypasses mux, so PathValue("queue_name") returns "").
	p.handleEnqueue(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing queue_name, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestJQEnqueueExecError(t *testing.T) {
	_, handler, store, _, _ := setupTestPlugin(t)

	store.failNextExec = true

	body := `{"payload":"test"}`
	req := authedRequest("POST", "/jobqueue/q/jobs", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for DB exec error, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// handleListJobs — error paths
// ===========================================================================

func TestJQListJobsMissingQueueName(t *testing.T) {
	p, _, _, _, _ := setupTestPlugin(t)

	req := httptest.NewRequest("GET", "/jobqueue//jobs", nil)
	req = req.WithContext(auth.WithTenantID(req.Context(), testTenantID))
	rec := httptest.NewRecorder()

	// Call handler directly (bypasses mux, so PathValue("queue_name") returns "").
	p.handleListJobs(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing queue_name, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestJQListJobsQueryError(t *testing.T) {
	_, handler, store, _, _ := setupTestPlugin(t)

	// Auth middleware does one query; skip it, then fail the handler query.
	store.failNextQuery = true
	store.querySkip = 1

	req := authedRequest("GET", "/jobqueue/test-queue/jobs", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for DB query error, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestJQListJobsInvalidLimit(t *testing.T) {
	_, handler, _, _, _ := setupTestPlugin(t)

	// Limit=0 should be ignored (use default 50).
	req := authedRequest("GET", "/jobqueue/test-queue/jobs?limit=0", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for limit=0, got %d: %s", rec.Code, rec.Body.String())
	}

	// Limit=-1 should be ignored.
	req = authedRequest("GET", "/jobqueue/test-queue/jobs?limit=-1", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for limit=-1, got %d: %s", rec.Code, rec.Body.String())
	}

	// Limit=1001 should be ignored (max is 1000).
	req = authedRequest("GET", "/jobqueue/test-queue/jobs?limit=1001", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for limit=1001, got %d: %s", rec.Code, rec.Body.String())
	}

	// Limit=10 should be accepted.
	req = authedRequest("GET", "/jobqueue/test-queue/jobs?limit=10", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for limit=10, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// handleGetJob — error paths
// ===========================================================================

func TestJQGetJobInvalidUUID(t *testing.T) {
	_, handler, _, _, _ := setupTestPlugin(t)

	req := authedRequest("GET", "/jobqueue/q/jobs/not-a-uuid", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid UUID, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestJQGetJobQueryError(t *testing.T) {
	_, handler, store, _, _ := setupTestPlugin(t)

	// Auth middleware does one query; skip it, then fail the handler query.
	store.failNextQuery = true
	store.querySkip = 1

	req := authedRequest("GET", "/jobqueue/q/jobs/00000000-0000-0000-0000-000000000001", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for DB query error, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// handleCancelJob — error paths
// ===========================================================================

func TestJQCancelJobInvalidUUID(t *testing.T) {
	_, handler, _, _, _ := setupTestPlugin(t)

	req := authedRequest("DELETE", "/jobqueue/q/jobs/not-a-uuid", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid UUID, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestJQCancelJobExecError(t *testing.T) {
	_, handler, store, _, _ := setupTestPlugin(t)

	store.failNextExec = true

	req := authedRequest("DELETE", "/jobqueue/q/jobs/00000000-0000-0000-0000-000000000001", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for DB exec error, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// Run — nil database
// ===========================================================================

func TestJQRunWithNoDB(t *testing.T) {
	p := &Plugin{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := p.Run(ctx)
	if err != nil {
		t.Fatalf("Run() with nil db should return nil, got: %v", err)
	}
}

// ===========================================================================
// Tenant isolation for job listings
// ===========================================================================

func TestJQ_ListJobs_TenantIsolation(t *testing.T) {
	_, handler, store, _, _ := setupTestPlugin(t)

	// Add a second tenant API key.
	tenant2ID := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	keyHash2 := sha256.Sum256([]byte("tenant2-key"))
	store.mu.Lock()
	store.apiKeys[fmt.Sprintf("%x", keyHash2)] = tenant2ID.String()
	store.mu.Unlock()

	// Enqueue a job as tenant 1.
	_ = store // store is used indirectly through the handler
	body := `{"payload":"tenant1-job"}`
	req := authedRequest("POST", "/jobqueue/tqueue/jobs", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("tenant1 enqueue: expected 201, got %d", rec.Code)
	}

	// List as tenant 1 — should see 1 job.
	req = authedRequest("GET", "/jobqueue/tqueue/jobs", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("tenant1 list: expected 200, got %d", rec.Code)
	}
	var jobs1 []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &jobs1); err != nil {
		t.Fatalf("tenant1 decode: %v", err)
	}
	if len(jobs1) != 1 {
		t.Errorf("tenant1 should see 1 job, got %d", len(jobs1))
	}

	// List as tenant 2 — should see 0 jobs.
	req2 := httptest.NewRequest("GET", "/jobqueue/tqueue/jobs", nil)
	req2.Header.Set("Authorization", "Bearer tenant2-key")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("tenant2 list: expected 200, got %d", rec2.Code)
	}
	var jobs2 []map[string]interface{}
	if err := json.Unmarshal(rec2.Body.Bytes(), &jobs2); err != nil {
		t.Fatalf("tenant2 decode: %v", err)
	}
	if len(jobs2) != 0 {
		t.Errorf("tenant2 should see 0 jobs, got %d", len(jobs2))
	}
}

// ===========================================================================
// Cancel job that is not pending (already cancelled, running, etc.)
// ===========================================================================

func TestJQ_CancelNonPendingJob(t *testing.T) {
	_, handler, _, _, _ := setupTestPlugin(t)

	// Enqueue a job.
	body := `{"payload":"cancel-me"}`
	req := authedRequest("POST", "/jobqueue/myqueue/jobs", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("enqueue: expected 201, got %d", rec.Code)
	}
	var enqueueResp map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &enqueueResp)
	jobID := enqueueResp["job_id"].(string)

	// First cancel succeeds.
	req = authedRequest("DELETE", "/jobqueue/myqueue/jobs/"+jobID, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("first cancel: expected 204, got %d", rec.Code)
	}

	// Second cancel should return 404 because the job is no longer pending.
	req = authedRequest("DELETE", "/jobqueue/myqueue/jobs/"+jobID, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("second cancel: expected 404 (no longer pending), got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// handleEnqueue — all fields (def_name and input)
// ===========================================================================

func TestJQ_Enqueue_AllFields(t *testing.T) {
	_, handler, store, _, _ := setupTestPlugin(t)

	body := `{"payload":{"key":"value"},"def_name":"test-wf","input":{"x":1}}`
	req := authedRequest("POST", "/jobqueue/full-queue/jobs", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	jobID := resp["job_id"].(string)

	store.mu.RLock()
	defer store.mu.RUnlock()
	key := rowKey(testTenantID.String(), "full-queue", jobID)
	row, ok := store.rows[key]
	if !ok {
		t.Fatal("expected job to exist")
	}
	if row.defName == nil || *row.defName != "test-wf" {
		t.Errorf("expected def_name 'test-wf', got %v", row.defName)
	}
	if string(row.input) != `{"x":1}` {
		t.Errorf("expected input %q, got %q", `{"x":1}`, string(row.input))
	}
}

// ===========================================================================
// pollPending — QueryContext error path
// ===========================================================================

func TestJQ_PollPending_QueryError(t *testing.T) {
	p, _, store, _, _ := setupTestPlugin(t)

	store.failNextQuery = true

	claimed, _, _, err := p.pollPending(context.Background())
	if err == nil {
		t.Fatal("expected error from pollPending with DB failure")
	}
	if claimed != 0 {
		t.Errorf("expected 0 claimed on query error, got %d", claimed)
	}
}

// ===========================================================================
// pollPending — Exec error on claim
// ===========================================================================

func TestJQ_PollPending_ClaimExecError(t *testing.T) {
	p, _, store, _, _ := setupTestPlugin(t)

	// Insert a pending job.
	jobID := uuid.New().String()
	insertPendingJob(store, "q", jobID, []byte(`{}`), nil, nil)

	// Make the claim Exec fail.
	store.failNextExec = true

	claimed, _, _, err := p.pollPending(context.Background())
	if err != nil {
		t.Fatalf("pollPending: %v", err)
	}
	if claimed != 0 {
		t.Errorf("expected 0 claimed when claim Exec fails, got %d", claimed)
	}

	// Job should still be pending (the claim failed, leaving it unchanged).
	store.mu.RLock()
	key := rowKey(testTenantID.String(), "q", jobID)
	row, ok := store.rows[key]
	store.mu.RUnlock()
	if !ok {
		t.Fatal("expected job to exist")
	}
	if row.status != "pending" {
		t.Errorf("expected status 'pending' after failed claim, got %q", row.status)
	}
}

// ===========================================================================
// Init — nil logger defaults to slog.Default
// ===========================================================================

func TestJQ_Init_NilLogger(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{}
	err := p.Init(context.Background(), env)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if p.logger == nil {
		t.Error("expected logger to be set")
	}
}

// ===========================================================================
// RegisterCommands — missing required flags
// ===========================================================================

func TestJQ_RegisterCommands_MissingArgs(t *testing.T) {
	p := &Plugin{}
	cmds := p.RegisterCommands()
	if len(cmds) == 0 {
		t.Fatal("no commands registered")
	}

	// Missing tenant flag.
	err := cmds[0].Run([]string{
		"--queue=test-queue",
		"--payload={}",
	})
	if err == nil {
		t.Fatal("expected error for missing tenant, got nil")
	}
	if !strings.Contains(err.Error(), "tenant and queue are required") {
		t.Errorf("expected 'tenant and queue are required' error, got: %v", err)
	}

	// Missing queue flag.
	err = cmds[0].Run([]string{
		"--tenant=00000000-0000-0000-0000-000000000001",
		"--payload={}",
	})
	if err == nil {
		t.Fatal("expected error for missing queue, got nil")
	}
	if !strings.Contains(err.Error(), "tenant and queue are required") {
		t.Errorf("expected 'tenant and queue are required' error, got: %v", err)
	}
}

// ===========================================================================
// RegisterCommands — invalid UUID
// ===========================================================================

func TestJQ_RegisterCommands_InvalidTenantUUID(t *testing.T) {
	p := &Plugin{}
	cmds := p.RegisterCommands()
	if len(cmds) == 0 {
		t.Fatal("no commands registered")
	}

	err := cmds[0].Run([]string{
		"--tenant=not-a-uuid",
		"--queue=test-queue",
		"--payload={}",
	})
	if err == nil {
		t.Fatal("expected error for invalid tenant UUID, got nil")
	}
	if !strings.Contains(err.Error(), "invalid tenant UUID") {
		t.Errorf("expected 'invalid tenant UUID' error, got: %v", err)
	}
}

// ===========================================================================
// pollPending — mark completed error path (with def_name dispatching)
// ===========================================================================

func TestJQ_PollPending_MarkCompletedError(t *testing.T) {
	p, _, store, _, _ := setupTestPlugin(t)

	// Insert a pending job with a def_name.
	jobID := uuid.New().String()
	defName := "test-wf"
	insertPendingJob(store, "q", jobID, []byte(`{"data":"test"}`), &defName, []byte(`{}`))

	// Make the claim succeed but the mark-completed exec fail.
	// claim is the 1st Exec, mark-completed is the 2nd Exec.
	store.failNextExec = true
	store.execSkip = 1 // let the claim succeed

	claimed, dispatched, failed, err := p.pollPending(context.Background())
	if err != nil {
		t.Fatalf("pollPending: %v", err)
	}
	if claimed != 1 {
		t.Errorf("expected 1 claimed, got %d", claimed)
	}
	if dispatched != 1 {
		t.Errorf("expected 1 dispatched (StartWorkflow succeeded), got %d", dispatched)
	}
	if failed != 0 {
		t.Errorf("expected 0 failed, got %d", failed)
	}

	// Job should be 'running' because the mark-completed failed.
	store.mu.RLock()
	key := rowKey(testTenantID.String(), "q", jobID)
	row, ok := store.rows[key]
	store.mu.RUnlock()
	if !ok {
		t.Fatal("expected job to exist")
	}
	if row.status != "running" {
		t.Errorf("expected status 'running' (mark-completed failed), got %q", row.status)
	}
}

// ===========================================================================
// pollPending — mark completed error path (without def_name)
// ===========================================================================

func TestJQ_PollPending_MarkCompletedNoDefNameError(t *testing.T) {
	p, _, store, _, _ := setupTestPlugin(t)

	// Insert a pending job without def_name.
	jobID := uuid.New().String()
	insertPendingJob(store, "q", jobID, []byte(`{}`), nil, nil)

	// Make the claim succeed but the mark-completed exec fail.
	store.failNextExec = true
	store.execSkip = 1

	claimed, _, _, err := p.pollPending(context.Background())
	if err != nil {
		t.Fatalf("pollPending: %v", err)
	}
	if claimed != 1 {
		t.Errorf("expected 1 claimed, got %d", claimed)
	}

	// Job should be 'running' because the mark-completed failed.
	store.mu.RLock()
	key := rowKey(testTenantID.String(), "q", jobID)
	row, ok := store.rows[key]
	store.mu.RUnlock()
	if !ok {
		t.Fatal("expected job to exist")
	}
	if row.status != "running" {
		t.Errorf("expected status 'running' (mark-completed failed), got %q", row.status)
	}
}

// ===========================================================================
// Run — background with mock DB (start/stop via cancel)
// ===========================================================================

func TestJQ_Run_Background(t *testing.T) {
	p, _, store, _, _ := setupTestPlugin(t)

	// Insert a pending job so the poll cycle has work to do.
	jobID := uuid.New().String()
	insertPendingJob(store, "bgq", jobID, []byte(`{}`), nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)

	go func() {
		errCh <- p.Run(ctx)
	}()

	// Let it run briefly (ticker fires every 5s, we just verify start/stop).
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after context cancellation")
	}
}

// ===========================================================================
// handleGetJob — no tenant in context (direct handler call)
// ===========================================================================

func TestJQ_HandleGetJob_NoTenant(t *testing.T) {
	p, _, _, _, _ := setupTestPlugin(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/jobqueue/q/jobs/00000000-0000-0000-0000-000000000001", nil)
	p.handleGetJob(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for missing tenant, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// handleCancelJob — no tenant in context (direct handler call)
// ===========================================================================

func TestJQ_HandleCancelJob_NoTenant(t *testing.T) {
	p, _, _, _, _ := setupTestPlugin(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/jobqueue/q/jobs/00000000-0000-0000-0000-000000000001", nil)
	p.handleCancelJob(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for missing tenant, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// handleCancelJob — empty queue_name (direct handler call)
// ===========================================================================

func TestJQ_HandleCancelJob_EmptyQueueName(t *testing.T) {
	p, _, _, _, _ := setupTestPlugin(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/jobqueue//jobs/id", nil)
	req = req.WithContext(auth.WithTenantID(req.Context(), testTenantID))
	p.handleCancelJob(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty queue_name, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// handleListJobs — no tenant in context (direct handler call)
// ===========================================================================

func TestJQ_HandleListJobs_NoTenant(t *testing.T) {
	p, _, _, _, _ := setupTestPlugin(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/jobqueue/q/jobs", nil)
	p.handleListJobs(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for missing tenant, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// handleEnqueue — no tenant in context (direct handler call)
// ===========================================================================

func TestJQ_HandleEnqueue_NoTenant(t *testing.T) {
	p, _, _, _, _ := setupTestPlugin(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/jobqueue/q/jobs", bytes.NewReader([]byte(`{"payload":"test"}`)))
	p.handleEnqueue(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for missing tenant, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// Run — background ticker cycle (waits 5s for one tick)
// ===========================================================================

func TestJQ_Run_TickerCycle(t *testing.T) {
	p, _, store, _, _ := setupTestPlugin(t)

	// Insert a pending job so pollPending has work.
	jobID := uuid.New().String()
	insertPendingJob(store, "tick-queue", jobID, []byte(`{}`), nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)

	go func() {
		errCh <- p.Run(ctx)
	}()

	// Wait for the ticker to fire once (5s interval).
	time.Sleep(6 * time.Second)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after context cancellation")
	}

	// The pending job should have been claimed and completed by pollPending.
	store.mu.RLock()
	key := rowKey(testTenantID.String(), "tick-queue", jobID)
	row, ok := store.rows[key]
	store.mu.RUnlock()
	if !ok {
		t.Fatal("expected job to exist")
	}
	if row.status != "completed" {
		t.Errorf("expected job to be completed after poll cycle, got %q", row.status)
	}
}
