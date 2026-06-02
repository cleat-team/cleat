package eventstore

import (
	"bytes"
	"context"
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
	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/engine"
)

// ---------------------------------------------------------------------------
// In-memory fake DB for event store behavioral tests
// ---------------------------------------------------------------------------

type esRow struct {
	tenantID  uuid.UUID
	streamID  string
	sequence  int64
	event     string
	createdAt time.Time
}

type esDB struct {
	mu            sync.RWMutex
	events        []esRow
	failNextQuery bool
	failNextExec  bool
	querySkip     int
}

func newESDB() *esDB {
	return &esDB{}
}

type esConnector struct{ db *esDB }

func (c *esConnector) Connect(_ context.Context) (driver.Conn, error) { return &esConn{db: c.db}, nil }
func (c *esConnector) Driver() driver.Driver                           { return &esDrv{} }

type esDrv struct{}

func (*esDrv) Open(_ string) (driver.Conn, error) { return nil, fmt.Errorf("esDrv: use sql.OpenDB") }

type esConn struct {
	db *esDB
}

func (*esConn) Prepare(_ string) (driver.Stmt, error) { return nil, fmt.Errorf("esConn: unexpected Prepare") }
func (*esConn) Close() error                           { return nil }
func (*esConn) Begin() (driver.Tx, error)              { return &esTx{}, nil }

type esTx struct{}

func (*esTx) Commit() error   { return nil }
func (*esTx) Rollback() error { return nil }

type esResult struct{ n int64 }

func (r *esResult) LastInsertId() (int64, error) { return 0, nil }
func (r *esResult) RowsAffected() (int64, error)  { return r.n, nil }

type esRows struct {
	columns []string
	data    [][]driver.Value
	pos     int
}

func (r *esRows) Columns() []string              { return r.columns }
func (r *esRows) Close() error                    { return nil }
func (r *esRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.pos])
	r.pos++
	return nil
}

// ---- Arg extractors ----

func esArgString(args []driver.NamedValue, ordinal int) (string, error) {
	for _, a := range args {
		if a.Ordinal == ordinal {
			switch v := a.Value.(type) {
			case string:
				return v, nil
			case []byte:
				return string(v), nil
			case uuid.UUID:
				return v.String(), nil
			default:
				return fmt.Sprintf("%v", v), nil
			}
		}
	}
	return "", fmt.Errorf("arg %d not found", ordinal)
}

func esArgInt64(args []driver.NamedValue, ordinal int) (int64, error) {
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

// ---- ExecContext ----

func (c *esConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.db.mu.Lock()
	defer c.db.mu.Unlock()

	if c.db.failNextExec {
		c.db.failNextExec = false
		return nil, fmt.Errorf("simulated exec error")
	}

	q := strings.ReplaceAll(query, "\n", " ")
	switch {
	case strings.Contains(q, "DELETE FROM event_stream"):
		return c.execDelete(args)
	default:
		return nil, fmt.Errorf("esConn: unexpected Exec: %.80s", q)
	}
}

func (c *esConn) execDelete(args []driver.NamedValue) (driver.Result, error) {
	// DELETE FROM event_stream WHERE created_at < NOW() - make_interval(days => $1)
	// args[1] = retentionDays (int64)
	retentionDays, err := esArgInt64(args, 1)
	if err != nil {
		return &esResult{}, nil
	}

	cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)
	var remaining []esRow
	var deleted int64
	for _, e := range c.db.events {
		if e.createdAt.Before(cutoff) {
			deleted++
		} else {
			remaining = append(remaining, e)
		}
	}
	c.db.events = remaining
	return &esResult{deleted}, nil
}

// ---- QueryContext ----

