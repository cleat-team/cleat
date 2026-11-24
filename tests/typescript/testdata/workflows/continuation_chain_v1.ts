import { workflow } from "@cleat/ts";

// cleat#4041. The same entry in two files, deployed as two versions of one definition: the bodies differ only in `via`, so each
// run says which version's code it ran under. A run on n=0 continues as new, asking for `version` when it is given; the run that
// follows reports where it landed.
export const chain = workflow<{ n: number; version?: number }, { n: number; via: string }>(async (ctx, input) => {
  if (input.n === 0) {
    await ctx.continueAsNew({ n: 1 }, input.version === undefined ? undefined : { version: input.version });
  }
  return { n: input.n, via: "v1" };
});
