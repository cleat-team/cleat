// AssemblyScript workflow testdata — calls all registered plugin functions.
//
// Compiled to WASM with: npx asc assembly/index.ts --target release
//   --transform ./node_modules/@cleat/transform/index.js
//
// Uses @cleatEntry decorator.  The transformer generates the ABI-compatible
// WASM export wrapper automatically.

import {
  HostCalls,
  cleatEntry,
  PluginCallOutcome,
} from "../../../../../packages/cleat-as/assembly/index";

// ---------------------------------------------------------------------------
// Helper: call a plugin function and build a result entry.
// ---------------------------------------------------------------------------

function callBuilt(h: HostCalls, pluginName: string, funcName: string, inputJson: string): string {
  let r: PluginCallOutcome = h.pluginCall(pluginName, funcName, inputJson);
  if (r.error !== null || r.response.length == 0 || r.response == "null") {
    let msg: string = "empty response";
    if (r.error !== null) {
      msg = r.error!;
    }
    return '{"error":"' + escapeJson(msg) + '"}';
  }
  return r.response;
}

/** Minimal JSON-string escaping for error messages. */
function escapeJson(s: string | null): string {
  if (s === null) return "";
  let out: string = "";
  for (let i: i32 = 0; i < s.length; i++) {
    let c: string = s.charAt(i);
    if (c == '"') out += '\\"';
    else if (c == '\\') out += '\\\\';
    else if (c == '\n') out += '\\n';
    else if (c == '\r') out += '\\r';
    else if (c == '\t') out += '\\t';
    else out += c;
  }
  return out;
}

// ---------------------------------------------------------------------------
// Workflow: call_all_plugins
//
// Calls every registered plugin function and returns a JSON map of results.
// ---------------------------------------------------------------------------

@cleatEntry("CallAllPlugins")
export function call_all_plugins(h: HostCalls, _input: string): string {
  let parts: string[] = [];

  parts.push('"blobstore.put":');
  parts.push(callBuilt(h, "blobstore", "put", '{"key":"test-key","data":"aGVsbG8="}'));

  parts.push(',"blobstore.get":');
  parts.push(callBuilt(h, "blobstore", "get", '{"key":"test-key"}'));

  parts.push(',"event-triggers.await_event":');
  parts.push(callBuilt(h, "event-triggers", "await_event", '{"event_type":"test.event","timeout_ms":100}'));

  parts.push(',"feature-flags.evaluate_flag":');
  parts.push(callBuilt(h, "feature-flags", "evaluate_flag", '{"key":"test-flag","context":{"user_id":"test-user"}}'));

  parts.push(',"kafka-connect.produce":');
  parts.push(callBuilt(h, "kafka-connect", "produce", '{"config_id":"00000000-0000-0000-0000-000000000001","value":"test-message","key":"test-key"}'));

  parts.push(',"notifications.send_webhook":');
  parts.push(callBuilt(h, "notifications", "send_webhook", '{"webhook_id":"00000000-0000-0000-0000-000000000002","event_type":"test.event","payload":{"message":"hello"}}'));

  parts.push(',"pagerduty-alert.trigger_incident":');
  parts.push(callBuilt(h, "pagerduty-alert", "trigger_incident", '{"config_id":"00000000-0000-0000-0000-000000000003","summary":"Test incident","severity":"critical","source":"test-harness"}'));

  parts.push(',"pagerduty-alert.resolve_incident":');
  parts.push(callBuilt(h, "pagerduty-alert", "resolve_incident", '{"config_id":"00000000-0000-0000-0000-000000000003","incident_key":"mock-key"}'));

  parts.push(',"pgvector.upsert":');
  parts.push(callBuilt(h, "pgvector", "upsert", '{"collection":"test-collection","external_id":"test-1","content":"test content","embedding":[0.1,0.2,0.3,0.4]}'));

  parts.push(',"pgvector.search":');
  parts.push(callBuilt(h, "pgvector", "search", '{"collection":"test-collection","query_vector":[0.1,0.2,0.3,0.4],"top_k":5}'));

  parts.push(',"pgvector.delete":');
  parts.push(callBuilt(h, "pgvector", "delete", '{"collection":"test-collection","external_id":"test-1"}'));

  parts.push(',"slack-notify.send_message":');
  parts.push(callBuilt(h, "slack-notify", "send_message", '{"config_id":"00000000-0000-0000-0000-000000000004","text":"Test message from plugin harness","channel":"#test"}'));

  parts.push(',"webhook-ingest.await_webhook":');
  parts.push(callBuilt(h, "webhook-ingest", "await_webhook", '{"source_id":"test-source"}'));

  parts.push(',"llm.chat":');
  parts.push(callBuilt(h, "llm", "chat", '{"provider":"openai","model":"mock-model","messages":[{"role":"user","content":"hello"}]}'));

  parts.push(',"llm.embed":');
  parts.push(callBuilt(h, "llm", "embed", '{"provider":"openai","model":"mock-model","input":["test text"]}'));

  parts.push(',"llm.list_models":');
  parts.push(callBuilt(h, "llm", "list_models", '{"provider":"openai"}'));

  let result: string = "{" + parts.join("") + "}";
  return result;
}
