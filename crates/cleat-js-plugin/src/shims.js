// Determinism shims. Evaluated once, at plugin initialisation, after the __cleat_* bindings exist and before
// any user code. Each shim is gated by name so a CONTROL build can remove exactly one (CLEAT_SHIMS_OFF=date,random,...),
// which is how "remove the shim; the test must fail" is run rather than asserted.
(function () {
  "use strict";
  const g = globalThis;
  const off = String(g.__cleat_shims_off || "").split(",");
  const on = (name) => off.indexOf(name) < 0;

  // Date: now and the no-argument constructor read the HOST's recorded clock (cleat_now), not the WASI clock. Dates built
  // from an explicit value or string are pure functions of their arguments and are left alone.
  if (on("date")) {
    const D = g.Date;
    const now = () => __cleat_now();
    const CleatDate = function Date(...args) {
      if (!new.target) return new D(now()).toString();
      if (args.length === 0) return new D(now());
      return new D(...args);
    };
    CleatDate.prototype = D.prototype;
    Object.defineProperty(D.prototype, "constructor", { value: CleatDate, writable: true, configurable: true });
    Object.setPrototypeOf(CleatDate, D);
    CleatDate.now = now;
    CleatDate.UTC = D.UTC;
    CleatDate.parse = D.parse;
    g.Date = CleatDate;
  }

  // Date setters (cleat#3829). Given a non-finite argument QuickJS answers NaN but LEAVES THE DATE UNCHANGED; the specification
  // makes the date Invalid (a setter returns the new time value, so NaN means the result is not a time). date-fns builds its dates
  // with setUTCFullYear, so `isValid(parseISO("nope"))` was true in the guest: a plausible, silent, wrong answer. The original
  // does all the argument conversion and every other case; this only completes the one it gets wrong, by setting the time to
  // NaN when the original says NaN. An already-invalid receiver, and setFullYear on one (which the specification treats as time
  // value +0), are the original's and unchanged: typescript_conformance_test.go compares all of it with Node.
  if (on("datesetters")) {
    const P = g.Date.prototype;
    const setTime = P.setTime;
    const names = ["setMilliseconds", "setUTCMilliseconds", "setSeconds", "setUTCSeconds", "setMinutes", "setUTCMinutes",
                   "setHours", "setUTCHours", "setDate", "setUTCDate", "setMonth", "setUTCMonth", "setFullYear", "setUTCFullYear"];
    for (const name of names) {
      const original = P[name];
      const patched = { [name](...args) {
        const result = original.apply(this, args);
        if (result !== result) setTime.call(this, NaN);
        return result;
      } }[name];
      Object.defineProperty(patched, "length", { value: original.length, configurable: true });
      Object.defineProperty(P, name, { value: patched, writable: true, configurable: true, enumerable: false });
    }
  }

  // URL and URLSearchParams (cleat#3829). QuickJS has neither, so `new URL(x)` was a ReferenceError; zod's z.url() calls it
  // inside a try/catch, which turned that into "invalid" for every URL: a plausible, silent, wrong answer. This is the WHATWG
  // algorithm for the part of it that can be done exactly: the special schemes (http, https, ws, wss, ftp, file) and generic
  // ones, relative resolution against a base, percent-encoding, IPv4 and IPv6 hosts, URLSearchParams in full, and the `search`
  // and `hash` setters. Everything else THROWS rather than guess (internationalised hosts, and the other setters), because a
  // value that is plausible and wrong is the failure this exists to remove. typescript_conformance_test.go runs a
  // corpus through this and through Node and compares every getter.
  if (on("url") && typeof g.URL === "undefined") {
    // Membership is what makes a scheme "special" (backslash is a separator, the host is parsed, the query uses the special
    // encode set), so file: belongs here even though it has no default port -- the value is never compared against a port
    // because a file: URL has none.
    const DEFAULT_PORT = Object.assign(Object.create(null), { ftp: 21, file: null, http: 80, https: 443, ws: 80, wss: 443 });
    const unsupported = (what) => new Error("cleat: URL: " + what + " is not supported in the guest");
    const usv = (v) => `${v}`.replace(/[\ud800-\udbff](?![\udc00-\udfff])|(?<![\ud800-\udbff])[\udc00-\udfff]/g, "�");
    const HEX = "0123456789ABCDEF";

    // Percent-encode sets, as predicates on a code point below 0x80 (everything above is always encoded).
    const C0 = (c) => c <= 0x1f || c > 0x7e;
    const FRAGMENT = (c) => C0(c) || c === 0x20 || c === 0x22 || c === 0x3c || c === 0x3e || c === 0x60;
    const QUERY = (c) => C0(c) || c === 0x20 || c === 0x22 || c === 0x23 || c === 0x3c || c === 0x3e;
    const SPECIAL_QUERY = (c) => QUERY(c) || c === 0x27;
    const PATH = (c) => QUERY(c) || c === 0x3f || c === 0x5e || c === 0x60 || c === 0x7b || c === 0x7d;
    const USERINFO = (c) => PATH(c) || c === 0x2f || c === 0x3a || c === 0x3b || c === 0x3d || c === 0x40 ||
      c === 0x5b || c === 0x5c || c === 0x5d || c === 0x7c;

    const utf8 = (str) => {
      const out = [];
      for (const ch of str) {
        let c = ch.codePointAt(0);
        if (c >= 0xd800 && c <= 0xdfff) c = 0xfffd;
        if (c < 0x80) out.push(c);
        else if (c < 0x800) out.push(0xc0 | c >> 6, 0x80 | c & 63);
        else if (c < 0x10000) out.push(0xe0 | c >> 12, 0x80 | c >> 6 & 63, 0x80 | c & 63);
        else out.push(0xf0 | c >> 18, 0x80 | c >> 12 & 63, 0x80 | c >> 6 & 63, 0x80 | c & 63);
      }
      return out;
    };
    const pct = (str, inSet) => {
      let out = "";
      for (const b of utf8(str)) out += b < 0x80 && !inSet(b) ? String.fromCharCode(b) : "%" + HEX[b >> 4] + HEX[b & 15];
      return out;
    };
    // The WHATWG UTF-8 decoder (without BOM handling): an invalid sequence is one U+FFFD per maximal subpart.
    const utf8Decode = (bytes) => {
      let out = "", cp = 0, seen = 0, needed = 0, lower = 0x80, upper = 0xbf;
      for (let i = 0; i < bytes.length;) {
        const b = bytes[i];
        if (needed === 0) {
          i++;
          if (b <= 0x7f) out += String.fromCharCode(b);
          else if (b >= 0xc2 && b <= 0xdf) { needed = 1; cp = b & 0x1f; }
          else if (b >= 0xe0 && b <= 0xef) { if (b === 0xe0) lower = 0xa0; if (b === 0xed) upper = 0x9f; needed = 2; cp = b & 0xf; }
          else if (b >= 0xf0 && b <= 0xf4) { if (b === 0xf0) lower = 0x90; if (b === 0xf4) upper = 0x8f; needed = 3; cp = b & 7; }
          else out += "�";
        } else if (b < lower || b > upper) {
          cp = needed = seen = 0; lower = 0x80; upper = 0xbf; out += "�";
        } else {
          i++; lower = 0x80; upper = 0xbf; cp = cp << 6 | b & 0x3f;
          if (++seen === needed) { out += String.fromCodePoint(cp); cp = needed = seen = 0; }
        }
      }
      return needed !== 0 ? out + "�" : out;
    };
    const formDecode = (str) => {
      const bytes = utf8(str.replace(/\+/g, " ")), out = [];
      for (let i = 0; i < bytes.length; i++) {
        const hex = bytes[i] === 0x25 ? String.fromCharCode(bytes[i + 1] || 0, bytes[i + 2] || 0) : "";
        if (/^[0-9a-fA-F]{2}$/.test(hex)) { out.push(parseInt(hex, 16)); i += 2; }
        else out.push(bytes[i]);
      }
      return utf8Decode(out);
    };
    const formEncode = (str) => {
      let out = "";
      for (const b of utf8(str)) {
        if (b === 0x20) out += "+";
        else if ((b >= 0x30 && b <= 0x39) || (b >= 0x41 && b <= 0x5a) || (b >= 0x61 && b <= 0x7a) || b === 0x2a || b === 0x2d || b === 0x2e || b === 0x5f) out += String.fromCharCode(b);
        else out += "%" + HEX[b >> 4] + HEX[b & 15];
      }
      return out;
    };

    const v4num = (s) => {
      if (s === "") return NaN;
      let r = 10;
      if (s.length >= 2 && (s[0] === "0") && (s[1] === "x" || s[1] === "X")) { s = s.slice(2); r = 16; }
      else if (s.length >= 2 && s[0] === "0") { s = s.slice(1); r = 8; }
      if (s === "") return 0;
      return (r === 16 ? /^[0-9a-fA-F]+$/ : r === 8 ? /^[0-7]+$/ : /^[0-9]+$/).test(s) ? parseInt(s, r) : NaN;
    };
    // The IPv6 parser and serialiser of the URL specification. `ipv6` returns the eight 16-bit pieces, or null; `ipv6Str`
    // writes them back the way the specification does (lowercase hex, the longest run of two or more zero pieces compressed,
    // no leading zeros) -- and, because the same routine writes an embedded IPv4 tail back as hex pairs, `[::ffff:1.2.3.4]`
    // becomes `[::ffff:102:304]`, which is what Node prints.
    const ipv6 = (input) => {
      const address = [0, 0, 0, 0, 0, 0, 0, 0];
      let pieceIndex = 0, compress = null, pointer = 0;
      const n = input.length;
      if (input[pointer] === ":") {
        if (input[pointer + 1] !== ":") return null;
        pointer += 2; pieceIndex++; compress = pieceIndex;
      }
      while (pointer < n) {
        if (pieceIndex === 8) return null;
        if (input[pointer] === ":") {
          if (compress !== null) return null;
          pointer++; pieceIndex++; compress = pieceIndex; continue;
        }
        let value = 0, length = 0;
        while (length < 4 && pointer < n && /[0-9A-Fa-f]/.test(input[pointer])) { value = value * 16 + parseInt(input[pointer], 16); pointer++; length++; }
        if (pointer < n && input[pointer] === ".") {
          if (length === 0) return null;
          pointer -= length;
          if (pieceIndex > 6) return null;
          let numbersSeen = 0;
          while (pointer < n) {
            let ipv4Piece = null;
            if (numbersSeen > 0) {
              if (input[pointer] === "." && numbersSeen < 4) pointer++;
              else return null;
            }
            if (pointer >= n || !/[0-9]/.test(input[pointer])) return null;
            while (pointer < n && /[0-9]/.test(input[pointer])) {
              const number = parseInt(input[pointer], 10);
              if (ipv4Piece === null) ipv4Piece = number;
              else if (ipv4Piece === 0) return null;
              else ipv4Piece = ipv4Piece * 10 + number;
              if (ipv4Piece > 255) return null;
              pointer++;
            }
            address[pieceIndex] = address[pieceIndex] * 256 + ipv4Piece;
            numbersSeen++;
            if (numbersSeen === 2 || numbersSeen === 4) pieceIndex++;
          }
          if (numbersSeen !== 4) return null;
          break;
        } else if (pointer < n && input[pointer] === ":") {
          pointer++;
          if (pointer >= n) return null;
        } else if (pointer < n) return null;
        address[pieceIndex] = value;
        pieceIndex++;
      }
      if (compress !== null) {
        // Move the pieces after the compressed run to the end of the address.
        let swaps = pieceIndex - compress;
        pieceIndex = 7;
        while (pieceIndex !== 0 && swaps > 0) {
          const t = address[pieceIndex];
          address[pieceIndex] = address[compress + swaps - 1];
          address[compress + swaps - 1] = t;
          pieceIndex--; swaps--;
        }
      } else if (pieceIndex !== 8) return null;
      return address;
    };
    const ipv6Str = (address) => {
      let idx = null, longest = 1;
      for (let i = 0; i < 8;) {
        if (address[i] !== 0) { i++; continue; }
        let j = i;
        while (j < 8 && address[j] === 0) j++;
        if (j - i > longest) { longest = j - i; idx = i; }
        i = j;
      }
      let out = "", ignore0 = false;
      for (let i = 0; i < 8; i++) {
        if (ignore0 && address[i] === 0) continue;
        else if (ignore0) ignore0 = false;
        if (idx === i) { out += i === 0 ? "::" : ":"; ignore0 = true; continue; }
        out += address[i].toString(16);
        if (i !== 7) out += ":";
      }
      return out;
    };

    const parseHost = (str, special) => {
      if (!special) return /[\x00\t\n\r #\/:<>?@[\\\]^|]/.test(str) ? null : pct(str, C0);
      let d = str.replace(/%([0-9A-Fa-f]{2})/g, (_, h) => {
        const b = parseInt(h, 16);
        if (b >= 0x80) throw unsupported("a non-ASCII host");
        return String.fromCharCode(b);
      });
      if (/[^\x00-\x7f]/.test(d)) throw unsupported("a non-ASCII host");
      d = d.toLowerCase();
      if (/(^|\.)xn--/.test(d)) throw unsupported("an internationalised (xn--) host");
      if (d === "" || /[\x00-\x20#%\/:<>?@[\\\]^|\x7f]/.test(d)) return null;
      const parts = d.split(".");
      if (parts[parts.length - 1] === "" && parts.length > 1) parts.pop();
      const last = parts[parts.length - 1];
      if (last === "" || !(/^[0-9]+$/.test(last) || !Number.isNaN(v4num(last)))) return d;
      // The last label is a number: this is an IPv4 address, and not being one is a failure.
      if (parts.length > 4) return null;
      const nums = parts.map(v4num);
      if (nums.some((n) => Number.isNaN(n))) return null;
      for (let i = 0; i < nums.length - 1; i++) if (nums[i] > 255) return null;
      if (nums[nums.length - 1] >= Math.pow(256, 5 - nums.length)) return null;
      let ip = nums[nums.length - 1];
      for (let i = 0; i < nums.length - 1; i++) ip += nums[i] * Math.pow(256, 3 - i);
      return [ip / 16777216 | 0, ip / 65536 & 255, ip / 256 & 255, ip & 255].join(".");
    };

    // The path state: every separator ends a segment, so a leading separator leaves an empty first segment. `applyPath` is the
    // path-START state -- it consumes one leading separator first -- and is what every caller but a file: URL wants. `file`
    // turns on the two rules a file: path has and no other does: a Windows drive letter is written with ":" (never "|") when
    // it is the first segment, and ".." never removes it.
    const isDriveLetter = (str) => str.length === 2 && /[A-Za-z]/.test(str[0]) && (str[1] === ":" || str[1] === "|");
    // The specification distinguishes "is a Windows drive letter" (a two-code-point buffer) from "starts with a Windows
    // drive letter", which also requires the third code point to be end, a separator, "?" or "#". So `C:x` does NOT start
    // with one, and a base's drive letter is inherited in front of it.
    const startsWithDrive = (str) => isDriveLetter(str.slice(0, 2)) && (str.length === 2 || /[\/\\?#]/.test(str[2]));
    const pathState = (path, str, special, file) => {
      const segs = str.split(special ? /[\/\\]/ : "/");
      for (let i = 0; i < segs.length; i++) {
        const last = i === segs.length - 1, lc = segs[i].toLowerCase();
        if (lc === ".." || lc === ".%2e" || lc === "%2e." || lc === "%2e%2e") {
          // Only a two-code-point normalized drive letter survives shortening; `C:x` is popped. Node 24 does the same, and
          // Node 26 does not -- the corpus is compared against the version CI pins (24) and that shape is excluded from it,
          // with the specification's answer pinned guest-side (DIVERGENCES.md row 2).
          if (!(file && path.length === 1 && isDriveLetter(path[0])) && path.length) path.pop();
          if (last) path.push("");
        } else if (lc === "." || lc === "%2e") { if (last) path.push(""); }
        else {
          let seg = segs[i];
          if (file && path.length === 0 && isDriveLetter(seg) && seg[1] === "|") seg = seg[0] + ":";
          path.push(pct(seg, PATH));
        }
      }
      return path;
    };
    const applyPath = (path, str, special) => pathState(path, str.length && (str[0] === "/" || (special && str[0] === "\\")) ? str.slice(1) : str, special);
    const pathFrom = (rest, special) => rest === "" ? (special ? [""] : []) : applyPath([], rest, special);

    // The authority: [user[:pass]@]host[:port]. Returns null on failure.
    const parseAuthority = (str, scheme, special) => {
      let user = "", pass = "", hostport = str;
      const at = str.lastIndexOf("@");
      if (at >= 0) {
        const cred = str.slice(0, at), colon = cred.indexOf(":");
        hostport = str.slice(at + 1);
        user = pct(colon < 0 ? cred : cred.slice(0, colon), USERINFO);
        pass = colon < 0 ? "" : pct(cred.slice(colon + 1), USERINFO);
        if (hostport === "") return null;
      }
      let host, port = null;
      if (hostport[0] === "[") {
        // A bracketed host is an IPv6 literal for every scheme, special or not; the colon inside the brackets does not
        // start the port, so the authority is split here rather than below.
        const close = hostport.indexOf("]");
        if (close < 0) return null;
        const addr = ipv6(hostport.slice(1, close));
        if (addr === null) return null;
        host = "[" + ipv6Str(addr) + "]";
        const after = hostport.slice(close + 1);
        if (after !== "") {
          const portStr = after[0] === ":" ? after.slice(1) : null;
          if (portStr === null || !/^[0-9]*$/.test(portStr)) return null;
          if (portStr !== "") {
            port = parseInt(portStr, 10);
            if (port > 65535) return null;
            if (port === DEFAULT_PORT[scheme]) port = null;
          }
        }
      } else {
        const ci = hostport.indexOf(":"), hostStr = ci < 0 ? hostport : hostport.slice(0, ci), portStr = ci < 0 ? "" : hostport.slice(ci + 1);
        if (ci >= 0 && hostStr === "") return null;
        if (special && hostStr === "") return null;
        host = parseHost(hostStr, special);
        if (host === null) return null;
        if (portStr !== "") {
          if (!/^[0-9]+$/.test(portStr)) return null;
          port = parseInt(portStr, 10);
          if (port > 65535) return null;
          if (port === DEFAULT_PORT[scheme]) port = null;
        }
      }
      return { user, pass, host, port };
    };

    // The URL parser. Returns a record, or null for a failure (a TypeError at the API); throws for what is out of scope.
    const parse = (raw, base) => {
      let s = raw.replace(/^[\x00-\x20]+|[\x00-\x20]+$/g, "").replace(/[\t\n\r]/g, "");
      let frag = null, query = null;
      const hi = s.indexOf("#");
      if (hi >= 0) { frag = pct(s.slice(hi + 1), FRAGMENT); s = s.slice(0, hi); }
      const qi = s.indexOf("?");
      if (qi >= 0) { query = s.slice(qi + 1); s = s.slice(0, qi); }
      const encQuery = (special) => query === null ? null : pct(query, special ? SPECIAL_QUERY : QUERY);
      const authorityAndPath = (afterSlashes, scheme, special) => {
        const m = (special ? /[\/\\]/ : /\//).exec(afterSlashes), i = m ? m.index : afterSlashes.length;
        const a = parseAuthority(afterSlashes.slice(0, i), scheme, special);
        if (a === null) return null;
        return { scheme, user: a.user, pass: a.pass, host: a.host, port: a.port, path: pathFrom(afterSlashes.slice(i), special), query: encQuery(special), frag };
      };

      // file: URLs. The host is the empty string unless an authority names something other than "localhost" (or a Windows
      // drive letter, which belongs to the path); the path is in the path state, so `file:///a` and `file:////a` differ in the
      // first segment being empty; a drive letter is normalised and never removed by ".."; and a relative path against a
      // file base inherits that base's drive letter when it does not name one.
      const fileURL = (rest, base) => {
        const baseFile = base && base.scheme === "file" ? base : null;
        const drive = baseFile && baseFile.path.length && isDriveLetter(baseFile.path[0]) ? baseFile.path[0] : null;
        const done = (host, path) => ({ scheme: "file", user: "", pass: "", host, port: null, path, query: encQuery(true), frag });
        // The file path-start state: one leading separator is consumed, and only when there is none does a base's drive
        // letter become the first segment.
        const startPath = (path, str, inherit) => {
          if (str[0] === "/" || str[0] === "\\") return pathState(path, str.slice(1), true, true);
          if (inherit && drive) path.push(drive);
          return pathState(path, str, true, true);
        };
        if (rest.length >= 2 && /[\/\\]/.test(rest[0]) && /[\/\\]/.test(rest[1])) {
          const after = rest.slice(2);
          if (after[0] === "/" || after[0] === "\\") return done("", pathState([], after.slice(1), true, true));
          let i = 0;
          while (i < after.length && after[i] !== "/" && after[i] !== "\\") i++;
          const authority = after.slice(0, i), tail = after.slice(i);
          if (authority === "") return done("", pathState([], "", true, true));
          if (isDriveLetter(authority)) return done("", pathState([], authority + tail, true, true));
          let host = "";
          if (authority !== "" && authority.toLowerCase() !== "localhost") {
            if (authority[0] === "[") {
              const close = authority.indexOf("]");
              if (close < 0 || close + 1 !== authority.length) return null;
              const addr = ipv6(authority.slice(1, close));
              if (addr === null) return null;
              host = "[" + ipv6Str(addr) + "]";
            } else {
              host = parseHost(authority, true);
              if (host === null) return null;
            }
          }
          return done(host, startPath([], tail, false));
        }
        if (rest[0] === "/" || rest[0] === "\\") {
          // A single leading separator keeps the base's authority -- and, for a drive-letter base, its drive, unless the
          // input names one itself. `file:/x` against `file:///C:/a/b` is `file:///C:/x`, as a bare `/x` is.
          const body = rest.slice(1);
          return done(baseFile ? baseFile.host : "",
            pathState([], drive && !startsWithDrive(body) ? drive + "/" + body : body, true, true));
        }
        if (rest === "" && baseFile) {
          return { scheme: "file", user: "", pass: "", host: baseFile.host, port: null, path: baseFile.path.slice(), query: query !== null ? encQuery(true) : baseFile.query, frag };
        }
        if (baseFile) {
          // A relative path that names a drive letter replaces the base's path outright (`D|/y` against `C:/a/b` is
          // `D:/y`, not `C:/a/D|/y`); anything else is appended below the base's directory.
          if (startsWithDrive(rest)) return done(baseFile.host, pathState([], rest, true, true));
          const path = baseFile.path.slice();
          if (path.length > 1 || (path.length === 1 && !isDriveLetter(path[0]))) path.pop();
          return done(baseFile.host, pathState(path, rest, true, true));
        }
        return done("", pathState([], rest, true, true));
      };

      const sm = /^([A-Za-z][A-Za-z0-9+.\-]*):/.exec(s);
      if (sm) {
        const scheme = sm[1].toLowerCase(), special = scheme in DEFAULT_PORT;
        let rest = s.slice(sm[0].length);
        if (scheme === "file") return fileURL(rest, base);
        if (special) {
          if (base && base.scheme === scheme && rest.slice(0, 2) !== "//") return relative(rest);
          return authorityAndPath(rest.replace(/^[\/\\]*/, ""), scheme, true);
        }
        if (rest.slice(0, 2) === "//") return authorityAndPath(rest.slice(2), scheme, false);
        if (rest[0] === "/") return { scheme, user: "", pass: "", host: null, port: null, path: pathFrom(rest, false), query: encQuery(false), frag };
        return { scheme, user: "", pass: "", host: null, port: null, path: pct(rest, C0), query: encQuery(false), frag };
      }
      return relative(s);

      function relative(rel) {
        if (!base) return null;
        // A file base has its own resolution rules (the drive letter, the backslash separator, localhost folding), so every
        // relative input against one goes back through `fileURL` rather than through the generic path below.
        if (base.scheme === "file") {
          // A single leading separator keeps the base's authority, and for a drive-letter base that includes the drive.
          const sep = (c) => c === "/" || c === "\\";
          if (rel.length && sep(rel[0]) && !sep(rel[1])) {
            const drive = base.path.length && isDriveLetter(base.path[0]) ? base.path[0] : null;
            if (drive && !startsWithDrive(rel.slice(1))) return fileURL("/" + drive + rel, base);
          }
          return fileURL(rel, base);
        }
        if (typeof base.path === "string") {
          if (rel === "" && query === null && frag !== null) return Object.assign({}, base, { frag });
          return null;
        }
        const special = base.scheme in DEFAULT_PORT, isSep = (c) => c === "/" || (special && c === "\\");
        if (isSep(rel[0]) && isSep(rel[1])) return authorityAndPath(special ? rel.replace(/^[\/\\]*/, "") : rel.slice(2), base.scheme, special);
        const r = { scheme: base.scheme, user: base.user, pass: base.pass, host: base.host, port: base.port, path: [], query: null, frag };
        if (isSep(rel[0])) { r.path = applyPath([], rel, special); r.query = encQuery(special); return r; }
        r.path = base.path.slice();
        if (rel === "") { r.query = query !== null ? encQuery(special) : base.query; return r; }
        r.path.pop();
        applyPath(r.path, rel, special);
        r.query = encQuery(special);
        return r;
      }
    };

    const serialize = (r) => {
      let out = r.scheme + ":";
      if (r.host !== null) {
        out += "//";
        if (r.user !== "" || r.pass !== "") out += r.user + (r.pass !== "" ? ":" + r.pass : "") + "@";
        out += r.host + (r.port !== null ? ":" + r.port : "");
      } else if (typeof r.path !== "string" && r.path.length > 1 && r.path[0] === "") out += "/.";
      out += typeof r.path === "string" ? r.path : r.path.map((p) => "/" + p).join("");
      if (r.query !== null) out += "?" + r.query;
      if (r.frag !== null) out += "#" + r.frag;
      return out;
    };
    const pathname = (r) => typeof r.path === "string" ? r.path : r.path.map((p) => "/" + p).join("");
    // A URL with an opaque path loses trailing spaces from it when it has neither query nor fragment left.
    const stripOpaque = (r) => { if (typeof r.path === "string" && r.query === null && r.frag === null) r.path = r.path.replace(/ +$/, ""); };

    const RECORDS = new WeakMap(), PARAMS = new WeakMap(), LINKED = new WeakMap();
    const rec = (o) => { const r = RECORDS.get(o); if (!r) throw new TypeError("Illegal invocation"); return r; };
    const st = (o) => { const x = PARAMS.get(o); if (!x) throw new TypeError("Illegal invocation"); return x; };
    const parseList = (query) => {
      const list = [];
      for (const seq of query.split("&")) {
        if (seq === "") continue;
        const i = seq.indexOf("=");
        list.push(i < 0 ? [formDecode(seq), ""] : [formDecode(seq.slice(0, i)), formDecode(seq.slice(i + 1))]);
      }
      return list;
    };
    const listString = (list) => list.map((p) => formEncode(p[0]) + "=" + formEncode(p[1])).join("&");
    const update = (state) => {
      if (!state.url) return;
      const r = rec(state.url), q = listString(state.list);
      r.query = q === "" ? null : q;
      stripOpaque(r);
    };

    const IterProto = Object.create(Object.getPrototypeOf(Object.getPrototypeOf([][Symbol.iterator]())));
    const iterState = new WeakMap();
    Object.defineProperty(IterProto, "next", { configurable: true, writable: true, value: function next() {
      const it = iterState.get(this);
      if (!it) throw new TypeError("Illegal invocation");
      const list = it.state.list;
      if (it.i >= list.length) return { value: undefined, done: true };
      const p = list[it.i++];
      return { value: it.kind === "keys" ? p[0] : it.kind === "values" ? p[1] : [p[0], p[1]], done: false };
    } });
    Object.defineProperty(IterProto, Symbol.toStringTag, { value: "URLSearchParams Iterator", configurable: true });
    const makeIter = (o, kind) => { const it = Object.create(IterProto); iterState.set(it, { state: st(o), i: 0, kind }); return it; };
    const need = (args, n, what) => { if (args.length < n) throw new TypeError("The \"" + what + "\" argument must be specified"); };

    class URLSearchParams {
      constructor(init) {
        const state = { list: [], url: null };
        PARAMS.set(this, state);
        if (init === undefined) return;
        if (init !== null && (typeof init === "object" || typeof init === "function")) {
          if (typeof init[Symbol.iterator] === "function") {
            for (const pair of init) {
              if (pair === null || (typeof pair !== "object" && typeof pair !== "function") || typeof pair[Symbol.iterator] !== "function") throw new TypeError("Each query pair must be an iterable [name, value] tuple");
              const kv = Array.from(pair);
              if (kv.length !== 2) throw new TypeError("Each query pair must be an iterable [name, value] tuple");
              state.list.push([usv(kv[0]), usv(kv[1])]);
            }
          } else {
            for (const k of Reflect.ownKeys(init)) {
              if (Object.getOwnPropertyDescriptor(init, k).enumerable) state.list.push([usv(k), usv(init[k])]);
            }
          }
        } else {
          const str = usv(init);
          state.list = parseList(str[0] === "?" ? str.slice(1) : str);
        }
      }
      get size() { return st(this).list.length; }
      append(name, value) { need(arguments, 2, "name\" and \"value"); const s = st(this); s.list.push([usv(name), usv(value)]); update(s); }
      delete(name, value) {
        need(arguments, 1, "name"); const s = st(this), n = usv(name), v = value === undefined ? undefined : usv(value);
        s.list = s.list.filter((p) => !(p[0] === n && (v === undefined || p[1] === v)));
        update(s);
      }
      get(name) { need(arguments, 1, "name"); const n = usv(name), p = st(this).list.find((q) => q[0] === n); return p ? p[1] : null; }
      getAll(name) { need(arguments, 1, "name"); const n = usv(name); return st(this).list.filter((q) => q[0] === n).map((q) => q[1]); }
      has(name, value) {
        need(arguments, 1, "name"); const n = usv(name), v = value === undefined ? undefined : usv(value);
        return st(this).list.some((p) => p[0] === n && (v === undefined || p[1] === v));
      }
      set(name, value) {
        need(arguments, 2, "name\" and \"value"); const s = st(this), n = usv(name), v = usv(value), i = s.list.findIndex((p) => p[0] === n);
        if (i < 0) s.list.push([n, v]);
        else { s.list[i][1] = v; s.list = s.list.filter((p, j) => j <= i || p[0] !== n); }
        update(s);
      }
      sort() { const s = st(this); s.list.sort((a, b) => a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0); update(s); }
      forEach(cb, thisArg) {
        need(arguments, 1, "callback"); const s = st(this);
        if (typeof cb !== "function") throw new TypeError("The \"callback\" argument must be of type function");
        for (let i = 0; i < s.list.length; i++) cb.call(thisArg, s.list[i][1], s.list[i][0], this);
      }
      keys() { return makeIter(this, "keys"); }
      values() { return makeIter(this, "values"); }
      entries() { return makeIter(this, "entries"); }
      toString() { return listString(st(this).list); }
    }
    Object.defineProperty(URLSearchParams.prototype, Symbol.iterator, { value: URLSearchParams.prototype.entries, writable: true, configurable: true });
    Object.defineProperty(URLSearchParams.prototype, Symbol.toStringTag, { value: "URLSearchParams", configurable: true });

    class URL {
      constructor(url, base) {
        if (arguments.length === 0) throw new TypeError("The \"url\" argument must be specified");
        let b = null;
        if (base !== undefined) { b = parse(usv(base), null); if (b === null) throw new TypeError("Invalid URL"); }
        const r = parse(usv(url), b);
        if (r === null) throw new TypeError("Invalid URL");
        RECORDS.set(this, r);
      }
      static canParse(url, base) {
        if (arguments.length === 0) throw new TypeError("The \"url\" argument must be specified");
        const b = base === undefined ? null : parse(usv(base), null);
        return !(base !== undefined && b === null) && parse(usv(url), b) !== null;
      }
      static parse(url, base) {
        if (arguments.length === 0) throw new TypeError("The \"url\" argument must be specified");
        const b = base === undefined ? null : parse(usv(base), null);
        if (base !== undefined && b === null) return null;
        const r = parse(usv(url), b);
        if (r === null) return null;
        const o = Object.create(URL.prototype);
        RECORDS.set(o, r);
        return o;
      }
      get href() { return serialize(rec(this)); }
      get origin() {
        const r = rec(this);
        if (r.scheme in DEFAULT_PORT) return r.scheme === "file" ? "null" : r.scheme + "://" + r.host + (r.port !== null ? ":" + r.port : "");
        if (r.scheme === "blob") { const inner = parse(pathname(r), null); if (inner && (inner.scheme === "http" || inner.scheme === "https")) return inner.scheme + "://" + inner.host + (inner.port !== null ? ":" + inner.port : ""); }
        return "null";
      }
      get protocol() { return rec(this).scheme + ":"; }
      get username() { return rec(this).user; }
      get password() { return rec(this).pass; }
      get host() { const r = rec(this); return r.host === null ? "" : r.host + (r.port !== null ? ":" + r.port : ""); }
      get hostname() { const h = rec(this).host; return h === null ? "" : h; }
      get port() { const p = rec(this).port; return p === null ? "" : String(p); }
      get pathname() { return pathname(rec(this)); }
      get search() { const q = rec(this).query; return q === null || q === "" ? "" : "?" + q; }
      get searchParams() {
        let sp = LINKED.get(this);
        if (!sp) {
          sp = new URLSearchParams();
          const s = PARAMS.get(sp), q = rec(this).query;
          s.url = this; s.list = q === null ? [] : parseList(q);
          LINKED.set(this, sp);
        }
        return sp;
      }
      get hash() { const f = rec(this).frag; return f === null || f === "" ? "" : "#" + f; }
      set search(v) {
        const r = rec(this); let q = usv(v);
        if (q === "") { r.query = null; stripOpaque(r); }
        else { if (q[0] === "?") q = q.slice(1); r.query = pct(q.replace(/[\t\n\r]/g, ""), r.scheme in DEFAULT_PORT ? SPECIAL_QUERY : QUERY); }
        const sp = LINKED.get(this);
        if (sp) PARAMS.get(sp).list = r.query === null ? [] : parseList(r.query);
      }
      set hash(v) {
        const r = rec(this); let f = usv(v);
        if (f === "") { r.frag = null; stripOpaque(r); }
        else { if (f[0] === "#") f = f.slice(1); r.frag = pct(f.replace(/[\t\n\r]/g, ""), FRAGMENT); }
      }
      toString() { return serialize(rec(this)); }
      toJSON() { return serialize(rec(this)); }
    }
    for (const name of ["href", "protocol", "username", "password", "host", "hostname", "port", "pathname"]) {
      const getter = Object.getOwnPropertyDescriptor(URL.prototype, name).get;
      Object.defineProperty(URL.prototype, name, { get: getter, set(v) { throw unsupported("setting URL." + name); }, enumerable: true, configurable: true });
    }
    for (const name of ["origin", "searchParams", "search", "hash"]) {
      const d = Object.getOwnPropertyDescriptor(URL.prototype, name);
      Object.defineProperty(URL.prototype, name, Object.assign({}, d, { enumerable: true }));
    }
    Object.defineProperty(URL.prototype, Symbol.toStringTag, { value: "URL", configurable: true });
    for (const C of [URL, URLSearchParams]) Object.defineProperty(g, C.name, { value: C, writable: true, configurable: true, enumerable: false });
  }

  // Math.random: a uniform double in [0, 1) from the host's recorded random value (an i64, so reinterpret as unsigned and
  // keep the top 53 bits). Anything that is not a decimal integer is a host answer this shim cannot read (the "suspended"
  // envelope is the one the binding can produce), and it THROWS: returning 0 instead is stable across replays, so it would
  // pass every determinism check while giving user code a constant. The "randomstrict" name exists only so a control build can
  // put the old return-0 behaviour back and show the check tells the two apart.
  if (on("random")) {
    Math.random = function random() {
      const s = __cleat_random();
      if (!/^-?[0-9]+$/.test(s)) {
        if (on("randomstrict")) throw new Error("cleat: Math.random: the host did not return a number: " + String(s).slice(0, 60));
        return 0;
      }
      // i64 -> unsigned by offset (add 2^63), exact in BigInt: [0, 2^64). Do NOT use BigInt.asUintN: in this QuickJS it left the
      // value signed and Math.random returned numbers in [-0.5, 0.5) (found by the range check in phase2_controls.py).
      const u = BigInt(s) + 9223372036854775808n;
      return Number(u >> 11n) / 9007199254740992;
    };
  }

  // Globals that expose wall-clock time, GC timing or an event loop outside the recorded host calls. The Javy 9.1.0 build
  // the pinned Javy defines NONE of them (measured when the pin was chosen), so today this deletes nothing; it is here so a newer Javy
  // that adds one cannot silently reintroduce it. A control for these rows can therefore not fail today, and is reported so.
  if (on("lockdown")) {
    for (const k of ["WeakRef", "FinalizationRegistry", "setTimeout", "setInterval", "setImmediate", "clearTimeout",
                     "clearInterval", "performance", "crypto", "queueMicrotaskTimer"]) {
      if (k in g) delete g[k];
    }
  }

  delete g.__cleat_shims_off;
})();
