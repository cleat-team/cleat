package cleat;

import java.util.List;
import java.util.Map;

/**
 * Typed convenience wrappers for cleat plugin host functions.
 * <p>
 * Wraps a {@link HostCalls} instance and exposes every cleat plugin function
 * as a typed Java method with documented parameters and typed result classes.
 * <p>
 * Each method:
 * <ol>
 *   <li>Builds the input JSON from typed Java parameters
 *   <li>Calls {@link HostCalls#pluginCall(String, String, String)}
 *   <li>Parses the JSON response into a typed result class via
 *       {@code fromJson}
 *   <li>Throws {@link RuntimeException} on protocol errors
 * </ol>
 * <p>
 * <strong>Usage:</strong>
 * <pre>{@code
 * HostCalls host = new HostCalls();
 * Plugins p = new Plugins(host);
 *
 * // blobstore
 * BlobPutResult stored = p.blobstorePut("my-key", "binary data",
 *     "application/octet-stream", null, null);
 * System.out.println("Stored at key=" + stored.key + " sha256=" + stored.sha256);
 *
 * // event triggers
 * AwaitEventResult ev = p.awaitEvent("payment_received", 30000);
 * if (ev.found) {
 *     System.out.println("Got event " + ev.eventId + ": " + ev.eventData);
 * }
 *
 * // feature flags
 * EvaluateFlagResult flag = p.evaluateFlag("new_checkout_flow",
 *     Map.of("user_id", "u-42"));
 * if (flag.enabled) {
 *     System.out.println("Flag " + flag.key + " is enabled");
 * }
 *
 * // slack
 * SendMessageResult msg = p.sendMessage("slack-1", "Hello, world!",
 *     "#general", null);
 * }</pre>
 *
 * @see HostCalls
 * @see <a href="https://github.com/cleat/cleat/blob/main/ABI.md#219-plugin_call">
 *      ABI 2.19 — plugin_call</a>
 */
public class Plugins {

    // ========================================================================
    // JSON helpers
    // ========================================================================

    /**
     * Extract the raw JSON value (as a substring of the original JSON) for a
     * given top-level key in a JSON object.
     *
     * @param json the JSON object string
     * @param key  the field name
     * @return the raw JSON value substring, or {@code null} if the key is not
     *         found
     */
    private static String extractRawValue(String json, String key) {
        String search = "\"" + key + "\":";
        int idx = json.indexOf(search);
        if (idx < 0) {
            return null;
        }
        idx += search.length();

        // Skip whitespace
        while (idx < json.length() && json.charAt(idx) <= ' ') {
            idx++;
        }
        if (idx >= json.length()) {
            return null;
        }

        char c = json.charAt(idx);
        if (c == '"') {
            // String value — find closing unescaped quote
            int start = idx;
            idx++;
            boolean escaped = false;
            while (idx < json.length()) {
                char ch = json.charAt(idx);
                if (escaped) {
                    escaped = false;
                    idx++;
                    continue;
                }
                if (ch == '\\') {
                    escaped = true;
                    idx++;
                    continue;
                }
                if (ch == '"') {
                    idx++;
                    break;
                }
                idx++;
            }
            return json.substring(start, idx);
        } else if (c == '{' || c == '[') {
            // Object or array — find matching closing bracket
            int start = idx;
            char open = c;
            char close = (c == '{') ? '}' : ']';
            int depth = 1;
            idx++;
            boolean inString = false;
            boolean esc = false;
            while (idx < json.length() && depth > 0) {
                char ch = json.charAt(idx);
                if (esc) {
                    esc = false;
                    idx++;
                    continue;
                }
                if (ch == '\\') {
                    esc = true;
                    idx++;
                    continue;
                }
                if (ch == '"') {
                    inString = !inString;
                    idx++;
                    continue;
                }
                if (!inString) {
                    if (ch == open) {
                        depth++;
                    }
                    if (ch == close) {
                        depth--;
                    }
                }
                idx++;
            }
            return json.substring(start, idx);
        } else {
            // Number, boolean, or null — read until delimiter
            int start = idx;
            while (idx < json.length()
                && json.charAt(idx) != ','
                && json.charAt(idx) != '}'
                && json.charAt(idx) != ']'
                && json.charAt(idx) > ' ') {
                idx++;
            }
            return json.substring(start, idx).trim();
        }
    }

    /**
     * Extract a string value for a key from a JSON object.
     * <p>
     * Handles JSON unescaping.  Returns empty string if the key is missing
     * or the value is JSON null.
     *
     * @param json the JSON object string
     * @param key  the field name
     * @return the unescaped string value, or empty string if not found or null
     */
    private static String extractString(String json, String key) {
        String raw = extractRawValue(json, key);
        if (raw == null || "null".equals(raw)) {
            return "";
        }
        if (raw.startsWith("\"") && raw.endsWith("\"")) {
            return unescapeJson(raw.substring(1, raw.length() - 1));
        }
        return raw;
    }

