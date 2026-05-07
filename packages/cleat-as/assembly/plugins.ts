/**
 * Typed plugin convenience wrappers for the cleat AssemblyScript SDK.
 *
 * Provides the `Plugins` class with strongly-typed methods for each of the
 * 8 supported plugins (10 functions):
 *
 * - blobstore (put, get)
 * - eventtriggers (await_event)
 * - featureflags (evaluate_flag)
 * - kafkaconnect (produce)
 * - notifications (send_webhook)
 * - pagerdutyalert (trigger_incident, resolve_incident)
 * - slacknotify (send_message)
 * - webhookingest (await_webhook)
 *
 * Usage:
 * ```ts
 * import { HostCalls, Plugins } from "@cleat/sdk";
 * let host = new HostCalls();
 * let plugins = new Plugins(host);
 * let result = plugins.blobstorePut("my-key", "some data");
 * if (result.key.length > 0) {
 *   host.log("Stored blob: " + result.key);
 * }
 * ```
 *
 * Each method builds the input JSON from parameters (omitting empty optional
 * fields), calls `host.pluginCall`, and extracts typed fields from the JSON
 * response.
 */

import { HostCalls } from "./host-calls";

// ──────────────────────────────────────────────
// JSON field extractors for flat plugin responses
// ──────────────────────────────────────────────

/**
 * Extract a string field value from a flat JSON object string.
 * Returns "" when the field is not present or the value is not a quoted string.
 *
 * Searches for `"<field>":"<value>"` and returns the unquoted value.
 * Does not handle escaped characters inside the value (not needed for
 * plugin responses).
 */
function jsonStr(json: string, field: string): string {
  let key = '"' + field + '":"';
  let start = json.indexOf(key);
  if (start < 0) return "";
  start += key.length;
  let end = start;
  while (end < json.length && json.charCodeAt(end) != 0x22) end++;
  return json.substring(start, end);
}

/**
 * Extract a boolean field value from a flat JSON object string.
 * Returns false when the field is not present.
 */
function jsonBool(json: string, field: string): bool {
  let key = '"' + field + '":';
  let start = json.indexOf(key);
  if (start < 0) return false;
  start += key.length;
  return json.indexOf("true", start) == start;
}

/**
 * Extract an i64 field value from a flat JSON object string.
 * Returns 0 when the field is not present.
 */
function jsonI64(json: string, field: string): i64 {
  let key = '"' + field + '":';
  let start = json.indexOf(key);
  if (start < 0) return 0;
  start += key.length;
  let pos: i32 = start;
  let neg: bool = false;
  if (pos < json.length && json.charCodeAt(pos) == 0x2d) { neg = true; pos++; }
  while (pos < json.length) {
    let c = json.charCodeAt(pos);
    if (c >= 0x30 && c <= 0x39) pos++;
    else break;
  }
  let begin: i32 = neg ? start + 1 : start;
  if (pos == begin) return 0;
  let val: i64 = 0;
  for (let j = begin; j < pos; j++) {
    val = val * 10 + (json.charCodeAt(j) - 0x30);
  }
  return neg ? -val : val;
}

// ─══════════════════════════════════════════════
// Plugin result types
// ─══════════════════════════════════════════════

// blobstore

export class BlobPutResult {
  constructor(
    public readonly key: string,
    public readonly sha256: string,
    public readonly size: i64,
  ) {}
}

export class BlobGetResult {
  constructor(
    public readonly key: string,
    public readonly sha256: string,
    public readonly size: i64,
    public readonly content_type: string = "",
    public readonly data: string = "",
  ) {}
}

// eventtriggers

export class AwaitEventResult {
  constructor(
    public readonly found: bool,
    public readonly event_id: string = "",
    public readonly event_type: string = "",
    public readonly event_data: string = "",
    public readonly received_at: string = "",
  ) {}
}

// featureflags

export class EvaluateFlagResult {
  constructor(
    public readonly enabled: bool,
    public readonly key: string,
    public readonly evaluation: string = "",
  ) {}
}

// kafkaconnect

export class ProduceResult {
  constructor(
    public readonly success: bool,
    public readonly error: string = "",
  ) {}
}

// notifications

export class SendWebhookResult {
  constructor(
    public readonly delivery_id: string,
  ) {}
}

// pagerdutyalert

export class TriggerIncidentResult {
  constructor(
    public readonly incident_key: string,
    public readonly status: string,
  ) {}
}

export class ResolveIncidentResult {
  constructor(
    public readonly status: string,
  ) {}
}

// slacknotify

export class SendMessageResult {
  constructor(
    public readonly success: bool,
    public readonly ts: string = "",
  ) {}
}

