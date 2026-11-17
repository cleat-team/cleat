// The IDNA rules that need CONTEXT, so a single-code-point sweep cannot see them (cleat#3898, cleat-review's pairs probe):
//   E  UTS46 folds capital sigma to sigma ALWAYS, while JavaScript's toLowerCase applies Final_Sigma and turns a label-final
//      capital sigma into final sigma (`ΕΛΛΑΣ.gr` -> `xn--mxahsa5b.gr`); a per-label lowercase cannot see the next label.
//   F  a label made of Arabic-Indic digits is accepted by Node 26 and refused by Node 24, the version CI pins; the shim
//      refuses it (loud against 26, exact against 24).
//   G  lowercasing can move a combining mark out of canonical order (U+0130 + a mark), so NFC runs again after it.
// Every answer here is a literal, not Node's: E and G agree with Node under both versions, F is the version split.
declare function __cleat_result(json: string): void;

const cases: string[] = [
  "http://ΕΛΛΑΣ.gr/", "http://ΑΘΗΝΑΣ.gr/", "http://ΒΌΛΟΣ.com/", "http://Σ.gr/", "http://ΣΣ.gr/",
  "http://ασ.gr/", "http://ά.gr/", "http://İ़.com/", "http://İ.com/",
  "http://١.com/", "http://٠١.com/", "http://a١b.com/", "http://١٢٣.gr/",
  "http://\u{10D30}.com/", "http://\u{10E60}.com/", "http://a\u{10D30}b.com/",
];

export function urlsIdnaContext(): void {
  const out: string[] = [];
  for (const s of cases) {
    let r: string;
    try { r = "host " + new URL(s).host; } catch { r = "refuse"; }
    out.push(`${s} -> ${r}`);
  }
  __cleat_result(JSON.stringify(out));
}