    /**
     * Extract a long value for a key from a JSON object.
     *
     * @param json the JSON object string
     * @param key  the field name
     * @return the long value, or 0 if the key is missing or null
     */
    private static long extractLong(String json, String key) {
        String raw = extractRawValue(json, key);
        if (raw == null || "null".equals(raw)) {
            return 0;
        }
        try {
            return Long.parseLong(raw);
        } catch (NumberFormatException e) {
            return 0;
        }
    }

    /**
     * Extract a boolean value for a key from a JSON object.
     *
     * @param json the JSON object string
     * @param key  the field name
     * @return {@code true} if the value is the JSON literal {@code true}
     */
    private static boolean extractBoolean(String json, String key) {
        String raw = extractRawValue(json, key);
        return "true".equals(raw);
    }

    /**
     * Extract a raw JSON value (object, array, or literal) for a key.
     * <p>
     * Returns the raw JSON substring so that nested structures can be
     * preserved as strings for further parsing.
     *
     * @param json the JSON object string
     * @param key  the field name
     * @return the raw JSON value, or empty string if not found or null
     */
    private static String extractJsonValue(String json, String key) {
        String raw = extractRawValue(json, key);
        if (raw == null || "null".equals(raw)) {
            return "";
        }
        return raw;
    }

    /**
     * Unescape a JSON string value.
     */
    private static String unescapeJson(String s) {
        if (s == null || s.isEmpty()) {
            return s;
        }
        StringBuilder sb = new StringBuilder(s.length());
        for (int i = 0; i < s.length(); i++) {
            char c = s.charAt(i);
            if (c == '\\' && i + 1 < s.length()) {
                char next = s.charAt(i + 1);
                switch (next) {
                    case '"':
                        sb.append('"');
                        i++;
                        break;
                    case '\\':
                        sb.append('\\');
                        i++;
                        break;
                    case '/':
                        sb.append('/');
                        i++;
                        break;
                    case 'n':
                        sb.append('\n');
                        i++;
                        break;
                    case 'r':
                        sb.append('\r');
                        i++;
                        break;
                    case 't':
                        sb.append('\t');
                        i++;
                        break;
                    case 'b':
                        sb.append('\b');
                        i++;
                        break;
                    case 'f':
                        sb.append('\f');
                        i++;
                        break;
                    case 'u':
                        if (i + 5 < s.length()) {
                            String hex = s.substring(i + 2, i + 6);
                            try {
                                sb.append((char) Integer.parseInt(hex, 16));
                                i += 5;
                            } catch (NumberFormatException e) {
                                sb.append(c);
                            }
                        } else {
                            sb.append(c);
                        }
                        break;
                    default:
                        sb.append(c);
                        break;
                }
            } else {
                sb.append(c);
            }
        }
        return sb.toString();
    }

    // ---- JSON serialization helpers for input building ----

    /**
     * Convert a value to its JSON representation.
     *
     * @param value the value to serialize
     * @return the JSON string
     */
    @SuppressWarnings("unchecked")
    private static String valueToJson(Object value) {
        if (value == null) {
            return "null";
        }
        if (value instanceof String) {
            return "\"" + JsonHelper.escapeJson((String) value) + "\"";
        }
        if (value instanceof Boolean || value instanceof Number) {
            return value.toString();
        }
        if (value instanceof Map) {
            return mapToJson((Map<String, Object>) value);
        }
        if (value instanceof List) {
            return listToJson((List<Object>) value);
        }
        return "\"" + JsonHelper.escapeJson(value.toString()) + "\"";
    }

    /**
     * Convert a {@code List<Object>} to a JSON array string.
     */
    private static String listToJson(List<Object> list) {
        if (list == null || list.isEmpty()) {
            return "[]";
        }
        StringBuilder sb = new StringBuilder("[");
        for (int i = 0; i < list.size(); i++) {
            if (i > 0) {
                sb.append(",");
            }
            sb.append(valueToJson(list.get(i)));
        }
        sb.append("]");
        return sb.toString();
    }

    /**
     * Convert a {@code Map<String, String>} to a JSON object string.
     */
    private static String stringMapToJson(Map<String, String> map) {
        if (map == null || map.isEmpty()) {
            return "{}";
        }
        StringBuilder sb = new StringBuilder("{");
        boolean first = true;
        for (Map.Entry<String, String> entry : map.entrySet()) {
            if (!first) {
                sb.append(",");
            }
            sb.append("\"").append(JsonHelper.escapeJson(entry.getKey())).append("\":\"")
                .append(JsonHelper.escapeJson(entry.getValue())).append("\"");
            first = false;
        }
        sb.append("}");
        return sb.toString();
    }

