import { workflow } from "@cleat/ts";

// Child workflows through the test harness's store (cleat#3828).
//
// Two exports, because one workflow cannot both succeed and observe a failure: `childStore` takes a child's result, and
// `childStoreFails` takes a child's FAILURE. The second is the case a harness that could only script success would
// leave untestable by construction, and it is the parent's error path rather than the child's that is being tested here
// -- what the parent does with a rejection is the parent's decision.
//
// Nothing here settles a child. That is the store's job: the script says what a child NAMED `charge` returns, standing
// in for a child workflow this process never runs.

export const childStore = workflow(async (ctx) => {
  const charged = await ctx.child<{ id: string }>("charge", { amount: 42 });
  return { charged };
});

export const childStoreFails = workflow(async (ctx) => {
  // `child` rejects with the child's own error text, so the catch is the assertion surface: the test scripts the
  // message and reads it back out of the workflow's result.
  let failure = "";
  try {
    await ctx.child("charge", { amount: 42 });
  } catch (e) {
    failure = (e as Error).message;
  }
  return { failure };
});
