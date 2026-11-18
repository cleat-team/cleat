import { workflow } from "@cleat/ts";

// cleat#3922: the soft kill must charge the guest only for its OWN execution. The instance timeout is
// guest time, and the host re-arms the epoch deadline around every host call; an arm that measured
// wall clock instead would kill this workflow at its first poll, because the service call alone
// would look most of the budget.
export const slowThenSpin = workflow(async (ctx) => {
  await ctx.call("payments", "very-slow", {});
  for (let i = 0; ; i++) {
    if (i < 0) break;
  }
  return { ok: true };
});