    /**
     * Convert a {@code Map<String, Object>} to a JSON object string.
     */
    private static String mapToJson(Map<String, Object> map) {
        if (map == null || map.isEmpty()) {
            return "{}";
        }
        StringBuilder sb = new StringBuilder("{");
        boolean first = true;
        for (Map.Entry<String, Object> entry : map.entrySet()) {
            if (!first) {
                sb.append(",");
            }
            sb.append("\"").append(JsonHelper.escapeJson(entry.getKey())).append("\":")
                .append(valueToJson(entry.getValue()));
            first = false;
        }
        sb.append("}");
        return sb.toString();
    }

    // ========================================================================
    // BlobPutResult
    // ========================================================================

    /**
     * Result of a {@link #blobstorePut} call.
     */
    public static class BlobPutResult {
        /** The key under which the blob was stored. */
        public final String key;
        /** SHA-256 hex digest of the stored blob. */
        public final String sha256;
        /** Size of the stored blob in bytes. */
        public final long size;

        /**
         * Construct a new blob put result.
         *
         * @param key    the storage key
         * @param sha256 the SHA-256 digest
         * @param size   the blob size in bytes
         */
        public BlobPutResult(String key, String sha256, long size) {
            this.key = key;
            this.sha256 = sha256;
            this.size = size;
        }

        /**
         * Parse a {@code BlobPutResult} from a JSON response string.
         *
         * @param json the JSON response from {@code blobstore.put}
         * @return a parsed result
         */
        public static BlobPutResult fromJson(String json) {
            return new BlobPutResult(
                extractString(json, "key"),
                extractString(json, "sha256"),
                extractLong(json, "size")
            );
        }
    }

    // ========================================================================
    // BlobGetResult
    // ========================================================================

    /**
     * Result of a {@link #blobstoreGet} call.
     */
    public static class BlobGetResult {
        /** The key that was retrieved. */
        public final String key;
        /** SHA-256 hex digest of the retrieved blob. */
        public final String sha256;
        /** Size of the retrieved blob in bytes. */
        public final long size;
        /** Content type of the blob (empty if unknown). */
        public final String contentType;
        /** Base64-encoded blob data (empty if not present). */
        public final String data;

        /**
         * Construct a new blob get result.
         *
         * @param key         the storage key
         * @param sha256      the SHA-256 digest
         * @param size        the blob size in bytes
         * @param contentType the MIME content type
         * @param data        the base64-encoded blob data
         */
        public BlobGetResult(String key, String sha256, long size,
                             String contentType, String data) {
            this.key = key;
            this.sha256 = sha256;
            this.size = size;
            this.contentType = contentType;
            this.data = data;
        }

        /**
         * Parse a {@code BlobGetResult} from a JSON response string.
         *
         * @param json the JSON response from {@code blobstore.get}
         * @return a parsed result
         */
        public static BlobGetResult fromJson(String json) {
            return new BlobGetResult(
                extractString(json, "key"),
                extractString(json, "sha256"),
                extractLong(json, "size"),
                extractString(json, "content_type"),
                extractString(json, "data")
            );
        }
    }

    // ========================================================================
    // AwaitEventResult
    // ========================================================================

    /**
     * Result of an {@link #awaitEvent} call.
     */
    public static class AwaitEventResult {
        /** {@code true} if an event was received before the timeout. */
        public final boolean found;
        /** Unique event ID (empty if no event was found). */
        public final String eventId;
        /** The received event type (empty if no event was found). */
        public final String eventType;
        /** Arbitrary event payload as raw JSON (empty if no event was found). */
        public final String eventData;
        /** ISO-8601 timestamp of when the event was received. */
        public final String receivedAt;

        /**
         * Construct a new await-event result.
         *
         * @param found      whether an event was received
         * @param eventId    the event ID
         * @param eventType  the event type
         * @param eventData  the event payload as raw JSON
         * @param receivedAt the ISO-8601 reception timestamp
         */
        public AwaitEventResult(boolean found, String eventId, String eventType,
                                String eventData, String receivedAt) {
            this.found = found;
            this.eventId = eventId;
            this.eventType = eventType;
            this.eventData = eventData;
            this.receivedAt = receivedAt;
        }

