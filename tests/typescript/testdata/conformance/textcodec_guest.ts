// Guest-only facts about TextEncoder / TextDecoder (cleat#3829): the cases where Node's answer is not the oracle because Node
// HAS things the guest deliberately does not.
//
//  - `TextDecoderStream` and `TextEncoderStream` are undefined here and defined in Node, so a comparison would only record the
//    difference. Leaving them undefined is deliberate: a library that feature-detects `typeof TextDecoderStream === "function"`
//    must see the truth, which a throwing constructor would hide until later.
//  - the message for a valid-but-unimplemented encoding names cleat#3829 so a reader can tell "not supported here" from a typo.
//    Node's own message for the same label says something else, which is why this file is not compared with Node.
declare function __cleat_result(json: string): void;

export function textcodecGuest(): void {
  const out: string[] = [];
  out.push(`typeof TextDecoderStream = ${typeof (globalThis as any).TextDecoderStream}`);
  out.push(`typeof TextEncoderStream = ${typeof (globalThis as any).TextEncoderStream}`);

  const message = (label: string): string => {
    try {
      new TextDecoder(label);
      return "no error";
    } catch (e) {
      return `${(e as Error).name}: ${(e as Error).message}`;
    }
  };
  out.push(`utf-16le -> ${message("utf-16le")}`);
  out.push(`windows-1252 -> ${message("windows-1252")}`);
  out.push(`bogus -> ${message("bogus")}`);
  __cleat_result(JSON.stringify(out));
}
