package pluginharness

import (
	"encoding/json"
	"testing"

	"github.com/cleat-team/cleat/cleat/cleattest"
)

func TestPluginCalls_Cleattest(t *testing.T) {
	tests := []struct {
		name      string
		plugin    string
		function  string
		input     string
		stubResp  string
		checkKeys []string
	}{
		{
			name:      "blobstore/put",
			plugin:    "blobstore",
			function:  "put",
			input:     `{"key":"test-key","data":"aGVsbG8="}`,
			stubResp:  `{"key":"test-key","sha256":"abc123","size":5}`,
			checkKeys: []string{"key", "sha256", "size"},
		},
		{
			name:      "blobstore/get",
			plugin:    "blobstore",
			function:  "get",
			input:     `{"key":"test-key"}`,
			stubResp:  `{"key":"test-key","sha256":"abc123","size":5,"data":"aGVsbG8="}`,
			checkKeys: []string{"key", "sha256"},
		},
		{
			name:      "event-triggers/await_event",
			plugin:    "event-triggers",
			function:  "await_event",
			input:     `{"event_type":"test.event","timeout_ms":100}`,
			stubResp:  `{"found":false}`,
			checkKeys: []string{"found"},
		},
		{
			name:      "feature-flags/evaluate_flag",
			plugin:    "feature-flags",
			function:  "evaluate_flag",
			input:     `{"key":"test-flag","context":{"user_id":"test-user"}}`,
			stubResp:  `{"key":"test-flag","enabled":false}`,
			checkKeys: []string{"key", "enabled"},
		},
		{
			name:      "kafka-connect/produce",
			plugin:    "kafka-connect",
			function:  "produce",
			input:     `{"config_id":"00000000-0000-0000-0000-000000000001","value":"test"}`,
			stubResp:  `{"success":true}`,
			checkKeys: []string{"success"},
		},
		{
			name:      "notifications/send_webhook",
			plugin:    "notifications",
			function:  "send_webhook",
			input:     `{"webhook_id":"00000000-0000-0000-0000-000000000002","event_type":"test.event","payload":{}}`,
			stubResp:  `{"delivery_id":"550e8400-e29b-41d4-a716-446655440000"}`,
			checkKeys: []string{"delivery_id"},
		},
		{
			name:      "pagerduty-alert/trigger",
			plugin:    "pagerduty-alert",
			function:  "trigger_incident",
			input:     `{"config_id":"00000000-0000-0000-0000-000000000003","summary":"test","severity":"critical","source":"test"}`,
			stubResp:  `{"incident_key":"mock-key","status":"success"}`,
			checkKeys: []string{"incident_key", "status"},
		},
		{
			name:      "pagerduty-alert/resolve",
			plugin:    "pagerduty-alert",
			function:  "resolve_incident",
			input:     `{"config_id":"00000000-0000-0000-0000-000000000003","incident_key":"mock-key"}`,
			stubResp:  `{"status":"resolved"}`,
			checkKeys: []string{"status"},
		},
		{
			name:      "pgvector/upsert",
			plugin:    "pgvector",
			function:  "upsert",
			input:     `{"collection":"test","external_id":"1","embedding":[0.1]}`,
			stubResp:  `{"id":"550e8400-e29b-41d4-a716-446655440000"}`,
			checkKeys: []string{"id"},
		},
		{
			name:      "pgvector/search",
			plugin:    "pgvector",
			function:  "search",
			input:     `{"collection":"test","query_vector":[0.1],"top_k":5}`,
			stubResp:  `{"results":[]}`,
			checkKeys: []string{"results"},
		},
		{
			name:      "pgvector/delete",
			plugin:    "pgvector",
			function:  "delete",
			input:     `{"collection":"test","external_id":"1"}`,
			stubResp:  `{"deleted":1}`,
			checkKeys: []string{"deleted"},
		},
		{
			name:      "slack-notify/send",
			plugin:    "slack-notify",
			function:  "send_message",
			input:     `{"config_id":"00000000-0000-0000-0000-000000000004","text":"test"}`,
			stubResp:  `{"success":true}`,
			checkKeys: []string{"success"},
		},
		{
			name:      "webhook-ingest/await",
			plugin:    "webhook-ingest",
			function:  "await_webhook",
			input:     `{"source_id":"test"}`,
			stubResp:  `{"found":false}`,
			checkKeys: []string{"found"},
		},
		{
			name:      "llm/chat",
			plugin:    "llm",
			function:  "chat",
			input:     `{"provider":"openai","model":"mock","messages":[{"role":"user","content":"hi"}]}`,
			stubResp:  `{"response":"mock","finish_reason":"stop"}`,
			checkKeys: []string{"response"},
		},
		{
			name:      "llm/embed",
			plugin:    "llm",
			function:  "embed",
			input:     `{"provider":"openai","model":"mock","input":["test"]}`,
			stubResp:  `{"data":[{"embedding":[0.1]}],"model":"mock","total_tokens":1}`,
			checkKeys: []string{"data", "model"},
		},
		{
			name:      "llm/list_models",
			plugin:    "llm",
			function:  "list_models",
			input:     `{"provider":"openai"}`,
			stubResp:  `{"models":[{"id":"mock"}]}`,
			checkKeys: []string{"models"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := cleattest.NewTestEnv()
			env.OnPluginCall(tt.plugin, tt.function).Return(tt.stubResp, nil)

			resp, err := env.H().PluginCall(tt.plugin, tt.function, tt.input)
			if err != nil {
				t.Fatalf("PluginCall(%q, %q) error: %v", tt.plugin, tt.function, err)
			}

			var result map[string]interface{}
			if err := json.Unmarshal([]byte(resp), &result); err != nil {
				t.Fatalf("failed to parse response JSON for %s/%s: %v", tt.plugin, tt.function, err)
			}

			for _, key := range tt.checkKeys {
				if _, ok := result[key]; !ok {
					t.Errorf("response missing expected key %q in %s/%s", key, tt.plugin, tt.function)
				}
			}
		})
	}
}

// TestPluginCallStreaming_Cleattest tests the synchronous PluginCall path for
// the llm/chat_stream function name through the cleattest mock environment.
// Full streaming testing (PluginCallStreaming with channel-based events)
// requires the WASM runtime and is covered in wasm_plugin_test.go.
func TestPluginCallStreaming_Cleattest(t *testing.T) {
	env := cleattest.NewTestEnv()

	env.OnPluginCall("llm", "chat_stream").Return(`{"response":"mock stream","finish_reason":"stop"}`, nil)

	resp, err := env.H().PluginCall("llm", "chat_stream", `{"provider":"openai","model":"mock","messages":[{"role":"user","content":"hi"}]}`)
	if err != nil {
		t.Fatalf("PluginCall(llm, chat_stream) error: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(resp), &result); err != nil {
		t.Fatalf("failed to parse response JSON for llm/chat_stream: %v", err)
	}

	for _, key := range []string{"response", "finish_reason"} {
		if _, ok := result[key]; !ok {
			t.Errorf("response missing expected key %q in llm/chat_stream", key)
		}
	}
}
