// Every code point, in a host label, compared with Node (cleat#3898). The IDNA work is only exact inside the classes this
// sweep measured, so the sweep is a test rather than a one-off: it fails if the guest ACCEPTS a code point Node refuses,
// which is the silent divergence the URL shim exists to remove. The guest refuses some code points Node accepts; that is
// loud, and the test reports the count rather than failing.
//
// The whole U+0000-U+10FFFF range takes longer than one guest execution is allowed (about 35s against a 30s budget), so the
// committed sweep is the BMP in full plus a strided sample of the supplementary planes. The un-strided run was done once,
// by chunk, and recorded in DIVERGENCES.md (0 silent over all 1,112,064 code points).
declare function __cleat_result(json: string): void;

const PBASE = 36, PTMIN = 1, PTMAX = 26, PSKEW = 38, PDAMP = 700, PBIAS = 72;
const pAdapt = (delta: number, numPoints: number, first: boolean): number => {
  delta = first ? Math.floor(delta / PDAMP) : delta >> 1;
  delta += Math.floor(delta / numPoints);
  let k = 0;
  while (delta > ((PBASE - PTMIN) * PTMAX) >> 1) { delta = Math.floor(delta / (PBASE - PTMIN)); k += PBASE; }
  return Math.floor(k + (PBASE - PTMIN + 1) * delta / (delta + PSKEW));
};
const pDigit = (d: number): string => String.fromCharCode(d < 26 ? 97 + d : 22 + d);
const puny = (label: string): string => {
  const cp: number[] = [];
  for (const c of label) cp.push(c.codePointAt(0)!);
  const out: string[] = [];
  for (const c of cp) if (c < 0x80) out.push(String.fromCharCode(c));
  const basic = out.length;
  let h = basic, n = 128, delta = 0, bias = PBIAS;
  if (basic) out.push("-");
  while (h < cp.length) {
    let m = Infinity;
    for (const c of cp) if (c >= n && c < m) m = c;
    delta += (m - n) * (h + 1);
    n = m;
    for (const c of cp) {
      if (c < n) delta++;
      else if (c === n) {
        let q = delta;
        for (let k = PBASE; ; k += PBASE) {
          const t = k <= bias ? PTMIN : k >= bias + PTMAX ? PTMAX : k - bias;
          if (q < t) break;
          out.push(pDigit(t + (q - t) % (PBASE - t)));
          q = Math.floor((q - t) / (PBASE - t));
        }
        out.push(pDigit(q));
        bias = pAdapt(delta, h + 1, h === basic);
        delta = 0; h++;
      }
    }
    delta++; n++;
  }
  return out.join("");
};
const acc = (s: string): boolean => { try { new URL(s); return true; } catch { return false; } };
const ranges = (accepted: boolean[], samples: number[]): string => {
  const out: string[] = [];
  let i = 0;
  while (i < samples.length) {
    if (!accepted[i]) { i++; continue; }
    let j = i;
    while (j + 1 < samples.length && accepted[j + 1] && samples[j + 1] === samples[j] + 1) j++;
    out.push(samples[i].toString(16) + "-" + samples[j].toString(16));
    i = j + 1;
  }
  return out.join(",");
};

export function urlsSweep(): void {
  const samples: number[] = [];
  for (let cp = 0; cp <= 0xffff; cp++) samples.push(cp);
  for (let cp = 0x10000; cp < 0x110000; cp += 61) samples.push(cp);
  const raw: boolean[] = [], xn: boolean[] = [];
  for (const cp of samples) {
    if (cp >= 0xd800 && cp <= 0xdfff) { raw.push(false); xn.push(false); continue; }
    const ch = String.fromCodePoint(cp);
    raw.push(acc("http://a" + ch + "b.example/"));
    xn.push(acc("http://xn--" + puny("a" + ch + "b") + ".example/"));
  }
  __cleat_result(JSON.stringify([ranges(raw, samples), ranges(xn, samples), samples.length.toString()]));
}
