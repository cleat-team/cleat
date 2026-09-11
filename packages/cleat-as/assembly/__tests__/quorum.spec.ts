/**
 * A quorum counts DISTINCT voters, not deliveries. cleat#1136 / cleat#1132.
 *
 * `awaitSignalsWithQuorumMs` passed its `namesJson` parameter unchanged to
 * every `awaitSignalsMs` call, so three deliveries of one name satisfied a
 * quorum of three while the other two were never sent. Go was fixed in #1135,
 * Rust and Python in #1158, Java in #1160; this is the AssemblyScript half.
 *
 * WHY THESE CAN TEST HostCalls WHERE stop-bit.spec.ts SAYS IT CANNOT.
 *
 * That file's header is right: the as-pect harness stubs every
 * `@external("env", ...)` import, so a test that reaches one is measuring the
 * stub. These do not reach one. The quorum loop calls `this.awaitSignalsMs(...)`
 * -- a virtual dispatch -- so a subclass intercepts it before any import is
 * touched. What is under test is the LOOP: which names it asks for, and what it
 * does with what comes back.
 *
 * And asking is the half that matters. A test that only checked the returned
 * outcomes would pass against the defect, because three deliveries of "a" ARE
 * three outcomes. The defect is invisible in the results and plain in the
 * requests, so these assert on what was asked.
 */
import { HostCalls, AwaitSignalsOutcome, jsonStrArray } from "../index";

/**
 * A HostCalls whose awaitSignalsMs is scripted: it records the names it was
 * asked for and returns the next queued delivery.
 */
class ScriptedHost extends HostCalls {
  /** The `namesJson` argument of each call, in order. */
  asked: string[] = [];
  /** Signal names to deliver, in order. */
  script: string[] = [];
  /** Payloads parallel to `script`; "" where not set. */
  payloads: string[] = [];
  private next: i32 = 0;

  constructor(script: string[], payloads: string[]) {
    super();
    this.script = script;
    this.payloads = payloads;
  }

  awaitSignalsMs(namesJson: string, timeoutMs: i64): AwaitSignalsOutcome {
    this.asked.push(namesJson);
    if (this.next >= this.script.length) {
      return new AwaitSignalsOutcome("", "", true, null, "");
    }
    const name: string = this.script[this.next];
    const payload: string = this.next < this.payloads.length ? this.payloads[this.next] : "";
    this.next++;
    return new AwaitSignalsOutcome(name, payload, false, null, "");
  }
}

function namesAsked(h: ScriptedHost, i: i32): string[] {
  return jsonStrArray(h.asked[i]);
}

function contains(names: string[], want: string): bool {
  for (let i: i32 = 0; i < names.length; i++) {
    if (names[i] == want) return true;
  }
  return false;
}


/**
 * A module-level probe, because AssemblyScript has neither try/catch nor
 * closure capture.
 *
 * as-pect's toThrow() takes a function, and the refusal cases need to assert
 * on state the call left behind -- how many times the host was asked -- which
 * means the host must outlive the throw. A local captured by an arrow function
 * is not available (AS does not implement closures), so the probe is a global
 * and each case is a top-level function referenced by name.
 *
 * The first version of this file did capture a local, and it compiled: the
 * arrow function silently referred to a DIFFERENT host than the one asserted
 * on, so the ask-count check read zero and said nothing.
 */
let probe: ScriptedHost = new ScriptedHost([], []);
let probeNames: string = "";
let probeMin: i32 = 0;
let probeMaxRejections: i32 = -1;

function runProbe(): void {
  probe.awaitSignalsWithQuorumMs(probeNames, probeMin, probeMaxRejections, 60000);
}

function armProbe(script: string[], names: string, minCount: i32, maxRejections: i32): void {
  probe = new ScriptedHost(script, []);
  probeNames = names;
  probeMin = minCount;
  probeMaxRejections = maxRejections;
}

