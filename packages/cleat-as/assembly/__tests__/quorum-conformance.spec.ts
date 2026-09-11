/**
 * AssemblyScript reads the SAME quorum table as the other four SDKs.
 * cleat#1136.
 *
 * `tests/conformance/quorum_cases.json` says what it is for:
 *
 *   "This file is that comparison: one table, consumed by each SDK's own test
 *    runner, so the next divergence fails somewhere rather than nowhere."
 *
 * Go, Rust, Python and Java consume it by opening the file at test time.
 * #1236 fixed the AssemblyScript quorum loop and did NOT — it shipped six
 * hand-written cases instead, leaving the fifth SDK fixed and the only one
 * comparing against nothing. This closes that.
 *
 * HOW THE TABLE CROSSES THE BOUNDARY. AssemblyScript compiles to WASM and has
 * no filesystem, so the harness reads the canonical file in JS on every run and
 * hands each case over flattened — see as-pect.config.mjs. Nothing is generated
 * and nothing is checked in, so the table cannot go stale against this SDK the
 * way a generated fixture could.
 *
 * WHAT IT ASSERTS, and the table is explicit that this is the point:
 *
 *   "Each case drives the SDK's quorum loop against a scripted host and asserts
 *    what the loop ASKED FOR on each iteration -- awaited_sets -- not only what
 *    it returned."
 *
 * quorum.spec.ts, next door, keeps its hand-written cases. They are not
 * redundant: they assert the ASK COUNT for the refusal paths, which separates
 * "refused before asking" from "spun to the deadline" — a distinction the table
 * does not carry and one that caught three of those six tests passing for the
 * wrong reason.
 */
import { HostCalls, AwaitSignalsOutcome, readString } from "../index";

// ── the table, handed over by the harness ──────────────────────────────────

@external("env", "cleat_test_conformance_count")
declare function conformanceCount(): i32;

@external("env", "cleat_test_conformance_case")
declare function conformanceCase(index: i32, outPtr: usize, outMaxLen: i32): i64;

// A GUEST-ALLOCATED buffer, and the two wrong answers before it are worth
// recording because both looked like "the table is empty".
//
// First an invented 128 KiB offset; then the SDK's own OUTPUT_OFFSET, which
// seemed obviously right. Both TRAPPED: the harness writes through
// `new Uint8Array(memory.buffer, ptr, len)`, which throws when ptr is past the
// end, and the throw surfaced as a failed assertion rather than as a bad
// pointer. `conformanceCount()` answered 6 correctly throughout, so the
// symptom pointed at the table and the fault was in the address.
//
// memory.size() here is ONE PAGE -- 64 KiB. OUTPUT_OFFSET is 10,551,296, so
// the SDK's scratch addresses assume a runtime that provides about 10 MB and
// as-pect provides nothing of the sort. Allocating in the guest sidesteps the
// question: the pointer is valid by construction and the allocation grows
// memory if it must.
const CASE_MAX: i32 = 64 * 1024;

/** Field, list and group separators, matching the harness. */
const US: string = "\x1f";
const RS: string = "\x1e";
const GS: string = "\x1d";

function caseText(index: i32): string {
  const buf = new Uint8Array(CASE_MAX);
  const ptr: usize = changetype<usize>(buf.dataStart);
  const packed: i64 = conformanceCase(index, ptr, CASE_MAX);
  if (packed < 0) return "";
  return readString(ptr, <i32>(packed >> 32));
}

/** Splits on a separator, returning an empty array for an empty string. */
function parts(s: string, sep: string): string[] {
  if (s.length == 0) return [];
  return s.split(sep);
}

// ── a scripted host, in the two modes the table requires ───────────────────

class TableHost extends HostCalls {
  asked: string[] = [];
  polite: bool = false;
  deliveries: string[] = [];
  payloadNames: string[] = [];
  payloadValues: string[] = [];
  private next: i32 = 0;

  constructor(polite: bool, deliveries: string[], names: string[], values: string[]) {
    super();
    this.polite = polite;
    this.deliveries = deliveries;
    this.payloadNames = names;
    this.payloadValues = values;
  }

