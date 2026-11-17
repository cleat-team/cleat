// The contextual rules a single-code-point sweep cannot see (cleat#3898): joiners and bidi. Each host is one a sweep of
// single code points cannot construct -- ZWNJ after a virama, ZWNJ between Arabic dual-joining letters, an RTL label ending
// in a digit or a combining mark, EN and AN in one RTL label, an RTL label starting with a digit, RTL and Latin labels in one
// host. Both sides answer the same question per host, so the comparison is about the RULE and not about one character.
//
// CONTEXTO is not modelled, and Node does not model it either: `l·x` (a middle dot with no Catalan context) is accepted
// by Node, which the list records. The probe set is 48 hosts; the measured result is in DIVERGENCES.md row 2.
declare function __cleat_result(json: string): void;

const CASES: [string, string][] = [
  ["http://\u0915\u094d\u200c\u0937.example/", "devanagari virama ZWNJ (raw)"],
  ["http://xn--11b2ezcs70k.example/", "devanagari virama ZWNJ (xn--)"],
  ["http://\u0915\u094d\u200d\u0937.example/", "devanagari virama ZWJ (raw)"],
  ["http://xn--11b2ezcw70k.example/", "devanagari virama ZWJ (xn--)"],
  ["http://\u0915\u200c\u0937.example/", "devanagari no virama ZWNJ (raw)"],
  ["http://xn--11b2e898f.example/", "devanagari no virama ZWNJ (xn--)"],
  ["http://\u0915\u200d\u0937.example/", "devanagari no virama ZWJ (raw)"],
  ["http://xn--11b2er09f.example/", "devanagari no virama ZWJ (xn--)"],
  ["http://\u0d15\u0d4d\u200c\u0d37.example/", "malayalam virama ZWNJ (raw)"],
  ["http://xn--bwc2ezc097h.example/", "malayalam virama ZWNJ (xn--)"],
  ["http://\u0d15\u0d4d\u200d\u0d37.example/", "malayalam virama ZWJ (raw)"],
  ["http://xn--bwc2ezc497h.example/", "malayalam virama ZWJ (xn--)"],
  ["http://\u0628\u200c\u0628.example/", "arabic dual ZWNJ (raw)"],
  ["http://xn--ngba799q.example/", "arabic dual ZWNJ (xn--)"],
  ["http://\u0628\u200d\u0628.example/", "arabic dual ZWJ (raw)"],
  ["http://xn--ngba000r.example/", "arabic dual ZWJ (xn--)"],
  ["http://\u0627\u200c\u0627.example/", "arabic nonjoin ZWNJ (raw)"],
  ["http://xn--mgba000r.example/", "arabic nonjoin ZWNJ (xn--)"],
  ["http://a\u200cb.example/", "latin ZWNJ (raw)"],
  ["http://xn--ab-j1t.example/", "latin ZWNJ (xn--)"],
  ["http://\u05d0\u05d1\u05d2.example/", "all RTL (raw)"],
  ["http://xn--4dbcd.example/", "all RTL (xn--)"],
  ["http://\u05d0\u05d11.example/", "RTL european digit (raw)"],
  ["http://xn--1-zhcd.example/", "RTL european digit (xn--)"],
  ["http://\u0627\u0628\u0661.example/", "RTL arabic digit (raw)"],
  ["http://xn--mgbc5o.example/", "RTL arabic digit (xn--)"],
  ["http://\u05d0\u05d1\u05b0.example/", "RTL combining (raw)"],
  ["http://xn--7cb7dd.example/", "RTL combining (xn--)"],
  ["http://\u05d01\u0661.example/", "RTL EN and AN (raw)"],
  ["http://xn--1-zhc05b.example/", "RTL EN and AN (xn--)"],
  ["http://a.\u05d0\u05d1.example/", "LTR beside RTL (raw)"],
  ["http://a.xn--4dbc.example/", "LTR beside RTL (xn--)"],
  ["http://1\u05d0\u05d1.example/", "RTL starts digit (raw)"],
  ["http://xn--1-0hcd.example/", "RTL starts digit (xn--)"],
  ["http://\u05d0a.example/", "RTL with Latin (raw)"],
  ["http://xn--a-zhc.example/", "RTL with Latin (xn--)"],
  ["http://\u05d0\u05d1-\u05d2.example/", "RTL hyphen (raw)"],
  ["http://xn----zhcdf.example/", "RTL hyphen (xn--)"],
  ["http://\u0627\u0628\u05d0.example/", "RTL Arabic mixed (raw)"],
  ["http://xn--4db7od.example/", "RTL Arabic mixed (xn--)"],
  ["http://l\u00b7l.example/", "catalan middle dot (raw)"],
  ["http://xn--ll-0ea.example/", "catalan middle dot (xn--)"],
  ["http://l\u00b7x.example/", "catalan invalid context (raw)"],
  ["http://xn--lx-0ea.example/", "catalan invalid context (xn--)"],
  ["http://\u05e6\u05f3.example/", "hebrew geresh (raw)"],
  ["http://xn--reb0a.example/", "hebrew geresh (xn--)"],
  ["http://\u05d0\u05f3\u05d1.example/", "hebrew geresh context (raw)"],
  ["http://xn--4dbc5h.example/", "hebrew geresh context (xn--)"],
];

export function urlsContext(): void {
  const out: string[] = [];
  for (const [host, name] of CASES) {
    let verdict: string;
    try { new URL(host); verdict = "accept"; } catch { verdict = "refuse"; }
    out.push(name + " -> " + verdict);
  }
  __cleat_result(JSON.stringify(out));
}
