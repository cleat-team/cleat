/**
 * DBOS TypeScript implementation of SimpleWorkflow.
 *
 * Mirrors the Cleat SimpleWorkflow in benchmarks/workflows/simple.go:
 * N sequential @DBOS.step() calls measuring framework overhead per step.
 *
 * Usage:
 *   1. Set up the DBOS project:
 *        npx dbos init
 *        npx dbos migrate
 *
 *   2. Run:
 *        npx ts-node main.ts --warmup 10000 --benchtime 60000
 *
 *   3. Or compile and run:
 *        npx tsc && node dist/main.js --warmup 10000 --benchtime 60000
 *
 * Prerequisites:
 *   - Node.js 18+
 *   - PostgreSQL running locally
 *   - @dbos-inc/dbos-sdk installed (npm install @dbos-inc/dbos-sdk)
 *   - dbos-config.yaml in the project root
 */

import { DBOS } from "@dbos-inc/dbos-sdk";

// ---------------------------------------------------------------------------
// Types — mirrors benchmarks/workflows/simple.go
// ---------------------------------------------------------------------------

interface SimpleInput {
  steps: number;
}

interface SimpleOutput {
  done: boolean;
}

// ---------------------------------------------------------------------------
// Workflow class
// ---------------------------------------------------------------------------

export class SimpleWorkflow {
  /**
   * No-op step simulating a durable API call.
   * Equivalent to: h.DurableCall("bench", "noop", "{}")
   */
  @DBOS.step()
  static async noopStep(): Promise<string> {
    return JSON.stringify({ status: "ok" });
  }

  /**
   * SimpleWorkflow executes N sequential step calls.
   * Equivalent to the Cleat SimpleWorkflow function.
   */
  @DBOS.workflow()
  static async run(input: SimpleInput): Promise<SimpleOutput> {
    for (let i = 0; i < input.steps; i++) {
      await SimpleWorkflow.noopStep();
    }
    return { done: true };
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

/**
 * Runs a concurrent benchmark with warm-up and measurement phases.
 * Spawns concurrency workers that each execute workflows in a tight loop.
 */
async function runBenchmark(
  wfFn: (input: SimpleInput) => Promise<SimpleOutput>,
  input: SimpleInput,
  warmupMs: number,
  benchtimeMs: number,
  concurrency: number,
): Promise<BenchmarkResult> {
  let count = 0;
  const configLabel = `steps=${input.steps}`;
  const stepsPerWf = input.steps;

  // Shared counter for coordination
  let warmupDone = false;
  let measureDone = false;

  // ---- Warm-up ----
  console.error(`[warmup] running for ${warmupMs}ms with concurrency=${concurrency} ...`);
  const warmupStart = Date.now();
  const warmupEnd = warmupStart + warmupMs;

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
    return { name: "SimpleWorkflow", config: configLabel, count, elapsedMs: 1, wfPerSec: 0, stepsPerSec: 0 };
  }

  const wfPerSec = (count / elapsedMs) * 1000;
  const stepsPerSec = wfPerSec * stepsPerWf;

  return {
    name: "SimpleWorkflow",
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
  // Parse command-line arguments
  const args = process.argv.slice(2);
  const warmupMs = parseInt(parseArg(args, "--warmup", "10000"), 10);
  const benchtimeMs = parseInt(parseArg(args, "--benchtime", "60000"), 10);
  const concurrency = parseInt(parseArg(args, "--concurrency", "10"), 10);

  console.error(`SimpleWorkflow Benchmark`);
  console.error(`  warmup: ${warmupMs}ms`);
  console.error(`  benchtime: ${benchtimeMs}ms`);
  console.error(`  concurrency: ${concurrency}`);

  // Initialize DBOS runtime
  await DBOS.init();

  try {
    // Test cases matching benchmarks/cleat_bench_test.go
    const testCases = [10, 100, 1000];

    for (const steps of testCases) {
      console.error(`\n========== SimpleWorkflow steps=${steps} ==========`);
      const input: SimpleInput = { steps };

      const result = await runBenchmark(
        (inp) => SimpleWorkflow.run(inp),
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

// Only run main when this is the entry point
if (require.main === module) {
  main().catch((err) => {
    console.error("FATAL:", err);
    process.exit(1);
  });
}