        /**
         * Parse an {@code AwaitEventResult} from a JSON response string.
         *
         * @param json the JSON response from {@code eventtriggers.await_event}
         * @return a parsed result
         */
        public static AwaitEventResult fromJson(String json) {
            return new AwaitEventResult(
                extractBoolean(json, "found"),
                extractString(json, "event_id"),
                extractString(json, "event_type"),
                extractJsonValue(json, "event_data"),
                extractString(json, "received_at")
            );
        }
    }

    // ========================================================================
    // EvaluateFlagResult
    // ========================================================================

    /**
     * Result of an {@link #evaluateFlag} call.
     */
    public static class EvaluateFlagResult {
        /** {@code true} if the feature flag is enabled for the given context. */
        public final boolean enabled;
        /** The feature flag key. */
        public final String key;
        /** Detailed evaluation metadata as raw JSON (may be empty). */
        public final String evaluation;

        /**
         * Construct a new evaluate-flag result.
         *
         * @param enabled    whether the flag is enabled
         * @param key        the flag key
         * @param evaluation the evaluation metadata as raw JSON
         */
        public EvaluateFlagResult(boolean enabled, String key, String evaluation) {
            this.enabled = enabled;
            this.key = key;
            this.evaluation = evaluation;
        }

        /**
         * Parse an {@code EvaluateFlagResult} from a JSON response string.
         *
         * @param json the JSON response from {@code featureflags.evaluate_flag}
         * @return a parsed result
         */
        public static EvaluateFlagResult fromJson(String json) {
            return new EvaluateFlagResult(
                extractBoolean(json, "enabled"),
                extractString(json, "key"),
                extractJsonValue(json, "evaluation")
            );
        }
    }

    // ========================================================================
    // ProduceResult
    // ========================================================================

    /**
     * Result of a {@link #produce} call.
     */
    public static class ProduceResult {
        /** {@code true} if the message was produced successfully. */
        public final boolean success;
        /** Error message (empty on success). */
        public final String error;

        /**
         * Construct a new produce result.
         *
         * @param success whether the produce succeeded
         * @param error   the error message
         */
        public ProduceResult(boolean success, String error) {
            this.success = success;
            this.error = error;
        }

        /**
         * Parse a {@code ProduceResult} from a JSON response string.
         *
         * @param json the JSON response from {@code kafkaconnect.produce}
         * @return a parsed result
         */
        public static ProduceResult fromJson(String json) {
            return new ProduceResult(
                extractBoolean(json, "success"),
                extractString(json, "error")
            );
        }
    }

    // ========================================================================
    // SendWebhookResult
    // ========================================================================

    /**
     * Result of a {@link #sendWebhook} call.
     */
    public static class SendWebhookResult {
        /** Unique ID for the webhook delivery. */
        public final String deliveryId;

        /**
         * Construct a new send-webhook result.
         *
         * @param deliveryId the delivery ID
         */
        public SendWebhookResult(String deliveryId) {
            this.deliveryId = deliveryId;
        }

        /**
         * Parse a {@code SendWebhookResult} from a JSON response string.
         *
         * @param json the JSON response from {@code notifications.send_webhook}
         * @return a parsed result
         */
        public static SendWebhookResult fromJson(String json) {
            return new SendWebhookResult(
                extractString(json, "delivery_id")
            );
        }
    }

    // ========================================================================
    // TriggerIncidentResult
    // ========================================================================

    /**
     * Result of a {@link #triggerIncident} call.
     */
    public static class TriggerIncidentResult {
        /** Unique key for the triggered incident. */
        public final String incidentKey;
        /** Incident status (e.g. {@code "triggered"}, {@code "acknowledged"}). */
        public final String status;

        /**
         * Construct a new trigger-incident result.
         *
         * @param incidentKey the incident key
         * @param status      the incident status
         */
        public TriggerIncidentResult(String incidentKey, String status) {
            this.incidentKey = incidentKey;
            this.status = status;
        }

        /**
         * Parse a {@code TriggerIncidentResult} from a JSON response string.
         *
         * @param json the JSON response from {@code pagerdutyalert.trigger_incident}
         * @return a parsed result
         */
        public static TriggerIncidentResult fromJson(String json) {
            return new TriggerIncidentResult(
                extractString(json, "incident_key"),
                extractString(json, "status")
            );
        }
    }

    // ========================================================================
    // ResolveIncidentResult
    // ========================================================================

    /**
     * Result of a {@link #resolveIncident} call.
     */
    public static class ResolveIncidentResult {
        /** Resolution status (e.g. {@code "resolved"}). */
        public final String status;

        /**
         * Construct a new resolve-incident result.
         *
         * @param status the resolution status
         */
        public ResolveIncidentResult(String status) {
            this.status = status;
        }

