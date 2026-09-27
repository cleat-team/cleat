// KNOWN LIMIT -- this file is non-deterministic and the checker does not see
// it. That is the point of the fixture; it is not a bug to be fixed here.
//
// WHAT ESCAPES, AND WHY IT IS THE RIGHT LIMIT TO PICK. The checker resolves
// `use` declarations into local name -> full path and matches the resolved
// path of a PATH EXPRESSION (cleat#1811). A method call is not a path
// expression: `t.elapsed()` names no module, and nothing in this file resolves
// to a forbidden prefix.
//
//   use std::time::SystemTime;   resolves to std::time::SystemTime
//                                which is NOT under std::time::SystemTime::now
//   t.elapsed()                  consults the wall clock, and is a method
//
// `elapsed` reads the host clock and returns a different answer on every
// replay, so this crate is exactly as non-deterministic as r005_aliased_now
// next door -- and that one is caught while this one is not. The pair is the
// statement that the gate is wired and the gate is weak, and neither can be
// mistaken for the other.
//
// KNOWING A METHOD'S RECEIVER TYPE NEEDS TYPE RESOLUTION, which the design note
// (docs/contributor/design/rust-determinism-checker.md) declines: it is what
// separates a resolver from a compiler, and tree-sitter does not supply it
// either -- the tradeoff there is cgo, not this limit. A trait method, a
// re-export through a type, and a value handed in by a caller all land here.
//
// IF THIS FIXTURE STARTS BEING CAUGHT, that is an improvement and this arm goes
// red to say so. The instructions are in the test, as they were for the
// grouped-import fixture this one replaces -- which was promoted to
// r001_grouped_import when the resolver landed.
use std::time::Duration;
use std::time::SystemTime;

#[no_mangle]
pub fn age_in_seconds(t: SystemTime) -> u64 {
    // Non-deterministic: the host clock moves between replays.
    let d: Duration = t.elapsed().unwrap_or(Duration::from_secs(0));
    d.as_secs()
}