// webhookingest

export class AwaitWebhookResult {
  constructor(
    public readonly found: bool,
    public readonly id: string = "",
    public readonly event_type: string = "",
    public readonly payload: string = "",
    public readonly received_at: string = "",
  ) {}
}

// ─══════════════════════════════════════════════
// Plugins wrapper class
// ─══════════════════════════════════════════════

/**
 * Typed convenience wrapper around HostCalls.pluginCall for the 8 supported
 * cleat plugins.
 *
 * Each method:
 * 1. Builds the input JSON string from parameters (omitting empty optional
 *    fields).
 * 2. Calls `host.pluginCall("plugin_name", "function_name", inputJson)`.
 * 3. Checks for errors and returns a default result on failure.
 * 4. Extracts typed fields from the response JSON on success.
 */
export class Plugins {
  constructor(private host: HostCalls) {}

  // ── blobstore ───────────────────────────────

  /**
   * Store a blob in the blobstore plugin.
   *
   * @param key          - Blob key.
   * @param data         - Blob data content.
   * @param content_type - Optional MIME content type.
   * @param tags         - Optional comma-separated tags.
   * @param ttl          - Optional TTL duration string (e.g., "1h", "30m").
   * @returns The put result with key, sha256 hash, and size.
   */
  blobstorePut(key: string, data: string, content_type: string = "", tags: string = "", ttl: string = ""): BlobPutResult {
    let input = '{"key":"' + key + '","data":"' + data + '"';
    if (content_type.length > 0) input += ',"content_type":"' + content_type + '"';
    if (tags.length > 0) input += ',"tags":"' + tags + '"';
    if (ttl.length > 0) input += ',"ttl":"' + ttl + '"';
    input += "}";
    let outcome = this.host.pluginCall("blobstore", "put", input);
    if (outcome.isError) return new BlobPutResult("", "", 0);
    return new BlobPutResult(jsonStr(outcome.response, "key"), jsonStr(outcome.response, "sha256"), jsonI64(outcome.response, "size"));
  }

  /**
   * Retrieve a blob from the blobstore plugin.
   *
   * @param key - Blob key to retrieve.
   * @returns The get result with key, sha256, size, content_type, and data.
   */
  blobstoreGet(key: string): BlobGetResult {
    let input = '{"key":"' + key + '"}';
    let outcome = this.host.pluginCall("blobstore", "get", input);
    if (outcome.isError) return new BlobGetResult("", "", 0);
    let r = outcome.response;
    return new BlobGetResult(jsonStr(r, "key"), jsonStr(r, "sha256"), jsonI64(r, "size"), jsonStr(r, "content_type"), jsonStr(r, "data"));
  }

  // ── eventtriggers ───────────────────────────

  /**
   * Wait for an event from the eventtriggers plugin.
   *
   * @param event_type - The event type to wait for.
   * @param timeout_ms - Timeout in milliseconds (default 60000).
   * @returns The await result with found status and event details.
   */
  awaitEvent(event_type: string, timeout_ms: i64 = 60000): AwaitEventResult {
    let input = '{"event_type":"' + event_type + '","timeout_ms":' + timeout_ms.toString() + "}";
    let outcome = this.host.pluginCall("eventtriggers", "await_event", input);
    if (outcome.isError) return new AwaitEventResult(false);
    let r = outcome.response;
    return new AwaitEventResult(jsonBool(r, "found"), jsonStr(r, "event_id"), jsonStr(r, "event_type"), jsonStr(r, "event_data"), jsonStr(r, "received_at"));
  }

  // ── featureflags ────────────────────────────

  /**
   * Evaluate a feature flag.
   *
   * @param key     - Feature flag key.
   * @param context - Optional evaluation context JSON.
   * @returns The evaluation result with enabled status and details.
   */
  evaluateFlag(key: string, context: string = ""): EvaluateFlagResult {
    let input = '{"key":"' + key + '"';
    if (context.length > 0) input += ',"context":"' + context + '"';
    input += "}";
    let outcome = this.host.pluginCall("featureflags", "evaluate_flag", input);
    if (outcome.isError) return new EvaluateFlagResult(false, key);
    let r = outcome.response;
    return new EvaluateFlagResult(jsonBool(r, "enabled"), jsonStr(r, "key"), jsonStr(r, "evaluation"));
  }

  // ── kafkaconnect ────────────────────────────