        /**
         * Parse a {@code ResolveIncidentResult} from a JSON response string.
         *
         * @param json the JSON response from {@code pagerdutyalert.resolve_incident}
         * @return a parsed result
         */
        public static ResolveIncidentResult fromJson(String json) {
            return new ResolveIncidentResult(
                extractString(json, "status")
            );
        }
    }

    // ========================================================================
    // SendMessageResult
    // ========================================================================

    /**
     * Result of a {@link #sendMessage} call.
     */
    public static class SendMessageResult {
        /** {@code true} if the message was sent successfully. */
        public final boolean success;
        /** Slack message timestamp (empty on failure). */
        public final String ts;

        /**
         * Construct a new send-message result.
         *
         * @param success whether the message was sent
         * @param ts      the Slack message timestamp
         */
        public SendMessageResult(boolean success, String ts) {
            this.success = success;
            this.ts = ts;
        }

        /**
         * Parse a {@code SendMessageResult} from a JSON response string.
         *
         * @param json the JSON response from {@code slacknotify.send_message}
         * @return a parsed result
         */
        public static SendMessageResult fromJson(String json) {
            return new SendMessageResult(
                extractBoolean(json, "success"),
                extractString(json, "ts")
            );
        }
    }

    // ========================================================================
    // AwaitWebhookResult
    // ========================================================================

    /**
     * Result of an {@link #awaitWebhook} call.
     */
    public static class AwaitWebhookResult {
        /** {@code true} if a matching webhook was available. */
        public final boolean found;
        /** Unique webhook ID (empty if no webhook was found). */
        public final String id;
        /** The received webhook event type (empty if no webhook was found). */
        public final String eventType;
        /** Arbitrary webhook payload as raw JSON (empty if no webhook was found). */
        public final String payload;
        /** ISO-8601 timestamp of when the webhook was received. */
        public final String receivedAt;

        /**
         * Construct a new await-webhook result.
         *
         * @param found      whether a webhook was found
         * @param id         the webhook ID
         * @param eventType  the event type
         * @param payload    the webhook payload as raw JSON
         * @param receivedAt the ISO-8601 reception timestamp
         */
        public AwaitWebhookResult(boolean found, String id, String eventType,
                                  String payload, String receivedAt) {
            this.found = found;
            this.id = id;
            this.eventType = eventType;
            this.payload = payload;
            this.receivedAt = receivedAt;
        }

        /**
         * Parse an {@code AwaitWebhookResult} from a JSON response string.
         *
         * @param json the JSON response from {@code webhookingest.await_webhook}
         * @return a parsed result
         */
        public static AwaitWebhookResult fromJson(String json) {
            return new AwaitWebhookResult(
                extractBoolean(json, "found"),
                extractString(json, "id"),
                extractString(json, "event_type"),
                extractJsonValue(json, "payload"),
                extractString(json, "received_at")
            );
        }
    }

    // ========================================================================
    // LlmChatResult
    // ========================================================================

    /**
     * Result of an LLM chat completion call via {@link #chat}.
     * <p>
     * Contains the model's response text, any tool calls made,
     * the finish reason, and usage metadata.
     */
    public static class LlmChatResult {
        /** The response text from the LLM (empty if only tool calls were made). */
        public final String response;

        /** Raw JSON of tool calls made by the LLM (empty if none). */
        public final String toolCalls;

        /** The finish reason (e.g. {@code "stop"}, {@code "tool_calls"}, {@code "length"}). */
        public final String finishReason;

        /** Raw JSON of usage information (e.g. prompt/completion tokens). */
        public final String usageInfo;

        /**
         * Construct a new LLM chat result.
         *
         * @param response     the response text
         * @param toolCalls    the tool calls JSON
         * @param finishReason the finish reason
         * @param usageInfo    the usage info JSON
         */
        public LlmChatResult(String response, String toolCalls,
                             String finishReason, String usageInfo) {
            this.response = response;
            this.toolCalls = toolCalls;
            this.finishReason = finishReason;
            this.usageInfo = usageInfo;
        }

        /**
         * Parse an {@code LlmChatResult} from a JSON response string.
         *
         * @param json the JSON response from the LLM plugin
         * @return a parsed result
         */
        public static LlmChatResult fromJson(String json) {
            return new LlmChatResult(
                extractString(json, "response"),
                extractJsonValue(json, "tool_calls"),
                extractString(json, "finish_reason"),
                extractJsonValue(json, "usage")
            );
        }
    }

    // ========================================================================
    // HostCalls wrapper
    // ========================================================================

    private final HostCalls host;

    /**
     * Create a new {@code Plugins} instance wrapping a {@link HostCalls}.
     *
     * @param host the host calls instance to use for low-level plugin calls
     */
    public Plugins(HostCalls host) {
        this.host = host;
    }

