package goworkflow

import (
	"encoding/json"
	"fmt"

	"github.com/cleat-team/cleat/cleat"
)

func CallAllPlugins(h cleat.HostCalls, input string) (string, error) {
	results := make(map[string]interface{})

	// blobstore.put
	if resp, err := h.PluginCall("blobstore", "put", `{"key":"test-key","data":"aGVsbG8="}`); err != nil {
		results["blobstore.put"] = map[string]string{"error": fmt.Sprintf("%v", err)}
	} else {
		results["blobstore.put"] = resp
	}

	// blobstore.get
	if resp, err := h.PluginCall("blobstore", "get", `{"key":"test-key"}`); err != nil {
		results["blobstore.get"] = map[string]string{"error": fmt.Sprintf("%v", err)}
	} else {
		results["blobstore.get"] = resp
	}

	// event-triggers.await_event
	if resp, err := h.PluginCall("event-triggers", "await_event", `{"event_type":"test.event","timeout_ms":100}`); err != nil {
		results["event-triggers.await_event"] = map[string]string{"error": fmt.Sprintf("%v", err)}
	} else {
		results["event-triggers.await_event"] = resp
	}

	// feature-flags.evaluate_flag
	if resp, err := h.PluginCall("feature-flags", "evaluate_flag", `{"key":"test-flag","context":{"user_id":"test-user"}}`); err != nil {
		results["feature-flags.evaluate_flag"] = map[string]string{"error": fmt.Sprintf("%v", err)}
	} else {
		results["feature-flags.evaluate_flag"] = resp
	}

	// kafka-connect.produce
	if resp, err := h.PluginCall("kafka-connect", "produce", `{"config_id":"00000000-0000-0000-0000-000000000001","value":"test-message","key":"test-key"}`); err != nil {
		results["kafka-connect.produce"] = map[string]string{"error": fmt.Sprintf("%v", err)}
	} else {
		results["kafka-connect.produce"] = resp
	}

	// notifications.send_webhook
	if resp, err := h.PluginCall("notifications", "send_webhook", `{"webhook_id":"00000000-0000-0000-0000-000000000002","event_type":"test.event","payload":{"message":"hello"}}`); err != nil {
		results["notifications.send_webhook"] = map[string]string{"error": fmt.Sprintf("%v", err)}
	} else {
		results["notifications.send_webhook"] = resp
	}

	// pagerduty-alert.trigger_incident
	if resp, err := h.PluginCall("pagerduty-alert", "trigger_incident", `{"config_id":"00000000-0000-0000-0000-000000000003","summary":"Test incident","severity":"critical","source":"test-harness"}`); err != nil {
		results["pagerduty-alert.trigger_incident"] = map[string]string{"error": fmt.Sprintf("%v", err)}
	} else {
		results["pagerduty-alert.trigger_incident"] = resp
	}

	// pagerduty-alert.resolve_incident
	if resp, err := h.PluginCall("pagerduty-alert", "resolve_incident", `{"config_id":"00000000-0000-0000-0000-000000000003","incident_key":"mock-key"}`); err != nil {
		results["pagerduty-alert.resolve_incident"] = map[string]string{"error": fmt.Sprintf("%v", err)}
	} else {
		results["pagerduty-alert.resolve_incident"] = resp
	}

	// pgvector.upsert
	if resp, err := h.PluginCall("pgvector", "upsert", `{"collection":"test-collection","external_id":"test-1","content":"test content","embedding":[0.1,0.2,0.3,0.4]}`); err != nil {
		results["pgvector.upsert"] = map[string]string{"error": fmt.Sprintf("%v", err)}
	} else {
		results["pgvector.upsert"] = resp
	}

	// pgvector.search
	if resp, err := h.PluginCall("pgvector", "search", `{"collection":"test-collection","query_vector":[0.1,0.2,0.3,0.4],"top_k":5}`); err != nil {
		results["pgvector.search"] = map[string]string{"error": fmt.Sprintf("%v", err)}
	} else {
		results["pgvector.search"] = resp
	}

	// pgvector.delete
	if resp, err := h.PluginCall("pgvector", "delete", `{"collection":"test-collection","external_id":"test-1"}`); err != nil {
		results["pgvector.delete"] = map[string]string{"error": fmt.Sprintf("%v", err)}
	} else {
		results["pgvector.delete"] = resp
	}

	// slack-notify.send_message
	if resp, err := h.PluginCall("slack-notify", "send_message", `{"config_id":"00000000-0000-0000-0000-000000000004","text":"Test message from plugin harness","channel":"#test"}`); err != nil {
		results["slack-notify.send_message"] = map[string]string{"error": fmt.Sprintf("%v", err)}
	} else {
		results["slack-notify.send_message"] = resp
	}

	// webhook-ingest.await_webhook
	if resp, err := h.PluginCall("webhook-ingest", "await_webhook", `{"source_id":"test-source"}`); err != nil {
		results["webhook-ingest.await_webhook"] = map[string]string{"error": fmt.Sprintf("%v", err)}
	} else {
		results["webhook-ingest.await_webhook"] = resp
	}

	// llm.chat
	if resp, err := h.PluginCall("llm", "chat", `{"provider":"openai","model":"mock-model","messages":[{"role":"user","content":"hello"}]}`); err != nil {
		results["llm.chat"] = map[string]string{"error": fmt.Sprintf("%v", err)}
	} else {
		results["llm.chat"] = resp
	}

	// llm.embed
	if resp, err := h.PluginCall("llm", "embed", `{"provider":"openai","model":"mock-model","input":["test text"]}`); err != nil {
		results["llm.embed"] = map[string]string{"error": fmt.Sprintf("%v", err)}
	} else {
		results["llm.embed"] = resp
	}

	// llm.list_models
	if resp, err := h.PluginCall("llm", "list_models", `{"provider":"openai"}`); err != nil {
		results["llm.list_models"] = map[string]string{"error": fmt.Sprintf("%v", err)}
	} else {
		results["llm.list_models"] = resp
	}

	// llm.chat_stream (streaming)
	ch, err := h.PluginCallStreaming("llm", "chat_stream", `{"provider":"openai","model":"mock-model","messages":[{"role":"user","content":"hello"}]}`)
	if err != nil {
		results["llm.chat_stream"] = map[string]string{"error": fmt.Sprintf("%v", err)}
	} else {
		var events []interface{}
		for evt := range ch {
			events = append(events, evt)
		}
		results["llm.chat_stream"] = events
	}

	b, err := json.Marshal(results)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
