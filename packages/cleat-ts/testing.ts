// cleat-ts/testing: run a BUILT TypeScript workflow module in a test (cleat#3828).
//
// The constraint comes from the issue and it decides the whole design: a test must run the module that gets DEPLOYED,
// not a reimplementation of the host calls. So this is a thin wrapper over `cleat-ts-run`, the Go binary that loads a
// .wasm built by packages/cleat-ts/build.sh and executes it in the same engine a worker uses. Nothing about the host is
// re-implemented in JavaScript, which is the only way a green test can be a claim about the deployed module.
//
// WHAT KEEPS THIS HONEST. The module is the built artefact, so it cannot drift. The STORES can: `cleat-ts-run` gives the
// engine stubbed answers, and where a stub cannot match the shipped store the difference has to be recorded the way
// packages/cleat-ts/DIVERGENCES.md records a language difference. Today that means durable CALLS answer `{}` and nothing
// else is stubbed, so a workflow that needs a scripted answer needs the next increment -- not a workaround here.
//
// UNMEASURED is the third outcome and it is not a failure of the workflow: a missing binary or an unreadable module
// throws an `Unmeasured` error so a broken harness cannot read as a passing test.

import { execFileSync } from "node:child_process";

export interface WorkflowEvent {
  step: number;
  type: string;
  service?: string;
  operation?: string;
  stateKey?: string;
  error?: string;
}

export interface WorkflowRun {
  /** Whether the workflow itself succeeded. A workflow that threw is `ok: false` with `error` set. */
  ok: boolean;
  entry: string;
  /** The guest's result, parsed from the JSON text it returned. Absent when the run suspended. */
  result?: unknown;
  /** The result exactly as the guest produced it, for a test that cares about byte form. */
  resultText?: string;
  suspended: boolean;
  suspendReason?: string;
  queryState?: Record<string, string>;
  events: WorkflowEvent[];
  error?: string;
}

/** A failure of the HARNESS, not of the workflow under test. Never assert on this as if it were a result. */
export class Unmeasured extends Error {}

function runnerPath(): string {
  const fromEnv = process.env.CLEAT_TS_RUN;
  if (fromEnv && fromEnv !== "") return fromEnv;
  return "cleat-ts-run"; // PATH; the tier-2 job and the package's own test put it there
}

/**
 * Run `entry` from a built module with `input`, and return what happened.
 *
 * Throws `Unmeasured` when the runner could not look (exit 2, or no binary): that is the harness failing, and a test
 * that treated it as a workflow failure would be reporting the wrong cause.
 */
export function runWorkflow(modulePath: string, entry: string, input: unknown = {}): WorkflowRun {
  const args = ["-module", modulePath, "-entry", entry, "-input", JSON.stringify(input)];
  let stdout: string;
  try {
    stdout = execFileSync(runnerPath(), args, { encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] });
  } catch (e) {
    const err = e as { status?: number; stdout?: string; stderr?: string; message?: string };
    if (err.status === 2) {
      throw new Unmeasured(`cleat-ts-run could not run the module: ${(err.stderr ?? "").trim() || err.message}`);
    }
    if (typeof err.status !== "number") {
      // No process at all: the binary was not found. That is the harness, not the workflow.
      throw new Unmeasured(`could not execute cleat-ts-run (${err.message}); set CLEAT_TS_RUN or put it on PATH`);
    }
    if (err.status !== 1) {
      // Only 1 means the WORKFLOW failed. Anything else -- a signal, a crash, an exit code the runner does not document
      // -- is the harness failing, and reporting it as a workflow failure would send the reader to the wrong file.
      throw new Unmeasured(`cleat-ts-run exited ${err.status}, which is not one of its documented outcomes (0 ran, 1 workflow failed, 2 unmeasured)`);
    }
    // Exit 1 is the workflow failing, and the envelope on stdout says how.
    stdout = err.stdout ?? "";
  }
  let raw: WorkflowRun;
  try {
    raw = JSON.parse(stdout) as WorkflowRun;
  } catch {
    throw new Unmeasured(`cleat-ts-run printed something that is not the JSON envelope: ${stdout.slice(0, 300)}`);
  }
  if (raw.result !== undefined) {
    raw.resultText = raw.result as unknown as string;
    try {
      raw.result = JSON.parse(raw.result as unknown as string);
    } catch {
      // A result that is not JSON text stays as the string the guest returned.
    }
  }
  return raw;
}

/**
 * `runWorkflow` plus the assertion that it ran at all: returns the run when `ok`, throws with the workflow's own error
 * when not, and never swallows an Unmeasured. A test that wants to assert a FAILING workflow calls `runWorkflow`.
 */
export function runWorkflowOrFail(modulePath: string, entry: string, input: unknown = {}): WorkflowRun {
  const run = runWorkflow(modulePath, entry, input);
  if (!run.ok) throw new Error(`the workflow failed: ${run.error ?? "(no error text)"}`);
  return run;
}
