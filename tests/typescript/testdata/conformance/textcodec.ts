// TextEncoder / TextDecoder, compared with Node (cleat#3829). Neither global exists in the guest until the `textcodec` shim
// installs it, and the absence was loud; what can go wrong once they exist is SILENT, which is what this file is for: a value
// that differs from Node's (a lone surrogate encoded as WTF-8, a BOM kept when it should be dropped, a partial sequence
// flushed as one U+FFFD where Node emits two) reads as a perfectly good string.
//
// This is plain JavaScript in a .ts name, so the SAME source runs in the guest (packages/cleat-ts/build.sh) and in Node
// (esbuild bundles it); typescript_conformance_test.go compares what each prints, line for line.
declare function __cleat_result(json: string): void;

const hex = (u: Uint8Array): string => Array.from(u).map((b) => b.toString(16).padStart(2, "0")).join("");

// JSON.stringify keeps a U+FFFD visible as � and escapes the controls, so two decoders that disagree about a replacement
// character cannot agree by accident.
const shown = (s: string): string => JSON.stringify(s);

// Only the error NAME is compared for the label cases: the guest's message for an unimplemented encoding names cleat#3829,
// and Node's wording for an unknown label is Node's own. The guest's messages are asserted separately.
const errName = (f: () => unknown): string => {
  try {
    f();
    return "no error";
  } catch (e) {
    return (e as Error).name;
  }
};

const bytes = (...v: number[]): Uint8Array => new Uint8Array(v);

