use cleat_sdk::HostCalls;
use cleat_macro::cleat_entry;
use serde_json::{json, Map, Value};

fn call_plugin(results: &mut Map<String, Value>, key: &str, h: &HostCalls, plugin: &str, func: &str, input: &str) {
    match h.plugin_call(plugin, func, input) {
        (resp, None) => {
            match serde_json::from_str::<Value>(&resp) {
                Ok(v) => { results.insert(key.to_string(), v); }
                Err(_) => { results.insert(key.to_string(), json!(resp)); }
            }
        }
        (_, Some(err)) => {
            results.insert(key.to_string(), json!({"error": err}));
        }
    }
}

#[cleat_entry]
fn call_all_plugins(h: &HostCalls, _input: Value) -> Result<String, String> {
    let mut results = Map::new();

    call_plugin(&mut results, "blobstore.put", h, "blobstore", "put", r#"{"key":"test-key","data":"aGVsbG8="}"#);
    call_plugin(&mut results, "blobstore.get", h, "blobstore", "get", r#"{"key":"test-key"}"#);
    call_plugin(&mut results, "event-triggers.await_event", h, "event-triggers", "await_event", r#"{"event_type":"test.event","timeout_ms":100}"#);
    call_plugin(&mut results, "feature-flags.evaluate_flag", h, "feature-flags", "evaluate_flag", r#"{"key":"test-flag","context":{"user_id":"test-user"}}"#);
    call_plugin(&mut results, "kafka-connect.produce", h, "kafka-connect", "produce", r#"{"config_id":"00000000-0000-0000-0000-000000000001","value":"test-message","key":"test-key"}"#);
    call_plugin(&mut results, "notifications.send_webhook", h, "notifications", "send_webhook", r#"{"webhook_id":"00000000-0000-0000-0000-000000000002","event_type":"test.event","payload":{"message":"hello"}}"#);
    call_plugin(&mut results, "pagerduty-alert.trigger_incident", h, "pagerduty-alert", "trigger_incident", r#"{"config_id":"00000000-0000-0000-0000-000000000003","summary":"Test incident","severity":"critical","source":"test-harness"}"#);
    call_plugin(&mut results, "pagerduty-alert.resolve_incident", h, "pagerduty-alert", "resolve_incident", r#"{"config_id":"00000000-0000-0000-0000-000000000003","incident_key":"mock-key"}"#);
    call_plugin(&mut results, "pgvector.upsert", h, "pgvector", "upsert", r#"{"collection":"test-collection","external_id":"test-1","content":"test content","embedding":[0.1,0.2,0.3,0.4]}"#);
    call_plugin(&mut results, "pgvector.search", h, "pgvector", "search", r#"{"collection":"test-collection","query_vector":[0.1,0.2,0.3,0.4],"top_k":5}"#);
    call_plugin(&mut results, "pgvector.delete", h, "pgvector", "delete", r#"{"collection":"test-collection","external_id":"test-1"}"#);
    call_plugin(&mut results, "slack-notify.send_message", h, "slack-notify", "send_message", r##"{"config_id":"00000000-0000-0000-0000-000000000004","text":"Test message from plugin harness","channel":"#test"}"##);
    call_plugin(&mut results, "webhook-ingest.await_webhook", h, "webhook-ingest", "await_webhook", r#"{"source_id":"test-source"}"#);
    call_plugin(&mut results, "llm.chat", h, "llm", "chat", r#"{"provider":"openai","model":"mock-model","messages":[{"role":"user","content":"hello"}]}"#);
    call_plugin(&mut results, "llm.embed", h, "llm", "embed", r#"{"provider":"openai","model":"mock-model","input":["test text"]}"#);
    call_plugin(&mut results, "llm.list_models", h, "llm", "list_models", r#"{"provider":"openai"}"#);

    // llm.chat_stream — streaming call (returns buffered response like plugin_call)
    match h.plugin_call_streaming("llm", "chat_stream", r#"{"provider":"openai","model":"mock-model","messages":[{"role":"user","content":"hello"}]}"#) {
        (resp, None) => {
            match serde_json::from_str::<Value>(&resp) {
                Ok(v) => { results.insert("llm.chat_stream".to_string(), v); }
                Err(_) => { results.insert("llm.chat_stream".to_string(), json!(resp)); }
            }
        }
        (_, Some(err)) => {
            results.insert("llm.chat_stream".to_string(), json!({"error": err}));
        }
    }

    serde_json::to_string(&results).map_err(|e| e.to_string())
}
