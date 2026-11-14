// URL, compared with Node (cleat#3829). QuickJS has no URL, so `new URL(x)` was a ReferenceError, and zod's z.url() calls it inside a
// try/catch: every URL read as invalid, a plausible, silent, wrong answer. The shim in crates/cleat-js-plugin/src/shims.js is the
// WHATWG algorithm for the part that can be done exactly, and THROWS for the rest; this corpus is only the inputs it answers, and
// the inputs it refuses are in urls_loud.ts.
//
// Plain JavaScript in a .ts name: the SAME source runs in the guest and in Node (typescript_conformance_test.go compares the lines).
declare function __cleat_result(json: string): void;

const show = (f: () => unknown): string => {
  try { return String(f()); } catch (e) { return "throws " + (e as Error).name; }
};

// Every getter of a URL, so that two answers are the same only if all eleven are.
const describe = (u: any): string => JSON.stringify([u.href, u.origin, u.protocol, u.username, u.password, u.host, u.hostname,
  u.port, u.pathname, u.search, u.hash]);

const absolute: string[] = [
  // scheme and case
  "http://example.com", "HTTP://EXAMPLE.COM/", "HtTp://A", "https://example.com:443", "http://example.com:80/", "http://example.com:8080",
  "ftp://example.com:21/x", "ftp://example.com:22/x", "ws://h:80", "wss://h:443/", "ws://h:443/", "wss://h:80/",
  // userinfo
  "https://user:pass@host:8080/p/a/t/h?query=string#hash", "http://u@h", "http://u:@h", "http://:p@h", "http://u:p:q@h", "http://u@v@h",
  "http://us er@h", "http://u%40@h", "http://ü:ü@h", "http://@h", "http://:@h", "http://u:p@",
  // hosts
  "http://EXAMPLE.com", "http://exa%6Dple.com", "http://example.com.", "http://a_b.com", "http://a..b", "http://.a", "http://-a-", "http://a--b",
  "http://%41%42", "http://a%20b", "http://a b", "http://a<b", "http://a|b", "http://a^b", "http://a%2fb", "http://%", "http://a%zz",
  "http://", "http://:80", "http://a:", "http://a:/", "http://a:0", "http://a:65535", "http://a:65536", "http://a:-1", "http://a:1x",
  "http://a:08", "http://a:00080", "http://a:80:80", "http://a:99999999999999999999",
  // IPv4
  "http://127.0.0.1", "http://127.0.0.1:8080/x", "http://0x7f.1", "http://0177.0.0.1", "http://1.2.3", "http://1.2.3.4.5", "http://256.1.1.1",
  "http://1.256", "http://4294967295", "http://4294967296", "http://1.08", "http://a.1", "http://1.a", "http://0x", "http://a.0x", "http://0X7F.0.0.1",
  "http://1.2.3.4.", "http://1..2", "http://0", "http://00", "http://999999999999999999999.1", "http://1.2.3.04", "http://1.2.3.4a",
  // paths
  "http://a/b/c/../d", "http://a/b/./c", "http://a/../b", "http://a/b/..", "http://a/b/.", "http://a/%2e/%2E%2e/x", "http://a/b/%2e%2E/c",
  "http://a//b///c", "http://a\\b\\c", "http:\\\\a\\b", "http:/a/b", "http:a/b", "http:///a/b", "http:////a", "http://a/.", "http://a/..", "http://a/a/..%2f",
  "http://example.com/a b", "http://example.com/a\"b<c>d`e{f}g^h|i", "http://example.com/ü/é", "http://example.com/\u{1F600}", "http://example.com/\ud800x",
  "http://a/%7Efoo/%7efoo/%41", "http://a/%zz", "http://a/%", "http://a/%4", "http://a/%41%4g", "http://a/a%2Fb", "http://a/a;b=c,d$e!f(g)h*i+j'k", "http://a/[x]",
  // query and fragment
  "http://example.com/?a b\"c<d>e'f`g", "http://example.com/#a b\"c<d>e`f'g", "http://example.com/?ü#ü", "http://a/?", "http://a/#", "http://a/?#",
  "http://a/??b", "http://a/?a?b#c#d", "http://a/?a=%zz", "http://a/?a=b&c=d", "http://a?x", "http://a#y", "ws://a/?'", "foo://a/?'",
  // whitespace and control characters
  "  http://a/  ", "\thttp://a/\n", "ht\ttp://a", "http://a/\tb\nc\rd", "\u0001http://a/\u0002", "http://a/\u0000", "http://a/\u007f",
  // non-special
  "foo://bar", "foo://BAR/Baz", "foo://", "foo:///x", "foo:/x", "foo:x", "foo:x?y#z", "foo://h:80/", "foo://h:99999", "foo://u@h", "foo://u@",
  "foo://:80", "foo://a b", "foo://%41", "foo://ü", "foo://a/../b", "foo:/a/../..", "foo://h/", "foo://h", "foo://h?q", "foo://h#f",
  "mailto:a@b.com", "mailto:a b@c", "data:text/plain;base64,aGk=", "javascript:alert(1)", "urn:isbn:123", "tel:+1-555", "a:b", "a:", "urn:",
  "foo:?q#f", "foo:  x  ", "foo:a b", "foo:a\u00fc", "blob:https://a.com/uuid", "blob:nope", "x:/./a", "x://h/./a/../b", "x:a/../b", "foo:/", "foo://h/a b",
  "web+demo:/x", "a.b-c+d:e", "foo:a?b c#d e", "foo://a.b.C/D",
  // not URLs
  "", " ", "http", "http:", "https:", "ws:", "1http://a", "-http://a", "h t t p://a", ":a", "//", "/", "x", "../x", "#f", "?q", "nope", "www.example.com",
];

