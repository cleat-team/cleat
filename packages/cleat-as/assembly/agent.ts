/**
 * `runAgent` -- start the shipped agent workflow as a child and await it.
 *
 * The agent loop is itself a WORKFLOW (`cleat/agentworkflow` in the Go SDK),
 * so this module is the whole per-language surface: the LLM turns, the tool
 * dispatch, the step budget and the durability all live in the workflow, and
 * an AssemblyScript workflow gets every one of them by starting it under its
 * name. Before cleat#1983 the Go SDK carried a Go-only ReAct loop with no
 * importer, and the two agent templates carried hand-written copies of the
 * same loop that nothing tested. cleat#2978 is the Rust/Java/AssemblyScript
 * follow-up.
 *
 * Each LLM turn and each tool call is a durable step inside the workflow, so
 * an agent started this way survives a crash mid-conversation and resumes
 * without asking the model again for turns it already completed.
 *
 * Mirrors `python-sdk/cleat_sdk/agent.py`'s field scope exactly, not the
 * full Go `agentworkflow.Input` -- `arg_transforms`/`static_args`
 * (cleat#3169) are not yet exposed by any SDK wrapper, Python's included.
 *
 * Built with `JsonBuilder`/`JsonParser` (./json), NOT `plugins.ts`'s flat
 * string-scanning extractors -- `tools` and `tool_calls` are arrays of
 * objects, which no `jsonStr`/`jsonBool`-style flat extractor can express.
 *
 * ```ts
 * import { HostCalls } from "./host-calls";
 * import { AgentConfig, Tool, runAgent } from "./agent";
 *
 * let host = new HostCalls();
 * let lookup = new Tool("lookup", "service");
 * lookup.service = "weather";
 * lookup.operation = "get";
 *
 * let config = new AgentConfig();
 * config.tools.push(lookup);
 *
 * let result = runAgent(host, config, "How warm is Tokyo?");
 * host.log(result.answer);
 * ```
 */

import { HostCalls } from "./host-calls";
import { JsonBuilder, JsonParser, JsonVal, TYPE_OBJECT } from "./json";

/**
 * The workflow name the agent must be DEPLOYED under, because a child is
 * resolved by name against a deployed `workflow_defs` row.
 */
export const AGENT_WORKFLOW_NAME: string = "agent";

/**
 * One tool the model may call, and how to call it.
 *
 * `name`, `description` and `parametersJson` are what the model sees; the
 * rest is dispatch, and it is deliberately opaque to the model -- the same
 * tool list reaches a durable call, a plugin or a child workflow without the
 * model being told which.
 *
 * `kind` is one of `"service"` (`service` + `operation`), `"plugin"`
 * (`plugin` + `function`), `"workflow"` (`workflow`) or `"approval"`
 * (`plugin` + `function`, polled until the claim reports found).
 */
export class Tool {
  description: string = "";
  /**
   * The JSON-schema parameters shown to the model, as already-serialized
   * JSON text (an arbitrary object the model's tool-calling API expects,
   * not a shape this SDK can type) -- empty omits the field entirely.
   */
  parametersJson: string = "";
  service: string = "";
  operation: string = "";
  plugin: string = "";
  function_: string = "";
  workflow: string = "";
  /** An `approval` tool's wait. 0 takes the workflow's own default. */
  pollIntervalSeconds: i32 = 0;
  maxPolls: i32 = 0;

  constructor(
    /** The function name the model calls. Required, unique. */
    public name: string,
    /** One of `"service"`, `"plugin"`, `"workflow"`, `"approval"`. */
    public kind: string,
  ) {}

  writeTo(b: JsonBuilder): void {
    b.startObject();
    b.addString("name", this.name);
    b.addString("kind", this.kind);
    if (this.description.length > 0) b.addString("description", this.description);
    if (this.parametersJson.length > 0) b.addRawJsonField("parameters", this.parametersJson);
    if (this.service.length > 0) b.addString("service", this.service);
    if (this.operation.length > 0) b.addString("operation", this.operation);
    if (this.plugin.length > 0) b.addString("plugin", this.plugin);
    if (this.function_.length > 0) b.addString("function", this.function_);
    if (this.workflow.length > 0) b.addString("workflow", this.workflow);
    if (this.pollIntervalSeconds != 0) b.addNumber("poll_interval_seconds", this.pollIntervalSeconds);
    if (this.maxPolls != 0) b.addNumber("max_polls", this.maxPolls);
    b.endObject();
  }
}

/**
 * The agent's configuration. Mirrors `python-sdk/cleat_sdk/agent.py`'s
 * `AgentConfig`, which itself mirrors `cleat/agentworkflow`'s `Input`.
 *
 * Carries no `message` field: the message is `runAgent`'s own argument,
 * matching `agentworkflow.RunAsChild`/`run_agent`'s own shape, so there is
 * exactly one place to look for it.
 */
export class AgentConfig {
  tools: Tool[] = [];
  systemPrompt: string = "";
  provider: string = "";
  model: string = "";
  /** Bounds tool-calling rounds. 0 takes the workflow's own default. */
  maxSteps: i32 = 0;
  /** Passed through to the provider. 0.0 means the provider's own default. */
  temperature: f64 = 0.0;

