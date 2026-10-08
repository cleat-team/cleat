package com.cleat.example;

import cleat.Agent;
import cleat.CleatEntry;
import cleat.HostCalls;
import cleat.JsonHelper;

import java.util.List;
import java.util.Map;

/**
 * The Java half of cleat#2978's acceptance test: a workflow that runs the
 * shipped agent as a CHILD, handed the same config the Go, Python, Rust and
 * AssemblyScript clients are handed.
 * <p>
 * THE CONFIG IS INPUT, NOT CODE, and that is deliberate -- the acceptance is
 * that each client runs the SAME agent config through the shipped agent
 * workflow, so the config has to come from the run's own input, not a
 * per-client literal that could silently drift from the others.
 * <p>
 * This class is also the Java SDK's surface under test: {@link Agent#runAgent}
 * does the start-and-await, so the fixture measures that call rather than
 * hand-rolling the two host calls it is made of.
 * <p>
 * A single {@code String} parameter, matching every other client's
 * {@code @CleatEntry}/{@code #cleat_entry}/{@code @cleatEntry} method --
 * {@code @CleatEntry} requires exactly one user parameter, which receives
 * the WHOLE input payload verbatim. This parses it itself with
 * {@link JsonHelper}/{@link Agent}'s own field extraction, the same pattern
 * {@link Agent}'s class doc comment explains (not {@code HostCalls}'s
 * {@code childWorkflowTyped}, which cannot deserialize a custom type).
 */
public final class AgentClient {

    private AgentClient() {
    }

    @SuppressWarnings("unchecked")
    @CleatEntry(name = "run_agent_client")
    public static String runAgentClient(HostCalls h, String input) {
        Map<String, Object> raw = JsonHelper.parseObject(input);

        String message = stringField(raw, "message");

        Agent.AgentConfig config = new Agent.AgentConfig();
        config.systemPrompt = stringField(raw, "system_prompt");
        config.provider = stringField(raw, "provider");
        config.model = stringField(raw, "model");
        config.maxSteps = longField(raw, "max_steps");
        config.temperature = doubleField(raw, "temperature");

        Object rawTools = raw.get("tools");
        if (rawTools instanceof List) {
            for (Object item : (List<Object>) rawTools) {
                if (item instanceof Map) {
                    config.tools.add(parseTool((Map<String, Object>) item));
                }
            }
        }

        Agent.AgentResult result = Agent.runAgent(h, config, message);
        return toJson(result);
    }

    private static Agent.Tool parseTool(Map<String, Object> m) {
        Agent.Tool tool = new Agent.Tool();
        tool.name = stringField(m, "name");
        tool.kind = stringField(m, "kind");
        tool.description = stringField(m, "description");
        tool.service = stringField(m, "service");
        tool.operation = stringField(m, "operation");
        tool.plugin = stringField(m, "plugin");
        tool.function = stringField(m, "function");
        tool.workflow = stringField(m, "workflow");
        tool.pollIntervalSeconds = longField(m, "poll_interval_seconds");
        tool.maxPolls = longField(m, "max_polls");

        // parameters is an arbitrary JSON Schema object, not a flat field --
        // re-encode whatever JsonHelper.parseObject gave back for it so
        // Agent.Tool.toJsonBuilder's addRawJsonField splices valid JSON.
        Object params = m.get("parameters");
        if (params != null) {
            tool.parametersJson = JsonHelper.stringify(params);
        }
        return tool;
    }

    private static String stringField(Map<String, Object> m, String key) {
        Object v = m.get(key);
        return v instanceof String ? (String) v : "";
    }

    private static long longField(Map<String, Object> m, String key) {
        Object v = m.get(key);
        return v instanceof Number ? ((Number) v).longValue() : 0L;
    }

    private static double doubleField(Map<String, Object> m, String key) {
        Object v = m.get(key);
        return v instanceof Number ? ((Number) v).doubleValue() : 0.0;
    }

    /**
     * Re-encodes an {@link Agent.AgentResult} as JSON by hand, the same
     * reason {@link Agent} itself never needed to: {@code runAgent} only
     * ever DECODES one of these (from the child's output); this class is
     * the first caller that needs to forward one back out as its own
     * workflow result.
     */
    private static String toJson(Agent.AgentResult r) {
        StringBuilder sb = new StringBuilder("{");
        sb.append("\"status\":\"").append(JsonHelper.escapeJson(r.status)).append('"');
        if (!r.answer.isEmpty()) {
            sb.append(",\"answer\":\"").append(JsonHelper.escapeJson(r.answer)).append('"');
        }
        sb.append(",\"steps\":").append(r.steps);
        sb.append(",\"tool_calls\":[");
        for (int i = 0; i < r.toolCalls.size(); i++) {
            if (i > 0) {
                sb.append(',');
            }
            Agent.ToolCallRecord tc = r.toolCalls.get(i);
            sb.append('{');
            sb.append("\"step\":").append(tc.step);
            sb.append(",\"name\":\"").append(JsonHelper.escapeJson(tc.name)).append('"');
            if (!tc.arguments.isEmpty()) {
                sb.append(",\"arguments\":\"").append(JsonHelper.escapeJson(tc.arguments)).append('"');
            }
            if (!tc.result.isEmpty()) {
                sb.append(",\"result\":\"").append(JsonHelper.escapeJson(tc.result)).append('"');
            }
            if (!tc.error.isEmpty()) {
                sb.append(",\"error\":\"").append(JsonHelper.escapeJson(tc.error)).append('"');
            }
            sb.append('}');
        }
        sb.append(']');
        if (!r.model.isEmpty()) {
            sb.append(",\"model\":\"").append(JsonHelper.escapeJson(r.model)).append('"');
        }
        sb.append(",\"total_tokens\":").append(r.totalTokens);
        sb.append(",\"cost\":").append(r.cost);
        if (!r.tenantId.isEmpty()) {
            sb.append(",\"tenant_id\":\"").append(JsonHelper.escapeJson(r.tenantId)).append('"');
        }
        if (!r.artifactKey.isEmpty()) {
            sb.append(",\"artifact_key\":\"").append(JsonHelper.escapeJson(r.artifactKey)).append('"');
        }
        sb.append('}');
        return sb.toString();
    }
}
