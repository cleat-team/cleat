/**
 * DBOS TypeScript implementation of LLMWorkflow.
 *
 * Mirrors the Cleat LLMWorkflow in benchmarks/workflows/llm.go:
 * N prompts, each with M tool invocations. Simulates an AI agent loop
 * where each prompt involves one LLM chat call followed by multiple
 * tool invocations.
 *
 * Usage:
 *   1. Set up:
 *        npx dbos init && npx dbos migrate
 *   2. Run:
 *        npx ts-node main.ts --warmup 10000 --benchtime 60000
 *
 * Prerequisites:
 *   - Node.js 18+, PostgreSQL, @dbos-inc/dbos-sdk installed
 *   - dbos-config.yaml in the project root
 */

import { DBOS } from "@dbos-inc/dbos-sdk";

// ---------------------------------------------------------------------------
// Types — mirrors benchmarks/workflows/llm.go
// ---------------------------------------------------------------------------

interface LLMInput {
  prompts: number;
  toolsPerPrompt: number;
}

interface LLMOutput {
  totalCalls: number;
}

// ---------------------------------------------------------------------------
// Workflow class
// ---------------------------------------------------------------------------

export class LLMWorkflow {
  /**
   * Simulates an LLM chat call (e.g., GPT-4, Claude).
   * Equivalent to: h.DurableCall("llm", "chat", ...)
   */
  @DBOS.step()
  static async llmChat(prompt: string): Promise<string> {
    return JSON.stringify({
      response: "simulated_response",
      prompt,
    });
  }

  /**
   * Simulates a tool invocation triggered by an LLM response.
   * Equivalent to: h.DurableCall("tools", "invoke", ...)
   */
  @DBOS.step()
  static async toolInvoke(toolName: string, iteration: number): Promise<string> {
    return JSON.stringify({
      result: "ok",
      tool: toolName,
      iteration,
    });
  }

  /**
   * LLMWorkflow simulates an AI agent loop with LLM calls and tool
   * invocations. Each "prompt" involves one LLM chat step followed
   * by multiple tool invocation steps.
   *
   * Mirrors the Cleat LLMWorkflow:
   *   for i := 0; i < Prompts; i++ {
   *       h.DurableCall("llm", "chat", ...)
   *       for j := 0; j < ToolsPerPrompt; j++ {
   *           h.DurableCall("tools", "invoke", ...)
   *       }
   *   }
   */
  @DBOS.workflow()
  static async run(input: LLMInput): Promise<LLMOutput> {
    let totalCalls = 0;

    for (let i = 0; i < input.prompts; i++) {
      // LLM chat call
      const prompt = `benchmark_prompt_${i}`;
      await LLMWorkflow.llmChat(prompt);
      totalCalls++;

      // Tool invocations from the LLM response
      for (let j = 0; j < input.toolsPerPrompt; j++) {
        const toolName = `bench_tool_${j}`;
        await LLMWorkflow.toolInvoke(toolName, i);
        totalCalls++;
      }
    }

    return { totalCalls };
  }
}

// ---------------------------------------------------------------------------
// Benchmark runner
// ---------------------------------------------------------------------------

interface BenchmarkResult {
  name: string;
  config: string;
  count: number;
  elapsedMs: number;
  wfPerSec: number;
  stepsPerSec: number;
}

