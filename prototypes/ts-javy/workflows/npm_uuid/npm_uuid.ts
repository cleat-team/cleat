import { workflow } from "../../packages/cleat-ts/index";
import { v3, v4, v5, v7, validate, version, parse, stringify, NIL } from "uuid";
import { runCase, type Cases } from "../npm_common";

const DNS = "6ba7b810-9dad-11d1-80b4-00c04fd430c8";
const bytes = Uint8Array.from({ length: 16 }, (_, i) => (i * 37 + 11) % 256);

export const CASES: Cases = {
  v5_v3: () => [v5("example.com", DNS), v3("example.com", DNS)],
  parse_stringify: () => [stringify(parse(v5("a", DNS))), validate(NIL), validate("nope"), version(v5("a", DNS)), version(NIL)],
  // Needs crypto.getRandomValues, which this runtime does not define.
  v4: () => {
    const a = v4(), b = v4();
    return validate(a) && version(a) === 4 && a !== b;
  },
  // The documented way around it: hand uuid the random bytes. Here they are fixed so the answer is comparable with Node.
  v4_with_random_option: () => v4({ random: bytes }),
  // v7 reads the clock (Date.now through the shim) and random bytes; with all three supplied it is a pure function.
  v7_with_options: () => v7({ msecs: 1_700_000_000_000, random: bytes, seq: 5 }),
  v7: () => {
    const a = v7();
    return validate(a) && version(a) === 7;
  },
  control_differs: () => typeof (globalThis as any).process,
};

export const npmUuid = workflow<{ case: string }, unknown>(async (ctx, input) => runCase(ctx, input.case, CASES));
