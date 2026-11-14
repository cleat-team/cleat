// Date setters, compared with Node (cleat#3829). The guest's engine (QuickJS) answers NaN to a Date setter given a non-finite
// argument but LEAVES THE DATE UNCHANGED; the specification makes it an Invalid Date. date-fns builds its dates with
// setUTCFullYear, so `isValid(parseISO("nope"))` was `true` in the guest: a plausible, silent, wrong answer.
//
// This file is plain JavaScript in a .ts name, so the SAME source runs in the guest (built by packages/cleat-ts/build.sh) and in
// Node (bundled by esbuild); typescript_conformance_test.go compares what each prints. Every value is recorded with String(), so
// NaN, Infinity and an Invalid Date stay distinct (JSON.stringify would turn the first two into null).
declare function __cleat_result(json: string): void;

const setters = [
  "setMilliseconds", "setUTCMilliseconds", "setSeconds", "setUTCSeconds", "setMinutes", "setUTCMinutes",
  "setHours", "setUTCHours", "setDate", "setUTCDate", "setMonth", "setUTCMonth", "setFullYear", "setUTCFullYear", "setTime",
] as const;

// Arguments that are non-finite, out of the representable range, or valid. 8.64e15 is the largest time value; one more is not.
const args: Array<[string, () => unknown]> = [
  ["NaN", () => NaN],
  ["Infinity", () => Infinity],
  ["-Infinity", () => -Infinity],
  ["undefined", () => undefined],
  ["'x'", () => "x"],
  ["{}", () => ({})],
  ["1e20", () => 1e20],
  ["8.64e15+1", () => 8.64e15 + 1],
  ["5", () => 5],
  ["-1", () => -1],
  ["2.7", () => 2.7],
];

// A receiver that is a valid date and one that is already invalid: the specification treats them differently (an invalid date is
// treated as time value +0 by setFullYear and setUTCFullYear, and stays invalid under every other setter).
const receivers: Array<[string, () => Date]> = [
  ["valid", () => new Date(Date.UTC(2020, 5, 15, 12, 30, 45, 500))],
  ["epoch", () => new Date(0)],
  ["invalid", () => new Date(NaN)],
];

export function dates(): void {
  const out: string[] = [];
  for (const [rname, mk] of receivers) {
    for (const s of setters) {
      for (const [aname, mkArg] of args) {
        const d = mk();
        const ret = (d as any)[s](mkArg());
        out.push(`${rname}.${s}(${aname}) -> ${String(ret)} | ${String(d.getTime())}`);
      }
    }
    // More than one argument: the first non-finite one, in any position, makes the whole result Invalid.
    for (const s of ["setHours", "setUTCHours", "setMinutes", "setUTCMinutes", "setSeconds", "setMonth", "setUTCMonth", "setFullYear", "setUTCFullYear"] as const) {
      for (const [label, a] of [["NaN,1", [NaN, 1]], ["1,NaN", [1, NaN]], ["1,2", [1, 2]], ["1,2,NaN", [1, 2, NaN]], ["Infinity,1,2", [Infinity, 1, 2]]] as Array<[string, number[]]>) {
        const d = mk();
        const ret = (d as any)[s](...a);
        out.push(`${rname}.${s}(${label}) -> ${String(ret)} | ${String(d.getTime())}`);
      }
    }
  }
  // The downstream symptom, spelled out: how a date library tells a valid date from an invalid one.
  const probe = new Date(0);
  probe.setUTCFullYear(NaN);
  out.push(`probe: isNaN=${String(isNaN(probe as any))} string=${String(probe)} iso=${(() => { try { return probe.toISOString(); } catch (e) { return String((e as Error).name); } })()}`);
  __cleat_result(JSON.stringify(out));
}
