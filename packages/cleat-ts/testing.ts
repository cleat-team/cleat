// cleat-ts/testing: run a BUILT TypeScript workflow module in a test (cleat#3828).
//
// The constraint comes from the issue and it decides the whole design: a test must run the module that gets DEPLOYED,
// not a reimplementation of the host calls. So this is a thin wrapper over `cleat-ts-run`, the Go binary that loads a
// .wasm built by packages/cleat-ts/build.sh and executes it in the same engine a worker uses. Nothing about the host is
// re-implemented in JavaScript, which is the only way a green test can be a claim about the deployed module.
//
// WHAT KEEPS THIS HONEST. The module is the built artefact, so it cannot drift. The STORES can: `cleat-ts-run` gives the
// engine stubbed answers, and where a stub cannot match the shipped store the difference has to be recorded the way
// packages/cleat-ts/DIVERGENCES.md records a language difference. Durable CALLS are scripted by the test through
// `options.answers`; a call the script does not name is REFUSED rather than answered with `{}`, and a run with no script
// at all says so in `stubbedCalls` (cleat#3828).
//
// UNMEASURED is the third outcome and it is not a failure of the workflow: a missing binary or an unreadable module
// throws an `Unmeasured` error so a broken harness cannot read as a passing test.

import { execFileSync } from "node:child_process";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

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
  /**
   * True when the run supplied no scripted answers, so every durable call returned ``{}`` unread. A test that cares
   * whether the calls were exercised should assert this is absent (cleat#3828).
   */
  stubbedCalls?: boolean;
  /**
   * The promise names the workflow CREATED, in order, as the store saw them. Deliberately not derived from the
   * workflow's own result: a test asserting only on `result` can pass without the store ever being touched, which is
   * the same hole `stubbedCalls` closes for calls (cleat#3828).
   */
  promises?: string[];
  /** The signal names the workflow CONSUMED, in order, as the store saw them. */
  signalsConsumed?: string[];
}

/** What a durable call returns, keyed by `"<service>/<operation>"`. A value is the JSON text the guest receives. */
export type HostAnswers = Record<string, string>;

export interface RunOptions {
  /**
   * Scripted answers for durable calls.
   *
   * WITHOUT THIS, EVERY CALL RETURNS ``{}`` AND NOBODY SEES IT. With it, a call the script does not name refuses with
   * an error that quotes the missing key, because a workflow passing on an empty object it never chose is the failure
   * this option exists to prevent.
   */
  answers?: HostAnswers;

  /**
   * Promise outlets, keyed by the NAME the workflow creates the promise under. A promise whose name appears here is
   * settled with that value at the moment it is created, standing in for the other run that would have settled it.
   *
   * WITHOUT THIS, EVERY PROMISE STAYS PENDING and a workflow awaiting one SUSPENDS -- it does not fail, and it does
   * not quietly get `{}`. That is the property a test relies on in both directions: script the name to let the workflow
   * finish, leave it out to assert that it parks.
   */
  promises?: Record<string, unknown>;

  /** Promise outlets that REJECT, keyed the same way. A name in both this and `promises` is a broken fixture and fails. */
  promiseRejects?: Record<string, string>;

  /**
   * Signals delivered BEFORE the run starts, oldest first per name, standing in for a caller that signalled first.
   *
   * There is no "deliver a signal later" here: this process runs one segment and exits, so a signal a workflow waits
   * for after the queue is empty suspends. Delivering mid-run is resume support, which is a different change.
   */
  signals?: Record<string, unknown[]>;
}

/** A failure of the HARNESS, not of the workflow under test. Never assert on this as if it were a result. */
export class Unmeasured extends Error {}

/** Serialise each value once: the runner takes JSON text per key, and a caller should not have to do this by hand. */
function stringifyValues(values: Record<string, unknown>): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [key, value] of Object.entries(values)) out[key] = JSON.stringify(value);
  return out;
}

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
export function runWorkflow(modulePath: string, entry: string, input: unknown = {}, options: RunOptions = {}): WorkflowRun {
  const args = ["-module", modulePath, "-entry", entry, "-input", JSON.stringify(input)];
  // Temp files rather than flag values: each script is JSON, and quoting arbitrary JSON through argv on every platform
  // is a class of bug this does not need to have.
  //
  // Every directory is removed in the `finally` below. A test file that runs a dozen runs would otherwise leave a dozen
  // directories in the temp area per execution, and on a CI runner that accumulates across a matrix.
  const scripts: string[] = [];
  const writeScript = (name: string, value: unknown, flag: string): void => {
    const dir = mkdtempSync(join(tmpdir(), `cleat-ts-${name}-`));
    scripts.push(dir);
    const file = join(dir, `${name}.json`);
    // Values are JSON TEXT on the Go side, so each is stringified HERE rather than there: the harness is JavaScript, and
    // a caller passing an object should not have to serialise it by hand for one argument and not the others.
    writeFileSync(file, JSON.stringify(value), "utf8");
    args.push(flag, file);
  };
  if (options.answers) {
    writeScript("answers", options.answers, "-answers");
  }
  if (options.promises) {
    writeScript("promises", stringifyValues(options.promises), "-promises");
  }
  if (options.promiseRejects) {
    writeScript("promise-rejects", options.promiseRejects, "-promise-rejects");
  }
  if (options.signals) {
    writeScript("signals", options.signals, "-signals");
  }
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
  } finally {
    // Every path out of the block above, including the throws for Unmeasured: the scripts have been handed to the
    // runner by then, and nothing needs them afterwards.
    for (const dir of scripts) rmSync(dir, { recursive: true, force: true });
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
export function runWorkflowOrFail(modulePath: string, entry: string, input: unknown = {}, options: RunOptions = {}): WorkflowRun {
  const run = runWorkflow(modulePath, entry, input, options);
  if (!run.ok) throw new Error(`the workflow failed: ${run.error ?? "(no error text)"}`);
  return run;
}
