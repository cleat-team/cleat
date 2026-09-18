// A KNOWN POSITIVE that must NOT refuse the build, which is what makes it
// different from every other fixture here.
//
// R101 is the only rule in the Rust checker that is a WARNING, and the reason
// is measured rather than assumed. From
// docs/contributor/design/rust-determinism-checker.md, measured 2026-09-17 with
// a negative control: a cdylib on wasm32-wasip1 containing a HashMap imports
// wasi_snapshot_preview1.random_get (read from section id 2, not grepped out of
// the binary); the same crate without one imports nothing at all; and the module
// run with random_get filled 0x00 / 0xFF / 0x00 produced -1443680002 /
// -252770918 / -1443680002.
//
// So iteration order genuinely depends on random_get, AND it reproduces under a
// fixed one. cleat binds random_get to handler.Random() -- seeded from workflow
// id and step -- and registers it after DefineWasi so it is the final word. Two
// replays of one workflow therefore see the same iteration order.
//
// THE DIVERGENCE THIS RULE SOUNDS LIKE IT PREVENTS DOES NOT HAPPEN, and the
// message says so. What the rule is for is the fragility of the arrangement:
// the property holds because of an invariant in a different subsystem, which
// nothing in vet_rust.go knows and no test there pins. A Component Model
// migration to wasi:random, a backend that forwards random_get rather than
// intercepting it, or a std that stops seeding through WASI would each turn the
// code below into a replay divergence with no diagnostic anywhere.
//
// Hence a warning: it costs a line of output, it does not refuse code that
// works today, and it cannot be mistaken for a claim that the build is broken.
//
// The three calls at the bottom are the NEGATIVE control inside the positive
// fixture. get, insert and len are order-independent and must stay silent; a
// rule that fires on using a HashMap at all would be a rule about the type
// rather than about determinism.

use std::collections::HashMap;

#[no_mangle]
pub fn workflow() {
    let mut counts: HashMap<String, u64> = HashMap::new();
    counts.insert("a".to_string(), 1);

    // R101: iteration order is unspecified in Rust.
    for (k, v) in &counts {
        let _ = (k, v);
    }
    // R101 again: the method form, on the same binding.
    let _ = counts.keys();

    // SILENT, all three: no order is exposed.
    let _ = counts.get("a");
    let _ = counts.len();
    counts.insert("b".to_string(), 2);
}
