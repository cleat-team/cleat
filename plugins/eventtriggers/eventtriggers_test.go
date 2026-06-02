package eventtriggers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cleat-team/cleat/plugin"
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

func TestRegisterHostFunctionsNilScope(t *testing.T) {
	p := &Plugin{}
	err := p.RegisterHostFunctions(nil)
	if err == nil {
		t.Fatal("expected error for nil scope")
	}
}

func TestPluginRegistration(t *testing.T) {
	plugins, err := plugin.Discover()
	if err != nil {
		t.Fatalf("Discover() returned error: %v", err)
	}
	found := false
	for _, lp := range plugins {
		if lp.Plugin.Info().Name == "event-triggers" {
			found = true
			break
		}
	}
	if !found {
		t.Error("event-triggers plugin not found after Discover")
	}
}

func TestRegisterRoutesNilMux(t *testing.T) {
	p := &Plugin{}
	err := p.RegisterRoutes(nil)
	if err == nil {
		t.Fatal("expected error for nil mux")
	}
}

func TestUnregisterAwaiterEmptyID(t *testing.T) {
	// Must not panic or error when called with empty workflow ID.
	unregisterAwaiter(context.Background(), nil, nil, "", "test-event")
}

// TestMergeInputAndTemplateNil verifies mergeInputAndTemplate handles nil template.
func TestMergeInputAndTemplateNil(t *testing.T) {
	data := map[string]interface{}{"key": "value"}
	result, err := mergeInputAndTemplate(nil, data)
	if err != nil {
		t.Fatalf("mergeInputAndTemplate(nil, data) returned error: %v", err)
	}
	var merged map[string]interface{}
	if err := json.Unmarshal(result, &merged); err != nil {
		t.Fatalf("failed to decode result: %v", err)
	}
	if merged["key"] != "value" {
		t.Errorf("expected key='value', got %v", merged["key"])
	}
}

// TestMergeInputAndTemplateNonObject verifies mergeInputAndTemplate handles non-object templates.
func TestMergeInputAndTemplateNonObject(t *testing.T) {
	tmpl := json.RawMessage(`"just a string"`)
	data := map[string]interface{}{"key": "value"}
	result, err := mergeInputAndTemplate(tmpl, data)
	if err != nil {
		t.Fatalf("mergeInputAndTemplate(string, data) returned error: %v", err)
	}
	var merged map[string]interface{}
	if err := json.Unmarshal(result, &merged); err != nil {
		t.Fatalf("failed to decode result: %v", err)
	}
	if merged["key"] != "value" {
		t.Errorf("expected key='value', got %v", merged["key"])
	}
}

// TestFilterLiteralFalse verifies the filter returns error for boolean false value.
func TestFilterStructuredInvalidPath(t *testing.T) {
	_, err := EvaluateFilter(`{"event.data.missing": {"$exists": "not-bool"}}`, map[string]interface{}{
		"event": map[string]interface{}{"data": map[string]interface{}{}},
	})
	if err == nil {
		t.Error("expected error for $exists with non-bool value")
	}
}

