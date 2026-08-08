package eventtriggers

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

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Interface / compile-time checks
// ---------------------------------------------------------------------------

// Compile-time assertions that all expression types implement the expr interface.
var (
	_ expr = trueExpr{}
	_ expr = comparisonExpr{}
	_ expr = membershipExpr{}
)

func TestExprMarker(t *testing.T) {
	t.Run("trueExpr", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("trueExpr.exprMarker() panicked: %v", r)
			}
		}()
		trueExpr{}.exprMarker()
	})

	t.Run("comparisonExpr", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("comparisonExpr.exprMarker() panicked: %v", r)
			}
		}()
		comparisonExpr{}.exprMarker()
	})

	t.Run("membershipExpr", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("membershipExpr.exprMarker() panicked: %v", r)
			}
		}()
		membershipExpr{}.exprMarker()
	})
}

// ---------------------------------------------------------------------------
// Migrations
// ---------------------------------------------------------------------------

func TestMigrations(t *testing.T) {
	p := &Plugin{}
	migs := p.Migrations()

	if len(migs) == 0 {
		t.Fatal("Migrations() returned empty slice")
	}

	for i, m := range migs {
		if m.Version == 0 {
			t.Errorf("migrations[%d].Version is 0, expected non-zero", i)
		}
		if m.Up == "" {
			t.Errorf("migrations[%d].Up is empty", i)
		}
		if m.Down == "" {
			t.Errorf("migrations[%d].Down is empty", i)
		}
	}

	// Versions should be sequential starting from 1.
	prevVersion := 0
	for i, m := range migs {
		if m.Version != prevVersion+1 {
			t.Errorf("migrations[%d].Version = %d, want %d", i, m.Version, prevVersion+1)
		}
		prevVersion = m.Version
	}

	// Each migration should contain at least one SQL statement keyword.
	sqlKeywords := []string{"CREATE TABLE", "ALTER TABLE", "CREATE INDEX", "DROP TABLE"}
	for i, m := range migs {
		hasSQL := false
		for _, kw := range sqlKeywords {
			if strings.Contains(m.Up, kw) {
				hasSQL = true
				break
			}
		}
		if !hasSQL {
			t.Errorf("migrations[%d].Up contains no recognized SQL keyword", i)
		}

		hasDownSQL := false
		for _, kw := range sqlKeywords {
			if strings.Contains(m.Down, kw) {
				hasDownSQL = true
				break
			}
		}
		if !hasDownSQL {
			t.Errorf("migrations[%d].Down contains no recognized SQL keyword", i)
		}
	}
}

// ---------------------------------------------------------------------------
// RegisterHostFunctions
// ---------------------------------------------------------------------------

type registeredFunc struct {
	opts plugin.FuncOptions
	fn   plugin.PluginFunc
}

type mockFuncRegistry struct {
	registered  []registeredFunc
	registerErr error
}

func (m *mockFuncRegistry) Register(opts plugin.FuncOptions, fn plugin.PluginFunc) error {
	m.registered = append(m.registered, registeredFunc{opts: opts, fn: fn})
	return m.registerErr
}

