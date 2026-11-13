import { workflow } from "../../packages/cleat-ts/index";

// Q4 / ABI 6.6 "NaN canonicalization": workflows that produce NaN and +Infinity. The Go twins (gocontrol.GoNotANumber, GoInfinity)
// hand the same values to encoding/json, which refuses them. The SDK now does too; NAN_POLICY=lenient is the red control.
export const nan = workflow<unknown, { v: number }>(async () => ({ v: 0 / 0 }));
export const infinity = workflow<unknown, { v: number }>(async () => ({ v: 1 / 0 }));
