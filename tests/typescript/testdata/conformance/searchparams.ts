// URLSearchParams, compared with Node (cleat#3829): every constructor form, every method, the iteration order, the percent
// decoding of invalid UTF-8, and the sort. See urls.ts for why the same source runs in both places.
declare function __cleat_result(json: string): void;

const show = (f: () => unknown): string => {
  try { return String(f()); } catch (e) { return "throws " + (e as Error).name; }
};
const pairs = (p: any): string => JSON.stringify(Array.from(p as Iterable<[string, string]>));

const strings: string[] = [
  "", "?", "a=1&b=2", "?a=1&a=2", "a", "a=", "=b", "&&a=1&&", "a=1=2", "a+b=c+d", "a%20b=%41", "a=%zz", "a=%", "a=%4", "ü=é", "%C3%BC=%C3%A9",
  "%ff=%fe", "%E2%82", "%E2%82%AC=%F0%9F%98%80", "%ED%A0%80", "%C0%80", "a=1;b=2", "a b=c d", "??a=1", "a=b&", "&", "=", "==", "a=%2B+%2b", "a=\ud800",
  "a=%00", "k=%F0%9F", "%f0%9f%98", "a=1&b=2&a=3", "x=%25", "x=%2", "x=%%41",
];

export function searchParams(): void {
  const out: string[] = [];
  for (const s of strings) {
    const p = new URLSearchParams(s);
    out.push(`new(${JSON.stringify(s)}) -> ${pairs(p)} | ${p.toString()} | size=${p.size}`);
  }
  // Other constructor forms.
  const inits: Array<[string, () => any]> = [
    ["pairs", () => [["a", "1"], ["b", "2"]]], ["object", () => ({ a: "1", b: "2" })], ["copy", () => new URLSearchParams("x=1&y=2")], ["empty array", () => []],
    ["short pair", () => [["a"]]], ["long pair", () => [["a", "1", "2"]]], ["null", () => null], ["number", () => 5], ["undefined", () => undefined],
    ["Map", () => new Map([["m", "1"], ["n", "2"]])], ["string pair", () => ["ab"]], ["non-iterable pair", () => [1]], ["object with numbers", () => ({ a: 1, b: null, c: undefined })],
    ["symbol key", () => ({ [Symbol("s")]: "x", t: "y" })], ["non-enumerable", () => Object.defineProperty({ v: "1" }, "h", { value: "2", enumerable: false })],
    ["array-like object", () => ({ length: 1, 0: "x" })], ["boolean", () => true], ["generator", () => (function* () { yield ["g", "1"]; yield ["h", "2"]; })()],
    ["duplicate keys object", () => ({ a: "1", "a ": "2" })], ["surrogate pair", () => [["\ud800", "\udc00"]]], ["lone surrogate", () => [["\ud800x", "y\udc00"]]],
  ];
  for (const [label, mk] of inits) out.push(`init ${label} -> ${show(() => { const p = new URLSearchParams(mk()); return pairs(p) + " | " + p.toString(); })}`);

  const base = () => new URLSearchParams("a=1&b=2&a=3&c=%C3%BC&d=e+f&=x&y=");
  const step = (label: string, f: (p: URLSearchParams) => unknown) => {
    const p = base();
    const r = show(() => f(p));
    out.push(`${label} -> ${r} | ${pairs(p)} | ${p.toString()} | size=${p.size}`);
  };
  for (const name of ["a", "b", "z", "", "c", "ü", "A"]) {
    step(`get(${JSON.stringify(name)})`, (p) => p.get(name));
    step(`getAll(${JSON.stringify(name)})`, (p) => JSON.stringify(p.getAll(name)));
    step(`has(${JSON.stringify(name)})`, (p) => p.has(name));
    step(`delete(${JSON.stringify(name)})`, (p) => p.delete(name));
    step(`append(${JSON.stringify(name)}, "v w")`, (p) => p.append(name, "v w"));
    step(`set(${JSON.stringify(name)}, "ü&=")`, (p) => p.set(name, "ü&="));
  }
  step("has(a, 3)", (p) => p.has("a", "3"));
  step("has(a, 2)", (p) => p.has("a", "2"));
  step("has(a, undefined)", (p) => p.has("a", undefined));
  step("delete(a, 3)", (p) => p.delete("a", "3"));
  step("delete(a, 9)", (p) => p.delete("a", "9"));
  step("delete(a, undefined)", (p) => p.delete("a", undefined));
  step("delete(a, 1) then has", (p) => { p.delete("a", "1"); return p.has("a", "1") + "," + p.has("a", "3"); });
  step("set existing keeps first position", (p) => p.set("a", "9"));
  step("set(b) moves nothing", (p) => p.set("b", "9"));
  step("get with no argument", (p) => (p as any).get());
  step("append with one argument", (p) => (p as any).append("x"));
  step("set with one argument", (p) => (p as any).set("x"));
  step("delete with no argument", (p) => (p as any).delete());
  step("number name", (p) => { p.append(1 as any, 2 as any); return p.get("1"); });
  step("lone surrogate name", (p) => { p.append("\ud800", "\udc00"); return p.get("�"); });
  step("toString twice", (p) => p.toString() + p.toString());
  step("brand", (p) => URLSearchParams.prototype.get.call({} as any, "a"));
  step("iterator brand", (p) => (p.keys() as any).next.call({}));

  // Sort: stable, by UTF-16 code unit (so U+1F600 sorts before U+FF5E, unlike a sort by code point).
  for (const s of ["b=1&a=2&b=0&a=1", "\u{1F600}=1&～=2&a=3", "B=1&a=2&A=3&b=4", "ä=1&z=2&a=3", "=1&a=2&=0", "10=a&9=b&1=c", "a=2&a=1&a=0"]) {
    const p = new URLSearchParams(s);
    p.sort();
    out.push(`sort(${JSON.stringify(s)}) -> ${pairs(p)} | ${p.toString()}`);
  }

  // Iteration.
  const it = base();
  out.push(`entries -> ${JSON.stringify([...it.entries()])} | keys -> ${JSON.stringify([...it.keys()])} | values -> ${JSON.stringify([...it.values()])}`);
  out.push(`for-of -> ${JSON.stringify([...it])} | iterator is entries -> ${String(it[Symbol.iterator] === it.entries)}`);
  const seen: string[] = [];
  it.forEach(function (this: any, v, k, self) { seen.push(k + "=" + v + ":" + String(self === it) + ":" + String(this && this.tag)); }, { tag: "t" });
  out.push(`forEach -> ${seen.join(",")}`);
  out.push(`forEach non-function -> ${show(() => (it as any).forEach(1))}`);
  // Mutation during iteration: the list is live, and the position is an index.
  const m = new URLSearchParams("a=1&b=2&c=3&d=4");
  const mseen: string[] = [];
  m.forEach((v, k) => { mseen.push(k); if (k === "a") m.delete("b"); if (k === "c") m.append("e", "5"); });
  out.push(`mutate in forEach -> ${mseen.join(",")} | ${m.toString()}`);
  const m2 = new URLSearchParams("a=1&b=2&c=3");
  const m2seen: string[] = [];
  for (const [k] of m2) { m2seen.push(k); if (k === "a") m2.delete("a"); }
  out.push(`mutate in for-of -> ${m2seen.join(",")} | ${m2.toString()}`);
  const iter = new URLSearchParams("a=1").entries();
  out.push(`iterator -> ${JSON.stringify(iter.next())} ${JSON.stringify(iter.next())} ${JSON.stringify(iter.next())} | tag ${Object.prototype.toString.call(iter)} | self ${String(iter[Symbol.iterator]() === iter)}`);
  out.push(`tag -> ${Object.prototype.toString.call(base())} | String -> ${String(base())} | JSON -> ${JSON.stringify({ p: base() })} | keys -> ${JSON.stringify(Object.keys(base()))}`);

  // Serialisation of every byte that matters: unreserved, space, reserved, and non-ASCII.
  const all: string[] = [];
  for (let c = 0; c < 0x100; c++) all.push(String.fromCharCode(c));
  const enc = new URLSearchParams();
  enc.append("all", all.join(""));
  enc.append("emoji\u{1F600}", "€");
  out.push(`encode -> ${enc.toString()}`);
  out.push(`roundtrip -> ${String(new URLSearchParams(enc.toString()).get("all") === all.join(""))}`);
  __cleat_result(JSON.stringify(out));
}
