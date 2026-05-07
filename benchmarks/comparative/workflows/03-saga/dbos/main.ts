/**
 * DBOS TypeScript implementation of SagaWorkflow and
 * SagaWithCompensationWorkflow.
 *
 * Mirrors the Cleat saga implementations in benchmarks/workflows/saga.go:
 * N steps with forward and compensation actions. Two variants:
 *   - SagaWorkflow (happy path): all steps succeed, no compensation.
 *   - SagaWithCompensationWorkflow: one step fails, all previously
 *     completed steps are compensated in reverse order.
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
// Types — mirrors benchmarks/workflows/saga.go
// ---------------------------------------------------------------------------

interface SagaInput {
  steps: number;
}

interface SagaOutput {
  done: boolean;
}

interface SagaWithCompensationInput {
  steps: number;
  failAtStep: number;
}

interface SagaWithCompensationOutput {
  compensated: number;
  failed: boolean;
}

// ---------------------------------------------------------------------------
// Workflow classes
// ---------------------------------------------------------------------------

/**
 * SagaWorkflow executes N forward steps. All steps succeed, so no
 * compensation occurs. This benchmarks the overhead of the saga
 * scaffolding: step iteration, compensation registration, and
 * per-step activity dispatch.
 *
 * Equivalent to Cleat's SagaWorkflow which uses durable.NewSaga().
 */
export class SagaWorkflow {
  @DBOS.step()
  static async forwardStep(): Promise<string> {
    return JSON.stringify({ status: "forward_ok" });
  }

  @DBOS.step()
  static async compensateStep(): Promise<string> {
    return JSON.stringify({ status: "compensated" });
  }

  @DBOS.workflow()
  static async run(input: SagaInput): Promise<SagaOutput> {
    // In Cleat, each saga step registers a forward and compensation action.
    // For the happy path, only forwards are executed.
    interface SagaStep {
      name: string;
      compensate: () => Promise<string>;
    }

    const steps: SagaStep[] = [];

    for (let i = 0; i < input.steps; i++) {
      // Forward action
      await SagaWorkflow.forwardStep();

      // Register compensation (not executed in happy path, but registered
      // to match the scaffolding overhead measured by Cleat).
      steps.push({
        name: `step_${i}`,
        compensate: async () => SagaWorkflow.compensateStep(),
      });
    }

    return { done: true };
  }
}

/**
 * SagaWithCompensationWorkflow runs a saga where one step fails,
 * triggering compensation of all previously completed steps in
 * reverse order.
 *
 * Equivalent to Cleat's SagaWithCompensationWorkflow.
 */
export class SagaWithCompensationWorkflow {
  @DBOS.step()
  static async forwardStep(stepIdx: number): Promise<string> {
    return JSON.stringify({ status: "forward_ok", step: stepIdx });
  }

  @DBOS.step()
  static async compensateStep(stepIdx: number): Promise<string> {
    return JSON.stringify({ status: "compensated", step: stepIdx });
  }

  @DBOS.workflow()
  static async run(input: SagaWithCompensationInput): Promise<SagaWithCompensationOutput> {
    const compensations: Array<() => Promise<string>> = [];

    for (let i = 0; i < input.steps; i++) {
      if (i === input.failAtStep) {
        // This step fails. Compensate all previously completed steps
        // in reverse order (the classic saga compensation pattern).
        for (let j = compensations.length - 1; j >= 0; j--) {
          try {
            await compensations[j]();
          } catch {
            // best-effort compensation
          }
        }
        return { compensated: input.failAtStep, failed: true };
      }

      // Forward action
      await SagaWithCompensationWorkflow.forwardStep(i);

      // Register compensation for this step
      const stepIdx = i; // capture for closure
      compensations.push(async () => {
        await SagaWithCompensationWorkflow.compensateStep(stepIdx);
        return JSON.stringify({ status: "compensated" });
      });
    }

    return { compensated: 0, failed: false };
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

async function runBenchmark<TInput, TOutput>(
  wfFn: (input: TInput) => Promise<TOutput>,
  input: TInput,
  stepsPerWf: number,
  warmupMs: number,
  benchtimeMs: number,
  concurrency: number,
  name: string,
  configLabel: string,
): Promise<BenchmarkResult> {
  let count = 0;

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
    return { name, config: configLabel, count, elapsedMs: 1, wfPerSec: 0, stepsPerSec: 0 };
  }

  const wfPerSec = (count / elapsedMs) * 1000;
  const stepsPerSec = wfPerSec * stepsPerWf;

  return { name, config: configLabel, count, elapsedMs, wfPerSec, stepsPerSec };
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

  console.error(`Saga Benchmark`);
  console.error(`  warmup: ${warmupMs}ms`);
  console.error(`  benchtime: ${benchtimeMs}ms`);
  console.error(`  concurrency: ${concurrency}`);

  await DBOS.init();

  try {
    // ---- Happy path (SagaWorkflow) ----
    const happyCases = [10, 100, 1000];

    for (const steps of happyCases) {
      console.error(`\n========== SagaWorkflow steps=${steps} ==========`);
      const input: SagaInput = { steps };
      // steps per workflow: one forward step per iteration
      const result = await runBenchmark(
        (inp: SagaInput) => SagaWorkflow.run(inp),
        input,
        steps,
        warmupMs,
        benchtimeMs,
        concurrency,
        "SagaWorkflow",
        `steps=${steps}`,
      );
      printResult(result);
    }

    // ---- Failure path (SagaWithCompensationWorkflow) ----
    const failureCases = [
      { steps: 10, failAt: 9 },
      { steps: 100, failAt: 99 },
    ];

    for (const tc of failureCases) {
      console.error(`\n========== SagaWithCompensationWorkflow steps=${tc.steps} ==========`);
      const input: SagaWithCompensationInput = {
        steps: tc.steps,
        failAtStep: tc.failAt,
      };
      // steps per workflow: failAt forwards + failAt compensates = 2*failAt
      const stepsPerWf = 2 * tc.failAt;
      const result = await runBenchmark(
        (inp: SagaWithCompensationInput) => SagaWithCompensationWorkflow.run(inp),
        input,
        stepsPerWf,
        warmupMs,
        benchtimeMs,
        concurrency,
        "SagaWithCompensationWorkflow",
        `steps=${tc.steps}`,
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
