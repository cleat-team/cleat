import { workflow } from "../../packages/cleat-ts/index";

// Q4 / ABI 6.3 and 6.6: where does raw JavaScript JSON.stringify differ from the host's canonical form (Go encoding/json)?
// Each case yields the raw JS text and the host's canonical text of it. The Go test recomputes the canonical form independently
// (Unmarshal + Marshal) and asserts the host agrees, so "canonical" is not just whatever the host returned.

const LONE = String.fromCharCode(0xd800);
const CASES: Record<string, () => string> = {
  keysUnsorted: () => JSON.stringify({ b: 1, a: 2, c: { z: 1, y: 2 } }),
  htmlChars: () => JSON.stringify("<script>a&b</script>"),
  lineSeparators: () => JSON.stringify("a b c"),
  floats: () => JSON.stringify([1.0, 100, 0.1 + 0.2, 1e21, 1e-7, 123456789012345680000, 5e-324, 1.7976931348623157e308]),
  negativeZero: () => JSON.stringify([-0, 0]),
  nanAndInfinity: () => JSON.stringify([NaN, Infinity, -Infinity]),
  undefinedAndFunction: () => JSON.stringify({ a: undefined, b: () => 1, c: 1, d: [undefined, () => 1] }),
  dateValue: () => JSON.stringify({ d: new Date(0) }),
  loneSurrogate: () => JSON.stringify(LONE),
  unicodeSafe: () => JSON.stringify("é 日本 😀"),
  controlChars: () => JSON.stringify("\u0007\u001f\t\n\u007f"),
  emptyContainers: () => JSON.stringify({ a: {}, b: [], c: "" }),
  bigIntegerLiteral: () => '{"big":9007199254740993,"neg":-9007199254740993}', // JSON TEXT, not a JS number: JS cannot hold it
  exponentLiteral: () => '{"a":1E3,"b":1.50,"c":0.0000001,"d":-0.0}',
  duplicateKeys: () => '{"a":1,"a":2}',
};

export const jsoncanon = workflow<unknown, Array<{ name: string; raw: string; canon: string | null; error: string | null }>>(async (ctx) => {
  const out = [];
  for (const [name, make] of Object.entries(CASES)) {
    const raw = make();
    let canon: string | null = null;
    let error: string | null = null;
    try {
      canon = ctx.canonical(raw);
    } catch (e) {
      error = (e as Error).message;
    }
    out.push({ name, raw, canon, error });
  }
  return out;
});
