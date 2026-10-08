/**
 * The AssemblyScript half of cleat#2978's acceptance test: a workflow that
 * runs the shipped agent as a CHILD, handed the same config the Go, Python
 * and Rust clients are handed.
 *
 * THE CONFIG IS INPUT, NOT CODE, and that is deliberate -- the acceptance is
 * that each client runs the SAME agent config through the shipped agent
 * workflow, so the config has to come from the run's own input, not a
 * per-client literal that could silently drift from the others.
 *
 * This file is also the AssemblyScript SDK's surface under test:
 * `runAgent` (from `@cleat/sdk`) does the start-and-await, so the fixture
 * measures that call rather than hand-rolling the two host calls it is made
 * of.
 *
 * WHY A LONE STRING PARAMETER, NOT A TYPED ONE. The `@cleat/transform`
 * plugin's `@cleatEntry` only supports primitive parameter types (string,
 * i32, u32, i64, u64, f64, f32, bool) -- a custom class parameter is a
 * compile-time error from the transformer, not a runtime one. A single
 * defaultless string parameter is the one case that receives the WHOLE
 * input payload verbatim rather than a value looked up by name (matching
 * the Go client's shape, examples/as-workflow/assembly/index.ts's own
 * `place_order(h, input: string)`), so this parses the config itself with
 * `JsonParser` rather than declaring fields the transform cannot bind.
 */

import {
  HostCalls,
  cleatEntry,
  AgentConfig,
  AgentResult,
  Tool,
  runAgent,
} from "@cleat/sdk";
import { JsonParser, JsonVal, TYPE_OBJECT, TYPE_ARRAY, serializeVal } from "@cleat/sdk";

/** The value at `key` in `obj`, or null if absent -- `getString`/`getNumber`/
 * `getArray`/`getBool` collapse "absent" into a type's zero value, which is
 * not good enough for `parameters`: an absent object and an object that is
 * `{}` are different inputs to re-serialize. */
function getVal(obj: JsonVal, key: string): JsonVal | null {
  for (let i = 0; i < obj.objKeys.length; i++) {
    if (obj.objKeys[i] == key) return obj.objValues[i];
  }
  return null;
}

function parseTool(parser: JsonParser, v: JsonVal): Tool {
  let name = parser.getString(v, "name");
  let kind = parser.getString(v, "kind");
  let tool = new Tool(name, kind);
  tool.description = parser.getString(v, "description");
  tool.service = parser.getString(v, "service");
  tool.operation = parser.getString(v, "operation");
  tool.plugin = parser.getString(v, "plugin");
  tool.function_ = parser.getString(v, "function");
  tool.workflow = parser.getString(v, "workflow");
  tool.pollIntervalSeconds = i32(parser.getNumber(v, "poll_interval_seconds"));
  tool.maxPolls = i32(parser.getNumber(v, "max_polls"));

  let params = getVal(v, "parameters");
  if (params !== null) {
    tool.parametersJson = serializeVal(params);
  }
  return tool;
}

@cleatEntry("RunAgentClient")
export function run_agent_client(h: HostCalls, input: string): string {
  let parser = new JsonParser();
  let val = parser.parse(input);
  if (val === null || val.type != TYPE_OBJECT) {
    return '{"error":"agentclientas: invalid input JSON"}';
  }

  let message = parser.getString(val, "message");

  let config = new AgentConfig();
  config.systemPrompt = parser.getString(val, "system_prompt");
  config.provider = parser.getString(val, "provider");
  config.model = parser.getString(val, "model");
  config.maxSteps = i32(parser.getNumber(val, "max_steps"));
  config.temperature = parser.getNumber(val, "temperature");

  let rawTools = getVal(val, "tools");
  if (rawTools !== null && rawTools.type == TYPE_ARRAY) {
    for (let i = 0; i < rawTools.arrItems.length; i++) {
      let item = rawTools.arrItems[i];
      if (item.type == TYPE_OBJECT) {
        config.tools.push(parseTool(parser, item));
      }
    }
  }

  let result: AgentResult = runAgent(h, config, message);
  return result.toJSON();
}