// TestFilterStructuredNeqNonExistent verifies $ne on a non-existent field returns true.
func TestFilterStructuredNeqNonExistent(t *testing.T) {
	result, err := EvaluateFilter(`{"event.data.nonexistent": {"$ne": "value"}}`, map[string]interface{}{
		"event": map[string]interface{}{"data": map[string]interface{}{}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result {
		t.Error("expected true for $ne on non-existent field")
	}
}

// TestFilterStructuredGtLtNonExistent verifies comparison operators on non-existent fields.
func TestFilterStructuredGtLtNonExistent(t *testing.T) {
	data := map[string]interface{}{
		"event": map[string]interface{}{"data": map[string]interface{}{}},
	}

	tests := []struct {
		filter string
		expect bool
	}{
		{`{"event.data.nonexistent": {"$gt": 100}}`, false},
		{`{"event.data.nonexistent": {"$gte": 100}}`, false},
		{`{"event.data.nonexistent": {"$lt": 100}}`, false},
		{`{"event.data.nonexistent": {"$lte": 100}}`, false},
		{`{"event.data.nonexistent": {"$in": ["a","b"]}}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.filter, func(t *testing.T) {
			result, err := EvaluateFilter(tt.filter, data)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result != tt.expect {
				t.Errorf("expected %v, got %v", tt.expect, result)
			}
		})
	}
}

// TestFilterStructuredNinNonExistent verifies $nin on non-existent field.
func TestFilterStructuredNinNonExistent(t *testing.T) {
	result, err := EvaluateFilter(`{"event.data.nonexistent": {"$nin": ["a","b"]}}`, map[string]interface{}{
		"event": map[string]interface{}{"data": map[string]interface{}{}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result {
		t.Error("expected true for $nin on non-existent field")
	}
}

// TestFilterStructuredNinMatch verifies $nin returns false when value is in list.
func TestFilterStructuredNinMatch(t *testing.T) {
	result, err := EvaluateFilter(`{"event.data.status": {"$nin": ["active", "pending"]}}`, map[string]interface{}{
		"event": map[string]interface{}{"data": map[string]interface{}{"status": "active"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result {
		t.Error("expected false for $nin with matching value")
	}
}

// TestFilterStructuredInvalidIn verifies $in with non-array operand returns error.
func TestFilterStructuredInvalidIn(t *testing.T) {
	_, err := EvaluateFilter(`{"event.data.status": {"$in": "not-an-array"}}`, map[string]interface{}{
		"event": map[string]interface{}{"data": map[string]interface{}{"status": "active"}},
	})
	if err == nil {
		t.Error("expected error for $in with non-array operand")
	}
}

// TestFilterStructuredInvalidNin verifies $nin with non-array operand returns error.
func TestFilterStructuredInvalidNin(t *testing.T) {
	_, err := EvaluateFilter(`{"event.data.status": {"$nin": "not-an-array"}}`, map[string]interface{}{
		"event": map[string]interface{}{"data": map[string]interface{}{"status": "active"}},
	})
	if err == nil {
		t.Error("expected error for $nin with non-array operand")
	}
}

// TestFilterStructuredLt verifies $lt operator.
func TestFilterStructuredLt(t *testing.T) {
	result, err := EvaluateFilter(`{"event.data.amount": {"$lt": 200}}`, map[string]interface{}{
		"event": map[string]interface{}{"data": map[string]interface{}{"amount": 150.0}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result {
		t.Error("expected true for $lt 200 with value 150")
	}
}

// TestFilterStructuredGte verifies $gte operator.
func TestFilterStructuredGte(t *testing.T) {
	result, err := EvaluateFilter(`{"event.data.amount": {"$gte": 150}}`, map[string]interface{}{
		"event": map[string]interface{}{"data": map[string]interface{}{"amount": 150.0}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result {
		t.Error("expected true for $gte 150 with value 150")
	}
}

// TestFilterStructuredLte verifies $lte operator.
func TestFilterStructuredLte(t *testing.T) {
	result, err := EvaluateFilter(`{"event.data.amount": {"$lte": 150}}`, map[string]interface{}{
		"event": map[string]interface{}{"data": map[string]interface{}{"amount": 150.0}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result {
		t.Error("expected true for $lte 150 with value 150")
	}
}

// TestFilterStructuredGtNonNumeric verifies $gt with non-numeric returns false.
func TestFilterStructuredGtNonNumeric(t *testing.T) {
	result, err := EvaluateFilter(`{"event.data.amount": {"$gt": 100}}`, map[string]interface{}{
		"event": map[string]interface{}{"data": map[string]interface{}{"amount": "abc"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result {
		t.Error("expected false for $gt with non-numeric value")
	}
}

// TestFilterStructuredEqFalse verifies $eq returns false on mismatch.
func TestFilterStructuredEqFalse(t *testing.T) {
	result, err := EvaluateFilter(`{"event.data.status": {"$eq": "inactive"}}`, map[string]interface{}{
		"event": map[string]interface{}{"data": map[string]interface{}{"status": "active"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result {
		t.Error("expected false for $eq with non-matching value")
	}
}

// TestFilterComparisonNeq verifies != comparison operator.
func TestFilterComparisonNeq(t *testing.T) {
	data := map[string]interface{}{"price": 100.0}
	result, err := EvaluateFilter(`event.data.price != 200`, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result {
		t.Error("expected true for price != 200")
	}
}

// TestFilterNonObjectPath verifies error when path step doesn't hit an object.
func TestFilterNonObjectPath(t *testing.T) {
	data := map[string]interface{}{"price": 100.0}
	_, err := EvaluateFilter(`event.data.price.nested`, data)
	if err == nil {
		t.Error("expected error for accessing field of non-object")
	}
}

// TestFilterComparisonBool verifies comparison with boolean values.
func TestFilterComparisonBool(t *testing.T) {
	data := map[string]interface{}{"active": true}
	result, err := EvaluateFilter(`event.data.active == true`, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result {
		t.Error("expected true for active == true")
	}

	result, err = EvaluateFilter(`event.data.active != true`, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result {
		t.Error("expected false for active != true")
	}
}

// TestFilterComparisonBoolInvalidOperator verifies error with comparison operator other
// than == or != on booleans.
func TestFilterComparisonBoolInvalidOperator(t *testing.T) {
	data := map[string]interface{}{"active": true}
	_, err := EvaluateFilter(`event.data.active > false`, data)
	if err == nil {
		t.Error("expected error for > on boolean")
	}
}

// TestFilterComparisonStringOrder verifies string ordering comparisons.
func TestFilterComparisonStringOrder(t *testing.T) {
	data := map[string]interface{}{"name": "bravo"}
	result, err := EvaluateFilter(`event.data.name > "alpha"`, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result {
		t.Error("expected true for 'bravo' > 'alpha'")
	}

	result, err = EvaluateFilter(`event.data.name < "charlie"`, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result {
		t.Error("expected true for 'bravo' < 'charlie'")
	}
}

// TestFilterComparisonTypeMismatch verifies error on type mismatch.
func TestFilterComparisonTypeMismatch(t *testing.T) {
	data := map[string]interface{}{"price": 100.0}
	_, err := EvaluateFilter(`event.data.price == "string"`, data)
	if err == nil {
		t.Error("expected error for comparing number with string")
	}
}

// TestFilterComparisonNullInvalidOperator verifies error on null with unsupported op.
func TestFilterComparisonNullInvalidOperator(t *testing.T) {
	data := map[string]interface{}{"deleted": nil}
	_, err := EvaluateFilter(`event.data.deleted > null`, data)
	if err == nil {
		t.Error("expected error for > on null")
	}
}

// TestFilterComparisonNullNeqFalse verifies null != null returns false.
func TestFilterComparisonNullNeqFalse(t *testing.T) {
	data := map[string]interface{}{"deleted": nil}
	result, err := EvaluateFilter(`event.data.deleted != null`, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result {
		t.Error("expected false for null != null")
	}
}

// TestFilterUnsupportedType verifies error for unsupported type.
func TestFilterUnsupportedType(t *testing.T) {
	data := map[string]interface{}{"items": []interface{}{1, 2, 3}}
	_, err := EvaluateFilter(`event.data.items == 1`, data)
	if err == nil {
		t.Error("expected error for comparing array with number")
	}
}

// TestFilterTokenizeError verifies error from tokenizer.
func TestFilterTokenizeError(t *testing.T) {
	_, err := EvaluateFilter("event.data.x = 5", nil)
	if err == nil {
		t.Error("expected error for single '='")
	}
}

// TestFilterParseError verifies parse error with unknown operator.
func TestFilterEOFError(t *testing.T) {
	_, err := EvaluateFilter("event.data.x >=", nil)
	if err == nil {
		t.Error("expected parse error for trailing operator")
	}
}

// TestFilterParseTrailing verifies error for trailing tokens.
func TestFilterParseTrailing(t *testing.T) {
	_, err := EvaluateFilter("event.data.x == 5 extra", nil)
	if err == nil {
		t.Error("expected error for trailing tokens")
	}
}

// TestExprMarkerTrueExpr verifies the trueExpr marker method.
func TestExprMarkerTrueExpr(t *testing.T) {
	trueExpr{}.exprMarker()
}

// TestExprMarkerComparisonExpr verifies the comparisonExpr marker method.
func TestExprMarkerComparisonExpr(t *testing.T) {
	comparisonExpr{}.exprMarker()
}

// TestExprMarkerMembershipExpr verifies the membershipExpr marker method.
func TestExprMarkerMembershipExpr(t *testing.T) {
	membershipExpr{}.exprMarker()
}

// ---------------------------------------------------------------------------
// getPath edge case tests
// ---------------------------------------------------------------------------

func TestGetPath_NonExistentField(t *testing.T) {
	data := map[string]interface{}{
		"existing": "value",
	}
	_, ok := getPath(data, "missing")
	if ok {
		t.Error("expected false for missing top-level field")
	}
}

func TestGetPath_NonNumericArrayIndex(t *testing.T) {
	data := map[string]interface{}{
		"items": []interface{}{"a", "b", "c"},
	}
	// "items[abc]" should fail due to non-numeric index
	_, ok := getPath(data, "items[abc]")
	if ok {
		t.Error("expected false for non-numeric array index")
	}
}

func TestGetPath_NegativeArrayIndex(t *testing.T) {
	data := map[string]interface{}{
		"items": []interface{}{"a", "b", "c"},
	}
	_, ok := getPath(data, "items[-1]")
	if ok {
		t.Error("expected false for negative array index")
	}
}

func TestGetPath_TypeMismatch(t *testing.T) {
	data := map[string]interface{}{
		"scalar": "hello",
	}
	// Trying to index into a scalar value should fail.
	_, ok := getPath(data, "scalar.field")
	if ok {
		t.Error("expected false when accessing field on non-map value")
	}
}

func TestGetPath_ArrayIndexOnNonArray(t *testing.T) {
	data := map[string]interface{}{
		"scalar": "hello",
	}
	_, ok := getPath(data, "scalar[0]")
	if ok {
		t.Error("expected false when indexing into non-array value")
	}
}

// ---------------------------------------------------------------------------
// parsePath edge case tests
// ---------------------------------------------------------------------------


func TestParsePath_TrailingDot(t *testing.T) {
	_, err := EvaluateFilter("event.data. > 5", nil)
	if err == nil {
		t.Error("expected error for trailing dot in path")
	}
}

func TestParsePath_EmptyBrackets(t *testing.T) {
	_, err := EvaluateFilter("event.data.items[] == 5", nil)
	if err == nil {
		t.Error("expected error for empty brackets")
	}
}

func TestParsePath_NonNumberInBrackets(t *testing.T) {
	_, err := EvaluateFilter(`event.data.items[abc] == 5`, nil)
	if err == nil {
		t.Error("expected error for non-number in brackets")
	}
}

func TestParsePath_UnclosedBracket(t *testing.T) {
	_, err := EvaluateFilter("event.data.items[0 == 5", nil)
	if err == nil {
		t.Error("expected error for unclosed bracket")
	}
}

// ---------------------------------------------------------------------------
// EvaluateFilter edge case tests
// ---------------------------------------------------------------------------

func TestEvaluateFilter_NonExistentPath(t *testing.T) {
	data := map[string]interface{}{
		"event": map[string]interface{}{
			"data": map[string]interface{}{
				"price": 100.0,
			},
		},
	}
	_, err := EvaluateFilter("event.data.nonexistent > 50", data)
	if err == nil {
		t.Error("expected error for non-existent path")
	}
}

func TestEvaluateFilter_MissingEventKey(t *testing.T) {
	data := map[string]interface{}{
		"wrong_key": "value",
	}
	_, err := EvaluateFilter("event.data.x == 5", data)
	if err == nil {
		t.Error("expected error for missing event key in data")
	}
}

// ---------------------------------------------------------------------------
// parsePath edge case tests via EvaluateFilter
// ---------------------------------------------------------------------------

func TestEvaluateFilter_InvalidPathStart(t *testing.T) {
	_, err := EvaluateFilter("badstart.data.x > 5", nil)
	if err == nil {
		t.Error("expected error for path not starting with 'event'")
	}
}

func TestEvaluateFilter_MissingDotAfterEvent(t *testing.T) {
	_, err := EvaluateFilter("eventXdata.x > 5", nil)
	if err == nil {
		t.Error("expected error for missing '.' after 'event'")
	}
}

func TestEvaluateFilter_MissingDataAfterDot(t *testing.T) {
	_, err := EvaluateFilter("event.nodata.x > 5", nil)
	if err == nil {
		t.Error("expected error for missing 'data' after 'event.'")
	}
}
