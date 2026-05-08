package notifications

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
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

// ===========================================================================
// Fake function registry (mirrors plugin.FuncRegistry for testing)
// ===========================================================================

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
// Recording driver — records Exec calls for state transition tests
// ===========================================================================

type execRecord struct {
	query string
	args  []driver.NamedValue
}

type recordingConnector struct {
	mu      sync.Mutex
	records []execRecord
}

func (c *recordingConnector) Connect(_ context.Context) (driver.Conn, error) {
	return &recordingConn{parent: c}, nil
}

func (c *recordingConnector) Driver() driver.Driver {
	return &recordingDrv{}
}

type recordingDrv struct{}

func (*recordingDrv) Open(_ string) (driver.Conn, error) {
	return nil, fmt.Errorf("recordingDriver: use sql.OpenDB")
}

type recordingConn struct {
	parent *recordingConnector
}

func (*recordingConn) Prepare(_ string) (driver.Stmt, error) {
	return nil, fmt.Errorf("recordingConn: unexpected Prepare call")
}

func (*recordingConn) Close() error { return nil }

func (*recordingConn) Begin() (driver.Tx, error) { return &recordingTx{}, nil }

type recordingTx struct{}

func (*recordingTx) Commit() error   { return nil }
func (*recordingTx) Rollback() error { return nil }

func (c *recordingConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.parent.mu.Lock()
	c.parent.records = append(c.parent.records, execRecord{query: query, args: args})
	c.parent.mu.Unlock()
	return &recordingResult{}, nil
}

type recordingResult struct{}

func (*recordingResult) LastInsertId() (int64, error) { return 0, nil }
func (*recordingResult) RowsAffected() (int64, error) { return 1, nil }

func (c *recordingConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	cols := []string{"id", "webhook_id", "event_type", "payload", "attempt_count"}
	return &recordingRows{columns: cols}, nil
}

type recordingRows struct {
	columns []string
}

func (r *recordingRows) Columns() []string { return r.columns }
func (r *recordingRows) Close() error      { return nil }
func (r *recordingRows) Next(dest []driver.Value) error {
	return io.EOF
}

// ===========================================================================
// Erroring driver — fails on all DB operations for error path coverage
// ===========================================================================

type erroringConnector struct{}

func (*erroringConnector) Connect(_ context.Context) (driver.Conn, error) {
	return &erroringConn{}, nil
}

func (*erroringConnector) Driver() driver.Driver {
	return &erroringDrv{}
}

type erroringDrv struct{}

func (*erroringDrv) Open(_ string) (driver.Conn, error) {
	return nil, fmt.Errorf("erroringDriver: use sql.OpenDB")
}

type erroringConn struct{}

func (*erroringConn) Prepare(_ string) (driver.Stmt, error) {
	return nil, fmt.Errorf("erroringConn: unexpected Prepare call")
}

func (*erroringConn) Close() error { return nil }

func (*erroringConn) Begin() (driver.Tx, error) { return &erroringTx{}, nil }

type erroringTx struct{}

func (*erroringTx) Commit() error   { return nil }
func (*erroringTx) Rollback() error { return nil }

func (*erroringConn) ExecContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
	return nil, fmt.Errorf("simulated exec error")
}

func (*erroringConn) QueryContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
	return nil, fmt.Errorf("simulated query error")
}

// ===========================================================================
// Helpers
// ===========================================================================

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func authedCtx() context.Context {
	return auth.WithTenantID(context.Background(), uuid.MustParse("00000000-0000-0000-0000-000000000001"))
}

func noAuthRequest(method, target string, body io.Reader) *http.Request {
	return httptest.NewRequest(method, target, body)
}

func authedRequestForTest(method, target string, body io.Reader) *http.Request {
	return httptest.NewRequest(method, target, body).WithContext(authedCtx())
}

// ===========================================================================
// nextBackoff — exponential backoff calculation
// ===========================================================================

func TestNextBackoff(t *testing.T) {
	tests := []struct {
		attempt  int
		expected time.Duration
	}{
		{0, 1 * time.Hour},
		{1, 1 * time.Minute},
		{2, 5 * time.Minute},
		{3, 15 * time.Minute},
		{4, 1 * time.Hour},
		{5, 1 * time.Hour},
		{9, 1 * time.Hour},
		{10, 1 * time.Hour},
		{100, 1 * time.Hour},
	}
	for _, tt := range tests {
		got := nextBackoff(tt.attempt)
		if got != tt.expected {
			t.Errorf("nextBackoff(%d) = %v, want %v", tt.attempt, got, tt.expected)
		}
	}
}

