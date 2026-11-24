// What the URL shim REFUSES (cleat#3829, cleat#3898). Each of these is an operation the guest cannot answer exactly, and the
// requirement is that it THROWS the shim's own error and never returns a value: a plausible wrong answer is the failure the shim
// exists to remove. Only the guest runs this; Node accepts or silently ignores every one of them, which is the point. Every line
// must read "loud", and the label after it says which case it was (cleat#4016 holds the setters).
declare function __cleat_result(json: string): void;

const loud = (label: string, f: () => unknown): string => {
  try {
    return `${label}: WRONG, returned ${String(f())}`;
  } catch (e) {
    const m = (e as Error).message;
    return m.indexOf("cleat: URL:") === 0 && m.indexOf("is not supported in the guest") > 0 ? "loud: " + label : `${label}: threw the wrong error: ${m}`;
  }
};

export function urlsLoud(): void {
  const out: string[] = [];
  const u = new URL("http://a/b?c#d");
  for (const name of ["href", "protocol", "username", "password", "host", "hostname", "port", "pathname"]) {
    out.push(loud(`set ${name}`, () => { (u as any)[name] = "x"; return u.href; }));
  }
  __cleat_result(JSON.stringify(out));
}
