import { workflow } from "../../packages/cleat-ts/index";
import { z } from "zod";
import { v5 } from "uuid";

// Phase 5 / Q7: a thrown error must name the .ts file and line. Each entry fails somewhere different; phase5_srcmap.py finds the
// expected line by searching THIS file for the marker text, so moving code does not make the test lie.
function inner(n: number): never {
  throw new Error("boom " + n); // MARK:inner-throw
}

export const srcmapThrow = workflow(async () => {
  inner(1); // MARK:call-from-workflow
});

export const srcmapLate = workflow(async (ctx) => {
  await ctx.sleep(300);
  inner(2); // MARK:call-after-resume
});

export const srcmapZod = workflow(async () => {
  z.object({ a: z.number() }).parse({ a: "x" }); // MARK:zod-parse
});

// A plain Error thrown from INSIDE a library (uuid validates the namespace and throws a TypeError from node_modules): the stack carries a
// library frame, which zod's does not (a ZodError's stack holds only the caller's frames).
export const srcmapLib = workflow(async () => {
  v5("a", "not-a-uuid"); // MARK:lib-throw
});