    // --------------------------------------------------------------------
    // blobstore
    // --------------------------------------------------------------------

    /**
     * Store a blob in the blobstore plugin.
     *
     * @param key         the key to store the blob under
     * @param data        the blob data (base64-encoded if binary)
     * @param contentType optional MIME content type (may be null or empty)
     * @param tags        optional tags to attach to the blob (may be null)
     * @param ttl         optional TTL duration (e.g. {@code "24h"},
     *                    {@code "7d"}; may be null or empty)
     * @return the result including the key, SHA-256 digest, and size
     * @throws RuntimeException if the plugin call fails
     */
    public BlobPutResult blobstorePut(String key, String data,
                                      String contentType,
                                      Map<String, String> tags,
                                      String ttl) {
        StringBuilder sb = new StringBuilder("{");
        sb.append("\"key\":\"").append(JsonHelper.escapeJson(key)).append("\"");
        sb.append(",\"data\":\"").append(JsonHelper.escapeJson(data)).append("\"");
        if (contentType != null && !contentType.isEmpty()) {
            sb.append(",\"content_type\":\"").append(JsonHelper.escapeJson(contentType)).append("\"");
        }
        if (tags != null && !tags.isEmpty()) {
            sb.append(",\"tags\":").append(stringMapToJson(tags));
        }
        if (ttl != null && !ttl.isEmpty()) {
            sb.append(",\"ttl\":\"").append(JsonHelper.escapeJson(ttl)).append("\"");
        }
        sb.append("}");

        String response = host.pluginCall("blobstore", "put", sb.toString());
        return BlobPutResult.fromJson(response);
    }

    /**
     * Retrieve a blob from the blobstore plugin.
     *
     * @param key the key to retrieve
     * @return the blob metadata and base64-encoded data
     * @throws RuntimeException if the plugin call fails
     */
    public BlobGetResult blobstoreGet(String key) {
        String input = "{\"key\":\"" + JsonHelper.escapeJson(key) + "\"}";
        String response = host.pluginCall("blobstore", "get", input);
        return BlobGetResult.fromJson(response);
    }

    // --------------------------------------------------------------------
    // eventtriggers
    // --------------------------------------------------------------------

    /**
     * Wait for an external event to arrive.
     *
     * @param eventType the event type to wait for
     * @param timeoutMs maximum wait time in milliseconds
     * @return the received event (if found within the timeout)
     * @throws RuntimeException if the plugin call fails
     */
    public AwaitEventResult awaitEvent(String eventType, long timeoutMs) {
        String input = "{\"event_type\":\"" + JsonHelper.escapeJson(eventType)
            + "\",\"timeout_ms\":" + timeoutMs + "}";
        String response = host.pluginCall("eventtriggers", "await_event", input);
        return AwaitEventResult.fromJson(response);
    }

    // --------------------------------------------------------------------
    // featureflags
    // --------------------------------------------------------------------

    /**
     * Evaluate a feature flag for a given context.
     *
     * @param key     the feature flag key
     * @param context optional evaluation context (may be null;
     *                e.g. {@code Map.of("user_id", "u-42")})
     * @return the flag evaluation result
     * @throws RuntimeException if the plugin call fails
     */
    public EvaluateFlagResult evaluateFlag(String key, Map<String, Object> context) {
        StringBuilder sb = new StringBuilder("{");
        sb.append("\"key\":\"").append(JsonHelper.escapeJson(key)).append("\"");
        if (context != null && !context.isEmpty()) {
            sb.append(",\"context\":").append(mapToJson(context));
        }
        sb.append("}");

        String response = host.pluginCall("featureflags", "evaluate_flag", sb.toString());
        return EvaluateFlagResult.fromJson(response);
    }

    // --------------------------------------------------------------------
    // kafkaconnect
    // --------------------------------------------------------------------

    /**
     * Produce a message to a Kafka topic.
     *
     * @param configId the Kafka connector configuration ID
     * @param value    the message value (JSON string)
     * @param key      the optional message key (may be null or empty)
     * @param headers  optional message headers (may be null)
     * @return whether the message was produced successfully
     * @throws RuntimeException if the plugin call fails
     */
    public ProduceResult produce(String configId, String value,
                                  String key,
                                  Map<String, String> headers) {
        StringBuilder sb = new StringBuilder("{");
        sb.append("\"config_id\":\"").append(JsonHelper.escapeJson(configId)).append("\"");
        sb.append(",\"value\":\"").append(JsonHelper.escapeJson(value)).append("\"");
        if (key != null && !key.isEmpty()) {
            sb.append(",\"key\":\"").append(JsonHelper.escapeJson(key)).append("\"");
        }
        if (headers != null && !headers.isEmpty()) {
            sb.append(",\"headers\":").append(stringMapToJson(headers));
        }
        sb.append("}");

        String response = host.pluginCall("kafkaconnect", "produce", sb.toString());
        return ProduceResult.fromJson(response);
    }

