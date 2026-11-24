// A kind of input where Node's own versions disagree, so the corpus cannot be its arbiter (cleat#3898): a malformed
// "xn--" label. Node 26 refuses an empty payload, one that decodes to a control character, one that is not NFC or not
// lowercase, and Cherokee smalls; Node 24 accepts them. The shim takes the stricter answer -- refuse -- which is exact
// against Node 26 and LOUD against Node 24, and this file pins it so neither version can change it quietly.
declare function __cleat_result(json: string): void;

const cases: string[] = [
  "http://a.xn--/", "http://xn--a.com/", "http://xn--0.com/", "http://xn--.com/", "http://xn----.com/",
  "http://xn---.com/", "http://xn--a-.com/", "http://xn--ab-.com/", "http://xn--1ug.com/", "http://xn---a.com/",
  "http://xn--a-xbb.com/", "http://xn--kkg.com/", "http://xn--kba.com/", "http://xn--ab-1ca.com/",
  "http://xn--ab-l13a.com/", "http://xn--tvg.com/", "http://xn--ib9b.com/",
  "http://xn--mnchen-3ya.de/", "http://xn--zca/", "http://xn--fa-hia.de/", "http://münchen.de/", "http://例え.テスト/",
];

export function urlsIdnaSpec(): void {
  const out: string[] = [];
  for (const s of cases) {
    let verdict: string;
    try { new URL(s); verdict = "accept"; } catch { verdict = "refuse"; }
    out.push(`${s} -> ${verdict}`);
  }
  __cleat_result(JSON.stringify(out));
}
