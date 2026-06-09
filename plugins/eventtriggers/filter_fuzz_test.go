package eventtriggers

import (
	"testing"
)

// FuzzFilterParser fuzzes the EvaluateFilter function with random string
// expressions. It exercises both the structured JSON filter path and the
// text-expression tokenizer+parser+evaluator path.
func FuzzFilterParser(f *testing.F) {
	// Seed corpus: structured JSON filters
	f.Add(`{"event.data.amount": {"$gt": 100}}`)
	f.Add(`{"event.data.status": {"$in": ["active","pending"]}}`)
	f.Add(`{"event.data.flag": {"$exists": true}}`)
	f.Add(`{"event.data.count": {"$gte": 10, "$lte": 100}}`)
	f.Add(`{"event.data.name": {"$ne": "admin"}}`)
	f.Add(`{"event.data.amount": {"$gt": 100}, "event.data.status": {"$in": ["active","pending"]}}`)

	// Seed corpus: text expression filters
	f.Add(`event.data.amount > 100`)
	f.Add(`event.data.amount == 100`)
	f.Add(`event.data.name == "test"`)
	f.Add(`event.data.amount in (1, 2, 3)`)
	f.Add(`event.data.count >= 10`)
	f.Add(`event.data.count <= 100`)
	f.Add(`event.data.name != "admin"`)
	f.Add(`event.data.amount < 50`)
	f.Add(`true`)

	// Seed corpus: edge cases
	f.Add(``)
	f.Add(`{`)
	f.Add(`}`)
	f.Add(`not valid at all`)
	f.Add(`event.data.amount >`)
	f.Add(`event.data.missing == "value"`)
	f.Add(`event.data.amount ==`)
	f.Add(`event.data.amount in ()`)
	f.Add(`event.data.amount in (`)
	f.Add(`{"event.data.amount": {"$unknown_op": 1}}`)
	f.Add(`{"event.data.amount": {"$gt": "not_a_number"}}`)
	f.Add(`{"event.data.amount": {"$in": "not_an_array"}}`)

	f.Fuzz(func(t *testing.T, input string) {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("panic on input %q: %v", input, r)
			}
		}()

		eventData := map[string]any{
			"event": map[string]any{
				"data": map[string]any{
					"amount": float64(100),
					"status": "active",
					"name":   "test-user",
					"count":  float64(42),
					"flag":   true,
					"tags":   []any{"a", "b", "c"},
					"nested": map[string]any{
						"key": "value",
					},
					"items": []any{
						map[string]any{"id": "1"},
						map[string]any{"id": "2"},
					},
				},
			},
		}

		// The function should either succeed or return an error — never panic.
		_, _ = EvaluateFilter(input, eventData)
	})
}