    // --------------------------------------------------------------------
    // notifications
    // --------------------------------------------------------------------

    /**
     * Send a webhook notification.
     *
     * @param webhookId the webhook configuration ID
     * @param eventType the event type to send
     * @param payload   optional payload to include in the webhook body
     *                  (may be null; a String is embedded as a JSON string,
     *                  any other value is serialised via {@link Object#toString()}
     *                  and assumed to be valid JSON)
     * @return the webhook delivery ID
     * @throws RuntimeException if the plugin call fails
     */
    public SendWebhookResult sendWebhook(String webhookId, String eventType,
                                          Object payload) {
        StringBuilder sb = new StringBuilder("{");
        sb.append("\"webhook_id\":\"").append(JsonHelper.escapeJson(webhookId)).append("\"");
        sb.append(",\"event_type\":\"").append(JsonHelper.escapeJson(eventType)).append("\"");
        if (payload != null) {
            sb.append(",\"payload\":").append(valueToJson(payload));
        }
        sb.append("}");

        String response = host.pluginCall("notifications", "send_webhook", sb.toString());
        return SendWebhookResult.fromJson(response);
    }

    // --------------------------------------------------------------------
    // pagerdutyalert
    // --------------------------------------------------------------------

    /**
     * Trigger a PagerDuty incident.
     *
     * @param configId the PagerDuty connector configuration ID
     * @param summary  a short summary of the incident
     * @param severity severity level (e.g. {@code "critical"},
     *                 {@code "warning"}, {@code "info"})
     * @param source   the source of the incident (e.g. {@code "monitoring"})
     * @param details  optional detailed description (may be null or empty)
     * @return the triggered incident details
     * @throws RuntimeException if the plugin call fails
     */
    public TriggerIncidentResult triggerIncident(String configId, String summary,
                                                  String severity, String source,
                                                  String details) {
        StringBuilder sb = new StringBuilder("{");
        sb.append("\"config_id\":\"").append(JsonHelper.escapeJson(configId)).append("\"");
        sb.append(",\"summary\":\"").append(JsonHelper.escapeJson(summary)).append("\"");
        sb.append(",\"severity\":\"").append(JsonHelper.escapeJson(severity)).append("\"");
        sb.append(",\"source\":\"").append(JsonHelper.escapeJson(source)).append("\"");
        if (details != null && !details.isEmpty()) {
            sb.append(",\"details\":\"").append(JsonHelper.escapeJson(details)).append("\"");
        }
        sb.append("}");

        String response = host.pluginCall("pagerdutyalert", "trigger_incident", sb.toString());
        return TriggerIncidentResult.fromJson(response);
    }

    /**
     * Resolve a PagerDuty incident.
     *
     * @param configId    the PagerDuty connector configuration ID
     * @param incidentKey the incident key (from {@link #triggerIncident})
     * @return the resolution status
     * @throws RuntimeException if the plugin call fails
     */
    public ResolveIncidentResult resolveIncident(String configId, String incidentKey) {
        String input = "{\"config_id\":\"" + JsonHelper.escapeJson(configId)
            + "\",\"incident_key\":\"" + JsonHelper.escapeJson(incidentKey) + "\"}";
        String response = host.pluginCall("pagerdutyalert", "resolve_incident", input);
        return ResolveIncidentResult.fromJson(response);
    }

    // --------------------------------------------------------------------
    // slacknotify
    // --------------------------------------------------------------------

    /**
     * Send a Slack message.
     *
     * @param configId the Slack connector configuration ID
     * @param text     the message text
     * @param channel  optional target channel (may be null or empty;
     *                 overrides the configured default)
     * @param blocks   optional Slack Block Kit blocks as a JSON value
     *                 (may be null; a String is embedded as a JSON string,
     *                 any other value is serialised via
     *                 {@link Object#toString()} and assumed to be valid JSON)
     * @return whether the message was sent successfully
     * @throws RuntimeException if the plugin call fails
     */
    public SendMessageResult sendMessage(String configId, String text,
                                          String channel, Object blocks) {
        StringBuilder sb = new StringBuilder("{");
        sb.append("\"config_id\":\"").append(JsonHelper.escapeJson(configId)).append("\"");
        sb.append(",\"text\":\"").append(JsonHelper.escapeJson(text)).append("\"");
        if (channel != null && !channel.isEmpty()) {
            sb.append(",\"channel\":\"").append(JsonHelper.escapeJson(channel)).append("\"");
        }
        if (blocks != null) {
            sb.append(",\"blocks\":").append(valueToJson(blocks));
        }
        sb.append("}");

        String response = host.pluginCall("slacknotify", "send_message", sb.toString());
        return SendMessageResult.fromJson(response);
    }

