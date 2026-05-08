package eventtriggers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rcownie/cleat/internal/auth"
	"github.com/rcownie/cleat/internal/plugin"
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
			TenantID: uuid.New(),
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
			TenantID: uuid.New(),
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
		data := map[string]interface{}{"order_id": "123", "amount": 99.5}

		result, err := mergeInputAndTemplate(tmpl, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		var parsed map[string]interface{}
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
		data := map[string]interface{}{"order_id": "456", "amount": 50.0}

		result, err := mergeInputAndTemplate(tmpl, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		var parsed map[string]interface{}
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
		data := map[string]interface{}{"priority": "high", "extra": "value"}

		result, err := mergeInputAndTemplate(tmpl, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		var parsed map[string]interface{}
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
		data := map[string]interface{}{"key": "value"}

		result, err := mergeInputAndTemplate(tmpl, data)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		var parsed map[string]interface{}
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

		var raw map[string]interface{}
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
			Data:      map[string]interface{}{"amount": 99.5, "currency": "USD"},
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

	payload := map[string]interface{}{"status": "ok", "value": 42}
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
	var decoded map[string]interface{}
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
	var decoded map[string]interface{}
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

	p.writeJSON(rec, http.StatusOK, map[string]interface{}{})

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
		input interface{}
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
		a, b interface{}
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
		data := map[string]interface{}{"status": "active"}
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
		data := map[string]interface{}{"flag": true}
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
		result, err = evalComparison(ce, data)
		if err == nil {
			t.Error("expected error for > on bool")
		}
	})

	t.Run("null comparisons", func(t *testing.T) {
		data := map[string]interface{}{"value": nil}
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
		data := map[string]interface{}{"amount": float64(100)}
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
		data := map[string]interface{}{}
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
		data := map[string]interface{}{"items": []interface{}{"a", "b"}}
		_, found := getPath(data, "items[invalid]")
		if found {
			t.Error("expected found=false for invalid array index string")
		}
	})

	t.Run("index out of bounds", func(t *testing.T) {
		data := map[string]interface{}{"items": []interface{}{"a", "b"}}
		_, found := getPath(data, "items[5]")
		if found {
			t.Error("expected found=false for out-of-bounds index")
		}
	})

	t.Run("negative index", func(t *testing.T) {
		data := map[string]interface{}{"items": []interface{}{"a", "b"}}
		_, found := getPath(data, "items[-1]")
		if found {
			t.Error("expected found=false for negative index")
		}
	})

	t.Run("index into non-array", func(t *testing.T) {
		data := map[string]interface{}{"items": "not-an-array"}
		_, found := getPath(data, "items[0]")
		if found {
			t.Error("expected found=false for indexing non-array")
		}
	})

	t.Run("field not found after array", func(t *testing.T) {
		data := map[string]interface{}{"items": []interface{}{
			map[string]interface{}{"sku": "ABC"},
		}}
		_, found := getPath(data, "items[0].missing")
		if found {
			t.Error("expected found=false for missing field in nested object")
		}
	})

	t.Run("path into non-object", func(t *testing.T) {
		data := map[string]interface{}{"items": []interface{}{"a", "b"}}
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
	data := map[string]interface{}{
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
	data := map[string]interface{}{
		"event": map[string]interface{}{
			"data": map[string]interface{}{
				"amount": float64(100),
				"status": "active",
				"flag":   true,
				"items":  []interface{}{"a", "b"},
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
		data := map[string]interface{}{
			"event": map[string]interface{}{
				"data": map[string]interface{}{
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
		data := map[string]interface{}{
			"event": map[string]interface{}{
				"data": map[string]interface{}{
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
		data := map[string]interface{}{
			"event": map[string]interface{}{
				"data": map[string]interface{}{
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
	data := map[string]interface{}{}
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
	data := map[string]interface{}{
		"event": map[string]interface{}{
			"data": map[string]interface{}{
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
	data := map[string]interface{}{"amount": -5.0}
	result, err := EvaluateFilter(`event.data.amount == -5`, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result {
		t.Error("expected -5 == -5")
	}
}

func TestTokenizerDecimalNumber(t *testing.T) {
	data := map[string]interface{}{"price": 99.5}
	result, err := EvaluateFilter(`event.data.price == 99.5`, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result {
		t.Error("expected 99.5 == 99.5")
	}
}

func TestTokenizerTrueKeyword(t *testing.T) {
	data := map[string]interface{}{"flag": true}
	result, err := EvaluateFilter(`event.data.flag == true`, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result {
		t.Error("expected true == true")
	}
}

func TestTokenizerFalseKeyword(t *testing.T) {
	data := map[string]interface{}{"flag": false}
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
	data := map[string]interface{}{
		"event": map[string]interface{}{
			"data": map[string]interface{}{
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
	data := map[string]interface{}{
		"event": map[string]interface{}{
			"data": map[string]interface{}{
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
	data := map[string]interface{}{
		"event": map[string]interface{}{
			"data": map[string]interface{}{
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
			map[string]interface{}{"items": "not-an-array"},
			[]pathStep{{isIndex: true, index: 0}},
		)
		if err == nil {
			t.Error("expected error for indexing non-array")
		}
	})

	t.Run("field on non-object", func(t *testing.T) {
		_, err := evalPath(
			map[string]interface{}{"items": []interface{}{"a", "b"}},
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
	data := map[string]interface{}{
		"event": map[string]interface{}{
			"data": map[string]interface{}{
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
		map[string]interface{}{"items": []interface{}{"a", "b"}},
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
	data := map[string]interface{}{"amount": float64(100)}
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
	data := map[string]interface{}{"items": []interface{}{"a", "b"}}
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
	if err := p.Init(context.Background(), &plugin.Environment{DB: &sql.DB{}}); err != nil {
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
		data := map[string]interface{}{"status": "active"}
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
		data := map[string]interface{}{"flag": true}
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
		data := map[string]interface{}{"value": nil}
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

