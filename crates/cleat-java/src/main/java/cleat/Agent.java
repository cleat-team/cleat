package cleat;

import java.util.ArrayList;
import java.util.List;
import java.util.Map;

/**
 * {@code runAgent} -- start the shipped agent workflow as a child and await
 * it.
 * <p>
 * The agent loop is itself a WORKFLOW ({@code cleat/agentworkflow} in the Go
 * SDK), so this class is the whole per-language surface: the LLM turns, the
 * tool dispatch, the step budget and the durability all live in the
 * workflow, and a Java workflow gets every one of them by starting it under
 * its name. Before cleat#1983 the Go SDK carried a Go-only ReAct loop with no
 * importer, and the two agent templates carried hand-written copies of the
 * same loop that nothing tested. cleat#2978 is the Rust/Java/AssemblyScript
 * follow-up.
 * <p>
 * Each LLM turn and each tool call is a durable step inside the workflow, so
 * an agent started this way survives a crash mid-conversation and resumes
 * without asking the model again for turns it already completed.
 * <p>
 * Mirrors {@code python-sdk/cleat_sdk/agent.py}'s field scope exactly, not
 * the full Go {@code agentworkflow.Input} -- {@code arg_transforms}/
 * {@code static_args} (cleat#3169) are not yet exposed by any SDK wrapper,
 * Python's included.
 * <p>
 * Built with {@link JsonBuilder} and {@link JsonHelper#parseObject(String)}
 * directly, NOT {@link HostCalls#childWorkflowTyped}/
 * {@link HostCalls#awaitChildTyped} -- {@link JsonHelper#stringify(Object)}
 * and {@link JsonHelper#parse(String, Class)} only support {@code String},
 * {@code Map}, {@code List} and boxed primitives; both throw for an
 * arbitrary POJO, which is exactly the shape {@code AgentConfig}/
 * {@code AgentResult} are. This follows {@link Plugins}'s own established
 * pattern: build the request with {@link JsonBuilder}, parse the response
 * with hand-written field extraction.
 * <p>
 * <strong>Usage:</strong>
 * <pre>{@code
 * Agent.Tool lookup = new Agent.Tool();
 * lookup.name = "lookup";
 * lookup.kind = "service";
 * lookup.service = "weather";
 * lookup.operation = "get";
 *
 * Agent.AgentConfig config = new Agent.AgentConfig();
 * config.tools.add(lookup);
 *
 * Agent.AgentResult result = Agent.runAgent(host, config, "How warm is Tokyo?");
 * System.out.println(result.answer);
 * }</pre>
 */
public final class Agent {

    private Agent() {
        // Utility class -- no instantiation.
    }

    /**
     * The workflow name the agent must be DEPLOYED under, because a child is
     * resolved by name against a deployed {@code workflow_defs} row.
     */
    public static final String AGENT_WORKFLOW_NAME = "agent";

    // ========================================================================
    // Tool
    // ========================================================================

    /**
     * One tool the model may call, and how to call it.
     * <p>
     * {@link #name}, {@link #description} and {@link #parametersJson} are
     * what the model sees; the rest is dispatch, and it is deliberately
     * opaque to the model -- the same tool list reaches a durable call, a
     * plugin or a child workflow without the model being told which.
     * <p>
     * {@link #kind} is one of {@code "service"} ({@link #service} +
     * {@link #operation}), {@code "plugin"} ({@link #plugin} +
     * {@link #function}), {@code "workflow"} ({@link #workflow}) or
     * {@code "approval"} ({@link #plugin} + {@link #function}, polled until
     * the claim reports found).
     */
    public static class Tool {
        /** The function name the model calls. Required, unique. */
        public String name = "";
        /** One of {@code "service"}, {@code "plugin"}, {@code "workflow"}, {@code "approval"}. */
        public String kind = "";
        /** Shown to the model. */
        public String description = "";
        /**
         * The JSON-schema parameters shown to the model, as already-serialized
         * JSON text (an arbitrary object the model's tool-calling API expects,
         * not a shape this SDK can type) -- empty omits the field entirely.
         */
        public String parametersJson = "";

        /** {@code service} + {@code operation} dispatch a {@code "service"} tool via a durable call. */
        public String service = "";
        public String operation = "";

