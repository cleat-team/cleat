/**
 * Tests for agent.ts -- Tool, AgentConfig, ToolCallRecord, AgentResult.
 *
 * `runAgent` itself calls the real `childWorkflow`/`awaitChild` host calls,
 * which trap outside a real worker (same reason __compile__/all-host-calls.ts
 * is a compile check and never invoked). So these tests check the wire shape
 * directly -- the same way cleat-sdk's Rust tests and Python's
 * `Tool.to_json()` tests do -- rather than calling `runAgent` against a host.
 * The real round trip is proven by `tests/crash/agent_resume_test.go`,
 * against the actual deployed workflow.
 */
import {
  AgentConfig,
  AgentResult,
  Tool,
  JsonParser,
  JsonBuilder,
  TYPE_OBJECT,
} from "../index";

function expectStr(actual: string, expected: string): void {
  expect<string>(actual).toBe(expected);
}

function expectI32(actual: i32, expected: i32): void {
  expect<i32>(actual).toBe(expected);
}

function expectF64(actual: f64, expected: f64): void {
  expect<f64>(actual).toBe(expected);
}

describe("Tool.writeTo", (): void => {
  it("omits every optional field by default", (): void => {
    let tool = new Tool("lookup", "service");
    let b = new JsonBuilder();
    tool.writeTo(b);
    expectStr(b.build(), '{"name":"lookup","kind":"service"}');
  });

  it("round-trips an approval tool's poll fields", (): void => {
    let tool = new Tool("wait-for-sign-off", "approval");
    tool.plugin = "approvals";
    tool.function_ = "claim";
    tool.pollIntervalSeconds = 5;
    tool.maxPolls = 3;

    let b = new JsonBuilder();
    tool.writeTo(b);
    let json = b.build();

    let parser = new JsonParser();
    let val = parser.parse(json);
    let notNull: bool = val !== null;
    expect<bool>(notNull).toBe(true);
    if (val !== null) {
      expectF64(parser.getNumber(val, "poll_interval_seconds"), 5.0);
      expectF64(parser.getNumber(val, "max_polls"), 3.0);
    }
  });

  it("splices parametersJson in verbatim, not re-escaped", (): void => {
    let tool = new Tool("lookup", "service");
    tool.parametersJson = '{"type":"object","properties":{"city":{"type":"string"}}}';

    let b = new JsonBuilder();
    tool.writeTo(b);
    let json = b.build();

    let parser = new JsonParser();
    let val = parser.parse(json);
    let notNull: bool = val !== null;
    expect<bool>(notNull).toBe(true);
    if (val !== null) {
      // parameters is an already-serialized OBJECT, spliced in verbatim --
      // confirm it parsed as a nested object rather than a string literal.
      expectI32(parser.typeOf(val, "parameters"), TYPE_OBJECT);
    }
  });
});

describe("AgentConfig.toJSON", (): void => {
  it("sends only the message by default", (): void => {
    let config = new AgentConfig();
    expectStr(config.toJSON("hello"), '{"message":"hello"}');
  });

  it("serializes every declared field", (): void => {
    let config = new AgentConfig();
    config.systemPrompt = "be terse";
    config.provider = "openai";
    config.model = "gpt-4o-mini";
    config.maxSteps = 4;
    config.temperature = 0.2;
    config.budget = 1.5;
    config.tenantId = "tenant-1";
    config.artifactKey = "answer.txt";
    config.toolErrorMode = "fail";

    let tool = new Tool("lookup", "service");
    tool.service = "weather";
    tool.operation = "get";
    config.tools.push(tool);

    let json = config.toJSON("How warm is Tokyo?");
    let parser = new JsonParser();
    let val = parser.parse(json);
    let notNull: bool = val !== null;
    expect<bool>(notNull).toBe(true);
    if (val !== null) {
      expectStr(parser.getString(val, "message"), "How warm is Tokyo?");
      expectStr(parser.getString(val, "system_prompt"), "be terse");
      expectStr(parser.getString(val, "provider"), "openai");
      expectStr(parser.getString(val, "model"), "gpt-4o-mini");
      expectF64(parser.getNumber(val, "max_steps"), 4.0);
      expectF64(parser.getNumber(val, "temperature"), 0.2);
      expectF64(parser.getNumber(val, "budget"), 1.5);
      expectStr(parser.getString(val, "tenant_id"), "tenant-1");
      expectStr(parser.getString(val, "artifact_key"), "answer.txt");
      expectStr(parser.getString(val, "tool_error_mode"), "fail");

      let tools = parser.getArray(val, "tools");
      expectI32(tools.length, 1);
      expectStr(parser.getString(tools[0], "service"), "weather");
      expectStr(parser.getString(tools[0], "operation"), "get");
    }
  });
});

describe("AgentResult.fromJSON", (): void => {
  it("decodes the workflow's own JSON shape", (): void => {
    // agentworkflow.Result's own JSON shape, byte for byte.
    let raw = '{"status":"done","answer":"It\'s 28C in Tokyo.","steps":2,' +
      '"tool_calls":[{"step":0,"name":"lookup","arguments":"{\\"city\\":\\"Tokyo\\"}","result":"28C"}],' +
      '"model":"gpt-4o-mini","total_tokens":123,"cost":0.0021}';
    let result = AgentResult.fromJSON(raw);
    expectStr(result.status, "done");
    expectStr(result.answer, "It's 28C in Tokyo.");
    expectI32(result.steps, 2);
    expectI32(result.toolCalls.length, 1);
    expectStr(result.toolCalls[0].name, "lookup");
    expectI32(result.totalTokens, 123);
    expectF64(result.cost, 0.0021);
  });

  it("decodes cleanly with only status present", (): void => {
    // Every field but status falls back to its typed default, so a minimal
    // result (the budget_exceeded shape, which may omit answer/tool_calls
    // when nothing ran yet) still decodes rather than erroring.
    let result = AgentResult.fromJSON('{"status":"budget_exceeded"}');
    expectStr(result.status, "budget_exceeded");
    expectStr(result.answer, "");
    expectI32(result.toolCalls.length, 0);
  });
});
