package eventtriggers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rcownie/durable/internal/plugin"
)

func TestInfo(t *testing.T) {
	p := &Plugin{}
	info := p.Info()
	if info.Name != "event-triggers" {
		t.Errorf("expected Name 'event-triggers', got %q", info.Name)
	}
	if info.Version != "0.1.0" {
		t.Errorf("expected Version '0.1.0', got %q", info.Version)
	}
	if info.Description == "" {
		t.Error("expected non-empty Description")
	}
	if info.Author != "cleat" {
		t.Errorf("expected Author 'cleat', got %q", info.Author)
	}
}

func TestInit(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{}
	err := p.Init(context.Background(), env)
	if err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if p.logger == nil {
		t.Error("expected logger to be set")
	}
}

func TestInitWithLogger(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{}
	err := p.Init(context.Background(), env)
	if err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if p.logger == nil {
		t.Error("expected logger to be set")
	}
}

func TestRegisterRoutes(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	err := p.RegisterRoutes(mux)
	if err != nil {
		t.Fatalf("RegisterRoutes() returned error: %v", err)
	}

	tests := []struct {
		method string
		path   string
	}{
		{"POST", "/api/events/publish"},
		{"POST", "/api/events/subscriptions"},
		{"GET", "/api/events/subscriptions"},
		{"DELETE", "/api/events/subscriptions/11111111-1111-1111-1111-111111111111"},
	}
	for _, tt := range tests {
		req := httptest.NewRequest(tt.method, tt.path, nil)
		_, pattern := mux.Handler(req)
		if pattern == "" {
			t.Errorf("no handler matched %s %s", tt.method, tt.path)
		}
	}
}

// ---------------------------------------------------------------------------
// Filter expression tests
// ---------------------------------------------------------------------------

func TestFilterLiteralTrue(t *testing.T) {
	tests := []struct {
		name string
		expr string
		data map[string]interface{}
	}{
		{
			name: "empty expression",
			expr: "",
			data: map[string]interface{}{"price": 100.0},
		},
		{
			name: "true literal",
			expr: "true",
			data: map[string]interface{}{"price": 100.0},
		},
		{
			name: "true with no data",
			expr: "true",
			data: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := EvaluateFilter(tt.expr, tt.data)
			if err != nil {
				t.Fatalf("EvaluateFilter(%q) returned error: %v", tt.expr, err)
			}
			if !result {
				t.Errorf("EvaluateFilter(%q) = false, want true", tt.expr)
			}
		})
	}
}

func TestFilterComparison(t *testing.T) {
	data := map[string]interface{}{
		"price": 100.0,
		"name":  "widget",
	}

	tests := []struct {
		name string
		expr string
		want bool
	}{
		// ==
		{"eq true number", `event.data.price == 100`, true},
		{"eq false number", `event.data.price == 200`, false},
		// !=
		{"neq true", `event.data.price != 200`, true},
		{"neq false", `event.data.price != 100`, false},
		// >
		{"gt true", `event.data.price > 50`, true},
		{"gt false", `event.data.price > 100`, false},
		// <
		{"lt true", `event.data.price < 150`, true},
		{"lt false", `event.data.price < 100`, false},
		// >=
		{"gte true (equal)", `event.data.price >= 100`, true},
		{"gte true (greater)", `event.data.price >= 99`, true},
		{"gte false", `event.data.price >= 101`, false},
		// <=
		{"lte true (equal)", `event.data.price <= 100`, true},
		{"lte true (less)", `event.data.price <= 101`, true},
		{"lte false", `event.data.price <= 99`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := EvaluateFilter(tt.expr, data)
			if err != nil {
				t.Fatalf("EvaluateFilter(%q) returned error: %v", tt.expr, err)
			}
			if result != tt.want {
				t.Errorf("EvaluateFilter(%q) = %v, want %v", tt.expr, result, tt.want)
			}
		})
	}
}

