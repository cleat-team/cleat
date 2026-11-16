import { workflow } from "@cleat/ts";

// cleat#3828: a workflow whose RESULT is the answer to a durable call.
//
// It exists so the harness's scripted-answer path has something that actually depends on the answer. A fixture that
// called and discarded the result would pass with any script, including none, which is the shape this test is for.
export const answer = workflow(async (ctx) => {
  const charged = await ctx.call("payments", "charge", { amount: 10 });
  return { charged };
});
