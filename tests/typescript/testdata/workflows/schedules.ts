import { workflow } from "@cleat/ts";

// cleat#3827, group F.

// The argument checks the engine makes BEFORE it consults a store, each caught so one run reports all of them. They fail the same
// way wherever the workflow runs, so they are the part of this group the in-process harness can show without a database.
export const schedulesBadArgs = workflow<Record<string, never>, Record<string, string>>(async (ctx) => {
  const messageOf = async (p: Promise<unknown>): Promise<string> => {
    try {
      await p;
      return "";
    } catch (e) {
      return (e as Error).message;
    }
  };
  return {
    badExpr: await messageOf(ctx.scheduleCron("wf", "not a cron")),
    badZone: await messageOf(ctx.scheduleCron("wf", "0 0 1 1 *", { timezone: "Mars/Olympus_Mons" })),
    noName: await messageOf(ctx.scheduleCron("", "0 0 1 1 *")),
    // NaN cannot be JSON: refused in the guest before it reaches the host, as Go's encoding/json refuses it.
    badInput: await messageOf(ctx.scheduleCron("wf", "0 0 1 1 *", { input: { n: NaN } })),
  };
});

// A VALID call, with no store behind it: the engine answers "no workflow store configured" for all three calls.
export const schedulesNoStore = workflow<Record<string, never>, Record<string, string>>(async (ctx) => {
  const messageOf = async (p: Promise<unknown>): Promise<string> => {
    try {
      await p;
      return "";
    } catch (e) {
      return (e as Error).message;
    }
  };
  return {
    create: await messageOf(ctx.scheduleCron("wf", "0 0 1 1 *")),
    remove: await messageOf(ctx.deleteCron("sched-1")),
    list: await messageOf(ctx.listCrons()),
  };
});

// Create a schedule, read it back, delete it, confirm it is gone, delete it AGAIN (a retry's success), and reject a bad expression.
// `target` names a workflow that is never deployed, so a schedule that outlived a failed test would fire at nothing.
export const schedulesRoundTrip = workflow<{ target: string }, Record<string, unknown>>(async (ctx, input) => {
  const id = await ctx.scheduleCron(input.target, "0 0 1 1 *", { timezone: "Europe/Berlin", input: { n: 1, tag: "x" } });
  const mine = (list: { scheduleId: string }[]) => list.filter((c) => c.scheduleId === id);
  const listed = mine(await ctx.listCrons());
  await ctx.deleteCron(id);
  const afterDelete = mine(await ctx.listCrons());
  await ctx.deleteCron(id); // already gone: must resolve
  let badExpr = "";
  try {
    await ctx.scheduleCron(input.target, "not a cron");
  } catch (e) {
    badExpr = (e as Error).message;
  }
  return { id, listed, afterDeleteCount: afterDelete.length, badExpr };
});

// The three calls AFTER the call that suspended the segment, none awaited. Each records an event when it reaches the host, and a
// segment must record nothing past the call that suspended it.
export const schedulesLate = workflow(async (ctx) => {
  const p = ctx.sleep(60_000);
  ctx.scheduleCron("wf", "0 0 1 1 *");
  ctx.deleteCron("sched-1");
  ctx.listCrons();
  await p;
  return null;
});
