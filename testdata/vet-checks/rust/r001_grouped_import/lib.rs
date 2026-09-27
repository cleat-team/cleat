// A KNOWN POSITIVE since cleat#1811 -- it was the known LIMIT before it.
//
// A grouped import defeated the literal-spelling table: the scanner looked for
// the module path and the leading `use` fused into one string, a grouped import
// separates them, and the call site then refers to the module by its bare name,
// which was likewise not on the list. So the two statements at the bottom read
// real files and real stdin, and vet reported nothing:
//
//   cleat vet --lang rust <this crate>
//   Summary: 1 files, 0 errors, 0 warnings     exit=0
//
// Nothing about the spelling is exotic. Grouped imports are idiomatic Rust and
// are what rustfmt produces from repeated single-module imports -- which is why
// this was a limit worth a fixture rather than a curiosity.
//
// The checker now resolves `use` declarations into local name -> full path and
// matches the RESOLVED path, so both sites are reported and each says how it
// resolved:
//
//   R001 lib.rs:29:1   (a grouped import; this arm resolves to std::fs)
//   R001 lib.rs:34:13  (written "fs::read_to_string", which resolves to std::fs::read_to_string)
//
// The comment may quote the pattern list freely now: cleat#1782 blanks comments
// and string literals before the scan sees them. The previous header said "read
// the list there rather than here" and that restriction is gone with it.
//
// known_limit_trait_method is what escapes the resolver, and is the fixture that
// keeps "the gate is wired" and "the gate is weak" both stated.

use std::{fs, io};

#[no_mangle]
pub fn workflow() {
    // Non-deterministic: file contents differ between runs.
    let _ = fs::read_to_string("data.txt");
    // Non-deterministic: stdin differs between runs.
    let _ = io::stdin();
}
