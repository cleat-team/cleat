// KNOWN LIMIT -- this file is non-deterministic and the Rust checker does not
// see it. That is the point of the fixture; it is not a bug to be fixed here.
//
// forbiddenRustPatterns (cmd/cleat/vet_rust.go) is literal substring matching
// over a short list of spellings. Read the list there rather than here: this
// comment deliberately does not quote it, because the scanner does not skip
// comments, so prose naming a forbidden spelling is itself reported as a
// violation. The first draft of this file did quote it and produced seven
// errors from its own explanation.
//
// A grouped import defeats every entry on that list. The scanner looks for the
// module path and the leading `use` fused into one string; a grouped import
// separates them, and the call site below then refers to the module by its
// bare name, which is likewise not on the list. So the two statements at the
// bottom read real files and real stdin, and vet reports nothing. Measured
// 2026-09-17, after this comment was rewritten:
//
//   cleat vet --lang rust testdata/vet-checks/rust/known_limit_grouped_use
//   Summary: 1 files, 0 errors, 0 warnings     exit=0
//
// Nothing about this spelling is exotic. Grouped imports are idiomatic Rust
// and are what rustfmt produces from repeated single-module imports.
//
// The companion fixture e001_fs_access imports the same module the plain way
// and IS caught. The pair is asserted together in
// rust_build_refuses_nondeterminism_test.go so that "the gate is wired" and
// "the gate is weak" are both stated, and neither can be mistaken for the
// other.
use std::{fs, io};

#[no_mangle]
pub fn workflow() {
    // Non-deterministic: file contents differ between runs.
    let _ = fs::read_to_string("data.txt");
    // Non-deterministic: stdin differs between runs.
    let _ = io::stdin();
}