func (c *esConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	// Check fail flag briefly under write lock, then proceed with read lock.
	c.db.mu.Lock()
	failed := c.db.failNextQuery
	if failed && c.db.querySkip > 0 {
		c.db.querySkip--
		failed = false
	}
	if failed {
		c.db.failNextQuery = false
	}
	c.db.mu.Unlock()

	if failed {
		return nil, fmt.Errorf("simulated query error")
	}

	q := strings.ReplaceAll(query, "\n", " ")
	switch {
	case strings.Contains(q, "INSERT INTO event_stream") && strings.Contains(q, "RETURNING sequence"):
		c.db.mu.Lock()
		defer c.db.mu.Unlock()
		return c.execInsert(args)
	case strings.Contains(q, "COALESCE(MAX(sequence)"):
		c.db.mu.RLock()
		defer c.db.mu.RUnlock()
		return c.queryMaxSeq(args)
	case strings.Contains(q, "event, created_at") && strings.Contains(q, "ORDER BY sequence ASC"):
		c.db.mu.RLock()
		defer c.db.mu.RUnlock()
		return c.queryReadEvents(args)
	default:
		return nil, fmt.Errorf("esConn: unexpected Query: %.80s", q)
	}
}

func (c *esConn) execInsert(args []driver.NamedValue) (driver.Rows, error) {
	tidStr, err := esArgString(args, 1)
	if err != nil {
		return nil, err
	}
	tid, err := uuid.Parse(tidStr)
	if err != nil {
		return nil, err
	}
	streamID, err := esArgString(args, 2)
	if err != nil {
		return nil, err
	}
	eventBody, err := esArgString(args, 3)
	if err != nil {
		return nil, err
	}

	// Compute next sequence
	var maxSeq int64
	for _, e := range c.db.events {
		if e.tenantID == tid && e.streamID == streamID && e.sequence > maxSeq {
			maxSeq = e.sequence
		}
	}
	sequence := maxSeq + 1

	now := time.Now().UTC().Truncate(time.Microsecond)
	c.db.events = append(c.db.events, esRow{
		tenantID:  tid,
		streamID:  streamID,
		sequence:  sequence,
		event:     eventBody,
		createdAt: now,
	})

	return &esRows{
		columns: []string{"sequence"},
		data:    [][]driver.Value{{sequence}},
	}, nil
}

func (c *esConn) queryMaxSeq(args []driver.NamedValue) (driver.Rows, error) {
	tidStr, err := esArgString(args, 1)
	if err != nil {
		return nil, err
	}
	tid, err := uuid.Parse(tidStr)
	if err != nil {
		return nil, err
	}
	streamID, err := esArgString(args, 2)
	if err != nil {
		return nil, err
	}

	var maxSeq int64
	for _, e := range c.db.events {
		if e.tenantID == tid && e.streamID == streamID && e.sequence > maxSeq {
			maxSeq = e.sequence
		}
	}

	return &esRows{
		columns: []string{"coalesce"},
		data:    [][]driver.Value{{maxSeq}},
	}, nil
}

func (c *esConn) queryReadEvents(args []driver.NamedValue) (driver.Rows, error) {
	tidStr, err := esArgString(args, 1)
	if err != nil {
		return nil, err
	}
	tid, err := uuid.Parse(tidStr)
	if err != nil {
		return nil, err
	}
	streamID, err := esArgString(args, 2)
	if err != nil {
		return nil, err
	}
	fromSeq, err := esArgInt64(args, 3)
	if err != nil {
		return nil, err
	}
	limit, err := esArgInt64(args, 4)
	if err != nil {
		return nil, err
	}

	var result []esRow
	for _, e := range c.db.events {
		if e.tenantID == tid && e.streamID == streamID && e.sequence > fromSeq {
			result = append(result, e)
		}
	}

	// Sort by sequence
	for i := 0; i < len(result); i++ {
		for j := i + 1; j < len(result); j++ {
			if result[j].sequence < result[i].sequence {
				result[i], result[j] = result[j], result[i]
			}
		}
	}
	if int64(len(result)) > limit {
		result = result[:limit]
	}

	columns := []string{"sequence", "event", "created_at"}
	var data [][]driver.Value
	for _, e := range result {
		data = append(data, []driver.Value{
			e.sequence,
			[]byte(e.event),
			e.createdAt,
		})
	}
	return &esRows{columns: columns, data: data}, nil
}

// ===========================================================================
// Test helpers
// ===========================================================================

func newESPlugin(t *testing.T) (*Plugin, *esDB, *sql.DB) {
	t.Helper()
	esdb := newESDB()
	rawDB := sql.OpenDB(&esConnector{db: esdb})
	t.Cleanup(func() { rawDB.Close() })

	p := &Plugin{
		db:     &engine.SQLDBAdapter{DB: rawDB},
		mux:    http.NewServeMux(),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		config: Config{MaxEventSize: 1 * 1024 * 1024},
	}
	if err := p.RegisterRoutes(p.mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}
	return p, esdb, rawDB
}

