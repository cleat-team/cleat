package cleat

import (
	"encoding/json"
	"testing"
)

// FuzzWorkflowInput fuzzes JSON deserialization of the cleat package types
// that represent workflow inputs, checkpoints, and configuration. Random
// JSON bytes are unmarshaled into each type to verify no panics occur.
func FuzzWorkflowInput(f *testing.F) {
	// Seed corpus: valid JSON for each type
	f.Add([]byte(`{"schedule_id":"s1","workflow_name":"test","cron_expr":"* * * * *","timezone":"UTC","input":"{}","enabled":true}`))
	f.Add([]byte(`{"workflow_id":"wf1","step":0,"call_history":[],"complete":false}`))
	f.Add([]byte(`{"i":0,"c":"hello","f":false}`))
	f.Add([]byte(`{"service":"svc","operation":"op","request":"req","response":"resp"}`))
	f.Add([]byte(`{"name":"dep","constraint":">=1.0"}`))
	f.Add([]byte(`{"run_id":"r1","result":"ok"}`))
	f.Add([]byte(`{"service":"svc","operation":"op","request":"req","response":"resp","err":""}`))

	// Seed corpus: edge cases
	f.Add([]byte(``))
	f.Add([]byte(`{`))
	f.Add([]byte(`not json`))
	f.Add([]byte(`null`))
	f.Add([]byte(`[]`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"call_history": [{"service":"s1","operation":"o1","request":"r1","response":"resp1"}]}`))
	f.Add([]byte(`{"workflow_id":"wf1","step":999,"call_history":[],"complete":true,"final_result":"done"}`))
	f.Add(bytesRepeatJSON(0xff, 100))

	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("panic on input %q: %v", string(data), r)
			}
		}()

		// CronSchedule — used for recurring workflow triggers.
		var cs CronSchedule
		_ = json.Unmarshal(data, &cs)

		// Checkpoint — the persistent state of a workflow at a point in time.
		var cp Checkpoint
		_ = json.Unmarshal(data, &cp)

		// StreamEvent — a single chunk of a streaming response.
		var se StreamEvent
		_ = json.Unmarshal(data, &se)

		// CallResult — the serialized result of a durable API call.
		var cr CallResult
		_ = json.Unmarshal(data, &cr)

		// ChildResult — the outcome of a child workflow.
		var child ChildResult
		_ = json.Unmarshal(data, &child)

		// PluginDependency — a plugin version constraint.
		var pd PluginDependency
		_ = json.Unmarshal(data, &pd)

		// RetryPolicy — retry configuration for durable calls.
		var rp RetryPolicy
		_ = json.Unmarshal(data, &rp)

		// CallOptions — per-call configuration including retry policy.
		var co CallOptions
		_ = json.Unmarshal(data, &co)

		// TerminalError fields (Err is an interface, verify no panic on bad data).
		var te TerminalError
		_ = json.Unmarshal(data, &te)
	})
}

// bytesRepeatJSON returns a byte slice of length n filled with value b,
// used as a seed for malformed binary inputs.
func bytesRepeatJSON(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}
