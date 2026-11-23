// The `schedules` group of host-call bindings (cleat#3827): `scheduleCron`, `deleteCron` and `listCrons`, recurring triggers a
// workflow creates, removes and reads at run time. None of them waits, so none is a dispatch point and none goes through
// `dispatchThen` (the update group's hook for calls that can park the workflow).

import type { Helpers } from "./helpers";

declare function __cleat_schedule_cron(workflowName: string, cronExpr: string, timezone: string, inputJson: string): string;
declare function __cleat_delete_cron(scheduleId: string): string;
declare function __cleat_list_crons(): string;

/** One schedule as `listCrons` reports it. The host's own field names are snake_case; these are the same fields in this API's case. */
export interface CronSchedule {
  /** The id `scheduleCron` returned, which `deleteCron` takes. */
  scheduleId: string;
  /** The workflow the schedule starts. */
  workflowName: string;
  cronExpr: string;
  /** The zone the expression is read in; the engine's default when none was given. */
  timezone: string;
  /** The input each run is started with, parsed as JSON when it is JSON. */
  input: unknown;
  /** False once the schedule has been disabled (through the HTTP API); a disabled schedule is still listed. */
  enabled: boolean;
}

export interface ScheduleCronOptions {
  /** An IANA zone name such as `"Europe/Berlin"`. Omitted means the engine's default (UTC). */
  timezone?: string;
  /** The input each started run receives, JSON-encoded. Omitted means `{}`. */
  input?: unknown;
}

export interface SchedulesCtx {
  /**
   * Register a recurring trigger: from now on the worker starts `workflowName` whenever `cronExpr` (five fields, read in
   * `options.timezone`) says so. Resolves with the schedule's id.
   *
   * THIS OUTLIVES THE RUN. The schedule belongs to the tenant, not to the workflow that created it, and keeps firing after that
   * workflow finishes, so a workflow that does not mean to leave one behind must `deleteCron` it. The id is derived from the call
   * site (the run and the step), so a retry of the same step gets the same schedule back and not a second one.
   *
   * Rejects with a `CleatError` carrying the host's own reason: an invalid expression, an unknown zone, input that is not JSON,
   * an empty workflow name, a schedule quota, or "no workflow store configured". Those argument checks come before the store is
   * consulted, so a bad expression is reported as one wherever the workflow runs.
   *
   * On replay the recorded id is returned and nothing is created again; a replay whose arguments differ from the recorded call (any
   * of the four, including the zone) is a divergence and rejects. Refused in a defer phase, which exists to clean up and not to
   * start work.
   */
  scheduleCron(workflowName: string, cronExpr: string, options?: ScheduleCronOptions): Promise<string>;
  /**
   * Remove the schedule `scheduleId`. Resolves whether or not it existed: a retry of a delete that already happened is a success
   * and not an error (the engine says so deliberately, because workflow execution is at-least-once).
   *
   * A delete that FAILS (the store errored, or there is none) is rejected with only a numeric code and a hint to check the id: the
   * host writes no reason for this call, as it does for `scheduleCron` and `listCrons`, and the id is the one thing that is not wrong.
   */
  deleteCron(scheduleId: string): Promise<void>;
  /**
   * Every schedule in the tenant, ordered by id: not only the ones this workflow created. The answer is recorded, so a replay
   * returns what the run saw and not what is there now. The host returns the whole list in one 64 KiB buffer, so a tenant with
   * enough schedules to overflow it gets a rejection and not a truncated list.
   */
  listCrons(): Promise<CronSchedule[]>;
}

interface WireCron {
  schedule_id: string;
  workflow_name: string;
  cron_expr: string;
  timezone: string;
  input: string;
  enabled: boolean;
}

export function makeSchedules(h: Helpers): SchedulesCtx {
  return {
    // `async` so that an input that cannot be JSON (NaN, an infinity) is a REJECTION like every other failure of this call, and not a
    // synchronous throw that `scheduleCron(...).catch(...)` would never see. The encoding happens inside the promise.
    scheduleCron: async (workflowName, cronExpr, options) =>
      h.settle<string>(
        __cleat_schedule_cron(workflowName, cronExpr, options?.timezone ?? "", options?.input === undefined ? "" : h.encode(options.input)),
      ),
    deleteCron: (scheduleId) => h.settle<null>(__cleat_delete_cron(scheduleId)).then(() => undefined),
    listCrons: () =>
      h.settle<string>(__cleat_list_crons()).then((text) =>
        (JSON.parse(text) as WireCron[]).map((c) => ({
          scheduleId: c.schedule_id,
          workflowName: c.workflow_name,
          cronExpr: c.cron_expr,
          timezone: c.timezone,
          input: h.parseJSON(c.input),
          enabled: c.enabled,
        })),
      ),
  };
}
