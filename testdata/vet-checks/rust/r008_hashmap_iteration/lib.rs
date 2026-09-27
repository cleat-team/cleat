// A KNOWN POSITIVE since cleat#1864. This is the issue's own measured
// example: a workflow taking a HashMap by reference and iterating it
// directly. HashMap's default RandomState seeds its hasher per process, so
// the enumeration order is not guaranteed by the language -- see R008's own
// comment in vet_rust.go for why that is defence in depth rather than an
// observed replay divergence in cleat specifically.
//
//   cleat vet --lang rust <this crate>
//   R008 lib.rs:9:5   (a for-loop over a map)
use std::collections::HashMap;

pub fn total(items: &HashMap<String, u64>) -> Vec<String> {
    let mut out = Vec::new();
    for (k, v) in items {
        out.push(format!("{}={}", k, v));
    }
    out
}