  awaitSignalsMs(namesJson: string, timeoutMs: i64): AwaitSignalsOutcome {
    this.asked.push(namesJson);
    // A POLITE host hands over a queued name only while it is still awaited,
    // which is what the engine's own DurableAwaitSignals does -- it exercises
    // the narrowing. An IMPOLITE one hands over whatever is queued, which
    // exercises the out-of-set guard. The table requires both, because a fix
    // that rests only on the polite case rests on the host's manners.
    while (this.next < this.deliveries.length) {
      const name: string = this.deliveries[this.next];
      if (this.polite && namesJson.indexOf('"' + name + '"') < 0) {
        this.next++;
        continue;
      }
      this.next++;
      return new AwaitSignalsOutcome(name, this.payloadFor(name), false, null, "");
    }
    return new AwaitSignalsOutcome("", "", true, null, "");
  }

  private payloadFor(name: string): string {
    for (let i: i32 = 0; i < this.payloadNames.length; i++) {
      if (this.payloadNames[i] == name) return this.payloadValues[i];
    }
    return "";
  }
}

// ── the probe, since AS has neither try/catch nor closure capture ──────────

let probe: TableHost = new TableHost(false, [], [], []);
let probeNames: string = "";
let probeMin: i32 = 0;
let probeMaxRejections: i32 = -1;
let probeResults: AwaitSignalsOutcome[] = [];

function runProbe(): void {
  probeResults = probe.awaitSignalsWithQuorumMs(probeNames, probeMin, probeMaxRejections, 60000);
}

function namesToJson(names: string[]): string {
  let out: string = "[";
  for (let i: i32 = 0; i < names.length; i++) {
    if (i > 0) out += ",";
    out += '"' + names[i] + '"';
  }
  return out + "]";
}

describe("the shared quorum conformance table", () => {
  it("is non-empty, so a harness that handed over nothing fails here", () => {
    // Without this the whole file passes vacuously when the table cannot be
    // read: zero cases, zero assertions, green. That is the failure this
    // table exists to prevent, arriving in the file that consumes it.
    expect<i32>(conformanceCount()).toBeGreaterThan(0);
    expect<i32>(caseText(0).length).toBeGreaterThan(0);
  });

  it("agrees with the AssemblyScript quorum loop on every case", () => {
    const n: i32 = conformanceCount();
    for (let c: i32 = 0; c < n; c++) {
      const f: string[] = caseText(c).split(US);
      const name: string = f[0];
      const signalNames: string[] = parts(f[1], RS);
      const minCount: i32 = I32.parseInt(f[2]);
      const maxRejections: i32 = I32.parseInt(f[3]);
      const polite: bool = f[4] == "polite";
      const deliveries: string[] = parts(f[5], RS);
      const outcome: string = f[7];
      const resultNames: string[] = parts(f[9], RS);
      const awaitedSets: string[] = parts(f[10], GS);

      // payloads: name RS json, groups separated by GS
      const payloadNames: string[] = [];
      const payloadValues: string[] = [];
      const rawPayloads: string[] = parts(f[6], GS);
      for (let i: i32 = 0; i < rawPayloads.length; i++) {
        const kv: string[] = rawPayloads[i].split(RS);
        if (kv.length == 2) {
          payloadNames.push(kv[0]);
          payloadValues.push(kv[1]);
        }
      }

      probe = new TableHost(polite, deliveries, payloadNames, payloadValues);
      probeNames = namesToJson(signalNames);
      probeMin = minCount;
      probeMaxRejections = maxRejections;
      probeResults = [];

      if (outcome == "error") {
        expect<() => void>(runProbe).toThrow("case: " + name);
      } else {
        runProbe();
        expect<i32>(probeResults.length).toBe(resultNames.length, "case: " + name);
        for (let i: i32 = 0; i < resultNames.length; i++) {
          expect<string>(probeResults[i].signalName).toBe(resultNames[i], "case: " + name);
        }
      }

      // The assertion the table is FOR: what the loop asked for, each round.
      // A case that only checked the outcome would pass against an
      // implementation failing for an unrelated reason.
      expect<i32>(probe.asked.length).toBe(awaitedSets.length, "asked rounds, case: " + name);
      for (let i: i32 = 0; i < awaitedSets.length && i < probe.asked.length; i++) {
        expect<string>(probe.asked[i]).toBe(
          namesToJson(parts(awaitedSets[i], RS)),
          "awaited set " + i.toString() + ", case: " + name,
        );
      }
    }
  });
});