async function runBenchmark(
  wfFn: (input: LLMInput) => Promise<LLMOutput>,
  input: LLMInput,
  warmupMs: number,
  benchtimeMs: number,
  concurrency: number,
): Promise<BenchmarkResult> {
  let count = 0;
  const configLabel = `prompts=${input.prompts}_tools=${input.toolsPerPrompt}`;
  // steps per workflow: prompts * (1 chat + tools) = total step calls
  const stepsPerWf = input.prompts * (1 + input.toolsPerPrompt);

  // ---- Warm-up ----
  console.error(`[warmup] running for ${warmupMs}ms with concurrency=${concurrency} ...`);
  const warmupEnd = Date.now() + warmupMs;

  const warmupWorkers = Array.from({ length: concurrency }, async () => {
    while (Date.now() < warmupEnd) {
      try {
        await wfFn(input);
      } catch {
        // ignore warm-up errors
      }
    }
  });
  await Promise.all(warmupWorkers);
  console.error("[warmup] done");

  // ---- Measurement ----
  count = 0;
  console.error(`[measure] running for ${benchtimeMs}ms with concurrency=${concurrency} ...`);
  const measureStart = Date.now();
  const measureEnd = measureStart + benchtimeMs;

  const measureWorkers = Array.from({ length: concurrency }, async () => {
    while (Date.now() < measureEnd) {
      try {
        await wfFn(input);
        count++;
      } catch {
        // ignore measurement errors
      }
    }
  });
  await Promise.all(measureWorkers);

  const elapsedMs = Date.now() - measureStart;
  if (elapsedMs <= 0) {
    return { name: "LLMWorkflow", config: configLabel, count, elapsedMs: 1, wfPerSec: 0, stepsPerSec: 0 };
  }

  const wfPerSec = (count / elapsedMs) * 1000;
  const stepsPerSec = wfPerSec * stepsPerWf;

  return {
    name: "LLMWorkflow",
    config: configLabel,
    count,
    elapsedMs,
    wfPerSec,
    stepsPerSec,
  };
}

function printResult(r: BenchmarkResult): void {
  console.log(`\nBenchmark${r.name}/config=${r.config}  count=${r.count}  ${(r.elapsedMs / r.count * 1e6).toFixed(0)} ns/wf  ${r.wfPerSec.toFixed(2)} wf/s  ${r.stepsPerSec.toFixed(2)} steps/s`);
  console.log(`BENCHMARK_RESULT  name=${r.name}  config=${r.config}  count=${r.count}  elapsed=${(r.elapsedMs / 1000).toFixed(3)}s  wf_per_sec=${r.wfPerSec.toFixed(2)}  steps_per_sec=${r.stepsPerSec.toFixed(2)}`);
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

async function main(): Promise<void> {
  const args = process.argv.slice(2);
  const warmupMs = parseInt(parseArg(args, "--warmup", "10000"), 10);
  const benchtimeMs = parseInt(parseArg(args, "--benchtime", "60000"), 10);
  const concurrency = parseInt(parseArg(args, "--concurrency", "10"), 10);

  console.error(`LLMWorkflow Benchmark`);
  console.error(`  warmup: ${warmupMs}ms`);
  console.error(`  benchtime: ${benchtimeMs}ms`);
  console.error(`  concurrency: ${concurrency}`);

  await DBOS.init();

  try {
    // Test cases matching benchmarks/cleat_bench_test.go
    const testCases = [
      { prompts: 1, tools: 5, label: "prompts=1_tools=5" },
      { prompts: 5, tools: 3, label: "prompts=5_tools=3" },
      { prompts: 10, tools: 2, label: "prompts=10_tools=2" },
      { prompts: 50, tools: 1, label: "prompts=50_tools=1" },
    ];

    for (const tc of testCases) {
      console.error(`\n========== LLMWorkflow ${tc.label} ==========`);
      const input: LLMInput = {
        prompts: tc.prompts,
        toolsPerPrompt: tc.tools,
      };

      const result = await runBenchmark(
        (inp: LLMInput) => LLMWorkflow.run(inp),
        input,
        warmupMs,
        benchtimeMs,
        concurrency,
      );
      printResult(result);
    }
  } finally {
    await DBOS.destroy();
  }
}

function parseArg(args: string[], name: string, defaultValue: string): string {
  const idx = args.indexOf(name);
  if (idx >= 0 && idx + 1 < args.length) {
    return args[idx + 1];
  }
  return defaultValue;
}

if (require.main === module) {
  main().catch((err) => {
    console.error("FATAL:", err);
    process.exit(1);
  });
}
