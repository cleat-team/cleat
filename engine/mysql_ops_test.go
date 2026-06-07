package engine

import (
	"sort"
	"testing"
)

// ---------------------------------------------------------------------------
// compactJSONString
// ---------------------------------------------------------------------------

func TestCompactJSONString(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty string", "", ""},
		{"already compact", `{"a":1,"b":2}`, `{"a":1,"b":2}`},
		{"spaces after colon", `{"a": 1, "b": 2}`, `{"a":1,"b":2}`},
		{"newlines and spaces", "{\n  \"a\": 1,\n  \"b\": 2\n}", `{"a":1,"b":2}`},
		{"nested object", `{"a": {"b": 2, "c": 3}, "d": 4}`, `{"a":{"b":2,"c":3},"d":4}`},
		{"array of objects", `[{"a": 1}, {"b": 2}]`, `[{"a":1},{"b":2}]`},
		{"spaces in string values", `{"a": "hello world"}`, `{"a":"hello world"}`},
		{"invalid JSON fallback", `not json`, `not json`},
		{"partial JSON fallback", `{"a": 1`, `{"a": 1`},
		{"leading whitespace only", "  {\"a\":1}", `{"a":1}`},
		{"trailing whitespace only", "{\"a\":1}  ", `{"a":1}`},
		{"tab characters", "{\t\"a\":\t1}", `{"a":1}`},
		{"empty object", `{}`, `{}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := compactJSONString(tt.input)
			if got != tt.want {
				t.Errorf("compactJSONString(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// percentile
// ---------------------------------------------------------------------------

func TestPercentile(t *testing.T) {
	tests := []struct {
		name   string
		data   []int64
		p      float64
		want   int64
	}{
		{"empty slice", []int64{}, 0.5, 0},
		{"empty slice p=0", []int64{}, 0.0, 0},
		{"empty slice p=100", []int64{}, 1.0, 0},
		{"single element p=0", []int64{42}, 0.0, 42},
		{"single element p=50", []int64{42}, 0.5, 42},
		{"single element p=100", []int64{42}, 1.0, 42},
		{"two elements p=0", []int64{10, 20}, 0.0, 10},
		{"two elements p=0.5", []int64{10, 20}, 0.5, 10},
		{"two elements p=1.0", []int64{10, 20}, 1.0, 20},
		{"three elements p=0.5", []int64{10, 20, 30}, 0.5, 20},
		{"three elements p=0.33", []int64{10, 20, 30}, 0.33, 10},
		{"three elements p=0.67", []int64{10, 20, 30}, 0.67, 30},
		{"large slice p=95", makeRange(1, 100), 0.95, 95},
		{"large slice p=50", makeRange(1, 100), 0.5, 50},
		{"large slice p=100", makeRange(1, 100), 1.0, 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// percentile expects sorted input
			sorted := make([]int64, len(tt.data))
			copy(sorted, tt.data)
			sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

			got := percentile(sorted, tt.p)
			if got != tt.want {
				t.Errorf("percentile(%v, %v) = %d, want %d", tt.data, tt.p, got, tt.want)
			}
		})
	}
}

func makeRange(start, end int64) []int64 {
	r := make([]int64, 0, end-start+1)
	for i := start; i <= end; i++ {
		r = append(r, i)
	}
	return r
}