        /** {@code plugin} + {@code function} dispatch a {@code "plugin"} (or {@code "approval"}) tool. */
        public String plugin = "";
        public String function = "";

        /** Dispatches a {@code "workflow"} tool: started as a child and awaited. */
        public String workflow = "";

        /** An {@code "approval"} tool's wait. 0 takes the workflow's own default. */
        public long pollIntervalSeconds = 0;
        public long maxPolls = 0;

        JsonBuilder toJsonBuilder() {
            JsonBuilder b = new JsonBuilder().add("name", name).add("kind", kind);
            if (!description.isEmpty()) {
                b.add("description", description);
            }
            if (!parametersJson.isEmpty()) {
                b.addRawJson("parameters", parametersJson);
            }
            if (!service.isEmpty()) {
                b.add("service", service);
            }
            if (!operation.isEmpty()) {
                b.add("operation", operation);
            }
            if (!plugin.isEmpty()) {
                b.add("plugin", plugin);
            }
            if (!function.isEmpty()) {
                b.add("function", function);
            }
            if (!workflow.isEmpty()) {
                b.add("workflow", workflow);
            }
            if (pollIntervalSeconds != 0) {
                b.add("poll_interval_seconds", pollIntervalSeconds);
            }
            if (maxPolls != 0) {
                b.add("max_polls", maxPolls);
            }
            return b;
        }
    }

    // ========================================================================
    // AgentConfig
    // ========================================================================

    /**
     * The agent's configuration. Mirrors {@code python-sdk/cleat_sdk/agent.py}'s
     * {@code AgentConfig}, which itself mirrors {@code cleat/agentworkflow}'s
     * {@code Input}.
     * <p>
     * Carries no {@code message} field: the message is {@link #runAgent}'s
     * own argument, matching {@code agentworkflow.RunAsChild}/{@code run_agent}'s
     * own shape, so there is exactly one place to look for it.
     */
    public static class AgentConfig {
        public List<Tool> tools = new ArrayList<>();
        public String systemPrompt = "";
        public String provider = "";
        public String model = "";
        /** Bounds tool-calling rounds. 0 takes the workflow's own default. */
        public long maxSteps = 0;
        /** Passed through to the provider. 0.0 means the provider's own default. */
        public double temperature = 0.0;

        /**
         * The run's spend ceiling in dollars, against the {@code cost} the
         * llm plugin reports. 0.0 (the default) means unbounded. The workflow
         * stops before starting a turn once the ceiling is REACHED, and
         * reports {@code status == "budget_exceeded"} in the result.
         */
        public double budget = 0.0;

        /** Attribution, echoed back in the result and never required by the workflow. */
        public String tenantId = "";

        /**
         * When set, the workflow writes the finished answer to the bundled
         * blobstore plugin under this key and echoes it in the result.
         */
        public String artifactKey = "";

        /**
         * What a failed tool -- or a tool the model invented -- does.
         * {@code ""} is the workflow's default ({@code "inject"}: the error
         * goes back to the model as the tool's result). {@code "fail"} ends
         * the run instead. This is a CONTRACT difference, not a preference,
         * so it is stated rather than assumed.
         */
        public String toolErrorMode = "";

        JsonBuilder toJsonBuilder(String message) {
            JsonBuilder b = new JsonBuilder().add("message", message);
            if (!tools.isEmpty()) {
                JsonBuilder[] elements = new JsonBuilder[tools.size()];
                for (int i = 0; i < tools.size(); i++) {
                    elements[i] = tools.get(i).toJsonBuilder();
                }
                b.addArray("tools", elements);
            }
            if (!systemPrompt.isEmpty()) {
                b.add("system_prompt", systemPrompt);
            }
            if (!provider.isEmpty()) {
                b.add("provider", provider);
            }
            if (!model.isEmpty()) {
                b.add("model", model);
            }
            if (maxSteps != 0) {
                b.add("max_steps", maxSteps);
            }
            if (temperature != 0.0) {
                b.add("temperature", temperature);
            }
            if (budget != 0.0) {
                b.add("budget", budget);
            }
            if (!tenantId.isEmpty()) {
                b.add("tenant_id", tenantId);
            }
            if (!artifactKey.isEmpty()) {
                b.add("artifact_key", artifactKey);
            }
            if (!toolErrorMode.isEmpty()) {
                b.add("tool_error_mode", toolErrorMode);
            }
            return b;
        }
    }