  /**
   * The run's spend ceiling in dollars, against the `cost` the llm plugin
   * reports. 0.0 (the default) means unbounded. The workflow stops before
   * starting a turn once the ceiling is REACHED, and reports
   * `status == "budget_exceeded"` in the result.
   */
  budget: f64 = 0.0;

  /** Attribution, echoed back in the result and never required by the workflow. */
  tenantId: string = "";

  /**
   * When set, the workflow writes the finished answer to the bundled
   * blobstore plugin under this key and echoes it in the result.
   */
  artifactKey: string = "";

  /**
   * What a failed tool -- or a tool the model invented -- does. `""` is the
   * workflow's default (`"inject"`: the error goes back to the model as the
   * tool's result). `"fail"` ends the run instead. This is a CONTRACT
   * difference, not a preference, so it is stated rather than assumed.
   */
  toolErrorMode: string = "";

  toJSON(message: string): string {
    let b = new JsonBuilder();
    b.startObject();
    b.addString("message", message);
    if (this.tools.length > 0) {
      b.startArray("tools");
      for (let i = 0; i < this.tools.length; i++) {
        this.tools[i].writeTo(b);
      }
      b.endArray();
    }
    if (this.systemPrompt.length > 0) b.addString("system_prompt", this.systemPrompt);
    if (this.provider.length > 0) b.addString("provider", this.provider);
    if (this.model.length > 0) b.addString("model", this.model);
    if (this.maxSteps != 0) b.addNumber("max_steps", this.maxSteps);
    if (this.temperature != 0.0) b.addNumber("temperature", this.temperature);
    if (this.budget != 0.0) b.addNumber("budget", this.budget);
    if (this.tenantId.length > 0) b.addString("tenant_id", this.tenantId);
    if (this.artifactKey.length > 0) b.addString("artifact_key", this.artifactKey);
    if (this.toolErrorMode.length > 0) b.addString("tool_error_mode", this.toolErrorMode);
    b.endObject();
    return b.build();
  }
}

/** One tool call the agent made, as reported back in `AgentResult`. */
export class ToolCallRecord {
  step: i32 = 0;
  name: string = "";
  args: string = "";
  result: string = "";
  error: string = "";

  static fromJsonVal(parser: JsonParser, v: JsonVal): ToolCallRecord {
    let r = new ToolCallRecord();
    r.step = i32(parser.getNumber(v, "step"));
    r.name = parser.getString(v, "name");
    r.args = parser.getString(v, "arguments");
    r.result = parser.getString(v, "result");
    r.error = parser.getString(v, "error");
    return r;
  }
}

/** The agent workflow's result. */
export class AgentResult {
  /**
   * `"done"` when the model answered, or `"budget_exceeded"` when the
   * spend ceiling stopped it first -- see the workflow's own
   * `StatusDone`/`StatusBudgetExceeded`.
   */
  status: string = "";
  answer: string = "";
  steps: i32 = 0;
  toolCalls: ToolCallRecord[] = [];
  model: string = "";
  totalTokens: i32 = 0;
  cost: f64 = 0.0;
  tenantId: string = "";
  artifactKey: string = "";

  static fromJSON(json: string): AgentResult {
    let parser = new JsonParser();
    let val = parser.parse(json);
    let r = new AgentResult();
    if (val === null || val.type != TYPE_OBJECT) return r;
    r.status = parser.getString(val, "status");
    r.answer = parser.getString(val, "answer");
    r.steps = i32(parser.getNumber(val, "steps"));
    let rawToolCalls = parser.getArray(val, "tool_calls");
    for (let i = 0; i < rawToolCalls.length; i++) {
      let item = rawToolCalls[i];
      if (item.type == TYPE_OBJECT) {
        r.toolCalls.push(ToolCallRecord.fromJsonVal(parser, item));
      }
    }
    r.model = parser.getString(val, "model");
    r.totalTokens = i32(parser.getNumber(val, "total_tokens"));
    r.cost = parser.getNumber(val, "cost");
    r.tenantId = parser.getString(val, "tenant_id");
    r.artifactKey = parser.getString(val, "artifact_key");
    return r;
  }
}

/**
 * Starts the shipped agent workflow as a child and awaits its result.
 *
 * THIS IS THE WHOLE PER-LANGUAGE SURFACE -- everything else (the loop, the
 * tool dispatch, the step budget, the durability) lives in the workflow,
 * which is why the same agent works from Go, Python, Rust, Java or
 * AssemblyScript without any of them implementing it. See
 * `cleat/agentworkflow`'s own `RunAsChild` doc comment, which this mirrors
 * exactly.
 *
 * The caller's workflow needs the agent deployed under
 * `AGENT_WORKFLOW_NAME`. A deployment under a different name means calling
 * `host.childWorkflow`/`host.awaitChild` directly with the marshalled config
 * -- two calls, and the reason this is a convenience rather than a
 * mechanism.
 */
export function runAgent(host: HostCalls, config: AgentConfig, message: string): AgentResult {
  let payload = config.toJSON(message);

  let started = host.childWorkflow(AGENT_WORKFLOW_NAME, payload);
  if (started.isError) {
    throw new Error("agent: start child \"" + AGENT_WORKFLOW_NAME + "\": " + (started.error as string));
  }

  let awaited = host.awaitChild(started.value);
  if (awaited.isError) {
    throw new Error("agent: await child \"" + AGENT_WORKFLOW_NAME + "\": " + (awaited.error as string));
  }

  return AgentResult.fromJSON(awaited.value);
}