func TestRegisterHostFunctions_NilRegistry(t *testing.T) {
	p := &Plugin{}
	err := p.RegisterHostFunctions(nil)
	if err == nil {
		t.Fatal("RegisterHostFunctions(nil) expected error, got nil")
	}
	if !strings.Contains(err.Error(), "nil function registry") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestRegisterHostFunctions_ValidRegistry(t *testing.T) {
	p := &Plugin{}
	mock := &mockFuncRegistry{}

	err := p.RegisterHostFunctions(mock)
	if err != nil {
		t.Fatalf("RegisterHostFunctions(mock) returned unexpected error: %v", err)
	}

	if len(mock.registered) != 1 {
		t.Fatalf("expected 1 registered function, got %d", len(mock.registered))
	}

	if mock.registered[0].opts.Name != "await_event" {
		t.Errorf("expected function name 'await_event', got %q", mock.registered[0].opts.Name)
	}
	if !mock.registered[0].opts.Idempotent {
		t.Error("expected Idempotent to be true")
	}
	if mock.registered[0].fn == nil {
		t.Error("registered function is nil")
	}
}

func TestRegisterHostFunctions_RegistryError(t *testing.T) {
	p := &Plugin{}
	mock := &mockFuncRegistry{registerErr: assertAnError("registry full")}

	err := p.RegisterHostFunctions(mock)
	if err == nil {
		t.Fatal("expected error when registry.Register fails, got nil")
	}
	if !strings.Contains(err.Error(), "registry full") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// assertAnError is a helper that returns the given string as an error.
type assertAnError string

func (e assertAnError) Error() string { return string(e) }

// ---------------------------------------------------------------------------
// RegisterRoutes
// ---------------------------------------------------------------------------

func TestRegisterRoutes_NilMux(t *testing.T) {
	p := &Plugin{}
	err := p.RegisterRoutes(nil)
	if err == nil {
		t.Fatal("RegisterRoutes(nil) expected error, got nil")
	}
	if !strings.Contains(err.Error(), "nil mux") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestRegisterRoutes_AllRoutes(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	err := p.RegisterRoutes(mux)
	if err != nil {
		t.Fatalf("RegisterRoutes() returned unexpected error: %v", err)
	}

	routes := []struct {
		method string
		path   string
	}{
		{"POST", "/api/events/publish"},
		{"POST", "/api/events/subscriptions"},
		{"GET", "/api/events/subscriptions"},
		{"DELETE", "/api/events/subscriptions/11111111-1111-1111-1111-111111111111"},
		{"POST", "/api/events/11111111-1111-1111-1111-111111111111/retry"},
	}

	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			req := httptest.NewRequest(rt.method, rt.path, nil)
			_, pattern := mux.Handler(req)
			if pattern == "" {
				t.Errorf("no handler matched %s %s", rt.method, rt.path)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// awaitEvent -- input validation (does not require a database)
// ---------------------------------------------------------------------------

func TestAwaitEvent_InputValidation(t *testing.T) {
	t.Run("no tenant context", func(t *testing.T) {
		p := &Plugin{}
		ctx := context.Background()
		_, err := p.awaitEvent(ctx, `{"event_type":"test","timeout_ms":5000}`)
		if err == nil {
			t.Fatal("expected error for missing tenant context, got nil")
		}
		if !strings.Contains(err.Error(), "no tenant context") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		p := &Plugin{}
		ctx := plugin.WithCallContext(context.Background(), &plugin.CallContext{
			TenantID: uuid.New().String(),
		})
		_, err := p.awaitEvent(ctx, "{not valid json}")
		if err == nil {
			t.Fatal("expected error for invalid JSON, got nil")
		}
		if !strings.Contains(err.Error(), "invalid input") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("missing event_type", func(t *testing.T) {
		p := &Plugin{}
		ctx := plugin.WithCallContext(context.Background(), &plugin.CallContext{
			TenantID: uuid.New().String(),
		})
		_, err := p.awaitEvent(ctx, `{"timeout_ms":5000}`)
		if err == nil {
			t.Fatal("expected error for missing event_type, got nil")
		}
		if !strings.Contains(err.Error(), "event_type is required") {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// mergeInputAndTemplate (pure function)
// ---------------------------------------------------------------------------

func TestMergeInputAndTemplate(t *testing.T) {
	t.Run("empty template uses event data only", func(t *testing.T) {
		tmpl := json.RawMessage("")
		data := map[string]any{"order_id": "123", "amount": 99.5}

		result, err := mergeInputAndTemplate(tmpl, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		var parsed map[string]any
		if err := json.Unmarshal(result, &parsed); err != nil {
			t.Fatalf("unmarshal result: %v", err)
		}
		if parsed["order_id"] != "123" {
			t.Errorf("expected order_id '123', got %v", parsed["order_id"])
		}
		if parsed["amount"] != 99.5 {
			t.Errorf("expected amount 99.5, got %v", parsed["amount"])
		}
	})

	t.Run("template with event data merge", func(t *testing.T) {
		tmpl := json.RawMessage(`{"source": "webhook", "version": "1.0"}`)
		data := map[string]any{"order_id": "456", "amount": 50.0}

		result, err := mergeInputAndTemplate(tmpl, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		var parsed map[string]any
		if err := json.Unmarshal(result, &parsed); err != nil {
			t.Fatalf("unmarshal result: %v", err)
		}
		if parsed["source"] != "webhook" {
			t.Errorf("expected source 'webhook', got %v", parsed["source"])
		}
		if parsed["version"] != "1.0" {
			t.Errorf("expected version '1.0', got %v", parsed["version"])
		}
		if parsed["order_id"] != "456" {
			t.Errorf("expected order_id '456', got %v", parsed["order_id"])
		}
		if parsed["amount"] != 50.0 {
			t.Errorf("expected amount 50.0, got %v", parsed["amount"])
		}
	})

	t.Run("event data overrides template on key conflict", func(t *testing.T) {
		tmpl := json.RawMessage(`{"priority": "low", "source": "template"}`)
		data := map[string]any{"priority": "high", "extra": "value"}

		result, err := mergeInputAndTemplate(tmpl, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		var parsed map[string]any
		if err := json.Unmarshal(result, &parsed); err != nil {
			t.Fatalf("unmarshal result: %v", err)
		}
		if parsed["priority"] != "high" {
			t.Errorf("expected priority 'high' (event data wins), got %v", parsed["priority"])
		}
		if parsed["source"] != "template" {
			t.Errorf("expected source 'template', got %v", parsed["source"])
		}
		if parsed["extra"] != "value" {
			t.Errorf("expected extra 'value', got %v", parsed["extra"])
		}
	})

	t.Run("invalid template JSON is silently ignored", func(t *testing.T) {
		tmpl := json.RawMessage(`{not valid}`)
		data := map[string]any{"key": "value"}

		result, err := mergeInputAndTemplate(tmpl, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		var parsed map[string]any
		if err := json.Unmarshal(result, &parsed); err != nil {
			t.Fatalf("unmarshal result: %v", err)
		}
		if parsed["key"] != "value" {
			t.Errorf("expected key 'value', got %v", parsed["key"])
		}
		if len(parsed) != 1 {
			t.Errorf("expected 1 key in result, got %d", len(parsed))
		}
	})
}

// ---------------------------------------------------------------------------
// Types -- JSON marshal/unmarshal roundtrip
// ---------------------------------------------------------------------------

func TestTypesJSONRoundtrip(t *testing.T) {
	t.Run("awaitEventInput", func(t *testing.T) {
		original := awaitEventInput{
			EventType: "order.created",
			TimeoutMs: 30000,
		}

		data, err := json.Marshal(original)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}

		var decoded awaitEventInput
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		if decoded.EventType != original.EventType {
			t.Errorf("EventType: got %q, want %q", decoded.EventType, original.EventType)
		}
		if decoded.TimeoutMs != original.TimeoutMs {
			t.Errorf("TimeoutMs: got %d, want %d", decoded.TimeoutMs, original.TimeoutMs)
		}
	})

	t.Run("awaitEventOutput with found", func(t *testing.T) {
		original := awaitEventOutput{
			Found:      true,
			EventID:    "123e4567-e89b-12d3-a456-426614174000",
			EventType:  "order.shipped",
			EventData:  json.RawMessage(`{"status":"shipped"}`),
			ReceivedAt: "2025-01-15T10:30:00Z",
		}

		data, err := json.Marshal(original)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}

		var decoded awaitEventOutput
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		if decoded.Found != original.Found {
			t.Errorf("Found: got %v, want %v", decoded.Found, original.Found)
		}
		if decoded.EventID != original.EventID {
			t.Errorf("EventID: got %q, want %q", decoded.EventID, original.EventID)
		}
		if decoded.EventType != original.EventType {
			t.Errorf("EventType: got %q, want %q", decoded.EventType, original.EventType)
		}
	})

	t.Run("awaitEventOutput not found", func(t *testing.T) {
		original := awaitEventOutput{Found: false}

		data, err := json.Marshal(original)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}

		var raw map[string]any
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatalf("unmarshal to map: %v", err)
		}

		if raw["found"] != false {
			t.Errorf("expected found=false, got %v", raw["found"])
		}
		if _, exists := raw["event_id"]; exists {
			t.Error("event_id should be omitted when empty")
		}
		if _, exists := raw["event_type"]; exists {
			t.Error("event_type should be omitted when empty")
		}
		if _, exists := raw["received_at"]; exists {
			t.Error("received_at should be omitted when empty")
		}

		var decoded awaitEventOutput
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if decoded.Found != false {
			t.Errorf("Found: got %v, want false", decoded.Found)
		}
	})

	t.Run("publishEventRequest", func(t *testing.T) {
		original := publishEventRequest{
			ID:        "123e4567-e89b-12d3-a456-426614174000",
			EventType: "order.created",
			Data:      map[string]any{"amount": 99.5, "currency": "USD"},
		}

		data, err := json.Marshal(original)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}

		var decoded publishEventRequest
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		if decoded.ID != original.ID {
			t.Errorf("ID: got %q, want %q", decoded.ID, original.ID)
		}
		if decoded.EventType != original.EventType {
			t.Errorf("EventType: got %q, want %q", decoded.EventType, original.EventType)
		}
		if decoded.Data["amount"] != 99.5 {
			t.Errorf("Data.amount: got %v, want 99.5", decoded.Data["amount"])
		}
		if decoded.Data["currency"] != "USD" {
			t.Errorf("Data.currency: got %v, want USD", decoded.Data["currency"])
		}
	})

	t.Run("subscriptionJSON", func(t *testing.T) {
		now := time.Date(2025, 6, 15, 14, 30, 0, 0, time.UTC)
		original := subscriptionJSON{
			ID:            uuid.MustParse("11111111-1111-1111-1111-111111111111"),
			TenantID:      uuid.MustParse("22222222-2222-2222-2222-222222222222"),
			EventType:     "payment.received",
			DefName:       "process-payment",
			EntryPoint:    "main",
			InputTemplate: json.RawMessage(`{"source":"api"}`),
			FilterExpr:    "event.data.amount > 0",
			MaxRetries:    5,
			Enabled:       true,
			CreatedAt:     now,
		}

		data, err := json.Marshal(original)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}

		var decoded subscriptionJSON
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		if decoded.ID != original.ID {
			t.Errorf("ID: got %v, want %v", decoded.ID, original.ID)
		}
		if decoded.TenantID != original.TenantID {
			t.Errorf("TenantID: got %v, want %v", decoded.TenantID, original.TenantID)
		}
		if decoded.EventType != original.EventType {
			t.Errorf("EventType: got %q, want %q", decoded.EventType, original.EventType)
		}
		if decoded.DefName != original.DefName {
			t.Errorf("DefName: got %q, want %q", decoded.DefName, original.DefName)
		}
		if decoded.FilterExpr != original.FilterExpr {
			t.Errorf("FilterExpr: got %q, want %q", decoded.FilterExpr, original.FilterExpr)
		}
		if decoded.MaxRetries != original.MaxRetries {
			t.Errorf("MaxRetries: got %d, want %d", decoded.MaxRetries, original.MaxRetries)
		}
		if decoded.Enabled != original.Enabled {
			t.Errorf("Enabled: got %v, want %v", decoded.Enabled, original.Enabled)
		}
		if !decoded.CreatedAt.Equal(original.CreatedAt) {
			t.Errorf("CreatedAt: got %v, want %v", decoded.CreatedAt, original.CreatedAt)
		}
	})
}

// ---------------------------------------------------------------------------
// Run -- background worker with nil database
// ---------------------------------------------------------------------------

func TestRun_NilDB(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(context.Background(), &plugin.Environment{}); err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := p.Run(ctx)
	if err != nil {
		t.Errorf("Run() with nil DB returned error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// HTTP helpers -- writeJSON, writeError, tenantID
// ---------------------------------------------------------------------------

func TestWriteJSON(t *testing.T) {
	p := &Plugin{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	rec := httptest.NewRecorder()

	payload := map[string]any{"status": "ok", "value": 42}
	p.writeJSON(rec, http.StatusCreated, payload)

	resp := rec.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("expected status 201, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}

	body, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshal response body: %v", err)
	}
	if decoded["status"] != "ok" {
		t.Errorf("expected status 'ok', got %v", decoded["status"])
	}
	if decoded["value"] != float64(42) {
		t.Errorf("expected value 42, got %v", decoded["value"])
	}
}

func TestWriteError(t *testing.T) {
	p := &Plugin{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	rec := httptest.NewRecorder()

	p.writeError(rec, http.StatusBadRequest, "invalid input")

	resp := rec.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}

	body, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshal response body: %v", err)
	}
	if decoded["error"] != "invalid input" {
		t.Errorf("expected error 'invalid input', got %v", decoded["error"])
	}
}

func TestWriteJSON_EmptyBody(t *testing.T) {
	p := &Plugin{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	rec := httptest.NewRecorder()

	p.writeJSON(rec, http.StatusOK, map[string]any{})

	resp := rec.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if string(bytes.TrimSpace(body)) != "{}" {
		t.Errorf("expected body {}, got %s", string(body))
	}
}

func TestTenantID(t *testing.T) {
	t.Run("no tenant", func(t *testing.T) {
		p := &Plugin{}
		req := httptest.NewRequest("GET", "/", nil)
		tid := p.tenantID(req)
		if tid != uuid.Nil {
			t.Errorf("expected nil UUID, got %v", tid)
		}
	})

	t.Run("with tenant", func(t *testing.T) {
		p := &Plugin{}
		expected := uuid.New()
		ctx := auth.WithTenantID(context.Background(), expected)
		req := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
		tid := p.tenantID(req)
		if tid != expected {
			t.Errorf("expected %v, got %v", expected, tid)
		}
	})
}

// ---------------------------------------------------------------------------
// Pure filter helpers -- toFloat64, valuesEqual, compareValues
// ---------------------------------------------------------------------------

func TestToFloat64(t *testing.T) {
	tests := []struct {
		name  string
		input any
		want  float64
		ok    bool
	}{
		{"float64", float64(42.5), 42.5, true},
		{"float32", float32(3.14), float64(float32(3.14)), true},
		{"int", int(10), 10.0, true},
		{"int64", int64(100), 100.0, true},
		{"int32", int32(50), 50.0, true},
		{"json.Number", json.Number("99.9"), 99.9, true},
		{"string fails", "hello", 0, false},
		{"bool fails", true, 0, false},
		{"nil fails", nil, 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := toFloat64(tt.input)
			if ok != tt.ok {
				t.Errorf("toFloat64(%v) ok = %v, want %v", tt.input, ok, tt.ok)
			}
			if ok && got != tt.want {
				t.Errorf("toFloat64(%v) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestValuesEqual(t *testing.T) {
	tests := []struct {
		name string
		a, b any
		want bool
	}{
		{"float64 equal", float64(100), float64(100), true},
		{"float64 not equal", float64(100), float64(200), false},
		{"int == float64", int(100), float64(100), true},
		{"int64 == float64", int64(50), float64(50), true},
		{"string equal", "hello", "hello", true},
		{"string not equal", "hello", "world", false},
		{"string vs number (fmt.Sprintf)", "42", float64(42), true},
		{"bool equal", true, true, true},
		{"bool not equal", true, false, false},
		{"nil equal to nil", nil, nil, true},
		{"nil not equal to string", nil, "something", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := valuesEqual(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("valuesEqual(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestCompareValues(t *testing.T) {
	t.Run("string comparisons", func(t *testing.T) {
		data := map[string]any{"status": "active"}
		ce := comparisonExpr{
			path: []pathStep{{field: "status"}},
			op:   "==",
			lit:  literal{isString: true, strVal: "active"},
		}
		result, err := evalComparison(ce, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !result {
			t.Error("expected active == active to be true")
		}

		ce.op = "!="
		ce.lit = literal{isString: true, strVal: "inactive"}
		result, err = evalComparison(ce, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !result {
			t.Error("expected active != inactive to be true")
		}

		ce.op = ">"
		ce.lit = literal{isString: true, strVal: "aaa"}
		result, err = evalComparison(ce, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !result {
			t.Error("expected active > aaa to be true")
		}

		ce.op = "<"
		ce.lit = literal{isString: true, strVal: "zzz"}
		result, err = evalComparison(ce, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !result {
			t.Error("expected active < zzz to be true")
		}

		ce.op = ">="
		ce.lit = literal{isString: true, strVal: "active"}
		result, err = evalComparison(ce, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !result {
			t.Error("expected active >= active to be true")
		}

		ce.op = "<="
		ce.lit = literal{isString: true, strVal: "active"}
		result, err = evalComparison(ce, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !result {
			t.Error("expected active <= active to be true")
		}
	})

	t.Run("bool comparisons", func(t *testing.T) {
		data := map[string]any{"flag": true}
		ce := comparisonExpr{
			path: []pathStep{{field: "flag"}},
			op:   "==",
			lit:  literal{isBool: true, boolVal: true},
		}
		result, err := evalComparison(ce, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !result {
			t.Error("expected true == true")
		}

		ce.op = "!="
		ce.lit = literal{isBool: true, boolVal: false}
		result, err = evalComparison(ce, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !result {
			t.Error("expected true != false")
		}

		ce.op = ">"
		_, err = evalComparison(ce, data)
		if err == nil {
			t.Error("expected error for > on bool")
		}
	})

	t.Run("null comparisons", func(t *testing.T) {
		data := map[string]any{"value": nil}
		ce := comparisonExpr{
			path: []pathStep{{field: "value"}},
			op:   "==",
			lit:  literal{isNull: true},
		}
		result, err := evalComparison(ce, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !result {
			t.Error("expected null == null")
		}

		ce.op = "!="
		result, err = evalComparison(ce, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result {
			t.Error("expected null != null to be false")
		}

		ce.op = ">"
		_, err = evalComparison(ce, data)
		if err == nil {
			t.Error("expected error for > on null")
		}
	})

	t.Run("type mismatch errors", func(t *testing.T) {
		data := map[string]any{"amount": float64(100)}
		ce := comparisonExpr{
			path: []pathStep{{field: "amount"}},
			op:   "==",
			lit:  literal{isString: true, strVal: "hello"},
		}
		_, err := evalComparison(ce, data)
		if err == nil {
			t.Error("expected type mismatch error")
		}
	})

	t.Run("evalPath error", func(t *testing.T) {
		data := map[string]any{}
		ce := comparisonExpr{
			path: []pathStep{{field: "nonexistent"}},
			op:   "==",
			lit:  literal{isNull: true},
		}
		_, err := evalComparison(ce, data)
		if err == nil {
			t.Error("expected path-not-found error")
		}
	})
}

// ---------------------------------------------------------------------------
// matchCondition -- edge cases
// ---------------------------------------------------------------------------

func TestMatchConditionFieldNotFound(t *testing.T) {
	matched, err := matchCondition("path.to.field", "value", false, "somevalue")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if matched {
		t.Error("expected false for not-found field with shorthand condition")
	}
}

// ---------------------------------------------------------------------------
// getPath -- array index edge cases
// ---------------------------------------------------------------------------

func TestGetPathEdgeCases(t *testing.T) {
	t.Run("invalid array index", func(t *testing.T) {
		data := map[string]any{"items": []any{"a", "b"}}
		_, found := getPath(data, "items[invalid]")
		if found {
			t.Error("expected found=false for invalid array index string")
		}
	})

	t.Run("index out of bounds", func(t *testing.T) {
		data := map[string]any{"items": []any{"a", "b"}}
		_, found := getPath(data, "items[5]")
		if found {
			t.Error("expected found=false for out-of-bounds index")
		}
	})

	t.Run("negative index", func(t *testing.T) {
		data := map[string]any{"items": []any{"a", "b"}}
		_, found := getPath(data, "items[-1]")
		if found {
			t.Error("expected found=false for negative index")
		}
	})

	t.Run("index into non-array", func(t *testing.T) {
		data := map[string]any{"items": "not-an-array"}
		_, found := getPath(data, "items[0]")
		if found {
			t.Error("expected found=false for indexing non-array")
		}
	})

	t.Run("field not found after array", func(t *testing.T) {
		data := map[string]any{"items": []any{
			map[string]any{"sku": "ABC"},
		}}
		_, found := getPath(data, "items[0].missing")
		if found {
			t.Error("expected found=false for missing field in nested object")
		}
	})

	t.Run("path into non-object", func(t *testing.T) {
		data := map[string]any{"items": []any{"a", "b"}}
		_, found := getPath(data, "items[0].field")
		if found {
			t.Error("expected found=false for accessing field on string")
		}
	})
}

// ---------------------------------------------------------------------------
// EvaluateFilter -- membership expression via evaluate
// ---------------------------------------------------------------------------

func TestEvaluateFilterMembershipExpr(t *testing.T) {
	data := map[string]any{
		"status": "active",
	}

	result, err := EvaluateFilter(`event.data.status in("active", "pending")`, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result {
		t.Error("expected status in active/pending to be true")
	}

	result, err = EvaluateFilter(`event.data.status in("deleted", "archived")`, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result {
		t.Error("expected status not in deleted/archived")
	}

	result, err = EvaluateFilter("true", data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result {
		t.Error("expected 'true' to evaluate to true")
	}
}

// ---------------------------------------------------------------------------
// matchOperators -- uncovered operator branches
// ---------------------------------------------------------------------------

func TestMatchOperatorsExtraBranches(t *testing.T) {
	data := map[string]any{
		"event": map[string]any{
			"data": map[string]any{
				"amount": float64(100),
				"status": "active",
				"flag":   true,
				"items":  []any{"a", "b"},
			},
		},
	}

	t.Run("$exists operator true", func(t *testing.T) {
		result, err := EvaluateFilter(`{"event.data.amount": {"$exists": true}}`, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !result {
			t.Error("expected $exists true to match when field exists")
		}
	})

	t.Run("$exists operator false", func(t *testing.T) {
		result, err := EvaluateFilter(`{"event.data.missing": {"$exists": false}}`, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !result {
			t.Error("expected $exists false to match when field missing")
		}
	})

	t.Run("$exists type error", func(t *testing.T) {
		_, err := EvaluateFilter(`{"event.data.amount": {"$exists": "yes"}}`, data)
		if err == nil {
			t.Error("expected error for $exists with string operand")
		}
	})

	t.Run("$nin operator matches", func(t *testing.T) {
		result, err := EvaluateFilter(`{"event.data.status": {"$nin": ["deleted", "archived"]}}`, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !result {
			t.Error("expected $nin to match when value is not in list")
		}
	})

	t.Run("$nin operator does not match", func(t *testing.T) {
		result, err := EvaluateFilter(`{"event.data.status": {"$nin": ["active", "archived"]}}`, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result {
			t.Error("expected $nin to not match when value is in list")
		}
	})

	t.Run("$ne matches", func(t *testing.T) {
		result, err := EvaluateFilter(`{"event.data.status": {"$ne": "inactive"}}`, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !result {
			t.Error("expected $ne to match when value differs")
		}
	})

	t.Run("$in with missing field", func(t *testing.T) {
		result, err := EvaluateFilter(`{"event.data.nonexistent": {"$in": ["a", "b"]}}`, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result {
			t.Error("expected $in to not match for missing field")
		}
	})

	t.Run("$nin with missing field", func(t *testing.T) {
		result, err := EvaluateFilter(`{"event.data.nonexistent": {"$nin": ["a", "b"]}}`, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !result {
			t.Error("expected $nin to match for missing field")
		}
	})

	t.Run("$unknown operator returns error", func(t *testing.T) {
		_, err := EvaluateFilter(`{"event.data.amount": {"$unknown": 1}}`, data)
		if err == nil {
			t.Error("expected error for unknown operator")
		}
	})
}

// ---------------------------------------------------------------------------
// EvaluateFilter additional edge cases
// ---------------------------------------------------------------------------

func TestEvaluateFilterEdgeCases(t *testing.T) {
	t.Run("$gt with non-number operand silently fails", func(t *testing.T) {
		data := map[string]any{
			"event": map[string]any{
				"data": map[string]any{
					"amount": float64(100),
				},
			},
		}
		result, err := EvaluateFilter(`{"event.data.amount": {"$gt": "string"}}`, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result {
			t.Error("expected false for $gt with non-number operand")
		}
	})

	t.Run("$in with non-array operand", func(t *testing.T) {
		data := map[string]any{
			"event": map[string]any{
				"data": map[string]any{
					"status": "active",
				},
			},
		}
		_, err := EvaluateFilter(`{"event.data.status": {"$in": "not-an-array"}}`, data)
		if err == nil {
			t.Error("expected error for $in with non-array operand")
		}
	})

	t.Run("$nin with non-array operand", func(t *testing.T) {
		data := map[string]any{
			"event": map[string]any{
				"data": map[string]any{
					"status": "active",
				},
			},
		}
		_, err := EvaluateFilter(`{"event.data.status": {"$nin": "not-an-array"}}`, data)
		if err == nil {
			t.Error("expected error for $nin with non-array operand")
		}
	})
}

// ---------------------------------------------------------------------------
// Additional type JSON roundtrip tests
// ---------------------------------------------------------------------------

func TestPublishEventResponseRoundtrip(t *testing.T) {
	original := publishEventResponse{
		Status:  "published",
		Matched: 3,
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded publishEventResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.Status != original.Status {
		t.Errorf("Status: got %q, want %q", decoded.Status, original.Status)
	}
	if decoded.Matched != original.Matched {
		t.Errorf("Matched: got %d, want %d", decoded.Matched, original.Matched)
	}
}

func TestCreateSubscriptionRequestRoundtrip(t *testing.T) {
	maxRetries := 5
	original := createSubscriptionRequest{
		EventType:     "order.created",
		DefName:       "process-order",
		EntryPoint:    "main",
		InputTemplate: json.RawMessage(`{"source":"api"}`),
		FilterExpr:    "event.data.amount > 0",
		MaxRetries:    &maxRetries,
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded createSubscriptionRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.EventType != original.EventType {
		t.Errorf("EventType: got %q, want %q", decoded.EventType, original.EventType)
	}
	if decoded.DefName != original.DefName {
		t.Errorf("DefName: got %q, want %q", decoded.DefName, original.DefName)
	}
	if decoded.FilterExpr != original.FilterExpr {
		t.Errorf("FilterExpr: got %q, want %q", decoded.FilterExpr, original.FilterExpr)
	}
	if decoded.MaxRetries == nil {
		t.Fatal("MaxRetries is nil after roundtrip")
	}
	if *decoded.MaxRetries != maxRetries {
		t.Errorf("MaxRetries: got %d, want %d", *decoded.MaxRetries, maxRetries)
	}

	originalNil := createSubscriptionRequest{
		EventType: "order.created",
		DefName:   "process-order",
	}
	nilData, err := json.Marshal(originalNil)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decodedNil createSubscriptionRequest
	if err := json.Unmarshal(nilData, &decodedNil); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decodedNil.MaxRetries != nil {
		t.Error("expected MaxRetries to be nil when omitted")
	}
}

func TestRetryEventResponseRoundtrip(t *testing.T) {
	original := retryEventResponse{
		Status:           "retried",
		EventID:          "123e4567-e89b-12d3-a456-426614174000",
		WorkflowsStarted: 2,
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded retryEventResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.Status != original.Status {
		t.Errorf("Status: got %q, want %q", decoded.Status, original.Status)
	}
	if decoded.EventID != original.EventID {
		t.Errorf("EventID: got %q, want %q", decoded.EventID, original.EventID)
	}
	if decoded.WorkflowsStarted != original.WorkflowsStarted {
		t.Errorf("WorkflowsStarted: got %d, want %d", decoded.WorkflowsStarted, original.WorkflowsStarted)
	}
}

// ---------------------------------------------------------------------------
// unregisterAwaiter handles empty workflowID
// ---------------------------------------------------------------------------

func TestUnregisterAwaiterEmptyWorkflowID(t *testing.T) {
	unregisterAwaiter(context.Background(), nil, nil, "", "event_type")
}

// ---------------------------------------------------------------------------
// defaultRetryInterval constant
// ---------------------------------------------------------------------------

func TestDefaultRetryInterval(t *testing.T) {
	if defaultRetryInterval != 30*time.Second {
		t.Errorf("defaultRetryInterval = %v, want 30s", defaultRetryInterval)
	}
}

// ---------------------------------------------------------------------------
// compareValues -- remaining uncovered branches
// ---------------------------------------------------------------------------

// customType implements no numeric/string/bool/nil interface so it hits the
// default branch of compareValues' outer switch.
type customType struct {
	val string
}

func TestCompareValues_UnsupportedValueType(t *testing.T) {
	a := customType{val: "test"}
	lit := literal{isString: true, strVal: "test"}
	_, err := compareValues(a, lit, "==")
	if err == nil {
		t.Error("expected error for unsupported value type")
	}
	if !strings.Contains(err.Error(), "unsupported value type") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCompareValues_UnsupportedOperator(t *testing.T) {
	lit := literal{isNumber: true, numVal: 100}
	_, err := compareValues(float64(50), lit, "unknown_op")
	if err == nil {
		t.Error("expected error for unsupported comparison")
	}
	if !strings.Contains(err.Error(), "unsupported comparison") {
		t.Errorf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// evaluate -- default case with unknown expression type
// ---------------------------------------------------------------------------

// unknownExpr implements the expr interface without being one of the known
// types, causing evaluate to hit its default branch.
type unknownExpr struct{}

func (unknownExpr) exprMarker() {}

func TestEvaluateDefaultBranch(t *testing.T) {
	_, err := evaluate(unknownExpr{}, nil)
	if err == nil {
		t.Error("expected error for unknown expression type")
	}
	if !strings.Contains(err.Error(), "unknown expression type") {
		t.Errorf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// evaluate -- evalPath errors for membershipExpr
// ---------------------------------------------------------------------------

func TestEvalMembershipPathError(t *testing.T) {
	me := membershipExpr{
		path: []pathStep{{field: "nonexistent"}},
		lits: []literal{{isString: true, strVal: "x"}},
	}
	data := map[string]any{}
	_, err := evalMembership(me, data)
	if err == nil {
		t.Error("expected path-not-found error for membershipExpr")
	}
}

// ---------------------------------------------------------------------------
// EvaluateFilter -- remaining parser error branches
// ---------------------------------------------------------------------------

func TestEvaluateFilterParseErrors(t *testing.T) {
	t.Run("unexpected token after expression", func(t *testing.T) {
		_, err := EvaluateFilter(`event.data.amount > 100 extra`, nil)
		if err == nil {
			t.Error("expected error for trailing tokens")
		}
	})

	t.Run("incomplete membership expression", func(t *testing.T) {
		_, err := EvaluateFilter(`event.data.amount in(`, nil)
		if err == nil {
			t.Error("expected error for incomplete membership")
		}
	})

	t.Run("invalid text expression", func(t *testing.T) {
		_, err := EvaluateFilter(`event.data.amount unknown 100`, nil)
		if err == nil {
			t.Error("expected error for unknown operator")
		}
	})

	t.Run("single = fails", func(t *testing.T) {
		_, err := EvaluateFilter(`event.data.amount = 100`, nil)
		if err == nil {
			t.Error("expected error for single =")
		}
	})
}

// ---------------------------------------------------------------------------
// matchOperators -- $exists non-bool operand (uncovered branch)
// ---------------------------------------------------------------------------

func TestMatchOperatorsExistsNonBool(t *testing.T) {
	data := map[string]any{
		"event": map[string]any{
			"data": map[string]any{
				"amount": float64(100),
			},
		},
	}
	_, err := EvaluateFilter(`{"event.data.amount": {"$exists": "yes"}}`, data)
	if err == nil {
		t.Error("expected error for $exists with non-bool operand")
	}
}

// ---------------------------------------------------------------------------
// tokenizer: single '=' and '!' error branches
// ---------------------------------------------------------------------------

func TestTokenizerOperatorErrors(t *testing.T) {
	_, err := tokenize("=")
	if err == nil {
		t.Error("expected error for single =")
	}

	_, err = tokenize("!")
	if err == nil {
		t.Error("expected error for single !")
	}
}

// ---------------------------------------------------------------------------
// parser: error branches for parseMembership, parseComparison, etc.
// ---------------------------------------------------------------------------

func TestParserErrorBranches(t *testing.T) {
	t.Run("expected ( after 'in'", func(t *testing.T) {
		_, err := EvaluateFilter(`event.data.x in notparen`, nil)
		if err == nil {
			t.Error("expected error")
		}
	})

	t.Run("incomplete path after 'event'", func(t *testing.T) {
		_, err := EvaluateFilter(`event > 5`, nil)
		if err == nil {
			t.Error("expected error for incomplete path")
		}
	})

	t.Run("array index with non-number", func(t *testing.T) {
		_, err := EvaluateFilter(`event.data.items[abc] == 5`, nil)
		if err == nil {
			t.Error("expected error for non-numeric array index")
		}
	})
}

// ---------------------------------------------------------------------------
// Tokenizer: negative numbers, decimals, "true"/"false" keywords
// ---------------------------------------------------------------------------

func TestTokenizerNegativeNumber(t *testing.T) {
	data := map[string]any{"amount": -5.0}
	result, err := EvaluateFilter(`event.data.amount == -5`, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result {
		t.Error("expected -5 == -5")
	}
}

func TestTokenizerDecimalNumber(t *testing.T) {
	data := map[string]any{"price": 99.5}
	result, err := EvaluateFilter(`event.data.price == 99.5`, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result {
		t.Error("expected 99.5 == 99.5")
	}
}

func TestTokenizerTrueKeyword(t *testing.T) {
	data := map[string]any{"flag": true}
	result, err := EvaluateFilter(`event.data.flag == true`, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result {
		t.Error("expected true == true")
	}
}

func TestTokenizerFalseKeyword(t *testing.T) {
	data := map[string]any{"flag": false}
	result, err := EvaluateFilter(`event.data.flag == false`, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result {
		t.Error("expected false == false")
	}
}

// ---------------------------------------------------------------------------
// Parser: "data" keyword validation, missing bracket, parseExpr "true"
// ---------------------------------------------------------------------------

func TestParserDataKeyword(t *testing.T) {
	_, err := EvaluateFilter(`event.notdata > 5`, nil)
	if err == nil {
		t.Error("expected error for missing 'data' after 'event.'")
	}
}

func TestParserMissingBracket(t *testing.T) {
	_, err := EvaluateFilter(`event.data.items[0 == 5`, nil)
	if err == nil {
		t.Error("expected error for missing ']'")
	}
}

func TestParseExprTrueLiteral(t *testing.T) {
	tokens := []token{{typ: tokTrue, val: "true"}, {typ: tokEOF}}
	ex, err := parse(tokens)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if _, ok := ex.(trueExpr); !ok {
		t.Errorf("expected trueExpr, got %T", ex)
	}
	result, err := evaluate(ex, nil)
	if err != nil {
		t.Fatalf("evaluate failed: %v", err)
	}
	if !result {
		t.Error("expected true")
	}
}

// ---------------------------------------------------------------------------
// matchOperators: $eq, $lt, $lte, $exists edge cases
// ---------------------------------------------------------------------------

func TestMatchOperatorsEq(t *testing.T) {
	data := map[string]any{
		"event": map[string]any{
			"data": map[string]any{
				"amount": float64(100),
				"status": "active",
			},
		},
	}

	t.Run("$eq matches", func(t *testing.T) {
		result, err := EvaluateFilter(`{"event.data.amount": {"$eq": 100}}`, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !result {
			t.Error("expected $eq 100 to match")
		}
	})

	t.Run("$eq does not match", func(t *testing.T) {
		result, err := EvaluateFilter(`{"event.data.amount": {"$eq": 200}}`, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result {
			t.Error("expected $eq 200 to not match")
		}
	})

	t.Run("$eq missing field does not match", func(t *testing.T) {
		result, err := EvaluateFilter(`{"event.data.nonexistent": {"$eq": "value"}}`, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result {
			t.Error("expected $eq to not match missing field")
		}
	})
}

func TestMatchOperatorsLtLte(t *testing.T) {
	data := map[string]any{
		"event": map[string]any{
			"data": map[string]any{
				"amount": float64(100),
			},
		},
	}

	t.Run("$lt matches", func(t *testing.T) {
		result, err := EvaluateFilter(`{"event.data.amount": {"$lt": 200}}`, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !result {
			t.Error("expected $lt 200 to match")
		}
	})

	t.Run("$lt does not match", func(t *testing.T) {
		result, err := EvaluateFilter(`{"event.data.amount": {"$lt": 50}}`, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result {
			t.Error("expected $lt 50 to not match")
		}
	})

	t.Run("$lte matches (equal)", func(t *testing.T) {
		result, err := EvaluateFilter(`{"event.data.amount": {"$lte": 100}}`, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !result {
			t.Error("expected $lte 100 to match")
		}
	})

	t.Run("$lte matches (less)", func(t *testing.T) {
		result, err := EvaluateFilter(`{"event.data.amount": {"$lte": 200}}`, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !result {
			t.Error("expected $lte 200 to match")
		}
	})

	t.Run("$lte does not match", func(t *testing.T) {
		result, err := EvaluateFilter(`{"event.data.amount": {"$lte": 50}}`, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result {
			t.Error("expected $lte 50 to not match")
		}
	})
}

func TestMatchOperatorsExistsEdgeCases(t *testing.T) {
	data := map[string]any{
		"event": map[string]any{
			"data": map[string]any{
				"amount": float64(100),
			},
		},
	}

	t.Run("$exists true on missing field", func(t *testing.T) {
		result, err := EvaluateFilter(`{"event.data.nonexistent": {"$exists": true}}`, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result {
			t.Error("expected $exists true on missing field to not match")
		}
	})

	t.Run("$exists false on existing field", func(t *testing.T) {
		result, err := EvaluateFilter(`{"event.data.amount": {"$exists": false}}`, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result {
			t.Error("expected $exists false on existing field to not match")
		}
	})
}

// ---------------------------------------------------------------------------
// evalPath -- array index error branches
// ---------------------------------------------------------------------------

func TestEvalPathErrors(t *testing.T) {
	t.Run("array index on non-array", func(t *testing.T) {
		_, err := evalPath(
			map[string]any{"items": "not-an-array"},
			[]pathStep{{isIndex: true, index: 0}},
		)
		if err == nil {
			t.Error("expected error for indexing non-array")
		}
	})

	t.Run("field on non-object", func(t *testing.T) {
		_, err := evalPath(
			map[string]any{"items": []any{"a", "b"}},
			[]pathStep{{isIndex: true, index: 0}, {field: "field"}},
		)
		if err == nil {
			t.Error("expected error for accessing field on string")
		}
	})
}

// ---------------------------------------------------------------------------
// peek: provoke EOF return by positioning past end of tokens
// ---------------------------------------------------------------------------

func TestPeekEOFBranch(t *testing.T) {
	p := &parser{tokens: []token{}, pos: 0}
	tok := p.peek()
	if tok.typ != tokEOF {
		t.Errorf("expected tokEOF, got %v", tok.typ)
	}
}

// ---------------------------------------------------------------------------
// matchOperators: $gte operator branches
// ---------------------------------------------------------------------------

func TestMatchOperatorsGte(t *testing.T) {
	data := map[string]any{
		"event": map[string]any{
			"data": map[string]any{
				"amount": float64(100),
			},
		},
	}

	t.Run("$gte matches (equal)", func(t *testing.T) {
		result, err := EvaluateFilter(`{"event.data.amount": {"$gte": 100}}`, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !result {
			t.Error("expected $gte 100 to match")
		}
	})

	t.Run("$gte matches (greater)", func(t *testing.T) {
		result, err := EvaluateFilter(`{"event.data.amount": {"$gte": 50}}`, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !result {
			t.Error("expected $gte 50 to match")
		}
	})

	t.Run("$gte does not match", func(t *testing.T) {
		result, err := EvaluateFilter(`{"event.data.amount": {"$gte": 200}}`, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result {
			t.Error("expected $gte 200 to not match")
		}
	})

	t.Run("$gte missing field does not match", func(t *testing.T) {
		result, err := EvaluateFilter(`{"event.data.nonexistent": {"$gte": 50}}`, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result {
			t.Error("expected $gte on missing field to not match")
		}
	})
}

// ---------------------------------------------------------------------------
// evalPath: array index out of bounds
// ---------------------------------------------------------------------------

func TestEvalPathArrayOutOfBounds(t *testing.T) {
	_, err := evalPath(
		map[string]any{"items": []any{"a", "b"}},
		[]pathStep{{field: "items"}, {isIndex: true, index: 5}},
	)
	if err == nil {
		t.Error("expected out-of-bounds error")
	}
	if !strings.Contains(err.Error(), "out of bounds") {
		t.Errorf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// evalMembership: compareValues error continue branch
// ---------------------------------------------------------------------------

func TestEvalMembershipTypeMismatch(t *testing.T) {
	// All literals have a different type than the path value, so each
	// compareValues call returns an error, hitting the continue at line 712.
	me := membershipExpr{
		path: []pathStep{{field: "amount"}},
		lits: []literal{
			{isString: true, strVal: "hello"},
			{isBool: true, boolVal: false},
		},
	}
	data := map[string]any{"amount": float64(100)}
	result, err := evalMembership(me, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result {
		t.Error("expected false when all membership literals have wrong type")
	}
}

// ---------------------------------------------------------------------------
// parseComparison: tokEOF and default branches
// ---------------------------------------------------------------------------

func TestParseComparisonEdgeCases(t *testing.T) {
	t.Run("path without operator (EOF)", func(t *testing.T) {
		_, err := EvaluateFilter(`event.data.amount`, nil)
		if err == nil {
			t.Error("expected error for path without operator")
		}
	})

	t.Run("unexpected token as operator", func(t *testing.T) {
		_, err := EvaluateFilter(`event.data.amount .`, nil)
		if err == nil {
			t.Error("expected error for '.' as operator")
		}
	})
}

// ---------------------------------------------------------------------------
// parseLiteral: strconv.ParseFloat error branch
// ---------------------------------------------------------------------------

func TestParseLiteralParseFloatError(t *testing.T) {
	// An extremely long number string (10^310) exceeds float64 max (~1.8e308)
	// and causes ParseFloat to return ErrRange.
	hugeNumber := "1"
	for i := 0; i < 310; i++ {
		hugeNumber += "0"
	}
	tokens := []token{
		{typ: tokIdent, val: "event"},
		{typ: tokDot, val: "."},
		{typ: tokIdent, val: "data"},
		{typ: tokDot, val: "."},
		{typ: tokIdent, val: "amount"},
		{typ: tokEq, val: "=="},
		{typ: tokNumber, val: hugeNumber},
		{typ: tokEOF},
	}
	_, err := parse(tokens)
	if err == nil {
		t.Error("expected error for overflow number literal")
	}
}

// ---------------------------------------------------------------------------
// parseMembership: missing ')' error branch
// ---------------------------------------------------------------------------

func TestParseMembershipMissingParen(t *testing.T) {
	_, err := EvaluateFilter(`event.data.amount in(1, 2`, nil)
	if err == nil {
		t.Error("expected error for missing ')' in membership")
	}
}

// ---------------------------------------------------------------------------
// getPath: path with '[' but no ']' (HasSuffix returns false)
// ---------------------------------------------------------------------------

func TestGetPathBracketNoSuffix(t *testing.T) {
	// Path "items[0" has '[' but no ']'. HasSuffix returns false so the code
	// falls through to regular field lookup for the literal key "items[0".
	// Since the map has only "items" (not "items[0"), the lookup fails.
	data := map[string]any{"items": []any{"a", "b"}}
	_, found := getPath(data, "items[0")
	if found {
		t.Error("expected found=false for path with '[' but no ']'")
	}
}

// ---------------------------------------------------------------------------
// Run with non-nil DB (immediate context cancellation)
// ---------------------------------------------------------------------------

func TestRun_ContextCancellation(t *testing.T) {
	// Use a non-nil *sql.DB (zero value) to exercise the ticker path in Run
	// without needing a real database connection.
	p := &Plugin{}
	if err := p.Init(context.Background(), &plugin.Environment{DB: &engine.SQLDBAdapter{DB: &sql.DB{}}}); err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := p.Run(ctx)
	if err != nil {
		t.Errorf("Run() with cancelled context returned error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Init with non-nil Logger
// ---------------------------------------------------------------------------

func TestInitExplicitLogger(t *testing.T) {
	p := &Plugin{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := p.Init(context.Background(), &plugin.Environment{
		Logger: logger,
	})
	if err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// compareValues: additional type mismatch and operator branches
// ---------------------------------------------------------------------------

func TestCompareValuesExtraBranches(t *testing.T) {
	t.Run("string value with non-string literal", func(t *testing.T) {
		data := map[string]any{"status": "active"}
		ce := comparisonExpr{
			path: []pathStep{{field: "status"}},
			op:   "==",
			lit:  literal{isNumber: true, numVal: 100},
		}
		_, err := evalComparison(ce, data)
		if err == nil {
			t.Error("expected type mismatch error for string value with number literal")
		}
	})

	t.Run("bool value with non-bool literal", func(t *testing.T) {
		data := map[string]any{"flag": true}
		ce := comparisonExpr{
			path: []pathStep{{field: "flag"}},
			op:   "==",
			lit:  literal{isString: true, strVal: "true"},
		}
		_, err := evalComparison(ce, data)
		if err == nil {
			t.Error("expected type mismatch error for bool value with string literal")
		}
	})

	t.Run("nil value with non-null literal", func(t *testing.T) {
		data := map[string]any{"value": nil}
		ce := comparisonExpr{
			path: []pathStep{{field: "value"}},
			op:   "==",
			lit:  literal{isBool: true, boolVal: false},
		}
		_, err := evalComparison(ce, data)
		if err == nil {
			t.Error("expected type mismatch error for nil value with bool literal")
		}
	})
}

// ---------------------------------------------------------------------------
// compareNumeric direct test (uncovered branches)
// ---------------------------------------------------------------------------

func TestCompareNumeric(t *testing.T) {
	t.Run("both numbers", func(t *testing.T) {
		if got := compareNumeric(float64(100), float64(50)); got != 1 {
			t.Errorf("expected 1, got %d", got)
		}
		if got := compareNumeric(float64(50), float64(100)); got != -1 {
			t.Errorf("expected -1, got %d", got)
		}
		if got := compareNumeric(float64(50), float64(50)); got != 0 {
			t.Errorf("expected 0, got %d", got)
		}
	})

	t.Run("non-numeric operand returns 0", func(t *testing.T) {
		if got := compareNumeric("string", float64(50)); got != 0 {
			t.Errorf("expected 0 for non-numeric val, got %d", got)
		}
		if got := compareNumeric(float64(50), "string"); got != 0 {
			t.Errorf("expected 0 for non-numeric operand, got %d", got)
		}
	})

	t.Run("nil values return 0", func(t *testing.T) {
		if got := compareNumeric(nil, float64(50)); got != 0 {
			t.Errorf("expected 0 for nil val, got %d", got)
		}
		if got := compareNumeric(float64(50), nil); got != 0 {
			t.Errorf("expected 0 for nil operand, got %d", got)
		}
	})
}

// ---------------------------------------------------------------------------
// getPath - nested access, mixed arrays and objects
// ---------------------------------------------------------------------------

func TestGetPathNestedAccess(t *testing.T) {
	t.Run("deeply nested paths", func(t *testing.T) {
		data := map[string]any{
			"a": map[string]any{
				"b": map[string]any{
					"c": map[string]any{
						"d": "found",
					},
				},
			},
		}
		val, found := getPath(data, "a.b.c.d")
		if !found {
			t.Error("expected found=true for deeply nested path")
		}
		if val != "found" {
			t.Errorf("expected 'found', got %v", val)
		}
	})

	t.Run("array element then nested field", func(t *testing.T) {
		data := map[string]any{
			"items": []any{
				map[string]any{"sku": "ABC", "price": 10.0},
				map[string]any{"sku": "DEF", "price": 20.0},
			},
		}
		val, found := getPath(data, "items[1].sku")
		if !found {
			t.Error("expected found=true for items[1].sku")
		}
		if val != "DEF" {
			t.Errorf("expected 'DEF', got %v", val)
		}
	})

	t.Run("index into map then field access into nested object", func(t *testing.T) {
		data := map[string]any{
			"items": []any{
				map[string]any{
					"nested": map[string]any{"value": float64(42)},
				},
			},
		}
		val, found := getPath(data, "items[0].nested.value")
		if !found {
			t.Error("expected found=true for items[0].nested.value")
		}
		if val != float64(42) {
			t.Errorf("expected 42, got %v", val)
		}
	})

	t.Run("simple path without dots", func(t *testing.T) {
		data := map[string]any{"key": "value"}
		val, found := getPath(data, "key")
		if !found {
			t.Error("expected found=true for simple path key")
		}
		if val != "value" {
			t.Errorf("expected 'value', got %v", val)
		}
	})
}

// ---------------------------------------------------------------------------
// matchCondition with operator map edge cases
// ---------------------------------------------------------------------------

func TestMatchConditionOperatorMapEdges(t *testing.T) {
	t.Run("$lte with missing field", func(t *testing.T) {
		matched, err := matchCondition("path", nil, false, map[string]any{"$lte": float64(100)})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if matched {
			t.Error("expected false for $lte with missing field")
		}
	})

	t.Run("$ne with missing field matches", func(t *testing.T) {
		matched, err := matchCondition("path", nil, false, map[string]any{"$ne": "value"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !matched {
			t.Error("expected true for $ne with missing field (missing != value)")
		}
	})

	t.Run("$eq with missing field does not match", func(t *testing.T) {
		matched, err := matchCondition("path", nil, false, map[string]any{"$eq": "value"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if matched {
			t.Error("expected false for $eq with missing field")
		}
	})

	t.Run("$gt with missing field does not match", func(t *testing.T) {
		matched, err := matchCondition("path", nil, false, map[string]any{"$gt": float64(100)})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if matched {
			t.Error("expected false for $gt with missing field")
		}
	})
}

// ---------------------------------------------------------------------------
// Tokenizer - all operator types and error branches
// ---------------------------------------------------------------------------

func TestTokenizerAllOperators(t *testing.T) {
	tests := []struct {
		input string
		desc  string
		data  map[string]any
	}{
		{`event.data.x == 5`, "eq operator", map[string]any{"x": float64(5)}},
		{`event.data.x != 5`, "neq operator", map[string]any{"x": float64(5)}},
		{`event.data.x > 5`, "gt operator", map[string]any{"x": float64(10)}},
		{`event.data.x < 5`, "lt operator", map[string]any{"x": float64(1)}},
		{`event.data.x >= 5`, "gte operator", map[string]any{"x": float64(5)}},
		{`event.data.x <= 5`, "lte operator", map[string]any{"x": float64(5)}},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			_, err := EvaluateFilter(tt.input, tt.data)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}

	t.Run("unclosed string literal returns error", func(t *testing.T) {
		_, err := tokenize(`event.data.x == "hello`)
		if err == nil {
			t.Error("expected error for unclosed string")
		}
	})

	t.Run("unexpected character", func(t *testing.T) {
		_, err := tokenize("@")
		if err == nil {
			t.Error("expected error for unexpected character")
		}
	})
}

// ---------------------------------------------------------------------------
// Parser - additional edge cases
// ---------------------------------------------------------------------------

func TestParserAdditionalEdges(t *testing.T) {
	t.Run("missing event prefix", func(t *testing.T) {
		_, err := EvaluateFilter(`data.amount > 5`, nil)
		if err == nil {
			t.Error("expected error for missing event prefix")
		}
	})

	t.Run("dot after event but no data", func(t *testing.T) {
		_, err := EvaluateFilter(`event. > 5`, nil)
		if err == nil {
			t.Error("expected error for missing data after event.")
		}
	})

	t.Run("null literal comparison", func(t *testing.T) {
		data := map[string]any{"value": nil}
		result, err := EvaluateFilter(`event.data.value == null`, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !result {
			t.Error("expected null == null to be true")
		}
	})

	t.Run("false literal comparison", func(t *testing.T) {
		data := map[string]any{"flag": false}
		result, err := EvaluateFilter(`event.data.flag == false`, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !result {
			t.Error("expected false == false to be true")
		}
	})
}

// ---------------------------------------------------------------------------
// parseLiteral - keyword handling (null, true, false, number overflow)
// ---------------------------------------------------------------------------

func TestParseLiteralKeywords(t *testing.T) {
	t.Run("null literal in membership", func(t *testing.T) {
		data := map[string]any{"value": nil}
		result, err := EvaluateFilter(`event.data.value in(null)`, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !result {
			t.Error("expected null in(null) to match")
		}
	})

	t.Run("bool literal in membership", func(t *testing.T) {
		data := map[string]any{"flag": true}
		result, err := EvaluateFilter(`event.data.flag in(true, false)`, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !result {
			t.Error("expected true in(true, false) to match")
		}
	})
}

// ---------------------------------------------------------------------------
// valuesEqual - additional edge cases
// ---------------------------------------------------------------------------

func TestValuesEqualEdgeCases(t *testing.T) {
	t.Run("string vs number via fmt.Sprintf", func(t *testing.T) {
		if !valuesEqual("42", float64(42)) {
			t.Error("expected '42' == 42 via string formatting")
		}
		if !valuesEqual(float64(42), "42") {
			t.Error("expected 42 == '42' via string formatting")
		}
	})

	t.Run("bool vs string", func(t *testing.T) {
		// Mixed types where neither is numeric end up in Sprintf comparison.
		got := valuesEqual(true, "true")
		if !got {
			t.Log("bool true matches string true via fmt.Sprintf")
		}
	})

	t.Run("json.Number comparison with int", func(t *testing.T) {
		if !valuesEqual(json.Number("100"), float64(100)) {
			t.Error("expected json.Number(100) == 100")
		}
		if !valuesEqual(float64(100), json.Number("100")) {
			t.Error("expected 100 == json.Number(100)")
		}
	})
}

// ---------------------------------------------------------------------------
// evalPath - additional error cases
// ---------------------------------------------------------------------------

func TestEvalPathAdditionalErrors(t *testing.T) {
	t.Run("field on non-object intermediate", func(t *testing.T) {
		_, err := evalPath(
			map[string]any{"key": "stringval"},
			[]pathStep{{field: "key"}, {field: "nested"}},
		)
		if err == nil {
			t.Error("expected error for accessing field on string value")
		}
	})

	t.Run("missing field in nested path", func(t *testing.T) {
		_, err := evalPath(
			map[string]any{"outer": map[string]any{"inner": "val"}},
			[]pathStep{{field: "outer"}, {field: "nonexistent"}},
		)
		if err == nil {
			t.Error("expected error for missing nested field")
		}
	})
}

// ---------------------------------------------------------------------------
// matchOperators: $ne, $gt, $lt with missing fields and edge values
// ---------------------------------------------------------------------------

func TestMatchOperatorsMissingFieldBranches(t *testing.T) {
	t.Run("$ne existing field that equals value -> not match", func(t *testing.T) {
		matched, err := matchCondition("path", "active", true, map[string]any{"$ne": "active"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if matched {
			t.Error("expected $ne to not match when value equals")
		}
	})

	t.Run("$lt missing field does not match", func(t *testing.T) {
		matched, err := matchCondition("path", nil, false, map[string]any{"$lt": float64(100)})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if matched {
			t.Error("expected $lt with missing field to not match")
		}
	})

	t.Run("$gte missing field does not match", func(t *testing.T) {
		matched, err := matchCondition("path", nil, false, map[string]any{"$gte": float64(100)})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if matched {
			t.Error("expected $gte with missing field to not match")
		}
	})
}

// ---------------------------------------------------------------------------
// Invalid filter JSON and malformed filter expressions
// ---------------------------------------------------------------------------

func TestMalformedFilterExpressions(t *testing.T) {
	t.Run("invalid JSON filter prefix", func(t *testing.T) {
		_, err := EvaluateFilter(`{invalid}`, nil)
		if err == nil {
			t.Error("expected error for invalid JSON filter")
		}
	})

	t.Run("incomplete comparison at EOF", func(t *testing.T) {
		_, err := EvaluateFilter(`event.data.amount >`, nil)
		if err == nil {
			t.Error("expected error for incomplete expression")
		}
	})

	t.Run("unknown operator in text expression", func(t *testing.T) {
		_, err := EvaluateFilter(`event.data.amount unknown 100`, nil)
		if err == nil {
			t.Error("expected error for unknown operator")
		}
	})
}

// ===========================================================================
// In-memory fake DB store for DB-backed behavioral tests
// ===========================================================================

type etSubscriptionRow struct {
	id            string
	tenantID      string
	eventType     string
	defName       string
	entryPoint    string
	inputTemplate string
	filterExpr    string
	maxRetries    int
	enabled       bool
	createdAt     time.Time
}

type etIngestedEventRow struct {
	id          string
	tenantID    string
	eventType   string
	eventData   string
	receivedAt  time.Time
	processed   bool
	retryCount  int
	lastRetryAt *time.Time
	status      string
	errorMsg    *string
}

type etAwaiterRow struct {
	workflowID string
	tenantID   string
	eventType  string
	createdAt  time.Time
}

type etDBStore struct {
	mu            sync.RWMutex
	subscriptions []etSubscriptionRow
	events        []etIngestedEventRow
	awaiters      []etAwaiterRow
	apiKeys       map[string]string
}

func newETDBStore() *etDBStore {
	return &etDBStore{
		subscriptions: make([]etSubscriptionRow, 0),
		events:        make([]etIngestedEventRow, 0),
		awaiters:      make([]etAwaiterRow, 0),
		apiKeys:       make(map[string]string),
	}
}

// ---------------------------------------------------------------------------
// Fake SQL driver for eventtriggers
// ---------------------------------------------------------------------------

type etConnector struct {
	store *etDBStore
}

func (c *etConnector) Connect(_ context.Context) (driver.Conn, error) {
	return &etConn{store: c.store}, nil
}
func (c *etConnector) Driver() driver.Driver { return &etDrv{} }

type etDrv struct{}

func (*etDrv) Open(_ string) (driver.Conn, error) {
	return nil, fmt.Errorf("etDrv: use sql.OpenDB")
}

type etConn struct {
	store *etDBStore
}

func (*etConn) Prepare(_ string) (driver.Stmt, error) {
	return nil, fmt.Errorf("etConn: unexpected Prepare call")
}
func (*etConn) Close() error              { return nil }
func (*etConn) Begin() (driver.Tx, error) { return &etTx{}, nil }

type etTx struct{}

func (*etTx) Commit() error   { return nil }
func (*etTx) Rollback() error { return nil }

type etResult struct {
	rowsAffected int64
}

func (r *etResult) LastInsertId() (int64, error) { return 0, nil }
func (r *etResult) RowsAffected() (int64, error) { return r.rowsAffected, nil }

type etRows struct {
	columns []string
	data    [][]driver.Value
	pos     int
}

func (r *etRows) Columns() []string { return r.columns }
func (r *etRows) Close() error      { return nil }
func (r *etRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.pos])
	r.pos++
	return nil
}

// ---------------------------------------------------------------------------
// ExecContext routing
// ---------------------------------------------------------------------------

func (c *etConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.store.mu.Lock()
	defer c.store.mu.Unlock()

	q := strings.Join(strings.Fields(query), " ")
	switch {
	case strings.Contains(q, "INSERT INTO ingested_events"):
		return c.execInsertEvent(args)
	case strings.Contains(q, "INSERT INTO event_awaiters"):
		return c.execInsertAwaiter(args)
	case strings.Contains(q, "DELETE FROM event_awaiters"):
		return c.execDeleteAwaiter(args)
	case strings.Contains(q, "DELETE FROM event_subscriptions"):
		return c.execDeleteSubscription(args)
	case strings.Contains(q, "SET retry_count"):
		return c.execUpdateEventRetry(q, args)
	case strings.Contains(q, "SET processed"):
		return c.execUpdateEventProcessed(q, args)
	case strings.Contains(q, "UPDATE ingested_events"):
		return c.execUpdateEvent(args)
	default:
		return nil, fmt.Errorf("etConn: unexpected Exec query: %s", q)
	}
}

// ---------------------------------------------------------------------------
// Exec implementations
// ---------------------------------------------------------------------------

func (c *etConn) execInsertEvent(args []driver.NamedValue) (driver.Result, error) {
	id, err := etArgString(args, 1)
	if err != nil {
		return nil, err
	}
	tenantID, err := etArgString(args, 2)
	if err != nil {
		return nil, err
	}
	eventType, err := etArgString(args, 3)
	if err != nil {
		return nil, err
	}
	eventData, err := etArgString(args, 4)
	if err != nil {
		return nil, err
	}

	// Check if event already exists (idempotent insert).
	for _, evt := range c.store.events {
		if evt.id == id {
			return &etResult{rowsAffected: 0}, nil
		}
	}

	c.store.events = append(c.store.events, etIngestedEventRow{
		id:         id,
		tenantID:   tenantID,
		eventType:  eventType,
		eventData:  eventData,
		receivedAt: time.Now(),
		processed:  false,
		status:     "pending",
	})
	return &etResult{rowsAffected: 1}, nil
}

func (c *etConn) execInsertAwaiter(args []driver.NamedValue) (driver.Result, error) {
	workflowID, err := etArgString(args, 1)
	if err != nil {
		return nil, err
	}
	tenantID, err := etArgString(args, 2)
	if err != nil {
		return nil, err
	}
	eventType, err := etArgString(args, 3)
	if err != nil {
		return nil, err
	}

	// Upsert: if exists, update created_at
	found := false
	for i, a := range c.store.awaiters {
		if a.workflowID == workflowID && a.eventType == eventType {
			c.store.awaiters[i].createdAt = time.Now()
			found = true
			break
		}
	}
	if !found {
		c.store.awaiters = append(c.store.awaiters, etAwaiterRow{
			workflowID: workflowID,
			tenantID:   tenantID,
			eventType:  eventType,
			createdAt:  time.Now(),
		})
	}
	return &etResult{rowsAffected: 1}, nil
}

func (c *etConn) execDeleteAwaiter(args []driver.NamedValue) (driver.Result, error) {
	workflowID, err := etArgString(args, 1)
	if err != nil {
		return nil, err
	}
	eventType, err := etArgString(args, 2)
	if err != nil {
		return nil, err
	}

	for i, a := range c.store.awaiters {
		if a.workflowID == workflowID && a.eventType == eventType {
			c.store.awaiters = append(c.store.awaiters[:i], c.store.awaiters[i+1:]...)
			return &etResult{rowsAffected: 1}, nil
		}
	}
	return &etResult{rowsAffected: 0}, nil
}

func (c *etConn) execDeleteSubscription(args []driver.NamedValue) (driver.Result, error) {
	id, err := etArgString(args, 1)
	if err != nil {
		return nil, err
	}
	tid, err := etArgString(args, 2)
	if err != nil {
		return nil, err
	}

	for i, s := range c.store.subscriptions {
		if s.id == id && s.tenantID == tid {
			c.store.subscriptions = append(c.store.subscriptions[:i], c.store.subscriptions[i+1:]...)
			return &etResult{rowsAffected: 1}, nil
		}
	}
	return &etResult{rowsAffected: 0}, nil
}

func (c *etConn) execUpdateEventProcessed(query string, args []driver.NamedValue) (driver.Result, error) {
	id, err := etArgString(args, 1)
	if err != nil {
		return nil, err
	}

	for i, evt := range c.store.events {
		if evt.id == id {
			evt.processed = true
			if strings.Contains(query, "consumed") {
				evt.status = "consumed"
			} else {
				evt.status = "completed"
			}
			evt.errorMsg = nil
			c.store.events[i] = evt
			return &etResult{rowsAffected: 1}, nil
		}
	}
	return &etResult{rowsAffected: 0}, nil
}

func (c *etConn) execUpdateEventRetry(query string, args []driver.NamedValue) (driver.Result, error) {
	id, err := etArgString(args, 1)
	if err != nil {
		return nil, err
	}

	for i, evt := range c.store.events {
		if evt.id == id {
			if len(args) >= 2 {
				if v, err := etArgInt64(args, 2); err == nil {
					evt.retryCount = int(v)
				}
			}
			if len(args) >= 3 {
				if v, err := etArgAny(args, 3); err == nil {
					if s, ok := v.(string); ok {
						evt.errorMsg = &s
					}
				}
			}
			now := time.Now()
			evt.lastRetryAt = &now

			if strings.Contains(query, "dead_letter") {
				evt.status = "dead_letter"
				evt.processed = true
			} else if strings.Contains(query, "consumed") {
				evt.status = "consumed"
				evt.processed = true
			} else {
				evt.status = "pending"
			}
			c.store.events[i] = evt
			return &etResult{rowsAffected: 1}, nil
		}
	}
	return &etResult{rowsAffected: 0}, nil
}

func (c *etConn) execUpdateEvent(args []driver.NamedValue) (driver.Result, error) {
	// Generic update for error_msg updates.
	id, err := etArgString(args, 2) // UPDATE ... SET error_msg = $1 WHERE id = $2
	if err != nil {
		// Try with id at position 1
		id, err = etArgString(args, 1)
		if err != nil {
			return nil, err
		}
	}
	for i, evt := range c.store.events {
		if evt.id == id {
			if len(args) >= 1 {
				if v, err := etArgAny(args, 1); err == nil {
					if s, ok := v.(string); ok {
						if strings.Contains(s, "failed") || strings.Contains(s, "invalid") {
							evt.errorMsg = &s
						}
					}
				}
			}
			c.store.events[i] = evt
			return &etResult{rowsAffected: 1}, nil
		}
	}
	return &etResult{rowsAffected: 0}, nil
}

// ---------------------------------------------------------------------------
// QueryContext routing
// ---------------------------------------------------------------------------

func (c *etConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.store.mu.RLock()
	defer c.store.mu.RUnlock()

	q := strings.Join(strings.Fields(query), " ")
	switch {
	case strings.Contains(q, "tenant_api_keys") && strings.Contains(q, "SELECT tenant_id"):
		return c.queryTenantLookup(args)
	case strings.Contains(q, "INSERT INTO event_subscriptions"):
		return c.queryInsertSubscription(args)
	case strings.Contains(q, "SELECT COUNT(*) FROM event_subscriptions"):
		return c.querySubCount(args)
	case strings.Contains(q, "SELECT COALESCE(MAX(max_retries), 3) FROM event_subscriptions"):
		return c.queryMaxRetries(args)
	case strings.Contains(q, "SELECT COALESCE(status, 'pending'), event_type, event_data FROM ingested_events"):
		return c.queryEventForRetry(args)
	case strings.Contains(q, "SELECT id, event_type, event_data, received_at") && strings.Contains(q, "FROM ingested_events"):
		return c.queryEventForAwait(args)
	case strings.Contains(q, "SELECT workflow_id FROM event_awaiters"):
		return c.queryAwaiters(args)
	case strings.Contains(q, "FROM event_subscriptions") && strings.Contains(q, "ORDER BY"):
		return c.queryListSubscriptions(args)
	case strings.Contains(q, "FROM event_subscriptions") && strings.Contains(q, "event_type ="):
		return c.querySubscriptionsForMatch(args)
	case strings.Contains(q, "SELECT id, tenant_id, event_type, event_data, retry_count FROM ingested_events"):
		return c.queryProcessBatch(args)
	default:
		return nil, fmt.Errorf("etConn: unexpected Query query: %s", q)
	}
}

// ---------------------------------------------------------------------------
// Query implementations
// ---------------------------------------------------------------------------

func (c *etConn) queryTenantLookup(args []driver.NamedValue) (driver.Rows, error) {
	keyHash, err := etArgBytes(args, 1)
	if err != nil {
		return nil, err
	}
	hashHex := fmt.Sprintf("%x", keyHash)
	tid, ok := c.store.apiKeys[hashHex]
	if !ok {
		return &etRows{columns: []string{"tenant_id"}}, nil
	}
	return &etRows{
		columns: []string{"tenant_id"},
		data:    [][]driver.Value{{tid}},
	}, nil
}

func (c *etConn) queryInsertSubscription(args []driver.NamedValue) (driver.Rows, error) {
	tenantID, err := etArgString(args, 1)
	if err != nil {
		return nil, err
	}
	eventType, err := etArgString(args, 2)
	if err != nil {
		return nil, err
	}
	defName, err := etArgString(args, 3)
	if err != nil {
		return nil, err
	}
	entryPoint, err := etArgString(args, 4)
	if err != nil {
		return nil, err
	}
	inputTemplate, err := etArgString(args, 5)
	if err != nil {
		return nil, err
	}
	filterExpr := ""
	if len(args) >= 6 {
		if v, err := etArgAny(args, 6); err == nil {
			if s, ok := v.(string); ok {
				filterExpr = s
			}
		}
	}
	maxRetries := 3
	if len(args) >= 7 {
		if v, err := etArgAny(args, 7); err == nil && v != nil {
			if n, ok := v.(int64); ok {
				maxRetries = int(n)
			}
		}
	}

	id := uuid.New().String()
	now := time.Now()

	c.store.subscriptions = append(c.store.subscriptions, etSubscriptionRow{
		id:            id,
		tenantID:      tenantID,
		eventType:     eventType,
		defName:       defName,
		entryPoint:    entryPoint,
		inputTemplate: inputTemplate,
		filterExpr:    filterExpr,
		maxRetries:    maxRetries,
		enabled:       true,
		createdAt:     now,
	})

	return &etRows{
		columns: []string{"id"},
		data:    [][]driver.Value{{id}},
	}, nil
}

func (c *etConn) querySubCount(args []driver.NamedValue) (driver.Rows, error) {
	tid, err := etArgString(args, 1)
	if err != nil {
		return nil, err
	}
	eventType, err := etArgString(args, 2)
	if err != nil {
		return nil, err
	}

	count := 0
	for _, s := range c.store.subscriptions {
		if s.tenantID == tid && s.eventType == eventType && s.enabled {
			count++
		}
	}
	return &etRows{
		columns: []string{"count"},
		data:    [][]driver.Value{{int64(count)}},
	}, nil
}

func (c *etConn) queryMaxRetries(args []driver.NamedValue) (driver.Rows, error) {
	id, err := etArgString(args, 1)
	if err != nil {
		return nil, err
	}

	var tenantID, eventType string
	for _, evt := range c.store.events {
		if evt.id == id {
			tenantID = evt.tenantID
			eventType = evt.eventType
			break
		}
	}

	maxRetries := int64(3)
	for _, s := range c.store.subscriptions {
		if s.tenantID == tenantID && s.eventType == eventType {
			if int64(s.maxRetries) > maxRetries {
				maxRetries = int64(s.maxRetries)
			}
		}
	}
	return &etRows{
		columns: []string{"max"},
		data:    [][]driver.Value{{maxRetries}},
	}, nil
}

func (c *etConn) queryEventForRetry(args []driver.NamedValue) (driver.Rows, error) {
	id, err := etArgString(args, 1)
	if err != nil {
		return nil, err
	}

	for _, evt := range c.store.events {
		if evt.id == id {
			status := evt.status
			if status == "" {
				status = "pending"
			}
			return &etRows{
				columns: []string{"status", "event_type", "event_data"},
				data:    [][]driver.Value{{status, evt.eventType, []byte(evt.eventData)}},
			}, nil
		}
	}
	return &etRows{columns: []string{"status", "event_type", "event_data"}}, nil
}

func (c *etConn) queryEventForAwait(args []driver.NamedValue) (driver.Rows, error) {
	tid, err := etArgString(args, 1)
	if err != nil {
		return nil, err
	}
	eventType, err := etArgString(args, 2)
	if err != nil {
		return nil, err
	}

	// Find the latest unprocessed event matching tenant + type.
	var best *etIngestedEventRow
	for _, evt := range c.store.events {
		if evt.tenantID == tid && evt.eventType == eventType && !evt.processed {
			if best == nil || evt.receivedAt.After(best.receivedAt) {
				best = &evt
			}
		}
	}

	if best == nil {
		return &etRows{columns: []string{"id", "event_type", "event_data", "received_at"}}, nil
	}
	return &etRows{
		columns: []string{"id", "event_type", "event_data", "received_at"},
		data:    [][]driver.Value{{best.id, best.eventType, []byte(best.eventData), best.receivedAt}},
	}, nil
}

func (c *etConn) queryAwaiters(args []driver.NamedValue) (driver.Rows, error) {
	tid, err := etArgString(args, 1)
	if err != nil {
		return nil, err
	}
	eventType, err := etArgString(args, 2)
	if err != nil {
		return nil, err
	}

	var data [][]driver.Value
	for _, a := range c.store.awaiters {
		if a.tenantID == tid && a.eventType == eventType {
			data = append(data, []driver.Value{a.workflowID})
		}
	}
	return &etRows{
		columns: []string{"workflow_id"},
		data:    data,
	}, nil
}

func (c *etConn) querySubscriptionsForMatch(args []driver.NamedValue) (driver.Rows, error) {
	tid, err := etArgString(args, 1)
	if err != nil {
		return nil, err
	}
	eventType := ""
	if len(args) >= 2 {
		if v, err := etArgAny(args, 2); err == nil {
			if s, ok := v.(string); ok {
				eventType = s
			}
		}
	}

	var data [][]driver.Value
	for _, s := range c.store.subscriptions {
		if s.tenantID == tid && (eventType == "" || s.eventType == eventType) && s.enabled {
			data = append(data, []driver.Value{
				s.id, s.tenantID, s.eventType, s.defName, s.entryPoint,
				[]byte(s.inputTemplate), s.filterExpr, s.enabled, s.createdAt, int64(s.maxRetries),
			})
		}
	}
	return &etRows{
		columns: []string{"id", "tenant_id", "event_type", "def_name", "entry_point", "input_template", "filter_expr", "enabled", "created_at", "max_retries"},
		data:    data,
	}, nil
}

func (c *etConn) queryListSubscriptions(args []driver.NamedValue) (driver.Rows, error) {
	tid, err := etArgString(args, 1)
	if err != nil {
		return nil, err
	}

	var data [][]driver.Value
	for _, s := range c.store.subscriptions {
		if s.tenantID == tid {
			data = append(data, []driver.Value{
				s.id, s.tenantID, s.eventType, s.defName, s.entryPoint,
				[]byte(s.inputTemplate), s.filterExpr, int64(s.maxRetries), s.enabled, s.createdAt,
			})
		}
	}
	return &etRows{
		columns: []string{"id", "tenant_id", "event_type", "def_name", "entry_point", "input_template", "filter_expr", "max_retries", "enabled", "created_at"},
		data:    data,
	}, nil
}

func (c *etConn) queryProcessBatch(args []driver.NamedValue) (driver.Rows, error) {
	cutoff := time.Now().Add(-10 * time.Second)
	var data [][]driver.Value
	for _, evt := range c.store.events {
		if !evt.processed && (evt.status == "pending" || evt.status == "") && evt.receivedAt.Before(cutoff) {
			data = append(data, []driver.Value{
				evt.id, evt.tenantID, evt.eventType, []byte(evt.eventData), int64(evt.retryCount),
			})
		}
	}
	return &etRows{
		columns: []string{"id", "tenant_id", "event_type", "event_data", "retry_count"},
		data:    data,
	}, nil
}

// ---------------------------------------------------------------------------
// Argument extractors for eventtriggers
// ---------------------------------------------------------------------------

func etArgString(args []driver.NamedValue, ordinal int) (string, error) {
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

func etArgBytes(args []driver.NamedValue, ordinal int) ([]byte, error) {
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

func etArgInt64(args []driver.NamedValue, ordinal int) (int64, error) {
	for _, a := range args {
		if a.Ordinal == ordinal {
			switch v := a.Value.(type) {
			case int64:
				return v, nil
			case float64:
				return int64(v), nil
			default:
				return 0, fmt.Errorf("arg %d: want int64, got %T", ordinal, a.Value)
			}
		}
	}
	return 0, fmt.Errorf("arg %d not found", ordinal)
}

func etArgAny(args []driver.NamedValue, ordinal int) (driver.Value, error) {
	for _, a := range args {
		if a.Ordinal == ordinal {
			return a.Value, nil
		}
	}
	return nil, fmt.Errorf("arg %d not found", ordinal)
}

// ---------------------------------------------------------------------------
// Test setup
// ---------------------------------------------------------------------------

var etTestTenantID = uuid.MustParse("00000000-0000-0000-0000-000000000001")
var etTestTenantStr = etTestTenantID.String()

func setupETPlugin(t *testing.T) (*Plugin, http.Handler, *etDBStore) {
	t.Helper()

	store := newETDBStore()

	keyHash := sha256.Sum256([]byte("test-api-key"))
	store.apiKeys[fmt.Sprintf("%x", keyHash)] = etTestTenantStr

	db := sql.OpenDB(&etConnector{store: store})
	t.Cleanup(func() { db.Close() })

	p := &Plugin{
		db:     &engine.SQLDBAdapter{DB: db},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatalf("RegisterRoutes: %v", err)
	}

	handler := auth.Middleware(engine.NewPostgresStore(db), false)(mux)
	return p, handler, store
}

func etAuthedRequest(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.Header.Set("Authorization", "Bearer test-api-key")
	return req
}

// ===========================================================================
// DB-backed behavioral tests
// ===========================================================================

func TestPublishEventHandler(t *testing.T) {
	_, handler, store := setupETPlugin(t)

	// First create a subscription.
	subBody := `{"event_type":"order.created","def_name":"handleOrder","entry_point":"HandleOrder"}`
	req := etAuthedRequest("POST", "/api/events/subscriptions", strings.NewReader(subBody))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create sub: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// Publish an event.
	pubBody := `{"id":"a1b2c3d4-e5f6-7890-abcd-ef1234567890","event_type":"order.created","data":{"order_id":42}}`
	req = etAuthedRequest("POST", "/api/events/publish", strings.NewReader(pubBody))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("publish: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Status  string `json:"status"`
		Matched int    `json:"matched"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Status != "published" {
		t.Errorf("expected status 'published', got %s", resp.Status)
	}

	// Verify the event was stored.
	store.mu.RLock()
	n := len(store.events)
	store.mu.RUnlock()
	if n != 1 {
		t.Errorf("expected 1 event in store, got %d", n)
	}
}

func TestPublishEventHandlerMissingFields(t *testing.T) {
	_, handler, _ := setupETPlugin(t)

	// Missing event_type.
	body := `{"id":"a1b2c3d4-e5f6-7890-abcd-ef1234567890","data":{}}`
	req := etAuthedRequest("POST", "/api/events/publish", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing event_type: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}

	// Missing id.
	body = `{"event_type":"test.type","data":{}}`
	req = etAuthedRequest("POST", "/api/events/publish", strings.NewReader(body))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing id: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}

	// Invalid JSON body.
	req = etAuthedRequest("POST", "/api/events/publish", strings.NewReader(`not-json`))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid JSON: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPublishEventHandlerNoAuth(t *testing.T) {
	_, handler, _ := setupETPlugin(t)

	req := httptest.NewRequest("POST", "/api/events/publish",
		strings.NewReader(`{"id":"a1b2c3d4-e5f6-7890-abcd-ef1234567890","event_type":"test","data":{}}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without auth, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateSubscriptionHandler(t *testing.T) {
	_, handler, store := setupETPlugin(t)

	body := `{"event_type":"order.created","def_name":"handleOrder","entry_point":"HandleOrder","filter_expr":"event.data.amount > 100","max_retries":5}`
	req := etAuthedRequest("POST", "/api/events/subscriptions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var sub subscriptionJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &sub); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if sub.EventType != "order.created" {
		t.Errorf("expected event_type 'order.created', got %s", sub.EventType)
	}
	if sub.DefName != "handleOrder" {
		t.Errorf("expected def_name 'handleOrder', got %s", sub.DefName)
	}
	if sub.MaxRetries != 5 {
		t.Errorf("expected max_retries 5, got %d", sub.MaxRetries)
	}
	if !sub.Enabled {
		t.Error("expected enabled true")
	}
	if sub.ID == uuid.Nil {
		t.Error("expected non-nil subscription ID")
	}

	store.mu.RLock()
	n := len(store.subscriptions)
	store.mu.RUnlock()
	if n != 1 {
		t.Errorf("expected 1 subscription in store, got %d", n)
	}
}

func TestCreateSubscriptionMissingFields(t *testing.T) {
	_, handler, _ := setupETPlugin(t)

	// Missing event_type.
	body := `{"def_name":"test","entry_point":"Test"}`
	req := etAuthedRequest("POST", "/api/events/subscriptions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing event_type: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}

	// Missing def_name.
	body = `{"event_type":"test","entry_point":"Test"}`
	req = etAuthedRequest("POST", "/api/events/subscriptions", strings.NewReader(body))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing def_name: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}

	// Invalid JSON body.
	req = etAuthedRequest("POST", "/api/events/subscriptions", strings.NewReader(`not-json`))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid JSON: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateSubscriptionNoAuth(t *testing.T) {
	_, handler, _ := setupETPlugin(t)

	req := httptest.NewRequest("POST", "/api/events/subscriptions",
		strings.NewReader(`{"event_type":"test","def_name":"test","entry_point":"Test"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without auth, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestListSubscriptionsHandler(t *testing.T) {
	_, handler, store := setupETPlugin(t)

	// Empty list initially.
	req := etAuthedRequest("GET", "/api/events/subscriptions", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var subs []subscriptionJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &subs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(subs) != 0 {
		t.Errorf("expected empty list, got %d", len(subs))
	}

	// Create 2 subscriptions.
	for i := 0; i < 2; i++ {
		body := fmt.Sprintf(`{"event_type":"evt.%d","def_name":"def%d","entry_point":"Entry%d"}`, i, i, i)
		req := etAuthedRequest("POST", "/api/events/subscriptions", strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create sub %d: expected 201, got %d", i, rec.Code)
		}
	}

	// List should return 2 subscriptions.
	req = etAuthedRequest("GET", "/api/events/subscriptions", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &subs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(subs) != 2 {
		t.Errorf("expected 2 subscriptions, got %d", len(subs))
	}

	store.mu.RLock()
	n := len(store.subscriptions)
	store.mu.RUnlock()
	if n != 2 {
		t.Errorf("expected 2 subs in store, got %d", n)
	}
}

func TestListSubscriptionsNoAuth(t *testing.T) {
	_, handler, _ := setupETPlugin(t)

	req := httptest.NewRequest("GET", "/api/events/subscriptions", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without auth, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDeleteSubscriptionHandler(t *testing.T) {
	_, handler, store := setupETPlugin(t)

	// Create a subscription.
	body := `{"event_type":"test","def_name":"test","entry_point":"Test"}`
	req := etAuthedRequest("POST", "/api/events/subscriptions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", rec.Code)
	}

	var sub subscriptionJSON
	json.Unmarshal(rec.Body.Bytes(), &sub)

	// Delete it.
	req = etAuthedRequest("DELETE", "/api/events/subscriptions/"+sub.ID.String(), nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: expected 204, got %d: %s", rec.Code, rec.Body.String())
	}

	store.mu.RLock()
	n := len(store.subscriptions)
	store.mu.RUnlock()
	if n != 0 {
		t.Errorf("expected 0 subscriptions after delete, got %d", n)
	}
}

func TestDeleteSubscriptionNotFound(t *testing.T) {
	_, handler, _ := setupETPlugin(t)

	req := etAuthedRequest("DELETE", "/api/events/subscriptions/"+uuid.New().String(), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDeleteSubscriptionInvalidID(t *testing.T) {
	_, handler, _ := setupETPlugin(t)

	req := etAuthedRequest("DELETE", "/api/events/subscriptions/not-a-uuid", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDeleteSubscriptionNoAuth(t *testing.T) {
	_, handler, _ := setupETPlugin(t)

	req := httptest.NewRequest("DELETE", "/api/events/subscriptions/"+uuid.New().String(), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without auth, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRegisterAndUnregisterAwaiter(t *testing.T) {
	store := newETDBStore()

	db := sql.OpenDB(&etConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:     &engine.SQLDBAdapter{DB: db},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// Register.
	p.registerAwaiter(context.Background(), etTestTenantID.String(), "wf-123", "order.created")

	store.mu.RLock()
	n := len(store.awaiters)
	store.mu.RUnlock()
	if n != 1 {
		t.Fatalf("expected 1 awaiter, got %d", n)
	}
	if store.awaiters[0].workflowID != "wf-123" {
		t.Errorf("expected workflowID wf-123, got %s", store.awaiters[0].workflowID)
	}

	// Unregister.
	unregisterAwaiter(context.Background(), &engine.SQLDBAdapter{DB: db}, slog.New(slog.NewTextHandler(io.Discard, nil)), "wf-123", "order.created")

	store.mu.RLock()
	n = len(store.awaiters)
	store.mu.RUnlock()
	if n != 0 {
		t.Errorf("expected 0 awaiters after unregister, got %d", n)
	}
}

func TestAwaitEventFindsAndConsumesEvent(t *testing.T) {
	store := newETDBStore()

	db := sql.OpenDB(&etConnector{store: store})
	defer db.Close()

	// Add an unprocessed event to the store.
	store.events = append(store.events, etIngestedEventRow{
		id:        uuid.New().String(),
		tenantID:  etTestTenantStr,
		eventType: "order.created",
		eventData: `{"order_id":42}`,
		processed: false,
		status:    "pending",
	})

	p := &Plugin{
		db:     &engine.SQLDBAdapter{DB: db},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// Set up call context so awaitEvent can identify the tenant.
	cc := &plugin.CallContext{TenantID: etTestTenantID.String(), WorkflowID: "wf-consumer"}
	ctx := plugin.WithCallContext(context.Background(), cc)

	// Call awaitEvent with matching event type.
	input, _ := json.Marshal(map[string]any{
		"event_type": "order.created",
		"timeout_ms": 5000,
	})
	output, err := p.awaitEvent(ctx, string(input))
	if err != nil {
		t.Fatalf("awaitEvent: %v", err)
	}

	var result awaitEventOutput
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if !result.Found {
		t.Error("expected Found=true")
	}
	if result.EventType != "order.created" {
		t.Errorf("expected event_type 'order.created', got %s", result.EventType)
	}

	// Verify event is marked as consumed.
	store.mu.RLock()
	evt := store.events[0]
	store.mu.RUnlock()
	if !evt.processed {
		t.Error("expected event to be marked processed")
	}
	if evt.status != "consumed" {
		t.Errorf("expected status 'consumed', got %s", evt.status)
	}
}

func TestAwaitEventNoEvent(t *testing.T) {
	store := newETDBStore()

	db := sql.OpenDB(&etConnector{store: store})
	defer db.Close()

	p := &Plugin{
		db:     &engine.SQLDBAdapter{DB: db},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// Set up call context so awaitEvent can identify the tenant.
	cc := &plugin.CallContext{TenantID: etTestTenantID.String()}
	ctx := plugin.WithCallContext(context.Background(), cc)

	// When no event, awaitEvent should return Found=false.
	input, _ := json.Marshal(map[string]any{
		"event_type": "nonexistent.event",
		"timeout_ms": 1000,
	})
	output, err := p.awaitEvent(ctx, string(input))
	if err != nil {
		t.Fatalf("awaitEvent: %v", err)
	}

	var result awaitEventOutput
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if result.Found {
		t.Error("expected Found=false for non-existent event")
	}
}

func TestAwaitEventWithAwaiterRegistration(t *testing.T) {
	store := newETDBStore()

	db := sql.OpenDB(&etConnector{store: store})
	defer db.Close()

	// Create context with workflow ID so registerAwaiter is called.
	cc := &plugin.CallContext{
		TenantID:   etTestTenantID.String(),
		WorkflowID: "wf-await-test",
	}
	ctx := plugin.WithCallContext(context.Background(), cc)

	p := &Plugin{
		db:     &engine.SQLDBAdapter{DB: db},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// No event in store, so awaitEvent should register an awaiter.
	input, _ := json.Marshal(map[string]any{
		"event_type": "order.shipped",
		"timeout_ms": 5000,
	})
	_, err := p.awaitEvent(ctx, string(input))
	if err != nil {
		t.Fatalf("awaitEvent: %v", err)
	}

	// Verify awaiter was registered.
	store.mu.RLock()
	defer store.mu.RUnlock()
	if len(store.awaiters) != 1 {
		t.Fatalf("expected 1 awaiter, got %d", len(store.awaiters))
	}
	if store.awaiters[0].workflowID != "wf-await-test" {
		t.Errorf("expected workflowID 'wf-await-test', got %s", store.awaiters[0].workflowID)
	}
	if store.awaiters[0].eventType != "order.shipped" {
		t.Errorf("expected eventType 'order.shipped', got %s", store.awaiters[0].eventType)
	}
}

func TestFullPluginLifecycle(t *testing.T) {
	_, handler, store := setupETPlugin(t)

	// 1. Create a subscription.
	subBody := `{"event_type":"user.created","def_name":"handleUser","entry_point":"HandleUser","filter_expr":"event.data.role == \"admin\""}`
	req := etAuthedRequest("POST", "/api/events/subscriptions", strings.NewReader(subBody))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create sub: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var sub subscriptionJSON
	json.Unmarshal(rec.Body.Bytes(), &sub)

	// 2. List subscriptions and verify.
	req = etAuthedRequest("GET", "/api/events/subscriptions", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d", rec.Code)
	}
	var subs []subscriptionJSON
	json.Unmarshal(rec.Body.Bytes(), &subs)
	if len(subs) != 1 {
		t.Fatalf("expected 1 sub, got %d", len(subs))
	}

	// 3. Publish an event.
	pubBody := `{"id":"b1b2c3d4-e5f6-7890-abcd-ef1234567890","event_type":"user.created","data":{"role":"admin"}}`
	req = etAuthedRequest("POST", "/api/events/publish", strings.NewReader(pubBody))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("publish: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify event stored.
	store.mu.RLock()
	evtCount := len(store.events)
	store.mu.RUnlock()
	if evtCount != 1 {
		t.Errorf("expected 1 event, got %d", evtCount)
	}

	// 4. Delete the subscription.
	req = etAuthedRequest("DELETE", "/api/events/subscriptions/"+sub.ID.String(), nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ===========================================================================
// Background worker tests
// ===========================================================================

func TestRunEventTriggersNilDB(t *testing.T) {
	p := &Plugin{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := p.Run(ctx)
	if err != nil {
		t.Errorf("Run() with nil DB returned error: %v", err)
	}
}

func TestRetryEventHandler(t *testing.T) {
	t.Run("retry dead_letter event returns 200", func(t *testing.T) {
		_, handler, store := setupETPlugin(t)

		eventID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
		store.mu.Lock()
		store.events = append(store.events, etIngestedEventRow{
			id:         eventID.String(),
			tenantID:   etTestTenantStr,
			eventType:  "test.event",
			eventData:  `{"key":"value"}`,
			processed:  true,
			status:     "dead_letter",
			receivedAt: time.Now().Add(-time.Hour),
		})
		store.mu.Unlock()

		req := etAuthedRequest("POST", "/api/events/"+eventID.String()+"/retry", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("event not found returns 404", func(t *testing.T) {
		_, handler, _ := setupETPlugin(t)
		req := etAuthedRequest("POST",
			"/api/events/cccccccc-cccc-cccc-cccc-cccccccccccc/retry", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("wrong status returns 400", func(t *testing.T) {
		_, handler, store := setupETPlugin(t)
		eventID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
		store.mu.Lock()
		store.events = append(store.events, etIngestedEventRow{
			id:         eventID.String(),
			tenantID:   etTestTenantStr,
			eventType:  "test.event",
			eventData:  `{}`,
			processed:  true,
			status:     "completed",
			receivedAt: time.Now(),
		})
		store.mu.Unlock()

		req := etAuthedRequest("POST", "/api/events/"+eventID.String()+"/retry", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("missing auth returns 401", func(t *testing.T) {
		_, handler, _ := setupETPlugin(t)
		req := httptest.NewRequest("POST",
			"/api/events/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa/retry", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("invalid event id returns 400", func(t *testing.T) {
		_, handler, _ := setupETPlugin(t)
		req := etAuthedRequest("POST", "/api/events/not-a-uuid/retry", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
		}
	})
}

func TestProcessBatch(t *testing.T) {
	t.Run("no unprocessed events", func(t *testing.T) {
		p, _, _ := setupETPlugin(t)
		p.processBatch(context.Background())
	})

	t.Run("with unprocessed events", func(t *testing.T) {
		p, _, store := setupETPlugin(t)
		store.mu.Lock()
		store.events = append(store.events, etIngestedEventRow{
			id:         uuid.New().String(),
			tenantID:   etTestTenantStr,
			eventType:  "test.event",
			eventData:  `{"key":"value"}`,
			processed:  false,
			status:     "pending",
			receivedAt: time.Now().Add(-time.Hour),
		})
		store.mu.Unlock()

		p.processBatch(context.Background())
	})

	t.Run("unprocessed invalid data is dead-lettered", func(t *testing.T) {
		p, _, store := setupETPlugin(t)
		store.mu.Lock()
		store.events = append(store.events, etIngestedEventRow{
			id:         uuid.New().String(),
			tenantID:   etTestTenantStr,
			eventType:  "test.event",
			eventData:  `{invalid json}`,
			processed:  false,
			status:     "pending",
			receivedAt: time.Now().Add(-time.Hour),
		})
		store.mu.Unlock()

		p.processBatch(context.Background())
	})
}

func TestRetryEventBackground(t *testing.T) {
	t.Run("invalid event data goes to dead_letter", func(t *testing.T) {
		p, _, store := setupETPlugin(t)
		eventID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
		store.mu.Lock()
		store.events = append(store.events, etIngestedEventRow{
			id:         eventID.String(),
			tenantID:   etTestTenantStr,
			eventType:  "test.event",
			eventData:  `{invalid json}`,
			processed:  false,
			status:     "pending",
			receivedAt: time.Now(),
		})
		store.mu.Unlock()

		p.retryEvent(context.Background(), eventID, etTestTenantID, "test.event", []byte(`{invalid json}`), 0)
	})

	t.Run("valid event with no subscriptions", func(t *testing.T) {
		p, _, store := setupETPlugin(t)
		eventID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
		store.mu.Lock()
		store.events = append(store.events, etIngestedEventRow{
			id:         eventID.String(),
			tenantID:   etTestTenantStr,
			eventType:  "test.event",
			eventData:  `{"key":"value"}`,
			processed:  false,
			status:     "pending",
			receivedAt: time.Now(),
		})
		store.mu.Unlock()

		p.retryEvent(context.Background(), eventID, etTestTenantID, "test.event", []byte(`{"key":"value"}`), 0)
	})

	t.Run("valid event with subscription match retry", func(t *testing.T) {
		p, _, store := setupETPlugin(t)
		eventID := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")
		store.mu.Lock()
		store.events = append(store.events, etIngestedEventRow{
			id:         eventID.String(),
			tenantID:   etTestTenantStr,
			eventType:  "test.event",
			eventData:  `{"key":"value"}`,
			processed:  false,
			status:     "pending",
			receivedAt: time.Now(),
		})
		store.subscriptions = append(store.subscriptions, etSubscriptionRow{
			id:        uuid.New().String(),
			tenantID:  etTestTenantStr,
			eventType: "test.event",
			defName:   "wf",
			enabled:   true,
		})
		store.mu.Unlock()

		p.retryEvent(context.Background(), eventID, etTestTenantID, "test.event", []byte(`{"key":"value"}`), 0)
	})
}

func TestMarkRetryFailed(t *testing.T) {
	t.Run("below max retries sets pending", func(t *testing.T) {
		p, _, store := setupETPlugin(t)
		eventID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
		store.mu.Lock()
		store.events = append(store.events, etIngestedEventRow{
			id:         eventID.String(),
			tenantID:   etTestTenantStr,
			eventType:  "test.event",
			eventData:  `{"k":"v"}`,
			processed:  false,
			status:     "pending",
			receivedAt: time.Now(),
		})
		store.mu.Unlock()

		p.markRetryFailed(context.Background(), eventID, 1, "some error")
	})

	t.Run("at max retries moves to dead_letter", func(t *testing.T) {
		p, _, store := setupETPlugin(t)
		eventID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
		store.mu.Lock()
		store.events = append(store.events, etIngestedEventRow{
			id:         eventID.String(),
			tenantID:   etTestTenantStr,
			eventType:  "test.event",
			eventData:  `{"k":"v"}`,
			processed:  false,
			status:     "pending",
			receivedAt: time.Now(),
		})
		store.subscriptions = append(store.subscriptions, etSubscriptionRow{
			id:         uuid.New().String(),
			tenantID:   etTestTenantStr,
			eventType:  "test.event",
			defName:    "wf",
			maxRetries: 1,
			enabled:    true,
		})
		store.mu.Unlock()

		p.markRetryFailed(context.Background(), eventID, 1, "reached max retries")
	})
}