    // ========================================================================
    // ToolCallRecord / AgentResult
    // ========================================================================

    /** One tool call the agent made, as reported back in {@link AgentResult}. */
    public static class ToolCallRecord {
        public long step;
        public String name = "";
        public String arguments = "";
        public String result = "";
        public String error = "";

        static ToolCallRecord fromMap(Map<String, Object> m) {
            ToolCallRecord r = new ToolCallRecord();
            r.step = longField(m, "step");
            r.name = stringField(m, "name");
            r.arguments = stringField(m, "arguments");
            r.result = stringField(m, "result");
            r.error = stringField(m, "error");
            return r;
        }
    }

    /** The agent workflow's result. */
    public static class AgentResult {
        /**
         * {@code "done"} when the model answered, or {@code "budget_exceeded"}
         * when the spend ceiling stopped it first -- see the workflow's own
         * {@code StatusDone}/{@code StatusBudgetExceeded}.
         */
        public String status = "";
        public String answer = "";
        public long steps;
        public List<ToolCallRecord> toolCalls = new ArrayList<>();
        public String model = "";
        public long totalTokens;
        public double cost;
        public String tenantId = "";
        public String artifactKey = "";

        @SuppressWarnings("unchecked")
        static AgentResult fromJson(String json) {
            Map<String, Object> m = JsonHelper.parseObject(json);
            AgentResult r = new AgentResult();
            r.status = stringField(m, "status");
            r.answer = stringField(m, "answer");
            r.steps = longField(m, "steps");
            Object rawToolCalls = m.get("tool_calls");
            if (rawToolCalls instanceof List) {
                for (Object item : (List<Object>) rawToolCalls) {
                    if (item instanceof Map) {
                        r.toolCalls.add(ToolCallRecord.fromMap((Map<String, Object>) item));
                    }
                }
            }
            r.model = stringField(m, "model");
            r.totalTokens = longField(m, "total_tokens");
            r.cost = doubleField(m, "cost");
            r.tenantId = stringField(m, "tenant_id");
            r.artifactKey = stringField(m, "artifact_key");
            return r;
        }
    }

    // ========================================================================
    // Field extraction helpers -- a parsed JSON object's values are already
    // String/Number/Boolean/Map/List/null (JsonHelper.parseObject), never a
    // sentinel for "absent"; a missing key is simply absent from the Map.
    // ========================================================================

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

    // ========================================================================
    // runAgent
    // ========================================================================

    /**
     * Starts the shipped agent workflow as a child and awaits its result.
     * <p>
     * THIS IS THE WHOLE PER-LANGUAGE SURFACE -- everything else (the loop,
     * the tool dispatch, the step budget, the durability) lives in the
     * workflow, which is why the same agent works from Go, Python, Rust,
     * Java or AssemblyScript without any of them implementing it. See
     * {@code cleat/agentworkflow}'s own {@code RunAsChild} doc comment,
     * which this mirrors exactly.
     * <p>
     * The caller's workflow needs the agent deployed under
     * {@link #AGENT_WORKFLOW_NAME}. A deployment under a different name
     * means calling {@link HostCalls#childWorkflow}/{@link HostCalls#awaitChild}
     * directly with the marshalled config -- two calls, and the reason this
     * is a convenience rather than a mechanism.
     *
     * @param host    the host calls instance for the current execution
     * @param config  what the agent is given: its tools, and the model to use
     * @param message the user's message
     * @return the agent's result
     * @throws RuntimeException if starting or awaiting the child fails
     */
    public static AgentResult runAgent(HostCalls host, AgentConfig config, String message) {
        String payload = config.toJsonBuilder(message).build();

        CleatResult<String> started = host.childWorkflow(AGENT_WORKFLOW_NAME, payload);
        if (started.isErr()) {
            throw new RuntimeException("agent: start child \"" + AGENT_WORKFLOW_NAME + "\": " + started.getError());
        }

        CleatResult<String> awaited = host.awaitChild(started.getValue());
        if (awaited.isErr()) {
            throw new RuntimeException("agent: await child \"" + AGENT_WORKFLOW_NAME + "\": " + awaited.getError());
        }

        return AgentResult.fromJson(awaited.getValue());
    }
}