describe("a quorum counts distinct voters", () => {
  it("narrows the set it asks for after each vote", () => {
    const h = new ScriptedHost(["a", "b", "c"], ["", "", ""]);
    const got = h.awaitSignalsWithQuorumMs('["a","b","c"]', 3, -1, 60000);

    expect<i32>(got.length).toBe(3);
    expect<i32>(h.asked.length).toBe(3);

    // The assertion the defect is about. Before the fix every entry in
    // `asked` was the full set, so a name that had already voted stayed
    // eligible to vote again.
    expect<i32>(namesAsked(h, 0).length).toBe(3);
    expect<i32>(namesAsked(h, 1).length).toBe(2);
    expect<i32>(namesAsked(h, 2).length).toBe(1);

    expect<bool>(contains(namesAsked(h, 1), "a")).toBe(false);
    expect<bool>(contains(namesAsked(h, 2), "a")).toBe(false);
    expect<bool>(contains(namesAsked(h, 2), "b")).toBe(false);
    expect<bool>(contains(namesAsked(h, 2), "c")).toBe(true);
  });

  // THE THREE REFUSAL CASES ASSERT HOW MANY TIMES THE HOST WAS ASKED, not
  // merely that something was thrown.
  //
  // Written the obvious way first, each of them passed against the UNFIXED
  // implementation -- because a script that runs dry makes the loop time out,
  // and a timeout throws too. Three tests green for a reason unrelated to
  // their names. `asked.length` separates "refused before asking", "refused
  // after one delivery" and "spun to the deadline", which is the only thing
  // that distinguishes the check under test from the timeout.

  it("refuses a second delivery of a name that has already voted", () => {
    // The original defect as a scenario rather than a mechanism: one sender,
    // three deliveries, a quorum of three. It used to succeed.
    armProbe(["a", "a", "a"], '["a","b","c"]', 3, -1);
    expect<() => void>(runProbe).toThrow();
    // Refused at the SECOND delivery: the first ask holds all three names,
    // the second excludes "a", and the scripted "a" is then out of set. Two
    // asks, not three, and not zero.
    expect<i32>(probe.asked.length).toBe(2);
  });

  it("refuses a quorum larger than the set, before asking at all", () => {
    armProbe(["a", "b", "c"], '["a","b"]', 3, -1);
    expect<() => void>(runProbe).toThrow();
    // Zero asks. A quorum of 3 over 2 names is arithmetic, not a slow sender,
    // and the old code spun to the deadline reporting "got k/3 signals" --
    // a message describing the wrong thing. The script here is deliberately
    // LONGER than the quorum so a timeout cannot be what threw.
    expect<i32>(probe.asked.length).toBe(0);
  });

  it("refuses a name that was never asked for, after one delivery", () => {
    // Narrowing what we ASK for is only half the fix if we accept whatever
    // arrives. A well-behaved host cannot produce this; the invariant the
    // quorum rests on is "each result is a distinct member of the set", and
    // this is the only place that can enforce it.
    armProbe(["z", "a", "b"], '["a","b","c"]', 2, -1);
    expect<() => void>(runProbe).toThrow();
    // Exactly one ask: "z" arrived and was refused. The script continues with
    // two valid names, so a dry script is not what threw.
    expect<i32>(probe.asked.length).toBe(1);
  });

  it("narrows on a rejection too", () => {
    // A voter that votes no has voted. Leaving it in the set would let one
    // rejector trip maxRejections alone -- the same defect wearing the other
    // outcome.
    const h = new ScriptedHost(["a", "b"], ['{"rejected":true}', ""]);
    const got = h.awaitSignalsWithQuorumMs('["a","b"]', 2, 1, 60000);

    expect<i32>(got.length).toBe(2);
    expect<bool>(contains(namesAsked(h, 1), "a")).toBe(false);
  });

  it("still reports a genuine timeout", () => {
    // The behaviour that must SURVIVE the narrowing, and the control for the
    // three cases above: here the script really does run dry, and the ask
    // count shows the loop got as far as it could before the host stopped
    // delivering.
    armProbe(["a"], '["a","b","c"]', 3, -1);
    expect<() => void>(runProbe).toThrow();
    expect<i32>(probe.asked.length).toBe(2);
  });
});
