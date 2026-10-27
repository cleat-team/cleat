package cleat;

import static org.junit.jupiter.api.Assertions.*;
import org.junit.jupiter.api.Test;

import java.util.List;
import java.util.Map;

/**
 * Unit tests for {@link Agent}.
 * <p>
 * {@link Agent#runAgent} itself calls the real {@code childWorkflow}/
 * {@code awaitChild} host calls, which {@link CleatTestEnv} cannot stub with
 * an arbitrary result for an unstubbed child name (it auto-completes with
 * {@code {"status":"completed"}}, and nothing in this package's public test
 * harness exposes {@code TestHostCalls#registerChildWorkflowStub}). So these
 * tests check the wire shape directly -- the same way {@code cleat-sdk}'s
 * Rust tests and Python's {@code Tool.to_json()} tests do -- rather than
 * calling {@code runAgent} against a stubbed host. The real round trip is
 * proven by {@code tests/crash/agent_resume_test.go}, against the actual
 * deployed workflow.
 *
 * @see Agent
 */
class AgentTest {

    @Test
    void aDefaultToolOmitsEveryOptionalField() {
        Agent.Tool tool = new Agent.Tool();
        tool.name = "lookup";
        tool.kind = "service";

        Map<String, Object> parsed = JsonHelper.parseObject(tool.toJsonBuilder().build());
        assertEquals(Map.of("name", "lookup", "kind", "service"), parsed);
    }

    @Test
    void anApprovalToolsPollFieldsRoundTrip() {
        Agent.Tool tool = new Agent.Tool();
        tool.name = "wait-for-sign-off";
        tool.kind = "approval";
        tool.plugin = "approvals";
        tool.function = "claim";
        tool.pollIntervalSeconds = 5;
        tool.maxPolls = 3;

        Map<String, Object> parsed = JsonHelper.parseObject(tool.toJsonBuilder().build());
        assertEquals(5L, ((Number) parsed.get("poll_interval_seconds")).longValue());
        assertEquals(3L, ((Number) parsed.get("max_polls")).longValue());
    }

    @Test
    void aToolsRawParametersAreSplicedInVerbatimNotReEscaped() {
        Agent.Tool tool = new Agent.Tool();
        tool.name = "lookup";
        tool.kind = "service";
        tool.parametersJson = "{\"type\":\"object\",\"properties\":{\"city\":{\"type\":\"string\"}}}";

        Map<String, Object> parsed = JsonHelper.parseObject(tool.toJsonBuilder().build());
        @SuppressWarnings("unchecked")
        Map<String, Object> params = (Map<String, Object>) parsed.get("parameters");
        assertEquals("object", params.get("type"));
    }

    @Test
    void aDefaultConfigSendsOnlyTheMessage() {
        Agent.AgentConfig config = new Agent.AgentConfig();
        Map<String, Object> parsed = JsonHelper.parseObject(config.toJsonBuilder("hello").build());
        assertEquals(Map.of("message", "hello"), parsed);
    }

    @Test
    void aFullConfigSerializesEveryDeclaredField() {
        Agent.AgentConfig config = new Agent.AgentConfig();
        config.systemPrompt = "be terse";
        config.provider = "openai";
        config.model = "gpt-4o-mini";
        config.maxSteps = 4;
        config.temperature = 0.2;
        config.budget = 1.5;
        config.tenantId = "tenant-1";
        config.artifactKey = "answer.txt";
        config.toolErrorMode = "fail";

        Agent.Tool tool = new Agent.Tool();
        tool.name = "lookup";
        tool.kind = "service";
        tool.service = "weather";
        tool.operation = "get";
        config.tools.add(tool);

        Map<String, Object> parsed = JsonHelper.parseObject(config.toJsonBuilder("How warm is Tokyo?").build());
        assertEquals("How warm is Tokyo?", parsed.get("message"));
        assertEquals("be terse", parsed.get("system_prompt"));
        assertEquals("openai", parsed.get("provider"));
        assertEquals("gpt-4o-mini", parsed.get("model"));
        assertEquals(4L, ((Number) parsed.get("max_steps")).longValue());
        assertEquals(0.2, (Double) parsed.get("temperature"), 1e-9);
        assertEquals(1.5, (Double) parsed.get("budget"), 1e-9);
        assertEquals("tenant-1", parsed.get("tenant_id"));
        assertEquals("answer.txt", parsed.get("artifact_key"));
        assertEquals("fail", parsed.get("tool_error_mode"));

        @SuppressWarnings("unchecked")
        List<Object> tools = (List<Object>) parsed.get("tools");
        @SuppressWarnings("unchecked")
        Map<String, Object> toolMap = (Map<String, Object>) tools.get(0);
        assertEquals("weather", toolMap.get("service"));
        assertEquals("get", toolMap.get("operation"));
    }

    @Test
    void aResultDecodesFromTheWorkflowsOwnJsonShape() {
        // agentworkflow.Result's own JSON shape, byte for byte.
        String raw = "{"
            + "\"status\":\"done\","
            + "\"answer\":\"It's 28C in Tokyo.\","
            + "\"steps\":2,"
            + "\"tool_calls\":[{\"step\":0,\"name\":\"lookup\",\"arguments\":\"{\\\"city\\\":\\\"Tokyo\\\"}\",\"result\":\"28C\"}],"
            + "\"model\":\"gpt-4o-mini\","
            + "\"total_tokens\":123,"
            + "\"cost\":0.0021"
            + "}";
        Agent.AgentResult result = Agent.AgentResult.fromJson(raw);
        assertEquals("done", result.status);
        assertEquals("It's 28C in Tokyo.", result.answer);
        assertEquals(2, result.steps);
        assertEquals(1, result.toolCalls.size());
        assertEquals("lookup", result.toolCalls.get(0).name);
        assertEquals(123, result.totalTokens);
        assertEquals(0.0021, result.cost, 1e-9);
    }

    @Test
    void aResultWithOnlyStatusDecodesCleanly() {
        // Every field but status falls back to a typed default, so a
        // minimal result (the budget_exceeded shape, which may omit
        // answer/tool_calls when nothing ran yet) still decodes rather than
        // throwing.
        Agent.AgentResult result = Agent.AgentResult.fromJson("{\"status\":\"budget_exceeded\"}");
        assertEquals("budget_exceeded", result.status);
        assertEquals("", result.answer);
        assertTrue(result.toolCalls.isEmpty());
    }
}