export function textcodec(): void {
  const out: string[] = [];

  // ---- TextEncoder.encode: the byte sequences, and the lone surrogate that must become U+FFFD --------------------------
  const encodeCases: Array<[string, string]> = [
    ["empty", ""],
    ["ascii", "abc"],
    ["two-byte", "é"],
    ["three-byte", "€"],
    ["four-byte", "\u{1f600}"],
    ["bom-as-text", "﻿"],
    ["lone-high", "\uD800"],
    ["lone-low", "\uDC00"],
    ["pair", "😀"],
    ["mixed-lone", "a\uD800b"],
    ["non-string", 12 as unknown as string],
  ];
  for (const [name, v] of encodeCases) {
    out.push(`encode(${name}) = ${hex(new TextEncoder().encode(v))}`);
  }
  out.push(`encoder.encoding = ${new TextEncoder().encoding}`);

  // ---- encodeInto: read is UTF-16 code units, written is bytes, and a code point that does not fit consumes neither -----
  const source = "aé\u{1f600}";
  for (let n = 0; n <= 8; n++) {
    const dest = new Uint8Array(n);
    const r = new TextEncoder().encodeInto(source, dest);
    out.push(`encodeInto(n=${n}) read=${r.read} written=${r.written} ${hex(dest.subarray(0, r.written))}`);
  }
  // A lone surrogate in the source is one code unit and three bytes, like any other replacement.
  {
    const dest = new Uint8Array(4);
    const r = new TextEncoder().encodeInto("\uD800x", dest);
    out.push(`encodeInto(lone) read=${r.read} written=${r.written} ${hex(dest)}`);
  }

  // ---- TextDecoder.decode: valid, invalid, overlong, truncated, BOM -----------------------------------------------
  const decodeCases: Array<[string, Uint8Array]> = [
    ["empty", bytes()],
    ["ascii", bytes(0x41)],
    ["two-byte", bytes(0xc3, 0xa9)],
    ["three-byte", bytes(0xe2, 0x82, 0xac)],
    ["four-byte", bytes(0xf0, 0x9f, 0x98, 0x80)],
    ["bom-then-A", bytes(0xef, 0xbb, 0xbf, 0x41)],
    ["bom-only", bytes(0xef, 0xbb, 0xbf)],
    ["invalid-ff", bytes(0xff)],
    ["invalid-80", bytes(0x80)],
    ["overlong-c0-af", bytes(0xc0, 0xaf)],
    ["broken-e0-80", bytes(0xe0, 0x80)],
    ["broken-e0-41", bytes(0xe0, 0x41)],
    ["truncated-e2-82", bytes(0xe2, 0x82)],
    ["surrogate-ed-a0-80", bytes(0xed, 0xa0, 0x80)],
    ["f4-90", bytes(0xf4, 0x90, 0x80, 0x80)],
    ["valid-then-invalid", bytes(0x41, 0xff, 0x42)],
  ];
  for (const [name, b] of decodeCases) {
    out.push(`decode(${name}) = ${shown(new TextDecoder().decode(b))}`);
    out.push(`decode(${name}, ignoreBOM) = ${shown(new TextDecoder("utf-8", { ignoreBOM: true }).decode(b))}`);
    out.push(`decode(${name}, fatal) = ${errName(() => new TextDecoder("utf-8", { fatal: true }).decode(b))}`);
  }

  // The same data as an ArrayBuffer and as a view into a larger buffer: the decoder must read exactly the view.
  {
    const big = bytes(0x41, 0xef, 0xbb, 0xbf, 0x42);
    out.push(`decode(view) = ${shown(new TextDecoder().decode(big.subarray(1, 4)))}`);
    out.push(`decode(arraybuffer) = ${shown(new TextDecoder().decode(big.slice(1, 4).buffer as ArrayBuffer))}`);
  }

  // ---- Streaming: a chunk boundary in the middle of a multi-byte sequence -------------------------------------------
  {
    const d = new TextDecoder();
    const first = d.decode(bytes(0xe2), { stream: true });
    const second = d.decode(bytes(0x82, 0xac), { stream: true });
    const end = d.decode();
    out.push(`stream(three-byte) = ${shown(first)} | ${shown(second)} | ${shown(end)}`);
  }
  {
    const d = new TextDecoder("utf-8", { fatal: true });
    const first = errName(() => d.decode(bytes(0xe2), { stream: true }));
    const second = d.decode(bytes(0x82, 0xac), { stream: true });
    out.push(`stream(fatal) = ${first} | ${shown(second)}`);
  }
  {
    const d = new TextDecoder();
    const first = d.decode(bytes(0xe2), { stream: true });
    const broken = d.decode(bytes(0x41), { stream: true });
    out.push(`stream(broken) = ${shown(first)} | ${shown(broken)}`);
  }
  {
    // A BOM split across two chunks of one stream is still skipped; a BOM at the start of a SECOND stream is not.
    const d = new TextDecoder();
    const a = d.decode(bytes(0xef), { stream: true });
    const b = d.decode(bytes(0xbb, 0xbf, 0x41), { stream: true });
    const c = d.decode(bytes(0xef, 0xbb, 0xbf, 0x42));
    out.push(`stream(bom-split) = ${shown(a)} | ${shown(b)} | ${shown(c)}`);
  }
  {
    // Not streamed: a truncated sequence is an error (or one U+FFFD), and the decoder is non-fatal by default.
    const d = new TextDecoder();
    const a = d.decode(bytes(0xe2, 0x82));
    out.push(`decode(truncated-then-more) = ${shown(a)}`);
  }

  // ---- Labels ------------------------------------------------------------------------------------------------------
  for (const label of ["utf-8", "UTF-8", " utf8 ", "unicode-1-1-utf-8", "unicode11utf8", "unicode20utf8", "x-unicode20utf8"]) {
    out.push(`label(${label}).encoding = ${new TextDecoder(label).encoding}`);
  }
  out.push(`label(bogus) = ${errName(() => new TextDecoder("bogus"))}`);
  // "utf-16le" is deliberately NOT here: Node implements it, so Node is not an oracle for the guest's refusal. That case is a
  // literal in TestTypeScriptTextCodecKeepsStreamsAbsentAndNamesItsRefusals.
  out.push(`label(empty) = ${errName(() => new TextDecoder(""))}`);
  out.push(`default.encoding = ${new TextDecoder().encoding}`);

  // ---- Options are readable back ----------------------------------------------------------------------------------
  {
    const d = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true });
    out.push(`options fatal=${String(d.fatal)} ignoreBOM=${String(d.ignoreBOM)} encoding=${d.encoding}`);
  }
  {
    const d = new TextDecoder();
    out.push(`defaults fatal=${String(d.fatal)} ignoreBOM=${String(d.ignoreBOM)}`);
  }

  __cleat_result(JSON.stringify(out));
}
