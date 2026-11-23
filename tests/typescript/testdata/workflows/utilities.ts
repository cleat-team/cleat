import { workflow } from "@cleat/ts";

// The utilities bindings (cleat#3827 group J): `cleat_fetch` and `cleat_json_parse`. Every answer comes from the scripted
// Fetcher the test configures; there is no network here, and a test that reached one would be measuring the internet.
//
// Both calls are SYNCHRONOUS -- neither suspends -- so the sources below do not await them. That is the property being
// exercised as much as the answers: a binding that returned a promise would make `fetch(...).status` undefined rather
// than wrong, which is the kind of failure that looks like a shape mistake in the workflow.

export const utilities = workflow(async (ctx) => {
  const answer = ctx.fetch("https://example.test/a", {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: `{"n":1}`,
  });

  // jsonParse hands back the host's canonical form: keys sorted, Go encoding/json byte form. The input's keys are in the
  // wrong order on purpose.
  const parsed = ctx.jsonParse<{ a: number; b: number }>(`{ "b": 2, "a": 1 }`);

  // The KEY ORDER of the value handed back is the host's, not this source's: the input above lists `b` first, so a
  // binding that returned its argument unchanged would report ["b","a"]. This is the observable form of "canonical".
  return { answer, parsed, keys: Object.keys(parsed) };
});

// A request the fetcher refuses. The refusal must arrive as an ERROR -- the host reports it as a call failure, not as a
// status -- so the workflow records the message rather than a number that a caller would read as an answer.
export const utilitiesRefused = workflow(async (ctx) => {
  let message = "";
  try {
    ctx.fetch("https://blocked.test/x");
  } catch (e) {
    message = (e as Error).message;
  }
  return { message };
});

// A fetch AFTER the call that suspended the segment, not awaited (there is nothing to await: fetch is synchronous). It records an
// event when it reaches the host, and a segment must record nothing past the call that suspended it.
export const utilitiesLate = workflow(async (ctx) => {
  const p = ctx.sleep(60_000);
  ctx.fetch("https://example.test/late");
  await p;
  return null;
});

// A response far past 255 bytes. The decoder read the length at the wrong bits, so a response under 256 bytes decoded as ZERO bytes
// and a larger one as its length divided by 256: this returns the body's length and its last characters, so a truncated length
// shows up as a wrong number and a missing tail, not only as an empty buffer.
export const utilitiesBig = workflow(async (ctx) => {
  const r = ctx.fetch("https://example.test/big");
  return { length: r.body.length, tail: r.body.slice(-8) };
});

// The host refuses text that is not JSON; the refusal arrives as a thrown error naming the input, not as an empty string (which
// is a valid JSON TEXT and could not be told apart).
export const utilitiesBadJson = workflow(async (ctx) => {
  try {
    ctx.jsonParse("{not json");
    return { message: "" };
  } catch (e) {
    return { message: (e as Error).message };
  }
});

// A fetch whose answer the replay must return without asking the fetcher again.
export const utilitiesOnce = workflow(async (ctx) => {
  const r = ctx.fetch("https://example.test/once");
  return { status: r.status, body: r.body };
});
