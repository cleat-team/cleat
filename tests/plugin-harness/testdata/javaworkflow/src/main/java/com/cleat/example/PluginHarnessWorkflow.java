package com.cleat.example;

import cleat.HostCalls;
import cleat.CleatEntry;
import cleat.CleatResult;

/**
 * Plugin harness workflow — calls every plugin host function.
 *
 * This workflow exercises every host function exposed by every cleat plugin
 * from a Java workflow.  It is the Java equivalent of the Go, Rust,
 * AssemblyScript, and Python workflows in the same testdata directory.
 *
 * Each plugin call is independently error-handled so that one failure does
 * not block testing the remaining plugins.  Results are collected into a
 * single JSON object keyed by {@code "plugin.function"}.
 */
public class PluginHarnessWorkflow {

    @CleatEntry(name = "CallAllPlugins")
    public static String callAllPlugins(HostCalls h, String input) {
        StringBuilder sb = new StringBuilder();
        sb.append("{");

        // -- blobstore -------------------------------------------------------
        call(sb, "blobstore.put", h, "blobstore", "put",
             "{\"key\":\"test-key\",\"data\":\"aGVsbG8=\"}");
        sb.append(",");
        call(sb, "blobstore.get", h, "blobstore", "get",
             "{\"key\":\"test-key\"}");

        // -- event-triggers --------------------------------------------------
        sb.append(",");
        call(sb, "event-triggers.await_event", h, "event-triggers", "await_event",
             "{\"event_type\":\"test.event\",\"timeout_ms\":100}");

        // -- feature-flags ---------------------------------------------------
        sb.append(",");
        call(sb, "feature-flags.evaluate_flag", h, "feature-flags", "evaluate_flag",
             "{\"key\":\"test-flag\",\"context\":{\"user_id\":\"test-user\"}}");

        // -- kafka-connect ---------------------------------------------------
        sb.append(",");
        call(sb, "kafka-connect.produce", h, "kafka-connect", "produce",
             "{\"config_id\":\"00000000-0000-0000-0000-000000000001\"," +
             "\"value\":\"test-message\",\"key\":\"test-key\"}");

        // -- notifications ---------------------------------------------------
        sb.append(",");
        call(sb, "notifications.send_webhook", h, "notifications", "send_webhook",
             "{\"webhook_id\":\"00000000-0000-0000-0000-000000000002\"," +
             "\"event_type\":\"test.event\",\"payload\":{\"message\":\"hello\"}}");

        // -- pagerduty-alert -------------------------------------------------
        sb.append(",");
        call(sb, "pagerduty-alert.trigger_incident", h, "pagerduty-alert", "trigger_incident",
             "{\"config_id\":\"00000000-0000-0000-0000-000000000003\"," +
             "\"summary\":\"Test incident\",\"severity\":\"critical\"," +
             "\"source\":\"test-harness\"}");

        sb.append(",");
        call(sb, "pagerduty-alert.resolve_incident", h, "pagerduty-alert", "resolve_incident",
             "{\"config_id\":\"00000000-0000-0000-0000-000000000003\"," +
             "\"incident_key\":\"mock-key\"}");

        // -- pgvector --------------------------------------------------------
        sb.append(",");
        call(sb, "pgvector.upsert", h, "pgvector", "upsert",
             "{\"collection\":\"test-collection\",\"external_id\":\"test-1\"," +
             "\"content\":\"test content\",\"embedding\":[0.1,0.2,0.3,0.4]}");

        sb.append(",");
        call(sb, "pgvector.search", h, "pgvector", "search",
             "{\"collection\":\"test-collection\"," +
             "\"query_vector\":[0.1,0.2,0.3,0.4],\"top_k\":5}");

        sb.append(",");
        call(sb, "pgvector.delete", h, "pgvector", "delete",
             "{\"collection\":\"test-collection\",\"external_id\":\"test-1\"}");

        // -- slack-notify ----------------------------------------------------
        sb.append(",");
        call(sb, "slack-notify.send_message", h, "slack-notify", "send_message",
             "{\"config_id\":\"00000000-0000-0000-0000-000000000004\"," +
             "\"text\":\"Test message from plugin harness\",\"channel\":\"#test\"}");

        // -- webhook-ingest --------------------------------------------------
        sb.append(",");
        call(sb, "webhook-ingest.await_webhook", h, "webhook-ingest", "await_webhook",
             "{\"source_id\":\"test-source\"}");

        // -- llm -------------------------------------------------------------
        sb.append(",");
        call(sb, "llm.chat", h, "llm", "chat",
             "{\"provider\":\"openai\",\"model\":\"mock-model\"," +
             "\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]}");

        sb.append(",");
        call(sb, "llm.embed", h, "llm", "embed",
             "{\"provider\":\"openai\",\"model\":\"mock-model\"," +
             "\"input\":[\"test text\"]}");

        sb.append(",");
        call(sb, "llm.list_models", h, "llm", "list_models",
             "{\"provider\":\"openai\"}");

        // -- llm chat_stream (streaming) ----------------------------------------
        sb.append(",");
        sb.append("\"llm.chat_stream\":");
        CleatResult<String> streamResult = h.pluginCallStreaming(
            "llm", "chat_stream",
            "{\"provider\":\"openai\",\"model\":\"mock-model\"," +
            "\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]}");
        if (streamResult.isErr()) {
            sb.append("{\"error\":\"")
              .append(escapeJson(streamResult.getError()))
              .append("\"}");
        } else {
            sb.append(streamResult.getValue());
        }

        sb.append("}");
        return sb.toString();
    }

    /**
     * Call a single plugin function and append its result to the JSON builder.
     *
     * @param sb       the accumulating JSON string builder
     * @param key      the result key (e.g. "blobstore.put")
     * @param h        the HostCalls instance
     * @param plugin   the plugin name
     * @param func     the function name
     * @param inputJson the JSON input string
     */
    private static void call(StringBuilder sb, String key, HostCalls h,
                             String plugin, String func, String inputJson) {
        sb.append("\"").append(escapeJson(key)).append("\":");
        CleatResult<String> result = h.pluginCall(plugin, func, inputJson);
        if (result.isErr()) {
            sb.append("{\"error\":\"").append(escapeJson(result.getError())).append("\"}");
        } else {
            sb.append(result.getValue());
        }
    }

    /**
     * Escape a string for safe embedding in a JSON string value.
     *
     * <p>TeaVM-safe escaping — no regex or String.format.
     *
     * @param s the raw string
     * @return the JSON-escaped string
     */
    private static String escapeJson(String s) {
        if (s == null) return "null";
        StringBuilder out = new StringBuilder();
        for (int i = 0; i < s.length(); i++) {
            char c = s.charAt(i);
            switch (c) {
                case '"':  out.append("\\\""); break;
                case '\\': out.append("\\\\"); break;
                case '\n': out.append("\\n");  break;
                case '\r': out.append("\\r");  break;
                case '\t': out.append("\\t");  break;
                default:
                    if (c < 0x20) {
                        out.append("\\u00");
                        out.append(Integer.toHexString(c));
                    } else {
                        out.append(c);
                    }
            }
        }
        return out.toString();
    }
}