  /**
   * Produce a message to a Kafka topic via the kafkaconnect plugin.
   *
   * @param config_id - Kafka connector configuration ID.
   * @param value     - Message value.
   * @param key       - Optional message key.
   * @param headers   - Optional message headers JSON.
   * @returns The produce result with success status and optional error.
   */
  produce(config_id: string, value: string, key: string = "", headers: string = ""): ProduceResult {
    let input = '{"config_id":"' + config_id + '","value":"' + value + '"';
    if (key.length > 0) input += ',"key":"' + key + '"';
    if (headers.length > 0) input += ',"headers":"' + headers + '"';
    input += "}";
    let outcome = this.host.pluginCall("kafkaconnect", "produce", input);
    if (outcome.isError) return new ProduceResult(false);
    let r = outcome.response;
    return new ProduceResult(jsonBool(r, "success"), jsonStr(r, "error"));
  }

  // ── notifications ───────────────────────────

  /**
   * Send a webhook via the notifications plugin.
   *
   * @param webhook_id - Webhook configuration ID.
   * @param event_type - Event type to trigger.
   * @param payload    - Optional payload JSON.
   * @returns The send result with delivery ID.
   */
  sendWebhook(webhook_id: string, event_type: string, payload: string = ""): SendWebhookResult {
    let input = '{"webhook_id":"' + webhook_id + '","event_type":"' + event_type + '"';
    if (payload.length > 0) input += ',"payload":"' + payload + '"';
    input += "}";
    let outcome = this.host.pluginCall("notifications", "send_webhook", input);
    if (outcome.isError) return new SendWebhookResult("");
    return new SendWebhookResult(jsonStr(outcome.response, "delivery_id"));
  }

  // ── pagerdutyalert ──────────────────────────

  /**
   * Trigger an incident via the pagerdutyalert plugin.
   *
   * @param config_id - PagerDuty configuration ID.
   * @param summary   - Incident summary message.
   * @param severity  - Incident severity (e.g., "critical", "warning").
   * @param source    - Incident source identifier.
   * @param details   - Optional incident details JSON.
   * @returns The trigger result with incident key and status.
   */
  triggerIncident(config_id: string, summary: string, severity: string, source: string, details: string = ""): TriggerIncidentResult {
    let input = '{"config_id":"' + config_id + '","summary":"' + summary + '","severity":"' + severity + '","source":"' + source + '"';
    if (details.length > 0) input += ',"details":"' + details + '"';
    input += "}";
    let outcome = this.host.pluginCall("pagerdutyalert", "trigger_incident", input);
    if (outcome.isError) return new TriggerIncidentResult("", "");
    let r = outcome.response;
    return new TriggerIncidentResult(jsonStr(r, "incident_key"), jsonStr(r, "status"));
  }

  /**
   * Resolve an incident via the pagerdutyalert plugin.
   *
   * @param config_id    - PagerDuty configuration ID.
   * @param incident_key - The incident key to resolve.
   * @returns The resolve result with status.
   */
  resolveIncident(config_id: string, incident_key: string): ResolveIncidentResult {
    let input = '{"config_id":"' + config_id + '","incident_key":"' + incident_key + '"}';
    let outcome = this.host.pluginCall("pagerdutyalert", "resolve_incident", input);
    if (outcome.isError) return new ResolveIncidentResult("");
    return new ResolveIncidentResult(jsonStr(outcome.response, "status"));
  }

  // ── slacknotify ─────────────────────────────

  /**
   * Send a Slack message via the slacknotify plugin.
   *
   * @param config_id - Slack connector configuration ID.
   * @param text      - Message text.
   * @param channel   - Optional channel override.
   * @param blocks    - Optional Slack blocks JSON for rich formatting.
   * @returns The send result with success status and optional timestamp.
   */
  sendMessage(config_id: string, text: string, channel: string = "", blocks: string = ""): SendMessageResult {
    let input = '{"config_id":"' + config_id + '","text":"' + text + '"';
    if (channel.length > 0) input += ',"channel":"' + channel + '"';
    if (blocks.length > 0) input += ',"blocks":"' + blocks + '"';
    input += "}";
    let outcome = this.host.pluginCall("slacknotify", "send_message", input);
    if (outcome.isError) return new SendMessageResult(false);
    let r = outcome.response;
    return new SendMessageResult(jsonBool(r, "success"), jsonStr(r, "ts"));
  }

  // ── webhookingest ───────────────────────────