const relInputs: string[] = [
  "", "x", "/x", "//h", "//h/p", "///p", "?q", "#f", "../x", "./x", "x/../y", "a/b?c#d", "\\x", "\\\\h\\p", "http:x", "http:/x", "http://other", "https:x",
  "foo:x", "?", "#", "x?", "x#", "..", ".", "../..", "/..", "//u@h:81/x", "//", "/\\h/p", "x y", "?a b", "#a b", "%2e%2e/x", "a//b", "/a/./b",
];
const bases: string[] = [
  "http://a/b/c/d?e#f", "http://u:p@h:81/p/q?r", "http://a/b/", "http://a", "http://a?x", "foo://h/p/q?r", "foo:/p/q", "foo://h", "mailto:a@b",
  "data:x", "https://a/b/c/", "ws://h/a/b",
];

export function urls(): void {
  const out: string[] = [];
  for (const s of absolute) {
    out.push(`new URL(${JSON.stringify(s)}) -> ${show(() => describe(new URL(s)))}`);
    out.push(`canParse(${JSON.stringify(s)}) -> ${show(() => URL.canParse(s))}`);
  }
  for (const b of bases) {
    // Node 26.4.0 (ada) accepts a relative input that carries a fragment against a base with an opaque path
    // (`new URL("x#y", "foo:bar")` is "foo:bar/x#y", while `new URL("x", "foo:bar")` throws). The specification makes every one of
    // them a failure unless the input STARTS with "#", and the guest follows the specification; those pairs are left out of the
    // comparison and listed in DIVERGENCES.md.
    const opaque = /^(mailto|data):/.test(b);
    for (const r of relInputs) {
      if (opaque && r.indexOf("#") > 0) continue;
      out.push(`new URL(${JSON.stringify(r)}, ${JSON.stringify(b)}) -> ${show(() => describe(new URL(r, b)))}`);
    }
    out.push(`canParse(x, ${JSON.stringify(b)}) -> ${show(() => URL.canParse("x", b))}`);
  }
  // A base that is itself invalid, and an absolute input that ignores its base.
  out.push(`bad base -> ${show(() => describe(new URL("x", "nope")))}`);
  out.push(`absolute ignores base -> ${show(() => describe(new URL("http://b/", "http://a/")))}`);
  out.push(`undefined base -> ${show(() => describe(new URL("http://b/", undefined)))}`);
  out.push(`no arguments -> ${show(() => (URL as any)())}`);
  out.push(`no arguments (new) -> ${show(() => new (URL as any)())}`);
  out.push(`URL.parse ok -> ${show(() => describe((URL as any).parse("http://a/b?c#d")))}`);
  out.push(`URL.parse bad -> ${show(() => (URL as any).parse("nope"))}`);
  out.push(`URL.parse relative -> ${show(() => describe((URL as any).parse("../x", "http://a/b/c/d")))}`);
  // Conversions: the argument is stringified, and a lone surrogate becomes U+FFFD.
  out.push(`object arg -> ${show(() => describe(new URL({ toString: () => "http://a/o" } as any)))}`);
  out.push(`number arg -> ${show(() => describe(new URL(5 as any)))}`);
  out.push(`null arg -> ${show(() => describe(new URL(null as any, "http://a/")))}`);
  out.push(`lone surrogate in query -> ${show(() => describe(new URL("http://a/?\ud800")))}`);
  // String conversions and JSON.
  const u = new URL("http://u:p@Host:81/a/b?c=d#e");
  out.push(`String -> ${String(u)} | toString -> ${u.toString()} | JSON -> ${JSON.stringify({ u })} | tag -> ${Object.prototype.toString.call(u)}`);
  out.push(`keys -> ${JSON.stringify(Object.keys(u))} | spread -> ${JSON.stringify({ ...u })}`);
  out.push(`brand -> ${show(() => Object.getOwnPropertyDescriptor(URL.prototype, "href")!.get!.call({}))}`);
  // The two setters that are supported, and how they reach searchParams.
  const s = new URL("http://a/p?x=1&y=2#h");
  const log = (label: string) => out.push(`${label}: ${describe(s)} | sp=${s.searchParams.toString()} | size=${s.searchParams.size}`);
  log("start");
  s.search = "?q=1 2&r=ü"; log("search = ?q=1 2&r=ü");
  s.search = "z=3"; log("search = z=3");
  s.search = ""; log("search = ''");
  s.search = "?"; log("search = ?");
  s.hash = "#a b"; log("hash = #a b");
  s.hash = "x"; log("hash = x");
  s.hash = ""; log("hash = ''");
  s.search = "\t?a\n=b\r"; log("search with tab/newline");
  const spq = s.searchParams;
  spq.append("k", "v w"); log("append");
  spq.delete("a"); spq.delete("k"); log("delete all");
  spq.set("n", "ü"); log("set");
  out.push(`searchParams is stable -> ${String(s.searchParams === spq)}`);
  const op = new URL("mailto:a@b?x=1  ");
  out.push(`opaque: ${describe(op)}`);
  op.searchParams.delete("x"); out.push(`opaque after delete: ${describe(op)}`);
  op.hash = "f"; op.hash = ""; out.push(`opaque after hash: ${describe(op)}`);
  // The downstream symptom: how zod's z.url() decides. It calls new URL(input) inside a try/catch and then looks at the hostname.
  for (const input of ["https://example.com", "http://localhost:3000/x", "nope", "ftp://a.b/c", "javascript:alert(1)", "mailto:a@b.c", "http://", "https://", "//a.b",
                       "https://example.com/a b", "HTTP://EXAMPLE.COM", "https://user:pw@a.b/", "http://1.2.3.4/", "https://-bad-.com"]) {
    let verdict: string;
    try { const x = new URL(input); verdict = "valid " + x.protocol + "//" + x.hostname; } catch (e) { verdict = "invalid"; }
    out.push(`zod-like ${JSON.stringify(input)} -> ${verdict}`);
  }
  __cleat_result(JSON.stringify(out));
}