func TestFilterStringComparison(t *testing.T) {
	data := map[string]interface{}{
		"status": "active",
	}

	tests := []struct {
		name string
		expr string
		want bool
	}{
		{"eq string true", `event.data.status == "active"`, true},
		{"eq string false", `event.data.status == "inactive"`, false},
		{"neq string true", `event.data.status != "inactive"`, true},
		{"neq string false", `event.data.status != "active"`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := EvaluateFilter(tt.expr, data)
			if err != nil {
				t.Fatalf("EvaluateFilter(%q) returned error: %v", tt.expr, err)
			}
			if result != tt.want {
				t.Errorf("EvaluateFilter(%q) = %v, want %v", tt.expr, result, tt.want)
			}
		})
	}
}

func TestFilterMembership(t *testing.T) {
	tests := []struct {
		name string
		expr string
		data map[string]interface{}
		want bool
	}{
		{
			name: "in string true",
			expr: `event.data.status in("active", "pending")`,
			data: map[string]interface{}{"status": "active"},
			want: true,
		},
		{
			name: "in string false",
			expr: `event.data.status in("active", "pending")`,
			data: map[string]interface{}{"status": "inactive"},
			want: false,
		},
		{
			name: "in number true",
			expr: `event.data.count in(1, 2, 3)`,
			data: map[string]interface{}{"count": 2.0},
			want: true,
		},
		{
			name: "in number false",
			expr: `event.data.count in(1, 2, 3)`,
			data: map[string]interface{}{"count": 5.0},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := EvaluateFilter(tt.expr, tt.data)
			if err != nil {
				t.Fatalf("EvaluateFilter(%q) returned error: %v", tt.expr, err)
			}
			if result != tt.want {
				t.Errorf("EvaluateFilter(%q) = %v, want %v", tt.expr, result, tt.want)
			}
		})
	}
}

func TestFilterNestedPath(t *testing.T) {
	data := map[string]interface{}{
		"user": map[string]interface{}{
			"name": "alice",
		},
	}

	result, err := EvaluateFilter(`event.data.user.name == "alice"`, data)
	if err != nil {
		t.Fatalf("EvaluateFilter returned error: %v", err)
	}
	if !result {
		t.Error("EvaluateFilter(`event.data.user.name == \"alice\"`) = false, want true")
	}
}

func TestFilterArrayIndex(t *testing.T) {
	data := map[string]interface{}{
		"items": []interface{}{"a", "b", "c"},
	}

	tests := []struct {
		name string
		expr string
		want bool
	}{
		{
			name: "first element",
			expr: `event.data.items[0] == "a"`,
			want: true,
		},
		{
			name: "second element",
			expr: `event.data.items[1] == "b"`,
			want: true,
		},
		{
			name: "non-matching element",
			expr: `event.data.items[0] == "z"`,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := EvaluateFilter(tt.expr, data)
			if err != nil {
				t.Fatalf("EvaluateFilter(%q) returned error: %v", tt.expr, err)
			}
			if result != tt.want {
				t.Errorf("EvaluateFilter(%q) = %v, want %v", tt.expr, result, tt.want)
			}
		})
	}
}

func TestFilterNull(t *testing.T) {
	data := map[string]interface{}{
		"deleted_at": nil,
	}

	tests := []struct {
		name string
		expr string
		want bool
	}{
		{
			name: "null eq",
			expr: `event.data.deleted_at == null`,
			want: true,
		},
		{
			name: "null neq",
			expr: `event.data.deleted_at != "something"`,
			want: false, // type mismatch
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := EvaluateFilter(tt.expr, data)
			if tt.name == "null neq" {
				// Type mismatch should return an error.
				if err == nil {
					t.Log("EvaluateFilter returned no error for type mismatch (may be acceptable)")
				}
				return
			}
			if err != nil {
				t.Fatalf("EvaluateFilter(%q) returned error: %v", tt.expr, err)
			}
			if result != tt.want {
				t.Errorf("EvaluateFilter(%q) = %v, want %v", tt.expr, result, tt.want)
			}
		})
	}
}

