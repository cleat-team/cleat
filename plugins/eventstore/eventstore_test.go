// Package eventstore tests the append-only event stream plugin with an in-memory
// fake database, avoiding any need for PostgreSQL.
package eventstore

import (
	"bufio"
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
// In-memory event store (replaces PostgreSQL entirely for testing)
// ---------------------------------------------------------------------------

type eventRow struct {
	tenantID  uuid.UUID
	streamID  string
	sequence  int64
	event     []byte
	createdAt time.Time
}

type fakeEventStore struct {
	mu      sync.RWMutex
	events  []eventRow // appended in order
	apiKeys map[string]string // key_hash_hex -> tenant_id string
}

func newFakeEventStore() *fakeEventStore {
	return &fakeEventStore{
		apiKeys: make(map[string]string),
	}
}

// ---------------------------------------------------------------------------
// Fake SQL driver
// ---------------------------------------------------------------------------

type fakeConnector struct {
	store *fakeEventStore
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
	store *fakeEventStore
}

func (*fakeConn) Prepare(_ string) (driver.Stmt, error) {
	return nil, fmt.Errorf("fakeConn: unexpected Prepare call")
}

func (*fakeConn) Close() error { return nil }
func (*fakeConn) Begin() (driver.Tx, error) { return &fakeTx{}, nil }

type fakeTx struct{}

func (*fakeTx) Commit() error   { return nil }
func (*fakeTx) Rollback() error { return nil }

// --- ExecContext ---
// (eventstore only uses QueryRowContext / QueryContext, no ExecContext needed)

func (c *fakeConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return nil, fmt.Errorf("fakeConn: unexpected Exec query: %s", query)
}

// --- QueryContext ---

func (c *fakeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	switch {
	case strings.Contains(query, "tenant_api_keys"):
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryTenantLookup(args)
	case strings.Contains(query, "INSERT INTO event_stream"):
		c.store.mu.Lock()
		defer c.store.mu.Unlock()
		return c.queryAppend(args)
	case strings.Contains(query, "COALESCE(MAX(sequence)"):
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryMaxSeq(args)
	case strings.Contains(query, "event, created_at"):
		// Read events query: SELECT sequence, event, created_at ...
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.queryReadEvents(args)
	case strings.Contains(query, "sequence >"):
		// SSE poll query: SELECT sequence, event ... WHERE sequence > $3
		c.store.mu.RLock()
		defer c.store.mu.RUnlock()
		return c.querySSEPoll(args)
	default:
		return nil, fmt.Errorf("fakeConn: unexpected Query query: %s", query)
	}
}

// --- Query implementations ---

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

func (c *fakeConn) queryAppend(args []driver.NamedValue) (driver.Rows, error) {
	tidStr, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	tid, err := uuid.Parse(tidStr)
	if err != nil {
		return nil, err
	}
	streamID, err := argString(args, 2)
	if err != nil {
		return nil, err
	}
	eventBody, err := argString(args, 3)
	if err != nil {
		return nil, err
	}

	// Compute next sequence number.
	var maxSeq int64
	for _, e := range c.store.events {
		if e.tenantID == tid && e.streamID == streamID && e.sequence > maxSeq {
			maxSeq = e.sequence
		}
	}
	sequence := maxSeq + 1

	now := time.Now().UTC()
	c.store.events = append(c.store.events, eventRow{
		tenantID:  tid,
		streamID:  streamID,
		sequence:  sequence,
		event:     []byte(eventBody),
		createdAt: now,
	})

	return &fakeRows{
		columns: []string{"sequence"},
		data:    [][]driver.Value{{sequence}},
	}, nil
}

func (c *fakeConn) queryMaxSeq(args []driver.NamedValue) (driver.Rows, error) {
	tidStr, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	tid, err := uuid.Parse(tidStr)
	if err != nil {
		return nil, err
	}
	streamID, err := argString(args, 2)
	if err != nil {
		return nil, err
	}

	var maxSeq int64
	for _, e := range c.store.events {
		if e.tenantID == tid && e.streamID == streamID && e.sequence > maxSeq {
			maxSeq = e.sequence
		}
	}

	return &fakeRows{
		columns: []string{"coalesce"},
		data:    [][]driver.Value{{maxSeq}},
	}, nil
}

func (c *fakeConn) queryReadEvents(args []driver.NamedValue) (driver.Rows, error) {
	tidStr, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	tid, err := uuid.Parse(tidStr)
	if err != nil {
		return nil, err
	}
	streamID, err := argString(args, 2)
	if err != nil {
		return nil, err
	}
	fromSeq, err := argInt64(args, 3)
	if err != nil {
		return nil, err
	}
	limit, err := argInt64(args, 4)
	if err != nil {
		return nil, err
	}

	var result []eventRow
	for _, e := range c.store.events {
		if e.tenantID == tid && e.streamID == streamID && e.sequence > fromSeq {
			result = append(result, e)
		}
	}

	// Sort by sequence and apply limit.
	sortEventsBySequence(result)
	if int64(len(result)) > limit {
		result = result[:limit]
	}

	columns := []string{"sequence", "event", "created_at"}
	var data [][]driver.Value
	for _, e := range result {
		data = append(data, []driver.Value{
			e.sequence,
			e.event,
			e.createdAt,
		})
	}
	return &fakeRows{columns: columns, data: data}, nil
}

func (c *fakeConn) querySSEPoll(args []driver.NamedValue) (driver.Rows, error) {
	tidStr, err := argString(args, 1)
	if err != nil {
		return nil, err
	}
	tid, err := uuid.Parse(tidStr)
	if err != nil {
		return nil, err
	}
	streamID, err := argString(args, 2)
	if err != nil {
		return nil, err
	}
	fromSeq, err := argInt64(args, 3)
	if err != nil {
		return nil, err
	}

	var data [][]driver.Value
	for _, e := range c.store.events {
		if e.tenantID == tid && e.streamID == streamID && e.sequence > fromSeq {
			data = append(data, []driver.Value{
				e.sequence,
				e.event,
			})
		}
	}

	sortEventsBySequenceData(data)

	return &fakeRows{
		columns: []string{"sequence", "event"},
		data:    data,
	}, nil
}

func sortEventsBySequence(events []eventRow) {
	for i := 0; i < len(events); i++ {
		for j := i + 1; j < len(events); j++ {
			if events[j].sequence < events[i].sequence {
				events[i], events[j] = events[j], events[i]
			}
		}
	}
}

func sortEventsBySequenceData(data [][]driver.Value) {
	for i := 0; i < len(data); i++ {
		for j := i + 1; j < len(data); j++ {
			si, _ := data[i][0].(int64)
			sj, _ := data[j][0].(int64)
			if sj < si {
				data[i], data[j] = data[j], data[i]
			}
		}
	}
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
				return "", fmt.Errorf("arg %d: want string or []byte, got %T", ordinal, a.Value)
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
// Test setup helpers
// ---------------------------------------------------------------------------

var testTenantID = uuid.MustParse("00000000-0000-0000-0000-000000000001")

const testAPIKey = "test-api-key"

func setupTestPlugin(t *testing.T) (*Plugin, http.Handler, *fakeEventStore) {
	t.Helper()

	store := newFakeEventStore()

	// Pre-populate tenant API key so auth middleware succeeds.
	keyHash := sha256.Sum256([]byte(testAPIKey))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = testTenantID.String()

	db := sql.OpenDB(&fakeConnector{store: store})
	t.Cleanup(func() { db.Close() })

	p := &Plugin{
		db:     db,
		mux:    http.NewServeMux(),
		logger: slog.Default(),
		config: Config{MaxEventSize: 1 * 1024 * 1024},
	}

	if err := p.RegisterRoutes(p.mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	handler := auth.Middleware(db)(p.mux)
	return p, handler, store
}

func authedRequest(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	return req
}

// ---------------------------------------------------------------------------
// Existing tests
// ---------------------------------------------------------------------------

func TestInfo(t *testing.T) {
	p := &Plugin{}
	info := p.Info()
	if info.Name != "eventstore" {
		t.Errorf("expected name 'eventstore', got %q", info.Name)
	}
	if info.Version != "0.1.0" {
		t.Errorf("expected version '0.1.0', got %q", info.Version)
	}
	if info.Description != "Append-only event streams with SSE" {
		t.Errorf("unexpected description: %q", info.Description)
	}
}

func TestInit(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		DB:     &sql.DB{},
		Mux:    http.NewServeMux(),
		Config: []byte(`{"max_event_size": 2097152}`),
	}

	err := p.Init(context.Background(), env)
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	if p.db == nil {
		t.Error("expected db to be set")
	}
	if p.mux == nil {
		t.Error("expected mux to be set")
	}
	if p.logger == nil {
		t.Error("expected logger to be set")
	}
	if p.config.MaxEventSize != 2097152 {
		t.Errorf("expected MaxEventSize 2097152, got %d", p.config.MaxEventSize)
	}
}

func TestInitWithDefaults(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		DB:  &sql.DB{},
		Mux: http.NewServeMux(),
	}

	err := p.Init(context.Background(), env)
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	if p.config.MaxEventSize != 1*1024*1024 {
		t.Errorf("expected default MaxEventSize %d, got %d", 1*1024*1024, p.config.MaxEventSize)
	}
}

func TestRegistered(t *testing.T) {
	infos := plugin.List()
	found := false
	for _, info := range infos {
		if info.Name == "eventstore" {
			found = true
			if info.Version != "0.1.0" {
				t.Errorf("expected version 0.1.0, got %q", info.Version)
			}
			break
		}
	}
	if !found {
		t.Error("eventstore plugin not found in registry")
	}
}

// ---------------------------------------------------------------------------
// Behavioral tests
// ---------------------------------------------------------------------------

// TestAppendAndRead appends a single event then reads the stream back,
// verifying the event content and sequence number.
func TestAppendAndRead(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)
	eventBody := `{"type":"user.created","user_id":"u1"}`

	// POST /events/test-stream
	req := authedRequest("POST", "/events/test-stream", bytes.NewReader([]byte(eventBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("APPEND: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var appendResp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &appendResp); err != nil {
		t.Fatalf("APPEND: failed to decode: %v", err)
	}
	if appendResp["stream_id"] != "test-stream" {
		t.Errorf("APPEND: expected stream_id 'test-stream', got %q", appendResp["stream_id"])
	}
	if seq, ok := appendResp["sequence"].(float64); !ok || seq != 1 {
		t.Errorf("APPEND: expected sequence 1, got %v", appendResp["sequence"])
	}

	// GET /events/test-stream
	req = authedRequest("GET", "/events/test-stream", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("READ: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var events []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &events); err != nil {
		t.Fatalf("READ: failed to decode: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("READ: expected 1 event, got %d", len(events))
	}
	if events[0]["sequence"].(float64) != 1 {
		t.Errorf("READ: expected sequence 1, got %v", events[0]["sequence"])
	}
}

// TestMultipleEvents appends several events to the same stream and verifies
// that the sequence numbers are monotonically increasing.
func TestMultipleEvents(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)
	events := []string{
		`{"seq":1}`,
		`{"seq":2}`,
		`{"seq":3}`,
	}

	for i, body := range events {
		req := authedRequest("POST", "/events/multi-stream", bytes.NewReader([]byte(body)))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("APPEND %d: expected 201, got %d: %s", i, rec.Code, rec.Body.String())
		}
		var resp map[string]interface{}
		json.Unmarshal(rec.Body.Bytes(), &resp)
		if resp["sequence"].(float64) != float64(i+1) {
			t.Errorf("APPEND %d: expected sequence %d, got %v", i, i+1, resp["sequence"])
		}
	}

	// Read back.
	req := authedRequest("GET", "/events/multi-stream", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var results []map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &results)
	if len(results) != 3 {
		t.Fatalf("READ: expected 3 events, got %d", len(results))
	}
	for i := 0; i < 3; i++ {
		seq := int(results[i]["sequence"].(float64))
		if seq != i+1 {
			t.Errorf("READ: event %d: expected sequence %d, got %d", i, i+1, seq)
		}
	}
}

// TestReadFromSequence verifies the from_sequence query parameter.
func TestReadFromSequence(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	// Append 3 events.
	for i := 1; i <= 3; i++ {
		body := fmt.Sprintf(`{"n":%d}`, i)
		req := authedRequest("POST", "/events/seq-stream", bytes.NewReader([]byte(body)))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("APPEND %d: expected 201, got %d", i, rec.Code)
		}
	}

	// Read with from_sequence=1 (sequence > 1, so events 2 and 3).
	req := authedRequest("GET", "/events/seq-stream?from_sequence=1", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var events []map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &events)
	if len(events) != 2 {
		t.Fatalf("READ from_seq=1: expected 2 events, got %d: %+v", len(events), events)
	}
	if int(events[0]["sequence"].(float64)) != 2 {
		t.Errorf("expected first event sequence 2, got %v", events[0]["sequence"])
	}
	if int(events[1]["sequence"].(float64)) != 3 {
		t.Errorf("expected second event sequence 3, got %v", events[1]["sequence"])
	}

	// Read with from_sequence=3 -> only event after seq 3 (none).
	req = authedRequest("GET", "/events/seq-stream?from_sequence=3", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("READ from_seq=3: expected 200, got %d", rec.Code)
	}
	json.Unmarshal(rec.Body.Bytes(), &events)
	if len(events) != 0 {
		t.Errorf("READ from_seq=3: expected 0 events, got %d: %+v", len(events), events)
	}
}

// TestEmptyStream verifies that reading from a stream with no events
// returns an empty JSON array.
func TestEmptyStream(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	req := authedRequest("GET", "/events/empty-stream", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("READ empty stream: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var events []interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &events); err != nil {
		t.Fatalf("READ: failed to decode: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("READ empty stream: expected empty array, got %d elements", len(events))
	}
}

// TestAppendExceedsMaxSize verifies that appending an event larger than
// MaxEventSize returns 413.
func TestAppendExceedsMaxSize(t *testing.T) {
	p, handler, _ := setupTestPlugin(t)
	p.config.MaxEventSize = 20 // tiny limit

	largeBody := `{"data":"` + strings.Repeat("x", 100) + `"}`
	if len(largeBody) <= p.config.MaxEventSize {
		t.Fatal("test body must be larger than MaxEventSize for this test")
	}

	req := authedRequest("POST", "/events/big-stream", bytes.NewReader([]byte(largeBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("APPEND oversize: expected 413, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestInvalidJSONBody verifies that appending non-JSON data returns 400.
func TestInvalidJSONBody(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	req := authedRequest("POST", "/events/bad-json", bytes.NewReader([]byte("not json")))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("APPEND invalid JSON: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestSSEDelivery tests the SSE subscription endpoint. It starts a real HTTP
// server, connects an SSE client to a stream, appends an event, and verifies
// the event is delivered via the SSE stream.
func TestSSEDelivery(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	// Use a real HTTP server (needed for http.Flusher).
	server := httptest.NewServer(handler)
	defer server.Close()

	// Start SSE client in a goroutine.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sseReq, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/events/sse-stream/stream", nil)
	sseReq.Header.Set("Authorization", "Bearer "+testAPIKey)

	eventCh := make(chan string, 10)
	go func() {
		resp, err := http.DefaultClient.Do(sseReq)
		if err != nil {
			return
		}
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") {
				eventCh <- strings.TrimPrefix(line, "data: ")
			}
		}
	}()

	// Wait for SSE handler to start polling.
	time.Sleep(200 * time.Millisecond)

	// Append an event.
	appendReq, _ := http.NewRequest("POST", server.URL+"/events/sse-stream",
		bytes.NewReader([]byte(`{"sse":"test"}`)))
	appendReq.Header.Set("Authorization", "Bearer "+testAPIKey)
	appendResp, err := http.DefaultClient.Do(appendReq)
	if err != nil {
		t.Fatal(err)
	}
	appendResp.Body.Close()

	// Wait for the SSE event (the poll interval is 1s, so give 5s for CI).
	select {
	case data := <-eventCh:
		if !strings.Contains(data, `"sse":"test"`) || !strings.Contains(data, `"sequence":1`) {
			t.Errorf("SSE: unexpected event data: %s", data)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for SSE event")
	}
}

// TestUnauthenticated verifies that requests without credentials get 401.
func TestUnauthenticated(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)

	req := httptest.NewRequest("POST", "/events/test-stream", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated request, got %d", rec.Code)
	}
}

// TestAppendDuplicateEvents verifies the current behavior when the same event
// body is appended twice: each append creates a separate entry with a unique
// sequence number. This plugin does not currently implement idempotent append;
// duplicate detection would require an idempotency key (not yet supported).
func TestAppendDuplicateEvents(t *testing.T) {
	_, handler, _ := setupTestPlugin(t)
	body := `{"event":"same"}`

	// Append twice.
	for i := 0; i < 2; i++ {
		req := authedRequest("POST", "/events/dedup-stream", bytes.NewReader([]byte(body)))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("APPEND %d: expected 201, got %d", i, rec.Code)
		}
	}

	// Read back -> should have 2 events (no built-in idempotency).
	req := authedRequest("GET", "/events/dedup-stream", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var events []map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &events)
	if len(events) != 2 {
		t.Fatalf("READ: expected 2 events (current behavior, no idempotency), got %d", len(events))
	}
	if int(events[0]["sequence"].(float64)) != 1 || int(events[1]["sequence"].(float64)) != 2 {
		t.Errorf("expected sequences 1 and 2, got %v and %v",
			events[0]["sequence"], events[1]["sequence"])
	}
}
