import { workflow } from "../../packages/cleat-ts/index";

// Q4 / ABI 6.6 "NaN canonicalization": a workflow that produces NaN. The Go twin (gocontrol.GoNotANumber) hands it to encoding/json.
export const nan = workflow<unknown, { v: number }>(async () => ({ v: 0 / 0 }));