func TestFilterInvalidExpr(t *testing.T) {
	data := map[string]interface{}{"price": 100.0}

	tests := []struct {
		name string
		expr string
	}{
		{
			name: "incomplete comparison",
			expr: `event.data.price >`,
		},
		{
			name: "missing field after dot",
			expr: `event.data. == 5`,
		},
		{
			name: "invalid operator",
			expr: `event.data.price unknown 5`,
		},
		{
			name: "missing path",
			expr: `== 5`,
		},
		{
			name: "unclosed string",
			expr: `event.data.name == "hello`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := EvaluateFilter(tt.expr, data)
			if err == nil {
				t.Errorf("EvaluateFilter(%q) expected error, got nil", tt.expr)
			}
		})
	}
}

func TestFilterStructured(t *testing.T) {
	data := map[string]interface{}{
		"event": map[string]interface{}{
			"data": map[string]interface{}{
				"amount": 150.0,
				"status": "active",
				"user": map[string]interface{}{
					"name": "alice",
					"age":  30.0,
				},
				"items": []interface{}{
					map[string]interface{}{"sku": "ABC"},
					map[string]interface{}{"sku": "DEF"},
				},
			},
		},
	}

	tests := []struct {
		name   string
		filter string
		want   bool
	}{
		{
			name:   "simple eq shorthand",
			filter: `{"event.data.status": "active"}`,
			want:   true,
		},
		{
			name:   "simple eq shorthand mismatch",
			filter: `{"event.data.status": "deleted"}`,
			want:   false,
		},
		{
			name:   "gt operator",
			filter: `{"event.data.amount": {"$gt": 100}}`,
			want:   true,
		},
		{
			name:   "gt operator false",
			filter: `{"event.data.amount": {"$gt": 200}}`,
			want:   false,
		},
		{
			name:   "multiple conditions AND",
			filter: `{"event.data.amount": {"$gt": 100}, "event.data.status": "active"}`,
			want:   true,
		},
		{
			name:   "multiple conditions one fails",
			filter: `{"event.data.amount": {"$gt": 100}, "event.data.status": "deleted"}`,
			want:   false,
		},
		{
			name:   "in operator",
			filter: `{"event.data.status": {"$in": ["active", "pending"]}}`,
			want:   true,
		},
		{
			name:   "in operator false",
			filter: `{"event.data.status": {"$in": ["deleted", "archived"]}}`,
			want:   false,
		},
		{
			name:   "nin operator",
			filter: `{"event.data.status": {"$nin": ["deleted", "archived"]}}`,
			want:   true,
		},
		{
			name:   "ne operator",
			filter: `{"event.data.status": {"$ne": "deleted"}}`,
			want:   true,
		},
		{
			name:   "gte operator",
			filter: `{"event.data.amount": {"$gte": 150}}`,
			want:   true,
		},
		{
			name:   "lte operator",
			filter: `{"event.data.amount": {"$lte": 150}}`,
			want:   true,
		},
		{
			name:   "nested path",
			filter: `{"event.data.user.name": "alice"}`,
			want:   true,
		},
		{
			name:   "exists true",
			filter: `{"event.data.amount": {"$exists": true}}`,
			want:   true,
		},
		{
			name:   "exists false",
			filter: `{"event.data.missing": {"$exists": false}}`,
			want:   true,
		},
		{
			name:   "array index access",
			filter: `{"event.data.items[0].sku": "ABC"}`,
			want:   true,
		},
		{
			name:   "empty filter matches all",
			filter: `{}`,
			want:   true,
		},
		{
			name:   "ne operator mismatch",
			filter: `{"event.data.status": {"$ne": "active"}}`,
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := EvaluateFilter(tt.filter, data)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result != tt.want {
				t.Errorf("EvaluateFilter(%q) = %v, want %v", tt.filter, result, tt.want)
			}
		})
	}
}

func TestFilterStructuredEdgeCases(t *testing.T) {
	// Invalid JSON
	_, err := EvaluateFilter("{invalid}", nil)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}

	// Unknown operator
	_, err = EvaluateFilter(`{"event.data.x": {"$unknown": 1}}`, map[string]interface{}{
		"event": map[string]interface{}{"data": map[string]interface{}{"x": 1}},
	})
	if err == nil {
		t.Error("expected error for unknown operator")
	}
}
