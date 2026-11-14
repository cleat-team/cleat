// What the URL shim REFUSES (cleat#3829). Each of these is an input or an operation the guest cannot answer exactly, and the
// requirement is that it THROWS the shim's own error and never returns a value: a plausible wrong answer is the failure the shim
// exists to remove. Only the guest runs this; Node answers all of them, which is the point. Every line must read "loud".
declare function __cleat_result(json: string): void;

const loud = (label: string, f: () => unknown): string => {
  try {
    return `${label}: WRONG, returned ${String(f())}`;
  } catch (e) {
    const m = (e as Error).message;
    return m.indexOf("cleat: URL:") === 0 && m.indexOf("is not supported in the guest") > 0 ? "loud" : `${label}: threw the wrong error: ${m}`;
  }
};

export function urlsLoud(): void {
  const out: string[] = [];
  for (const s of ["file:///a/b", "FILE://host/x", "file:x", "http://[::1]/", "http://[::1]:80/", "http://u@[::1]/", "foo://[::1]/", "http://ü.com", "http://a.ü",
                   "http://xn--mnchen-3ya.de", "http://%C3%BC.com", "http://a.XN--b", "http://a%E2%82%AC.com"]) {
    out.push(loud(`new URL(${JSON.stringify(s)})`, () => new URL(s).href));
    out.push(loud(`canParse(${JSON.stringify(s)})`, () => URL.canParse(s)));
  }
  out.push(loud("a file: base", () => new URL("x", "file:///a/").href));
  const u = new URL("http://a/b?c#d");
  for (const name of ["href", "protocol", "username", "password", "host", "hostname", "port", "pathname"]) {
    out.push(loud(`set ${name}`, () => { (u as any)[name] = "x"; return u.href; }));
  }
  __cleat_result(JSON.stringify(out));
}