// ===========================================================================
// RegisterHostFunctions — host function registration
// ===========================================================================

func TestRegisterHostFunctions_NilRegistry(t *testing.T) {
	p := &Plugin{}
	err := p.RegisterHostFunctions(nil)
	if err == nil {
		t.Fatal("expected error for nil registry, got nil")
	}
	if !strings.Contains(err.Error(), "nil function registry") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestRegisterHostFunctions_Valid(t *testing.T) {
	p := &Plugin{}
	reg := newFakeFuncRegistry()
	if err := p.RegisterHostFunctions(reg); err != nil {
		t.Fatalf("RegisterHostFunctions: %v", err)
	}
	if !reg.Has("send_webhook") {
		t.Error("expected send_webhook to be registered")
	}
}

// ===========================================================================
// Migrations — schema
// ===========================================================================

func TestMigrations(t *testing.T) {
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
// State transitions — markRetrying (success and error paths)
// ===========================================================================

func TestMarkRetrying(t *testing.T) {
	conn := &recordingConnector{}
	db := sql.OpenDB(conn)
	defer db.Close()

	p := &Plugin{
		db:     db,
		logger: discardLogger(),
	}

	deliveryID := uuid.New()
	ctx := context.Background()

	err := p.markRetrying(ctx, deliveryID, 1, "request failed: timeout")
	if err != nil {
		t.Fatalf("markRetrying: %v", err)
	}

	conn.mu.Lock()
	records := conn.records
	conn.mu.Unlock()

	if len(records) != 1 {
		t.Fatalf("expected 1 exec record, got %d", len(records))
	}

	query := records[0].query
	if !strings.Contains(query, "UPDATE webhook_delivery") {
		t.Errorf("expected UPDATE webhook_delivery in query, got: %s", query)
	}
	if !strings.Contains(query, "'retrying'") {
		t.Errorf("expected status = 'retrying' in query, got: %s", query)
	}
}

func TestMarkRetrying_ExecError(t *testing.T) {
	db := sql.OpenDB(&erroringConnector{})
	defer db.Close()

	p := &Plugin{
		db:     db,
		logger: discardLogger(),
	}

	err := p.markRetrying(context.Background(), uuid.New(), 1, "error test")
	if err == nil {
		t.Fatal("expected error from markRetrying with failing db, got nil")
	}
	if !strings.Contains(err.Error(), "mark retrying") {
		t.Errorf("expected error containing 'mark retrying', got: %v", err)
	}
}

// ===========================================================================
// State transitions — markFailed (success and error paths)
// ===========================================================================

func TestMarkFailed(t *testing.T) {
	conn := &recordingConnector{}
	db := sql.OpenDB(conn)
	defer db.Close()

	p := &Plugin{
		db:     db,
		logger: discardLogger(),
	}

	deliveryID := uuid.New()
	ctx := context.Background()

	err := p.markFailed(ctx, deliveryID, 3, "HTTP 500: internal error")
	if err != nil {
		t.Fatalf("markFailed: %v", err)
	}

	conn.mu.Lock()
	records := conn.records
	conn.mu.Unlock()

	if len(records) != 1 {
		t.Fatalf("expected 1 exec record, got %d", len(records))
	}

	query := records[0].query
	if !strings.Contains(query, "UPDATE webhook_delivery") {
		t.Errorf("expected UPDATE webhook_delivery in query, got: %s", query)
	}
	if !strings.Contains(query, "'failed'") {
		t.Errorf("expected status = 'failed' in query, got: %s", query)
	}
}

func TestMarkFailed_ExecError(t *testing.T) {
	db := sql.OpenDB(&erroringConnector{})
	defer db.Close()

	p := &Plugin{
		db:     db,
		logger: discardLogger(),
	}

	err := p.markFailed(context.Background(), uuid.New(), 5, "error test")
	if err == nil {
		t.Fatal("expected error from markFailed with failing db, got nil")
	}
	if !strings.Contains(err.Error(), "mark failed") {
		t.Errorf("expected error containing 'mark failed', got: %v", err)
	}
}

// ===========================================================================
// State transitions — markDelivered (error path)
// ===========================================================================

func TestMarkDelivered_ExecError(t *testing.T) {
	db := sql.OpenDB(&erroringConnector{})
	defer db.Close()

	p := &Plugin{
		db:     db,
		logger: discardLogger(),
	}

	err := p.markDelivered(context.Background(), uuid.New(), 1, 200, "ok")
	if err == nil {
		t.Fatal("expected error from markDelivered with failing db, got nil")
	}
	if !strings.Contains(err.Error(), "mark delivered") {
		t.Errorf("expected error containing 'mark delivered', got: %v", err)
	}
}

// ===========================================================================
// processDeliveries — query error path
// ===========================================================================

func TestProcessDeliveries_QueryError(t *testing.T) {
	db := sql.OpenDB(&erroringConnector{})
	defer db.Close()

	p := &Plugin{
		db:     db,
		logger: discardLogger(),
	}

	attempted, succeeded, failed, err := p.processDeliveries(context.Background())
	if err == nil {
		t.Fatal("expected error from processDeliveries with failing db, got nil")
	}
	if !strings.Contains(err.Error(), "query deliveries") {
		t.Errorf("expected error containing 'query deliveries', got: %v", err)
	}
	if attempted != 0 || succeeded != 0 || failed != 0 {
		t.Errorf("expected all zero counts, got %d/%d/%d", attempted, succeeded, failed)
	}
}

// ===========================================================================
// Run — background runner lifecycle
// ===========================================================================

func TestRun_NilDB(t *testing.T) {
	p := &Plugin{
		logger: discardLogger(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := p.Run(ctx)
	if err != nil {
		t.Fatalf("Run with nil db: expected nil, got: %v", err)
	}
}

func TestRun_WithDB(t *testing.T) {
	conn := &recordingConnector{}
	db := sql.OpenDB(conn)
	defer db.Close()

	p := &Plugin{
		db:     db,
		logger: discardLogger(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	err := p.Run(ctx)
	if err != nil {
		t.Fatalf("Run with db: expected nil, got: %v", err)
	}
}

// ===========================================================================
// RegisterRoutes — nil mux
// ===========================================================================

func TestRegisterRoutes_NilMux(t *testing.T) {
	p := &Plugin{}
	err := p.RegisterRoutes(nil)
	if err == nil {
		t.Fatal("expected error for nil mux, got nil")
	}
	if !strings.Contains(err.Error(), "nil mux") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// ===========================================================================
// Route handlers — all endpoints return 401 when no tenant in context
// ===========================================================================

func TestRouteHandlers_NoAuth(t *testing.T) {
	p := &Plugin{
		logger: discardLogger(),
		db:     sql.OpenDB(&recordingConnector{}),
	}
	t.Cleanup(func() { p.db.Close() })

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	tests := []struct {
		name   string
		method string
		path   string
		body   io.Reader
	}{
		{"create webhook", "POST", "/webhooks", bytes.NewReader([]byte(`{"url":"https://example.com/hook"}`))},
		{"list webhooks", "GET", "/webhooks", nil},
		{"get webhook", "GET", "/webhooks/" + uuid.New().String(), nil},
		{"update webhook", "PUT", "/webhooks/" + uuid.New().String(), bytes.NewReader([]byte(`{"url":"https://example.com/new"}`))},
		{"delete webhook", "DELETE", "/webhooks/" + uuid.New().String(), nil},
		{"list deliveries", "GET", "/webhooks/" + uuid.New().String() + "/deliveries", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := noAuthRequest(tt.method, tt.path, tt.body)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("expected 401 for unauthenticated request, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// ===========================================================================
// Route handlers — DB error returns 500
// ===========================================================================

func TestRouteHandlers_DBError(t *testing.T) {
	p := &Plugin{
		logger: discardLogger(),
		db:     sql.OpenDB(&erroringConnector{}),
	}
	t.Cleanup(func() { p.db.Close() })

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	t.Run("create webhook with db error", func(t *testing.T) {
		body := bytes.NewReader([]byte(`{"url":"https://example.com/hook","events":["test"]}`))
		req := authedRequestForTest("POST", "/webhooks", body)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("expected 500 for db error, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("list webhooks with db error", func(t *testing.T) {
		req := authedRequestForTest("GET", "/webhooks", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("expected 500 for db error, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("get webhook with db error", func(t *testing.T) {
		req := authedRequestForTest("GET", "/webhooks/"+uuid.New().String(), nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("expected 500 for db error, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("update webhook with db error", func(t *testing.T) {
		body := bytes.NewReader([]byte(`{"url":"https://example.com/new"}`))
		req := authedRequestForTest("PUT", "/webhooks/"+uuid.New().String(), body)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("expected 500 for db error, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("delete webhook with db error", func(t *testing.T) {
		req := authedRequestForTest("DELETE", "/webhooks/"+uuid.New().String(), nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("expected 500 for db error, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("list deliveries with db error", func(t *testing.T) {
		req := authedRequestForTest("GET", "/webhooks/"+uuid.New().String()+"/deliveries", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("expected 500 for db error, got %d: %s", rec.Code, rec.Body.String())
		}
	})
}

// ===========================================================================
// Route handlers — auxiliary error paths
// ===========================================================================

func TestHandleCreateWebhook_InvalidID(t *testing.T) {
	p := &Plugin{
		logger: discardLogger(),
	}
	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	// There's no auth check for the get/update/delete endpoints when called
	// without auth — they'd return 401 before reaching the ID parse check.
	// But we can trigger the ID parse error by going through auth.
	conn := &recordingConnector{}
	p.db = sql.OpenDB(conn)
	t.Cleanup(func() { p.db.Close() })

	t.Run("get webhook invalid id", func(t *testing.T) {
		req := authedRequestForTest("GET", "/webhooks/not-a-uuid", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("expected 400 for invalid id, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("update webhook invalid id", func(t *testing.T) {
		body := bytes.NewReader([]byte(`{"url":"https://example.com/new"}`))
		req := authedRequestForTest("PUT", "/webhooks/not-a-uuid", body)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("expected 400 for invalid id, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("delete webhook invalid id", func(t *testing.T) {
		req := authedRequestForTest("DELETE", "/webhooks/not-a-uuid", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("expected 400 for invalid id, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("list deliveries invalid id", func(t *testing.T) {
		req := authedRequestForTest("GET", "/webhooks/not-a-uuid/deliveries", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("expected 400 for invalid id, got %d: %s", rec.Code, rec.Body.String())
		}
	})
}

func TestHandleCreateWebhook_NoFieldsToUpdate(t *testing.T) {
	p := &Plugin{
		logger: discardLogger(),
		db:     sql.OpenDB(&recordingConnector{}),
	}
	t.Cleanup(func() { p.db.Close() })

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	// First create a webhook so we have a valid ID.
	body := bytes.NewReader([]byte(`{"url":"https://example.com/hook","events":["test"]}`))
	req := authedRequestForTest("POST", "/webhooks", body)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// Now update with no fields — fails with 400.
	req = authedRequestForTest("PUT", "/webhooks/"+uuid.New().String(), bytes.NewReader([]byte(`{}`)))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty update body, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleCreateWebhook_InvalidRequestBody(t *testing.T) {
	p := &Plugin{
		logger: discardLogger(),
	}
	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	// No db needed — the body parse happens before any db access.
	t.Run("create webhook invalid body", func(t *testing.T) {
		req := authedRequestForTest("POST", "/webhooks", bytes.NewReader([]byte(`not json`)))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("expected 400 for invalid body, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("update webhook invalid body", func(t *testing.T) {
		req := authedRequestForTest("PUT", "/webhooks/"+uuid.New().String(), bytes.NewReader([]byte(`not json`)))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("expected 400 for invalid body, got %d: %s", rec.Code, rec.Body.String())
		}
	})
}

// ===========================================================================
// sendWebhook — no tenant context error path
// ===========================================================================

func TestSendWebhook_NoTenantContext(t *testing.T) {
	p := &Plugin{}
	_, err := p.sendWebhook(context.Background(), `{}`)
	if err == nil {
		t.Fatal("expected error for missing tenant context, got nil")
	}
	if !strings.Contains(err.Error(), "no tenant context") {
		t.Errorf("expected 'no tenant context' error, got: %v", err)
	}
}

func TestSendWebhook_InvalidJSON(t *testing.T) {
	p := &Plugin{}
	cc := &plugin.CallContext{TenantID: uuid.MustParse("00000000-0000-0000-0000-000000000001")}
	ctx := plugin.WithCallContext(context.Background(), cc)
	_, err := p.sendWebhook(ctx, `not json`)
	if err == nil {
		t.Fatal("expected error for invalid json, got nil")
	}
	if !strings.Contains(err.Error(), "invalid input") {
		t.Errorf("expected 'invalid input' error, got: %v", err)
	}
}
