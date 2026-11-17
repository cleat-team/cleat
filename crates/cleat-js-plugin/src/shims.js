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
  // and `hash` setters. Internationalised hosts go through the part of IDNA that needs no tables (NFKC, the three non-ASCII
  // dots, Punycode). Everything else THROWS rather than guess (the eight setters), because a value that is plausible and wrong
  // is the failure this exists to remove. typescript_conformance_test.go runs a
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

    // RFC 3492 Punycode. `puny` encodes a label (the caller has already folded and normalised it); `unpuny` decodes one, and
    // exists only so an "xn--" label that is already in the input can be checked rather than trusted.
    const PBASE = 36, PTMIN = 1, PTMAX = 26, PSKEW = 38, PDAMP = 700, PBIAS = 72;
    const pAdapt = (delta, numPoints, first) => {
      delta = first ? Math.floor(delta / PDAMP) : delta >> 1;
      delta += Math.floor(delta / numPoints);
      let k = 0;
      while (delta > ((PBASE - PTMIN) * PTMAX) >> 1) { delta = Math.floor(delta / (PBASE - PTMIN)); k += PBASE; }
      return Math.floor(k + (PBASE - PTMIN + 1) * delta / (delta + PSKEW));
    };
    const pDigit = (d) => String.fromCharCode(d < 26 ? 97 + d : 22 + d);
    const puny = (label) => {
      const cp = Array.from(label, (c) => c.codePointAt(0));
      const out = [];
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
    const pValue = (ch) => {
      const c = ch.charCodeAt(0);
      if (c >= 97 && c <= 122) return c - 97;
      if (c >= 65 && c <= 90) return c - 65;
      if (c >= 48 && c <= 57) return c - 48 + 26;
      return -1;
    };
    const unpuny = (input) => {
      const out = [];
      const delim = input.lastIndexOf("-");
      let pos = 0;
      if (delim > 0) { for (let i = 0; i < delim; i++) out.push(input.charCodeAt(i)); pos = delim + 1; }
      else if (delim === 0) pos = 1;
      let n = 128, i = 0, bias = PBIAS;
      while (pos < input.length) {
        const oldi = i;
        let w = 1;
        for (let k = PBASE; ; k += PBASE) {
          if (pos >= input.length) return null;
          const d = pValue(input[pos++]);
          if (d < 0) return null;
          i += d * w;
          const t = k <= bias ? PTMIN : k >= bias + PTMAX ? PTMAX : k - bias;
          if (d < t) break;
          w *= PBASE - t;
        }
        const len = out.length + 1;
        bias = pAdapt(i - oldi, len, oldi === 0);
        n += Math.floor(i / len);
        i = i % len;
        if (n > 0x10ffff) return null;
        out.splice(i, 0, n);
        i++;
      }
      return out;
    };

    // Measured, not hand-written: the code points Node 26.4.0 accepts in a host label, and the ones it accepts only in an
    // all-right-to-left label (RFC 5893's bidi rule). Both were derived by sweeping every code point through Node --
    // `http://a<cp>b.example/` for ALLOWED and `http://<cp><cp>.example/` for RTL -- and are committed with the sweep that
    // re-derives them. A code point outside ALLOWED is a failed host, which is what Node answers; the RTL class is used by
    // the bidi check below. Keep them as ranges: 729 and 141 respectively.
    const URL_ALLOWED = (
      "9-a,d,21-24,26-39,3b,3d,3f-5a,5c,5f-7b,7d-7e,a1-a7,a9-ae,b0-b3,b5-b7,b9-2d7,2de-377,37b-37f,386-38a,38c,38e-3a" +
      "1,3a3-52f,531-556,559-58a,58d-58f,591-5bd,5bf,5c1-5c2,5c4-5c5,5c7,606-607,609-60a,60c,60e-61a,61d,64b-65f,66a," +
      "670,6d6-6dc,6de-6e4,6e7-6ed,6f0-6f9,711,730-74a,7a6-7b0,7eb-7f3,7f6-7f9,7fd,816-819,81b-823,825-827,829-82d,85" +
      "9-85b,870-88f,897-89f,8b5,8c8-8e1,8e3-983,985-98c,98f-990,993-9a8,9aa-9b0,9b2,9b6-9b9,9bc-9c4,9c7-9c8,9cb-9ce," +
      "9d7,9dc-9dd,9df-9e3,9e6-9fe,a01-a03,a05-a0a,a0f-a10,a13-a28,a2a-a30,a32-a33,a35-a36,a38-a39,a3c,a3e-a42,a47-a4" +
      "8,a4b-a4d,a51,a59-a5c,a5e,a66-a76,a81-a83,a85-a8d,a8f-a91,a93-aa8,aaa-ab0,ab2-ab3,ab5-ab9,abc-ac5,ac7-ac9,acb-" +
      "acd,ad0,ae0-ae3,ae6-af1,af9-aff,b01-b03,b05-b0c,b0f-b10,b13-b28,b2a-b30,b32-b33,b35-b39,b3c-b44,b47-b48,b4b-b4" +
      "d,b55-b57,b5c-b5d,b5f-b63,b66-b77,b82-b83,b85-b8a,b8e-b90,b92-b95,b99-b9a,b9c,b9e-b9f,ba3-ba4,ba8-baa,bae-bb9," +
      "bbe-bc2,bc6-bc8,bca-bcd,bd0,bd7,be6-bfa,c00-c0c,c0e-c10,c12-c28,c2a-c39,c3c-c44,c46-c48,c4a-c4d,c55-c56,c58-c5" +
      "a,c5c-c5d,c60-c63,c66-c6f,c77-c8c,c8e-c90,c92-ca8,caa-cb3,cb5-cb9,cbc-cc4,cc6-cc8,cca-ccd,cd5-cd6,cdc-cde,ce0-" +
      "ce3,ce6-cef,cf1-cf3,d00-d0c,d0e-d10,d12-d44,d46-d48,d4a-d4f,d54-d63,d66-d7f,d81-d83,d85-d96,d9a-db1,db3-dbb,db" +
      "d,dc0-dc6,dca,dcf-dd4,dd6,dd8-ddf,de6-def,df2-df4,e01-e3a,e3f-e5b,e81-e82,e84,e86-e8a,e8c-ea3,ea5,ea7-ebd,ec0-" +
      "ec4,ec6,ec8-ece,ed0-ed9,edc-edf,f00-f47,f49-f6c,f71-f97,f99-fbc,fbe-fcc,fce-fda,1000-10c5,10c7,10cd,10d0-1248," +
      "124a-124d,1250-1256,1258,125a-125d,1260-1288,128a-128d,1290-12b0,12b2-12b5,12b8-12be,12c0,12c2-12c5,12c8-12d6," +
      "12d8-1310,1312-1315,1318-135a,135d-137c,1380-1399,13a0-13f5,13f8-13fd,1400-167f,1681-169c,16a0-16f8,1700-1715," +
      "171f-1736,1740-1753,1760-176c,176e-1770,1772-1773,1780-17dd,17e0-17e9,17f0-17f9,1800-1819,1820-1878,1880-18aa," +
      "18b0-18f5,1900-191e,1920-192b,1930-193b,1940,1944-196d,1970-1974,1980-19ab,19b0-19c9,19d0-19da,19de-1a1b,1a1e-" +
      "1a5e,1a60-1a7c,1a7f-1a89,1a90-1a99,1aa0-1aad,1ab0-1add,1ae0-1aeb,1b00-1b4c,1b4e-1bf3,1bfc-1c37,1c3b-1c49,1c4d-" +
      "1c8a,1c90-1cba,1cbd-1cc7,1cd0-1cfa,1d00-1f15,1f18-1f1d,1f20-1f45,1f48-1f4d,1f50-1f57,1f59,1f5b,1f5d,1f5f-1f7d," +
      "1f80-1fb4,1fb6-1fbc,1fbe,1fc2-1fc4,1fc6-1fcc,1fd0-1fd3,1fd6-1fdb,1fe0-1fec,1fef,1ff2-1ff4,1ff6-1ffc,200b,2010-" +
      "2016,2018-2023,2027,2030-203d,203f-2046,204a-205e,2060-2064,206a-2071,2074-208e,2090-209c,20a0-20c1,20d0-20f0," +
      "2102-2104,2107-2134,2139-218b,2190-2429,2440-244a,2460-2487,249c-2a73,2a75-2b73,2b76-2cf3,2cf9-2d25,2d27,2d2d," +
      "2d30-2d67,2d6f-2d70,2d7f-2d96,2da0-2da6,2da8-2dae,2db0-2db6,2db8-2dbe,2dc0-2dc6,2dc8-2dce,2dd0-2dd6,2dd8-2dde," +
      "2de0-2e5d,2e80-2e99,2e9b-2ef3,2f00-2fd5,3001-303f,3041-3096,3099-309a,309d-30ff,3105-312f,3131-318e,3190-31e5," +
      "31f0-321e,3220-33c1,33c3-33c6,33c8-33d7,33d9-a48c,a490-a4c6,a4d0-a62b,a640-a6f7,a700-a7dc,a7f1-a82c,a830-a839," +
      "a840-a877,a880-a8c5,a8ce-a8d9,a8e0-a953,a95f-a97c,a980-a9cd,a9cf-a9d9,a9de-a9fe,aa00-aa36,aa40-aa4d,aa50-aa59," +
      "aa5c-aac2,aadb-aaf6,ab01-ab06,ab09-ab0e,ab11-ab16,ab20-ab26,ab28-ab2e,ab30-ab6b,ab70-abed,abf0-abf9,ac00-d7a3," +
      "d7b0-d7c6,d7cb-d7fb,f900-fa6d,fa70-fad9,fb00-fb06,fb13-fb17,fb1e,fb29,fbc2-fbd2,fd3e-fd4f,fd90-fd91,fdc8-fdcf," +
      "fdfd-fe11,fe14-fe15,fe17-fe18,fe20-fe2f,fe31-fe46,fe4d-fe51,fe54,fe57-fe5e,fe60-fe63,fe66,fe69,feff,ff01-ff02," +
      "ff04,ff06-ff0e,ff10-ff19,ff1b,ff1d,ff21-ff3a,ff3f-ff5b,ff5d-ffbe,ffc2-ffc7,ffca-ffcf,ffd2-ffd7,ffda-ffdc,ffe0-" +
      "ffe2,ffe4-ffe6,ffe8-ffee,10000-1000b,1000d-10026,10028-1003a,1003c-1003d,1003f-1004d,10050-1005d,10080-100fa,1" +
      "0100-10102,10107-10133,10137-1018e,10190-1019c,101a0,101d0-101fd,10280-1029c,102a0-102d0,102e0-102fb,10300-103" +
      "23,1032d-1034a,10350-1037a,10380-1039d,1039f-103c3,103c8-103d5,10400-1049d,104a0-104a9,104b0-104d3,104d8-104fb" +
      ",10500-10527,10530-10563,1056f-1057a,1057c-1058a,1058c-10592,10594-10595,10597-105a1,105a3-105b1,105b3-105b9,1" +
      "05bb-105bc,105c0-105f3,10600-10736,10740-10755,10760-10767,10780-10785,10787-107b0,107b2-107ba,1091f,10940-109" +
      "59,10a01-10a03,10a05-10a06,10a0c-10a0f,10a38-10a3a,10a3f,10ae5-10ae6,10b39-10b3f,10d24-10d27,10d40-10d65,10d69" +
      "-10d85,10d8e-10d8f,10eab-10eac,10ec2-10ec7,10ed0-10ed8,10efa-10eff,10f46-10f50,10f70-10f89,11000-1104d,11052-1" +
      "1075,1107f-110bc,110be-110c2,110d0-110e8,110f0-110f9,11100-11134,11136-11147,11150-11176,11180-111df,111e1-111" +
      "f4,11200-11211,11213-11241,11280-11286,11288,1128a-1128d,1128f-1129d,1129f-112a9,112b0-112ea,112f0-112f9,11300" +
      "-11303,11305-1130c,1130f-11310,11313-11328,1132a-11330,11332-11333,11335-11339,1133b-11344,11347-11348,1134b-1" +
      "134d,11350,11357,1135d-11363,11366-1136c,11370-11374,11380-11389,1138b,1138e,11390-113b5,113b7-113c0,113c2,113" +
      "c5,113c7-113ca,113cc-113d5,113d7-113d8,113e1-113e2,11400-1145b,1145d-11461,11480-114c7,114d0-114d9,11580-115b5" +
      ",115b8-115dd,11600-11644,11650-11659,11660-1166c,11680-116b9,116c0-116c9,116d0-116e3,11700-1171a,1171d-1172b,1" +
      "1730-11746,11800-1183b,118a0-118f2,118ff-11906,11909,1190c-11913,11915-11916,11918-11935,11937-11938,1193b-119" +
      "46,11950-11959,119a0-119a7,119aa-119d7,119da-119e4,11a00-11a47,11a50-11aa2,11ab0-11af8,11b00-11b09,11b60-11b67" +
      ",11bc0-11be1,11bf0-11bf9,11c00-11c08,11c0a-11c36,11c38-11c45,11c50-11c6c,11c70-11c8f,11c92-11ca7,11ca9-11cb6,1" +
      "1d00-11d06,11d08-11d09,11d0b-11d36,11d3a,11d3c-11d3d,11d3f-11d47,11d50-11d59,11d60-11d65,11d67-11d68,11d6a-11d" +
      "8e,11d90-11d91,11d93-11d98,11da0-11da9,11db0-11ddb,11de0-11de9,11ee0-11ef8,11f00-11f10,11f12-11f3a,11f3e-11f5a" +
      ",11fb0,11fc0-11ff1,11fff-12399,12400-1246e,12470-12474,12480-12543,12f90-12ff2,13000-1342f,13440-13455,13460-1" +
      "43fa,14400-14646,16100-16139,16800-16a38,16a40-16a5e,16a60-16a69,16a6e-16abe,16ac0-16ac9,16ad0-16aed,16af0-16a" +
      "f5,16b00-16b45,16b50-16b59,16b5b-16b61,16b63-16b77,16b7d-16b8f,16d40-16d79,16e40-16e9a,16ea0-16eb8,16ebb-16ed3" +
      ",16f00-16f4a,16f4f-16f87,16f8f-16f9f,16fe0-16fe4,16ff0-16ff6,17000-18cd5,18cff-18d1e,18d80-18df2,1aff0-1aff3,1" +
      "aff5-1affb,1affd-1affe,1b000-1b122,1b132,1b150-1b152,1b155,1b164-1b167,1b170-1b2fb,1bc00-1bc6a,1bc70-1bc7c,1bc" +
      "80-1bc88,1bc90-1bc99,1bc9c-1bca3,1cc00-1ccfc,1cd00-1ceb3,1ceba-1ced0,1cee0-1cef0,1cf00-1cf2d,1cf30-1cf46,1cf50" +
      "-1cfc3,1d000-1d0f5,1d100-1d126,1d129-1d1ea,1d200-1d245,1d2c0-1d2d3,1d2e0-1d2f3,1d300-1d356,1d360-1d378,1d400-1" +
      "d454,1d456-1d49c,1d49e-1d49f,1d4a2,1d4a5-1d4a6,1d4a9-1d4ac,1d4ae-1d4b9,1d4bb,1d4bd-1d4c3,1d4c5-1d505,1d507-1d5" +
      "0a,1d50d-1d514,1d516-1d51c,1d51e-1d539,1d53b-1d53e,1d540-1d544,1d546,1d54a-1d550,1d552-1d6a5,1d6a8-1d7cb,1d7ce" +
      "-1da8b,1da9b-1da9f,1daa1-1daaf,1df00-1df1e,1df25-1df2a,1e000-1e006,1e008-1e018,1e01b-1e021,1e023-1e024,1e026-1" +
      "e02a,1e030-1e06d,1e08f,1e100-1e12c,1e130-1e13d,1e140-1e149,1e14e-1e14f,1e290-1e2ae,1e2c0-1e2f9,1e2ff,1e4d0-1e4" +
      "f9,1e5d0-1e5fa,1e5ff,1e6c0-1e6de,1e6e0-1e6f5,1e6fe-1e6ff,1e7e0-1e7e6,1e7e8-1e7eb,1e7ed-1e7ee,1e7f0-1e7fe,1e8d0" +
      "-1e8d6,1e944-1e94a,1eef0-1eef1,1f000-1f02b,1f030-1f093,1f0a0-1f0ae,1f0b1-1f0bf,1f0c1-1f0cf,1f0d1-1f0f5,1f101-1" +
      "f1ad,1f1e6-1f202,1f210-1f23b,1f240-1f248,1f250-1f251,1f260-1f265,1f300-1f6d8,1f6dc-1f6ec,1f6f0-1f6fc,1f700-1f7" +
      "d9,1f7e0-1f7eb,1f7f0,1f800-1f80b,1f810-1f847,1f850-1f859,1f860-1f887,1f890-1f8ad,1f8b0-1f8bb,1f8c0-1f8c1,1f8d0" +
      "-1f8d8,1f900-1fa57,1fa60-1fa6d,1fa70-1fa7c,1fa80-1fa8a,1fa8e-1fac6,1fac8,1facd-1fadc,1fadf-1faea,1faef-1faf8,1" +
      "fb00-1fb92,1fb94-1fbfa,20000-2a6df,2a700-2b81d,2b820-2cead,2ceb0-2ebe0,2ebf0-2ee5d,2f800-2fa1d,30000-3134a,313" +
      "50-33479,e0100-e01ef"
    );
    const URL_RTL = (
      "5be,5c0,5c3,5c6,5d0-5ea,5ef-5f4,608,60b,60d,61b,61e-64a,660-669,66b-66f,671-6d5,6e5-6e6,6ee-6ef,6fa-70d,710,71" +
      "2-72f,74d-7a5,7b1,7c0-7ea,7f4-7f5,7fa,7fe-815,81a,824,828,830-83e,840-858,85e,860-86a,8a0-8b4,8b6-8c7,2135-213" +
      "8,fb1d,fb1f-fb28,fb2a-fb36,fb38-fb3c,fb3e,fb40-fb41,fb43-fb44,fb46-fbc1,fbd3-fc5d,fc64-fd3d,fd50-fd8f,fd92-fdc" +
      "7,fdf0-fdf9,fdfc,fe71,fe73,fe77,fe79,fe7b,fe7d,fe7f-fefc,10800-10805,10808,1080a-10835,10837-10838,1083c,1083f" +
      "-10855,10857-1089e,108a7-108af,108e0-108f2,108f4-108f5,108fb-1091b,10920-10939,1093f,10980-109b7,109bc-109cf,1" +
      "09d2-10a00,10a10-10a13,10a15-10a17,10a19-10a35,10a40-10a48,10a50-10a58,10a60-10a9f,10ac0-10ae4,10aeb-10af6,10b" +
      "00-10b35,10b40-10b55,10b58-10b72,10b78-10b91,10b99-10b9c,10ba9-10baf,10c00-10c48,10c80-10cb2,10cc0-10cf2,10cfa" +
      "-10d23,10d30-10d39,10e60-10e7e,10e80-10ea9,10ead,10eb0-10eb1,10f00-10f27,10f30-10f45,10f51-10f59,10fb0-10fcb,1" +
      "0fe0-10ff6,1e800-1e8c4,1e8c7-1e8cf,1e900-1e943,1e94b,1e950-1e959,1e95e-1e95f,1ec71-1ecb4,1ed01-1ed3d,1ee00-1ee" +
      "03,1ee05-1ee1f,1ee21-1ee22,1ee24,1ee27,1ee29-1ee32,1ee34-1ee37,1ee39,1ee3b,1ee42,1ee47,1ee49,1ee4b,1ee4d-1ee4f" +
      ",1ee51-1ee52,1ee54,1ee57,1ee59,1ee5b,1ee5d,1ee5f,1ee61-1ee62,1ee64,1ee67-1ee6a,1ee6c-1ee72,1ee74-1ee77,1ee79-1" +
      "ee7c,1ee7e,1ee80-1ee89,1ee8b-1ee9b,1eea1-1eea3,1eea5-1eea9,1eeab-1eebb"
    );
    const rangeTable = (str) => str.split(",").map((r) => {
      const i = r.indexOf("-");
      return i < 0 ? [parseInt(r, 16), parseInt(r, 16)] : [parseInt(r.slice(0, i), 16), parseInt(r.slice(i + 1), 16)];
    });
    const inTable = (rs, c) => {
      let lo = 0, hi = rs.length - 1;
      while (lo <= hi) {
        const m = (lo + hi) >> 1, a = rs[m][0], b = rs[m][1];
        if (c < a) hi = m - 1;
        else if (c > b) lo = m + 1;
        else return true;
      }
      return false;
    };
    let ALLOWED_RS = null, RTL_RS = null;
    const isRtlCp = (c) => { if (!RTL_RS) RTL_RS = rangeTable(URL_RTL); return inTable(RTL_RS, c); };
    const isDigitCp = (c) => (c >= 0x30 && c <= 0x39) || (c >= 0x660 && c <= 0x669) || (c >= 0x6f0 && c <= 0x6f9);
    const isMarkCp = (c) => /\p{M}/u.test(String.fromCodePoint(c));
    // A label's code points, checked against the measured allow-table and RFC 5893's bidi rule (approximated with the
    // measured RTL class: an RTL label may hold RTL letters, digits, marks and hyphens, and no other letter).
    const labelOk = (label) => {
      if (!ALLOWED_RS) ALLOWED_RS = rangeTable(URL_ALLOWED);
      const cp = Array.from(label, (c) => c.codePointAt(0));
      let rtl = false, hasEn = false, hasAn = false;
      for (const c of cp) {
        if (c > 0x7f && !inTable(ALLOWED_RS, c)) return false;
        if (isRtlCp(c)) rtl = true;
        if (c >= 0x30 && c <= 0x39) hasEn = true;
        if ((c >= 0x660 && c <= 0x669) || (c >= 0x6f0 && c <= 0x6f9)) hasAn = true;
      }
      if (!rtl) return !(hasEn && hasAn);
      // An RTL label: first character RTL, no LTR letter anywhere, and the last non-mark RTL or a digit.
      if (cp.length === 0 || !isRtlCp(cp[0])) return false;
      if (hasEn && hasAn) return false;
      for (const c of cp) if (/\p{L}/u.test(String.fromCodePoint(c)) && !isRtlCp(c)) return false;
      let last = cp.length - 1;
      while (last >= 0 && isMarkCp(cp[last])) last--;
      if (last < 0 || !(isRtlCp(cp[last]) || isDigitCp(cp[last]))) return false;
      return true;
    };

    // IDNA: NFKC folds fullwidth and compatibility forms, the three non-ASCII dot characters become ".", labels are
    // lowercased, and a label that is not ASCII becomes "xn--" + Punycode. Validity is the measured allow-table plus the
    // bidi rule. Two details come from the sweep that measured it, not from the specification:
    //   * the table is applied to the code points as WRITTEN, before NFKC, because Node refuses a code point that NFKC would
    //     rewrite just as it refuses one it cannot represent (U+2488 PARENTHESIZED DIGIT ONE becomes "(1)", and is refused);
    //   * a label whose NFKC form contains a dot is refused: U+2024 ONE DOT LEADER normalises to ".", which would otherwise
    //     silently create a second label.
    // An "xn--" label is checked rather than trusted: it must decode, its decoded form must be what UTS46 would have emitted
    // (lowercase, NFKC, and no default-ignorable code point such as a variation selector), it must contain an actual non-ASCII
    // code point (xn--a- decodes to the plain label a), it must satisfy the same validity and bidi rules, and its re-encoding
    // must be the same payload.
    const idna = (s) => {
      const labels = s.split(/[.。．｡]/);
      for (let i = 0; i < labels.length; i++) {
        let l = labels[i];
        for (const c of Array.from(l, (x) => x.codePointAt(0))) {
          if (c > 0x7f && !inTable(ALLOWED_RS || (ALLOWED_RS = rangeTable(URL_ALLOWED)), c)) return null;
        }
        l = l.normalize("NFKC").toLowerCase();
        if (l.indexOf(".") >= 0) return null;
        if (l.slice(0, 4) === "xn--") {
          const payload = l.slice(4), cp = unpuny(payload);
          if (cp === null || cp.length === 0 || !cp.some((c) => c > 0x7f)) return null;
          if (cp[0] === 0x2d || cp[cp.length - 1] === 0x2d) return null;
          for (const c of cp) {
            if (c < 0x80 && !((c >= 0x30 && c <= 0x39) || (c >= 0x61 && c <= 0x7a) || c === 0x2d)) return null;
            // A Punycode label may not decode to a dot (it would have been two labels), and the Cherokee small letters fold to
            // their uppercase block in Node's case tables but not in this engine's, so they are refused here by name.
            if (c === 0x2e || c === 0x3002 || c === 0xff0e || c === 0xff61) return null;
            if ((c >= 0x13f8 && c <= 0x13fd) || (c >= 0xab70 && c <= 0xabbf)) return null;
          }
          const decoded = String.fromCodePoint.apply(null, cp);
          const canon = decoded.replace(/[­͏؜ᅟ-ᅠ឴-឵᠋-᠏​-‏‪-‮⁠-⁯ㅤ︀-️﻿ﾠ￰-￸\u{1BCA0}-\u{1BCA3}\u{1D173}-\u{1D17A}\u{E0000}-\u{E0FFF}]/gu, "").normalize("NFKC");
          if (canon !== decoded) return null;
          if (/[A-Z]/.test(decoded)) return null;
          // UTS46 maps by case folding rather than by lowercasing. `toUpperCase().toLowerCase()` is that fold for the classes
          // the sweep could distinguish, with two measured exceptions: SHARP S uppercases to "SS" but is kept (non-transitional),
          // and Cherokee folds to its UPPERCASE block (its small letters are refused a few lines above).
          if (decoded.toUpperCase().toLowerCase() !== decoded) {
            if (!Array.from(decoded).every((ch) => { const c = ch.codePointAt(0); return c < 0x80 || c === 0xdf || (c >= 0x13a0 && c <= 0x13f5); })) return null;
          }
          if (!labelOk(decoded)) return null;
          const back = puny(decoded);
          if (back !== payload) return null;
          labels[i] = "xn--" + back;
          continue;
        }
        labels[i] = l;
        if (!/[^\x00-\x7f]/.test(l)) continue;
        if (/[\x00-\x20#%\/:<>?@[\\\]^|\x7f]/.test(l)) return null;
        if (!labelOk(l)) return null;
        labels[i] = "xn--" + puny(l);
      }
      return labels.join(".");
    };

    const parseHost = (str, special) => {
      if (!special) return /[\x00\t\n\r #\/:<>?@[\\\]^|]/.test(str) ? null : pct(str, C0);
      // Percent-escapes decode to bytes and then as UTF-8, so `%C3%BC.com` names the same host as `ü.com`. An escape below
      // 0x80 decodes to the character it names, as before; an invalid sequence is a failed host, not a throw.
      let d = str;
      if (/%[89A-Fa-f][0-9A-Fa-f]/.test(d)) {
        try { d = decodeURIComponent(d); } catch (_) { return null; }
      } else {
        d = d.replace(/%([0-9A-Fa-f]{2})/g, (_, h) => String.fromCharCode(parseInt(h, 16)));
      }
      d = idna(d);
      if (d === null) return null;
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
