// file: URLs, compared with Node (cleat#3898). cleat-review's probe found 19 silent wrong answers in the first version of
// the file: handling, all of them around Windows drive letters, backslash separators and a relative path against a file
// base -- shapes the main corpus did not carry. This file is the matrix those rules are held to: every absolute form, and
// every relative form against seven file bases, each compared through every getter.
declare function __cleat_result(json: string): void;

const show = (f: () => unknown): string => {
  try { return String(f()); } catch (e) { return "throws " + (e as Error).name; }
};

const describe = (u: any): string => JSON.stringify([u.href, u.origin, u.protocol, u.username, u.password, u.host, u.hostname,
  u.port, u.pathname, u.search, u.hash]);

const absolute: string[] = [
  "file:///C:/x", "file:///C|/x", "file:/C|/x", "file:/C:/x", "file:C:/x", "file:C|/x", "file:///c:/x", "file:///C:/",
  "file:///C:/a/../../b", "file:///C:/a/../../../b", "file:///C:/..", "file:///C:/a/..", "file:///C:/a/.", "file:////C:/x",
  "file:///C:/a/%2e%2e/b", "file:///C%3A/x", "file:///C:/x y", "file:///C:.", "file:///1:/x", "file:///C:/|/x",
  "file://server/share/x", "file:\\\\server\\share\\x", "file:\\\\server\\share", "file://server", "file://server/",
  "file://HOST/x", "file://EXAMPLE.COM/x", "file://localhost/x", "file://LOCALHOST/x", "file://localhost", "file://localhost/",
  "file://", "file:///", "file:////a", "file://///a", "file:///a\\b", "file:///a\\b/c", "file:///a b", "file:///a%20b",
  "file:///a?q#f", "file:///a/b/../c", "file:/a/./b", "file:x", "file:x/y", "file:./x", "file:../x", "file:..", "file:.",
  "file:/x", "file:?q", "file:#f", "file:", "file:///a%2fb", "file:///a%2eb", "file://[::1]/x", "file://[::1]:80/x",
  "file://user@host/x", "file://host:80/x", "FILE:///C:/X", "file:///C:/x?q#f", "file:///C:/x#f", "file:///C:/x?q",
  "file:///C:/a/b/../../..", "file:///C:/../a", "file:///C:/|", "file:///|:/x",
  // Mixed separators after the scheme: either separator opens the file-slash state, so a mixed pair is an authority.
  "file:/\\C:/x", "file:\\/C:/x", "file:/\\h/x", "file:\\/h/x", "file:/\\h\\x", "file://localhost", "file:/\\localhost",
  "file:///C:x/./y", "file:///C:x",
];

const bases: string[] = ["file:///C:/a/b", "file:///C:/a/", "file:///a/b/", "file://host/a/b", "file:///", "file:/C:/", "file://localhost/C:/a/b",
  "file:///C:", "file:///C:/dir/file", "file://host/C:/d/f", "file://C:/x", "file:///c:/", "file:///[::1]/a"];
// An input that carries its own scheme still resolves against a base when it does not name one of its own.
const withBase: string[] = ["file:/x", "file:///x", "file://", "file:///", "file://host/y", "file:x", "file:..", "file:.",
  "file:/C:/x", "file://h/p", "file:?q", "file:#f", "file:/x/y", "file:///C:/x"];
const rels: string[] = [
  "", "x", "/x", "//h/p", "//localhost/p", "//C:/x", "//C|/x", "D|/y", "D:/y", "D:", "x/../y", "../x", "..", ".",
  "?q", "#f", "\\x", "\\\\srv\\s\\x", "C:/x", "C|/x", "/C:/x", "/C|/x", "%2e%2e/x", "./x/./y", "x y", "x?q", "x#f",
  "//", "/", "\\\\", "./D:/y", "..\\D:\\y", "..\\x", "../..", "x", "D", "D|x", "C:x", "/x/y",
  "//h", "/\\h\\x", "\\/h/x", "/C:x", "/C|:x", "./C:x", "//localhost", "\\/localhost", "//LOCALHOST",
];

export function urlsFile(): void {
  const out: string[] = [];
  for (const s of absolute) {
    out.push(`new URL(${JSON.stringify(s)}) -> ${show(() => describe(new URL(s)))}`);
    out.push(`canParse(${JSON.stringify(s)}) -> ${show(() => URL.canParse(s))}`);
  }
  for (const b of bases) {
    for (const r of rels) {
      out.push(`new URL(${JSON.stringify(r)}, ${JSON.stringify(b)}) -> ${show(() => describe(new URL(r, b)))}`);
    }
    for (const r of withBase) {
      out.push(`new URL(${JSON.stringify(r)}, ${JSON.stringify(b)}) -> ${show(() => describe(new URL(r, b)))}`);
    }
  }
  __cleat_result(JSON.stringify(out));
}

// The one shape where Node's versions disagree with each other: shortening a file: path whose only segment starts with a
// drive letter but is longer than two code points. The specification pops it, Node 24 (which CI pins) does the same, and
// Node 26 keeps it -- so it is compared against neither and pinned here. `urls_file.ts` excludes it for that reason and
// DIVERGENCES.md row 2 records it.
export function urlsFileSpec(): void {
  const out: string[] = [];
  for (const s of ["file:///C:x/..", "file:///C:x/../y", "file:///h:1/..", "file:///a:/..", "file:///C:/..", "file:///C:/a/.."]) {
    out.push(`${s} -> ${show(() => new URL(s).href)}`);
  }
  __cleat_result(JSON.stringify(out));
}
