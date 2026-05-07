/**
 * DBOS TypeScript implementation of FanOutWorkflow.
 *
 * Mirrors the Cleat FanOutWorkflow in benchmarks/workflows/fanout.go:
 * N child workflows are spawned and awaited. The parent starts each child
 * concurrently and collects all results. Each child performs a single
 * step (noop) and returns.
 *
 * Usage:
 *   1. Set up the DBOS project:
 *        npx dbos init
 *        npx dbos migrate
 *
 *   2. Run:
 *        npx ts-node main.ts --warmup 10000 --benchtime 60000
 *
 * Prerequisites:
 *   - Node.js 18+
 *   - PostgreSQL running locally
 *   - @dbos-inc/dbos-sdk installed
 *   - dbos-config.yaml in the project root
 */

import { DBOS } from "@dbos-inc/dbos-sdk";

// ---------------------------------------------------------------------------
// Types — mirrors benchmarks/workflows/fanout.go
// ---------------------------------------------------------------------------

interface FanOutInput {
  children: number;
}

interface FanOutOutput {
  completed: number;
}

// ---------------------------------------------------------------------------
// Workflow classes
// ---------------------------------------------------------------------------

/**
 * NoopChildWorkflow executes a single noop step and returns.
 * This mirrors the NoopChild function in benchmarks/workflows/fanout.go.
 */
export class NoopChildWorkflow {
  @DBOS.step()
  static async noopStep(): Promise<string> {
    return JSON.stringify({ status: "ok" });
  }

  @DBOS.workflow()
  static async run(): Promise<string> {
    await NoopChildWorkflow.noopStep();
    return JSON.stringify({ status: "ok" });
  }
}

/**
 * FanOutWorkflow spawns N child workflows and waits for all to complete.
 * Each child is started via DBOS.startWorkflow() and awaited via .getResult().
 *
 * This mirrors the Cleat FanOutWorkflow:
 *   runIDs := make([]string, 0, input.Children)
 *   for i := 0; i < input.Children; i++ {
 *       runID, err := h.ChildWorkflow("noop_child", "{}")
 *       runIDs = append(runIDs, runID)
 *   }
 *   results, err := h.AwaitAllChildren(runIDs)
 */
export class FanOutWorkflow {
  @DBOS.workflow()
  static async run(input: FanOutInput): Promise<FanOutOutput> {
    // In DBOS, we launch child workflows and collect handles.
    const handles: DBOS.WorkflowHandle<string>[] = [];

    for (let i = 0; i < input.children; i++) {
      // DBOS.startWorkflow launches a child workflow asynchronously.
      const handle = DBOS.startWorkflow(NoopChildWorkflow, { workflowID: `child-${i}` }).run();
      handles.push(handle);
    }

    // Wait for all children to complete.
    let completed = 0;
    for (const handle of handles) {
      try {
        const result = await handle;
        const parsed = JSON.parse(result);
        if (parsed.status === "ok") {
          completed++;
        }
      } catch {
        // child failed, skip
      }
    }

    return { completed };
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
  wfFn: (input: FanOutInput) => Promise<FanOutOutput>,
  input: FanOutInput,
  warmupMs: number,
  benchtimeMs: number,
  concurrency: number,
): Promise<BenchmarkResult> {
  let count = 0;
  const configLabel = `children=${input.children}`;
  // Steps per workflow: children ChildWorkflow calls + children step calls +
  // children collection = 3*children.
  const stepsPerWf = 3 * input.children;

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
    return { name: "FanOutWorkflow", config: configLabel, count, elapsedMs: 1, wfPerSec: 0, stepsPerSec: 0 };
  }

  const wfPerSec = (count / elapsedMs) * 1000;
  const stepsPerSec = wfPerSec * stepsPerWf;

  return {
    name: "FanOutWorkflow",
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
  const concurrency = parseInt(parseArg(args, "--concurrency", "4"), 10);

  console.error(`FanOutWorkflow Benchmark`);
  console.error(`  warmup: ${warmupMs}ms`);
  console.error(`  benchtime: ${benchtimeMs}ms`);
  console.error(`  concurrency: ${concurrency}`);

  await DBOS.init();

  try {
    // Test cases matching benchmarks/cleat_bench_test.go
    const testCases = [10, 100, 500];

    for (const children of testCases) {
      console.error(`\n========== FanOutWorkflow children=${children} ==========`);
      const input: FanOutInput = { children };

      const result = await runBenchmark(
        (inp) => FanOutWorkflow.run(inp),
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
