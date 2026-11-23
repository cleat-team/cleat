import { workflow } from "@cleat/ts";

// Deliberately broken (cleat#3828): the harness must FAIL this workflow, or a harness that never runs anything would
// pass every test that uses it. Its own name carries the intent so nobody "fixes" it.
export const broken = workflow(async (_ctx) => {
  throw new Error("this workflow is broken on purpose");
});
