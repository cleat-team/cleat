// Phase 5 / Q6: the expected value of every case, from plain Node (TZ=UTC), using the SAME observe() the guest uses
// (workflows/npm_common.ts, bundled here) so the two sides cannot disagree about how a value is rendered.
// usage: TZ=UTC node phase5_npm_expected.mjs <dir with common.mjs zod.mjs datefns.mjs lodash.mjs uuid.mjs>
import { pathToFileURL } from "node:url";
import { resolve } from "node:path";
const dir = resolve(process.argv[2]);
const { observe } = await import(pathToFileURL(`${dir}/common.mjs`));
const out = {};
for (const pkg of ["zod", "datefns", "lodash", "uuid"]) {
  const { CASES } = await import(pathToFileURL(`${dir}/${pkg}.mjs`));
  out[pkg] = Object.fromEntries(Object.entries(CASES).map(([k, f]) => [k, observe(f)]));
}
process.stdout.write(JSON.stringify(out));