  /**
   * Wait for an incoming webhook via the webhookingest plugin.
   *
   * @param source_id  - Webhook source configuration ID.
   * @param event_type - Optional event type filter.
   * @returns The await result with found status and webhook data.
   */
  awaitWebhook(source_id: string, event_type: string = ""): AwaitWebhookResult {
    let input = '{"source_id":"' + source_id + '"';
    if (event_type.length > 0) input += ',"event_type":"' + event_type + '"';
    input += "}";
    let outcome = this.host.pluginCall("webhookingest", "await_webhook", input);
    if (outcome.isError) return new AwaitWebhookResult(false);
    let r = outcome.response;
    return new AwaitWebhookResult(jsonBool(r, "found"), jsonStr(r, "id"), jsonStr(r, "event_type"), jsonStr(r, "payload"), jsonStr(r, "received_at"));
  }

  // ── llm ─────────────────────────────────────

  /**
   * Send a chat completion request to an LLM via the llm plugin.
   *
   * @param model      - Model name (e.g., "gpt-4", "claude-3-opus").
   * @param messages   - JSON array of message objects.
   * @param tools      - Optional JSON array of tool definitions.
   * @returns The chat result with response, tool calls, finish reason, and usage info.
   */
  chat(model: string, messages: string, tools: string = ""): LLMChatResult {
    let input = '{"model":"' + model + '","messages":' + messages;
    if (tools.length > 0) input += ',"tools":' + tools;
    input += '}';
    let outcome = this.host.pluginCall("llm", "chat", input);
    if (outcome.isError) {
      return new LLMChatResult("", "", "", "", outcome.error);
    }
    let r = outcome.response;
    let response = jsonStr(r, "response");
    let toolCalls = jsonStr(r, "tool_calls");
    let finishReason = jsonStr(r, "finish_reason");
    let usageInfo = jsonStr(r, "usage");
    let error = jsonStr(r, "error");
    return new LLMChatResult(response, toolCalls, finishReason, usageInfo, error);
  }

  /**
   * Generate embeddings via the LLM plugin.
   *
   * @param model      - Model name for embeddings (e.g., "text-embedding-3-small").
   * @param textsJson  - JSON array of input strings to embed.
   * @returns The embedding result with data JSON, model info, and token usage.
   */
  embed(model: string, textsJson: string): EmbeddingsResult {
    let input = '{"model":"' + model + '","input":' + textsJson + '}';
    let outcome = this.host.pluginCall("llm", "embed", input);
    if (outcome.isError) {
      return new EmbeddingsResult("", "", 0, outcome.error);
    }
    let r = outcome.response;
    let dataJson = jsonStr(r, "data");
    let model_used = jsonStr(r, "model");
    let totalTokens = jsonI64(r, "total_tokens") as i32;
    let error = jsonStr(r, "error");
    if (dataJson.length == 0) {
      // Try alternate response format: embedding directly in response
      dataJson = r;
    }
    return new EmbeddingsResult(dataJson, model_used, totalTokens, error);
  }

  /**
   * List available models for an LLM provider.
   *
   * @param provider - Optional provider name. If empty, lists all configured providers.
   * @returns JSON string of available models, or error JSON on failure.
   */
  listModels(provider: string = ""): string {
    let input = '{';
    if (provider.length > 0) input += '"provider":"' + provider + '"';
    input += '}';
    let outcome = this.host.pluginCall("llm", "list_models", input);
    if (outcome.isError) return '{"error":"' + outcome.error + '"}';
    return outcome.response;
  }
}

// ── LLM result types ─────────────────────────

/**
 * Result of an LLM chat completion.
 *
 * Mirrors the Python SDK `plugins.py` LLM patterns.
 */
export class LLMChatResult {
  constructor(
    /** The generated response text content. */
    public readonly response: string,
    /** JSON array of tool calls, or empty string if none. */
    public readonly toolCalls: string,
    /** Finish reason: "stop", "length", "tool_calls", "content_filter", etc. */
    public readonly finishReason: string,
    /** JSON object with token usage info (prompt_tokens, completion_tokens, total_tokens). */
    public readonly usageInfo: string,
    /** Error message, or empty on success. */
    public readonly error: string,
  ) {}

  /** Returns true when this result carries an error. */
  get isError(): bool {
    return this.error.length > 0;
  }
}

/**
 * Result of an LLM embedding request.
 *
 * Mirrors the Python SDK `plugins.py` LLM patterns.
 */
export class EmbeddingsResult {
  constructor(
    /** JSON string of embedding data. For single-text input, this is an array
     *  of numbers. For multi-text input, an array of arrays. */
    public readonly dataJson: string,
    /** The model used for generating embeddings. */
    public readonly model: string,
    /** Total token count used in the request. */
    public readonly totalTokens: i32,
    /** Error message, or empty on success. */
    public readonly error: string,
  ) {}

  /** Returns true when this result carries an error. */
  get isError(): bool {
    return this.error.length > 0;
  }
}