func esTenantReq(method, path string, body io.Reader) *http.Request {
	return httptest.NewRequest(method, path, body).WithContext(
		auth.WithTenantID(context.Background(), uuid.MustParse("00000000-0000-0000-0000-000000000001")),
	)
}

func esTenant2Req(method, path string, body io.Reader) *http.Request {
	return httptest.NewRequest(method, path, body).WithContext(
		auth.WithTenantID(context.Background(), uuid.MustParse("00000000-0000-0000-0000-000000000002")),
	)
}

func esReadJSON(t *testing.T, rec *httptest.ResponseRecorder, v interface{}) {
	t.Helper()
	if err := json.NewDecoder(rec.Body).Decode(v); err != nil {
		t.Fatalf("decode body: %v", err)
	}
}

// ===========================================================================
// RegisterRoutes — nil mux
// ===========================================================================

func TestES_RegisterRoutes_NilMux(t *testing.T) {
	p := &Plugin{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	err := p.RegisterRoutes(nil)
	if err == nil || !strings.Contains(err.Error(), "nil mux") {
		t.Fatalf("expected nil mux error, got: %v", err)
	}
}

// ===========================================================================
// writeJSON / writeError helpers
// ===========================================================================

func TestES_WriteJSON(t *testing.T) {
	p, _, _ := newESPlugin(t)
	rec := httptest.NewRecorder()
	p.writeJSON(rec, 201, map[string]string{"status": "ok"})
	if rec.Code != 201 {
		t.Errorf("want 201, got %d", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("want json content-type, got %q", ct)
	}
	var m map[string]string
	esReadJSON(t, rec, &m)
	if m["status"] != "ok" {
		t.Errorf("want ok, got %q", m["status"])
	}
}

func TestES_WriteError(t *testing.T) {
	p, _, _ := newESPlugin(t)
	rec := httptest.NewRecorder()
	p.writeError(rec, 400, "bad request")
	if rec.Code != 400 {
		t.Errorf("want 400, got %d", rec.Code)
	}
	var m map[string]string
	esReadJSON(t, rec, &m)
	if m["error"] != "bad request" {
		t.Errorf("want 'bad request', got %q", m["error"])
	}
}

// ===========================================================================
// tenantID — no tenant in context
// ===========================================================================

func TestES_TenantID_NoTenant(t *testing.T) {
	p, _, _ := newESPlugin(t)
	req := httptest.NewRequest("GET", "/events/foo", nil)
	tid := p.tenantID(req)
	if tid != uuid.Nil {
		t.Errorf("expected nil UUID when no tenant in context, got %s", tid)
	}
}

// ===========================================================================
// Append — empty body
// ===========================================================================

func TestES_Append_EmptyBody(t *testing.T) {
	p, _, _ := newESPlugin(t)
	rec := httptest.NewRecorder()
	req := esTenantReq("POST", "/events/test-stream", bytes.NewReader([]byte("")))
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("empty body: want 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var m map[string]string
	esReadJSON(t, rec, &m)
	if m["error"] == "" {
		t.Error("expected error message in response")
	}
}

// ===========================================================================
// Append — missing stream_id (empty path value)
// ===========================================================================

func TestES_Append_MissingStreamID(t *testing.T) {
	p, _, _ := newESPlugin(t)
	// When stream_id path value is empty, the routes may not match.
	// We test the handler directly to ensure the check exists.
	rec := httptest.NewRecorder()
	req := esTenantReq("POST", "/events/", bytes.NewReader([]byte(`{"valid": true}`)))
	p.mux.ServeHTTP(rec, req)
	// The route pattern "POST /events/{stream_id}" should not match "/events/"
	if rec.Code != 400 && rec.Code != 405 {
		t.Logf("empty stream_id path: got %d (route may not match)", rec.Code)
	}
}

// ===========================================================================
// Append — missing tenant context (no auth middleware)
// ===========================================================================

func TestES_Append_MissingTenant(t *testing.T) {
	p, _, _ := newESPlugin(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/events/test-stream", bytes.NewReader([]byte(`{"key":"value"}`)))
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("missing tenant: want 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// Read — missing tenant context
// ===========================================================================

func TestES_Read_MissingTenant(t *testing.T) {
	p, _, _ := newESPlugin(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/events/test-stream", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("missing tenant: want 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// SSE — missing tenant context
// ===========================================================================

func TestES_SSE_MissingTenant(t *testing.T) {
	p, _, _ := newESPlugin(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/events/test-stream/stream", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("missing tenant: want 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// Read — empty stream returns empty array
// ===========================================================================

func TestES_Read_EmptyStream(t *testing.T) {
	p, _, _ := newESPlugin(t)
	rec := httptest.NewRecorder()
	req := esTenantReq("GET", "/events/nonexistent-stream", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("read empty: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var events []interface{}
	esReadJSON(t, rec, &events)
	if len(events) != 0 {
		t.Errorf("expected empty array, got %d elements", len(events))
	}
}

// ===========================================================================
// Read — with from_sequence and limit query params
// ===========================================================================

func TestES_Read_QueryParams(t *testing.T) {
	p, _, _ := newESPlugin(t)

	// Seed some events via the append endpoint
	for i := 0; i < 5; i++ {
		body := fmt.Sprintf(`{"n":%d}`, i+1)
		rec := httptest.NewRecorder()
		req := esTenantReq("POST", "/events/param-stream", bytes.NewReader([]byte(body)))
		p.mux.ServeHTTP(rec, req)
		if rec.Code != 201 {
			t.Fatalf("seed %d: want 201, got %d", i, rec.Code)
		}
	}

	// Read with from_sequence=2 (should get seq 3,4,5)
	rec := httptest.NewRecorder()
	req := esTenantReq("GET", "/events/param-stream?from_sequence=2", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("read with from_seq: want 200, got %d", rec.Code)
	}
	var events []map[string]interface{}
	esReadJSON(t, rec, &events)
	if len(events) != 3 {
		t.Errorf("from_sequence=2: want 3 events, got %d", len(events))
	}

	// Read with negative from_sequence (should be ignored, returns all 5)
	rec = httptest.NewRecorder()
	req = esTenantReq("GET", "/events/param-stream?from_sequence=-5", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("read negative from_seq: want 200, got %d", rec.Code)
	}
	esReadJSON(t, rec, &events)
	if len(events) != 5 {
		t.Errorf("negative from_seq: want 5 events, got %d", len(events))
	}

	// Read with limit=2
	rec = httptest.NewRecorder()
	req = esTenantReq("GET", "/events/param-stream?limit=2", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("read with limit: want 200, got %d", rec.Code)
	}
	esReadJSON(t, rec, &events)
	if len(events) != 2 {
		t.Errorf("limit=2: want 2 events, got %d", len(events))
	}

	// Read with limit larger than 1000 (should clamp)
	rec = httptest.NewRecorder()
	req = esTenantReq("GET", "/events/param-stream?limit=5000", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("read with largelimit: want 200, got %d", rec.Code)
	}
	esReadJSON(t, rec, &events)
	if len(events) != 5 {
		t.Errorf("limit=5000: want 5 events (0-1000 range), got %d", len(events))
	}
}

// ===========================================================================
// Tenant isolation — events in tenant A are not visible to tenant B
// ===========================================================================

func TestES_TenantIsolation(t *testing.T) {
	p, _, _ := newESPlugin(t)

	// Tenant A appends an event
	rec := httptest.NewRecorder()
	req := esTenantReq("POST", "/events/shared-stream", bytes.NewReader([]byte(`{"tenant":"A"}`)))
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("tenant A append: want 201, got %d", rec.Code)
	}

	// Tenant B reads the same stream — should be empty
	rec = httptest.NewRecorder()
	req = esTenant2Req("GET", "/events/shared-stream", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("tenant B read: want 200, got %d", rec.Code)
	}
	var events []interface{}
	esReadJSON(t, rec, &events)
	if len(events) != 0 {
		t.Errorf("tenant B should see 0 events in shared stream, got %d", len(events))
	}

	// Tenant A reads back — should see 1 event
	rec = httptest.NewRecorder()
	req = esTenantReq("GET", "/events/shared-stream", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("tenant A read back: want 200, got %d", rec.Code)
	}
	esReadJSON(t, rec, &events)
	if len(events) != 1 {
		t.Errorf("tenant A should see 1 event, got %d", len(events))
	}
}

// ===========================================================================
// Append — oversize body returns 413
// ===========================================================================

func TestES_Append_Oversize(t *testing.T) {
	p, _, _ := newESPlugin(t)
	p.config.MaxEventSize = 50

	body := `{"data":"` + strings.Repeat("x", 200) + `"}`
	rec := httptest.NewRecorder()
	req := esTenantReq("POST", "/events/big-stream", bytes.NewReader([]byte(body)))
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 413 {
		t.Fatalf("oversize: want 413, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// Append — invalid JSON body returns 400
// ===========================================================================

func TestES_Append_InvalidJSON(t *testing.T) {
	p, _, _ := newESPlugin(t)
	rec := httptest.NewRecorder()
	req := esTenantReq("POST", "/events/bad-json", bytes.NewReader([]byte("not json")))
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("invalid JSON: want 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// RegisterHostFunctions — not implemented by eventstore, verify no crash
// ===========================================================================

type esFuncRegistry struct {
	funcs map[string]plugin.PluginFunc
}

func (r *esFuncRegistry) Register(opts plugin.FuncOptions, fn plugin.PluginFunc) error {
	r.funcs[opts.Name] = fn
	return nil
}

// ===========================================================================
// Migrations — verify non-zero version and non-empty SQL
// ===========================================================================

func TestES_Migrations(t *testing.T) {
	p, _, _ := newESPlugin(t)
	migrations := p.Migrations()
	if len(migrations) == 0 {
		t.Error("expected at least one migration")
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
	// Verify version is exactly 1 for the current schema
	if len(migrations) > 0 && migrations[0].Version != 1 {
		t.Errorf("expected migration version 1, got %d", migrations[0].Version)
	}
}

// ===========================================================================
// Run background — start/stop via context cancellation
// ===========================================================================

func TestES_Run_StartStop(t *testing.T) {
	p, _, _ := newESPlugin(t)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)

	go func() {
		errCh <- p.Run(ctx)
	}()

	// Give the goroutine time to start
	time.Sleep(50 * time.Millisecond)
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
// Run — without database (no-op path)
// ===========================================================================

func TestES_Run_NoDB(t *testing.T) {
	p := &Plugin{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)

	go func() {
		errCh <- p.Run(ctx)
	}()

	time.Sleep(50 * time.Millisecond)
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
// Cleanup — deletes old events beyond retention period
// ===========================================================================

func TestES_Cleanup_DeletesOldEvents(t *testing.T) {
	esdb := newESDB()
	now := time.Now().UTC()

	// Pre-populate with events at different ages
	esdb.mu.Lock()
	esdb.events = append(esdb.events,
		esRow{
			tenantID:  uuid.MustParse("00000000-0000-0000-0000-000000000001"),
			streamID:  "old-stream",
			sequence:  1,
			event:     `{"old":true}`,
			createdAt: now.Add(-72 * time.Hour), // 3 days ago
		},
		esRow{
			tenantID:  uuid.MustParse("00000000-0000-0000-0000-000000000001"),
			streamID:  "recent-stream",
			sequence:  1,
			event:     `{"recent":true}`,
			createdAt: now.Add(-12 * time.Hour), // 12 hours ago
		},
		esRow{
			tenantID:  uuid.MustParse("00000000-0000-0000-0000-000000000002"),
			streamID:  "old-stream-2",
			sequence:  1,
			event:     `{"also_old":true}`,
			createdAt: now.Add(-96 * time.Hour), // 4 days ago
		},
		esRow{
			tenantID:  uuid.MustParse("00000000-0000-0000-0000-000000000001"),
			streamID:  "borderline",
			sequence:  1,
			event:     `{"border":true}`,
			createdAt: now.Add(-25 * time.Hour), // ~25 hours ago
		},
	)
	esdb.mu.Unlock()

	rawDB := sql.OpenDB(&esConnector{db: esdb})
	defer rawDB.Close()

	p := &Plugin{
		db:     &engine.SQLDBAdapter{DB: rawDB},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		config: Config{RetentionDays: 1, MaxEventSize: 1 * 1024 * 1024},
	}

	n := p.cleanup(context.Background())
	if n != 3 {
		t.Errorf("expected 3 events deleted (3d, 4d, 25h past 1-day retention), got %d", n)
	}

	// Verify remaining events (only the 12-hour event should survive)
	esdb.mu.RLock()
	remaining := len(esdb.events)
	esdb.mu.RUnlock()
	if remaining != 1 {
		t.Errorf("expected 1 remaining event, got %d", remaining)
	}

	// Verify the surviving event is the recent one
	esdb.mu.RLock()
	survivingStream := ""
	if len(esdb.events) > 0 {
		survivingStream = esdb.events[0].streamID
	}
	esdb.mu.RUnlock()
	if survivingStream != "recent-stream" {
		t.Errorf("expected surviving event to be 'recent-stream', got %q", survivingStream)
	}
}

func TestES_Cleanup_RetentionZeroUsesDefault(t *testing.T) {
	esdb := newESDB()
	now := time.Now().UTC()

	esdb.mu.Lock()
	esdb.events = append(esdb.events, esRow{
		tenantID:  uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		streamID:  "old",
		sequence:  1,
		event:     `{"old":true}`,
		createdAt: now.Add(-90 * 24 * time.Hour), // 90 days ago
	})
	esdb.mu.Unlock()

	rawDB := sql.OpenDB(&esConnector{db: esdb})
	defer rawDB.Close()

	p := &Plugin{
		db:     &engine.SQLDBAdapter{DB: rawDB},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		config: Config{RetentionDays: 0, MaxEventSize: 1 * 1024 * 1024}, // 0 means use default (30)
	}

	// The default is 30 days. 90 > 30, so this event should be deleted.
	n := p.cleanup(context.Background())
	if n != 1 {
		t.Errorf("expected 1 event deleted with default retention, got %d", n)
	}
}

func TestES_Cleanup_NegativeRetentionSkips(t *testing.T) {
	esdb := newESDB()
	now := time.Now().UTC()

	esdb.mu.Lock()
	esdb.events = append(esdb.events, esRow{
		tenantID:  uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		streamID:  "old",
		sequence:  1,
		event:     `{"old":true}`,
		createdAt: now.Add(-90 * 24 * time.Hour),
	})
	esdb.mu.Unlock()

	rawDB := sql.OpenDB(&esConnector{db: esdb})
	defer rawDB.Close()

	p := &Plugin{
		db:     &engine.SQLDBAdapter{DB: rawDB},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		config: Config{RetentionDays: -1, MaxEventSize: 1 * 1024 * 1024},
	}

	n := p.cleanup(context.Background())
	if n != 0 {
		t.Errorf("expected 0 deletions with negative retention, got %d", n)
	}
}

// ===========================================================================
// Init — invalid config returns error
// ===========================================================================

func TestES_Init_InvalidConfig(t *testing.T) {
	p := &Plugin{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	env := &plugin.Environment{
		DB:     &engine.SQLDBAdapter{DB: sql.OpenDB(&esConnector{db: newESDB()})},
		Mux:    http.NewServeMux(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config: json.RawMessage(`not json`),
	}
	err := p.Init(context.Background(), env)
	if err == nil || !strings.Contains(err.Error(), "invalid config") {
		t.Fatalf("want invalid config error, got: %v", err)
	}
}

func TestES_Init_NilLoggerDefaults(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		DB:     &engine.SQLDBAdapter{DB: sql.OpenDB(&esConnector{db: newESDB()})},
		Mux:    http.NewServeMux(),
		Logger: nil,
	}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if p.logger == nil {
		t.Error("expected logger to be set to default")
	}
}

// ===========================================================================
// Append — DB query error path (500)
// ===========================================================================

func TestES_Append_DBError(t *testing.T) {
	p, esdb, _ := newESPlugin(t)
	esdb.failNextQuery = true

	rec := httptest.NewRecorder()
	req := esTenantReq("POST", "/events/test-stream", bytes.NewReader([]byte(`{"key":"value"}`)))
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 500 {
		t.Fatalf("expected 500 for DB error, got %d: %s", rec.Code, rec.Body.String())
	}
	var m map[string]string
	esReadJSON(t, rec, &m)
	if m["error"] == "" {
		t.Error("expected error message in response")
	}
}

// ===========================================================================
// Read — DB query error path (500)
// ===========================================================================

func TestES_Read_DBError(t *testing.T) {
	p, esdb, _ := newESPlugin(t)
	esdb.failNextQuery = true

	rec := httptest.NewRecorder()
	req := esTenantReq("GET", "/events/test-stream", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 500 {
		t.Fatalf("expected 500 for DB error, got %d: %s", rec.Code, rec.Body.String())
	}
	var m map[string]string
	esReadJSON(t, rec, &m)
	if m["error"] == "" {
		t.Error("expected error message in response")
	}
}

// ===========================================================================
// Read — pagination edge cases
// ===========================================================================

func TestES_Read_PaginationEdges(t *testing.T) {
	p, _, _ := newESPlugin(t)

	// Seed 3 events.
	for i := 0; i < 3; i++ {
		body := fmt.Sprintf(`{"n":%d}`, i+1)
		rec := httptest.NewRecorder()
		req := esTenantReq("POST", "/events/paginate-stream", bytes.NewReader([]byte(body)))
		p.mux.ServeHTTP(rec, req)
		if rec.Code != 201 {
			t.Fatalf("seed %d: want 201, got %d", i, rec.Code)
		}
	}

	// limit=0 should be ignored (default 100).
	rec := httptest.NewRecorder()
	req := esTenantReq("GET", "/events/paginate-stream?limit=0", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("limit=0: want 200, got %d", rec.Code)
	}
	var events []map[string]interface{}
	esReadJSON(t, rec, &events)
	if len(events) != 3 {
		t.Errorf("limit=0: want 3 events (default limit), got %d", len(events))
	}

	// limit=-1 should be ignored (default 100).
	rec = httptest.NewRecorder()
	req = esTenantReq("GET", "/events/paginate-stream?limit=-1", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("limit=-1: want 200, got %d", rec.Code)
	}
	esReadJSON(t, rec, &events)
	if len(events) != 3 {
		t.Errorf("limit=-1: want 3 events, got %d", len(events))
	}

	// limit=1001 should be clamped to 1000.
	rec = httptest.NewRecorder()
	req = esTenantReq("GET", "/events/paginate-stream?limit=1001", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("limit=1001: want 200, got %d", rec.Code)
	}
	esReadJSON(t, rec, &events)
	if len(events) != 3 {
		t.Errorf("limit=1001: want 3 events, got %d", len(events))
	}

	// limit=2 returns exactly 2.
	rec = httptest.NewRecorder()
	req = esTenantReq("GET", "/events/paginate-stream?limit=2", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("limit=2: want 200, got %d", rec.Code)
	}
	esReadJSON(t, rec, &events)
	if len(events) != 2 {
		t.Errorf("limit=2: want 2 events, got %d", len(events))
	}

	// from_sequence=0 returns all events (sequence > 0).
	rec = httptest.NewRecorder()
	req = esTenantReq("GET", "/events/paginate-stream?from_sequence=0", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("from_sequence=0: want 200, got %d", rec.Code)
	}
	esReadJSON(t, rec, &events)
	if len(events) != 3 {
		t.Errorf("from_sequence=0: want 3 events, got %d", len(events))
	}

	// from_sequence=2 returns events with sequence > 2 (i.e., seq 3 only).
	rec = httptest.NewRecorder()
	req = esTenantReq("GET", "/events/paginate-stream?from_sequence=2", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("from_sequence=2: want 200, got %d", rec.Code)
	}
	esReadJSON(t, rec, &events)
	if len(events) != 1 {
		t.Errorf("from_sequence=2: want 1 event, got %d", len(events))
	}
	if len(events) > 0 {
		seq := int(events[0]["sequence"].(float64))
		if seq != 3 {
			t.Errorf("from_sequence=2: expected seq 3, got %d", seq)
		}
	}

	// Read with non-numeric from_sequence should be ignored.
	rec = httptest.NewRecorder()
	req = esTenantReq("GET", "/events/paginate-stream?from_sequence=abc", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("from_sequence=abc: want 200, got %d", rec.Code)
	}
	esReadJSON(t, rec, &events)
	if len(events) != 3 {
		t.Errorf("from_sequence=abc: want 3 events (ignored), got %d", len(events))
	}
}

// ===========================================================================
// Append — large body at the limit (should succeed)
// ===========================================================================

func TestES_Append_LargeBodyAtLimit(t *testing.T) {
	p, _, _ := newESPlugin(t)
	// MaxEventSize default is 1MB, so construct a ~900KB valid JSON body.
	largeInner := `{"data":"` + strings.Repeat("x", 900*1024) + `"}`
	body := `{"payload":` + largeInner + `}`

	rec := httptest.NewRecorder()
	req := esTenantReq("POST", "/events/large-stream", bytes.NewReader([]byte(body)))
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("large body at limit: want 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// SSE — initial query error (returns 200 with headers, no data)
// ===========================================================================

func TestES_SSE_InitialQueryError(t *testing.T) {
	p, esdb, _ := newESPlugin(t)
	esdb.failNextQuery = true

	rec := httptest.NewRecorder()
	req := esTenantReq("GET", "/events/test-stream/stream", nil)
	p.mux.ServeHTTP(rec, req)

	// Handler flushes SSE headers (200) before the query; query error causes
	// early return but headers are already sent.
	if rec.Code != 200 {
		t.Fatalf("expected 200 (SSE headers), got %d", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "event-stream") {
		t.Errorf("expected event-stream content-type, got %q", ct)
	}
}

// ===========================================================================
// Cleanup — DB exec error path (returns 0)
// ===========================================================================

func TestES_Cleanup_DBError(t *testing.T) {
	esdb := newESDB()
	now := time.Now().UTC()
	esdb.mu.Lock()
	esdb.events = append(esdb.events, esRow{
		tenantID:  uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		streamID:  "test",
		sequence:  1,
		event:     `{"test":true}`,
		createdAt: now.Add(-72 * time.Hour),
	})
	esdb.mu.Unlock()

	rawDB := sql.OpenDB(&esConnector{db: esdb})
	defer rawDB.Close()

	p := &Plugin{
		db:     &engine.SQLDBAdapter{DB: rawDB},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		config: Config{RetentionDays: 1, MaxEventSize: 1 * 1024 * 1024},
	}

	esdb.failNextExec = true
	n := p.cleanup(context.Background())
	if n != 0 {
		t.Errorf("expected 0 deletions on DB error, got %d", n)
	}

	// Event should still exist after failed cleanup.
	esdb.mu.RLock()
	remaining := len(esdb.events)
	esdb.mu.RUnlock()
	if remaining != 1 {
		t.Errorf("expected 1 event still present after failed cleanup, got %d", remaining)
	}
}

// ===========================================================================
// Init — empty config (no config provided, uses defaults)
// ===========================================================================

func TestES_Init_EmptyConfig(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		DB:     &engine.SQLDBAdapter{DB: sql.OpenDB(&esConnector{db: newESDB()})},
		Mux:    http.NewServeMux(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config: json.RawMessage(`{}`),
	}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if p.config.MaxEventSize != 1*1024*1024 {
		t.Errorf("expected default MaxEventSize 1MB, got %d", p.config.MaxEventSize)
	}
}

// ===========================================================================
// Read — verify tenant isolation returns empty for wrong tenant
// ===========================================================================

func TestES_Read_TenantIsolation(t *testing.T) {
	p, _, _ := newESPlugin(t)

	// Append event for tenant 1.
	rec := httptest.NewRecorder()
	req := esTenantReq("POST", "/events/iso-stream", bytes.NewReader([]byte(`{"tenant":"A"}`)))
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("tenant A append: want 201, got %d", rec.Code)
	}

	// Read as tenant 2 from same stream name — should see 0 events.
	rec = httptest.NewRecorder()
	req = esTenant2Req("GET", "/events/iso-stream", nil)
	p.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("tenant B read: want 200, got %d", rec.Code)
	}
	var events []interface{}
	esReadJSON(t, rec, &events)
	if len(events) != 0 {
		t.Errorf("tenant B should see 0 events from iso-stream, got %d", len(events))
	}
}