    // --------------------------------------------------------------------
    // webhookingest
    // --------------------------------------------------------------------

    /**
     * Wait for an incoming webhook from a webhook source.
     *
     * @param sourceId  the webhook source configuration ID
     * @param eventType optional event type filter (may be null or empty)
     * @return the received webhook (if one was available)
     * @throws RuntimeException if the plugin call fails
     */
    public AwaitWebhookResult awaitWebhook(String sourceId, String eventType) {
        StringBuilder sb = new StringBuilder("{");
        sb.append("\"source_id\":\"").append(JsonHelper.escapeJson(sourceId)).append("\"");
        if (eventType != null && !eventType.isEmpty()) {
            sb.append(",\"event_type\":\"").append(JsonHelper.escapeJson(eventType)).append("\"");
        }
        sb.append("}");

        String response = host.pluginCall("webhookingest", "await_webhook", sb.toString());
        return AwaitWebhookResult.fromJson(response);
    }

    // --------------------------------------------------------------------
    // llm (Large Language Model)
    // --------------------------------------------------------------------

    /**
     * Send a chat completion request to an LLM.
     * <p>
     * The messages parameter should contain a list of message objects, each
     * with {@code role} and {@code content} fields.  The tools parameter is
     * an optional list of tool definitions the LLM may call.
     *
     * @param model    the model name (e.g. {@code "gpt-4"}, {@code "claude-3-opus-20240229"})
     * @param messages the conversation messages (list of {@code {"role": ..., "content": ...}})
     * @param tools    optional tool definitions (may be null)
     * @return the LLM chat result with response, tool calls, and usage info
     * @throws RuntimeException if the plugin call fails
     */
    public LlmChatResult chat(String model, List<Map<String, Object>> messages,
                               List<Map<String, Object>> tools) {
        StringBuilder sb = new StringBuilder("{");
        sb.append("\"model\":\"").append(JsonHelper.escapeJson(model)).append("\"");
        sb.append(",\"messages\":");
        if (messages != null && !messages.isEmpty()) {
            sb.append(listToJson(new java.util.ArrayList<Object>((List<?>) messages)));
        } else {
            sb.append("[]");
        }
        if (tools != null && !tools.isEmpty()) {
            sb.append(",\"tools\":").append(listToJson(new java.util.ArrayList<Object>((List<?>) tools)));
        }
        sb.append("}");

        String response = host.pluginCall("llm", "chat", sb.toString());
        return LlmChatResult.fromJson(response);
    }

    /**
     * Generate embeddings for a list of text strings.
     * <p>
     * Returns a list of embedding vectors, one for each input text.
     * Each embedding vector is a list of double values.
     *
     * @param model the embedding model name (e.g. {@code "text-embedding-3-small"})
     * @param texts the list of texts to embed
     * @return a list of embedding vectors, each being a list of doubles
     * @throws RuntimeException if the plugin call fails
     */
    public List<List<Double>> embed(String model, List<String> texts) {
        StringBuilder sb = new StringBuilder("{");
        sb.append("\"model\":\"").append(JsonHelper.escapeJson(model)).append("\"");
        sb.append(",\"texts\":[");
        for (int i = 0; i < texts.size(); i++) {
            if (i > 0) {
                sb.append(",");
            }
            sb.append("\"").append(JsonHelper.escapeJson(texts.get(i))).append("\"");
        }
        sb.append("]}");

        String response = host.pluginCall("llm", "embed", sb.toString());

        // Parse response: {"embeddings": [[0.1, 0.2, ...], [0.3, 0.4, ...]]}
        String rawEmbeddings = extractJsonValue(response, "embeddings");
        if (rawEmbeddings.isEmpty()) {
            return new java.util.ArrayList<>();
        }

        java.util.List<Object> parsedEmbeddings = JsonHelper.parseArray(rawEmbeddings);
        List<List<Double>> result = new java.util.ArrayList<>();
        for (Object obj : parsedEmbeddings) {
            if (obj instanceof List) {
                List<Double> embedding = new java.util.ArrayList<>();
                for (Object val : (List<?>) obj) {
                    if (val instanceof Number) {
                        embedding.add(((Number) val).doubleValue());
                    } else {
                        embedding.add(0.0);
                    }
                }
                result.add(embedding);
            }
        }
        return result;
    }
}
