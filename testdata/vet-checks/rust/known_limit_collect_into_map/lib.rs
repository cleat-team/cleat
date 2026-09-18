// KNOWN LIMIT -- this file is non-deterministic and R008 does not see it.
// That is the point of the fixture; it is not a bug to be fixed here.
//
// WHAT ESCAPES, AND WHY IT IS THE RIGHT LIMIT TO PICK. R008's constructor
// inference matches a let-binding's right-hand side against
// `TypeName::new(`, `TypeName::with_capacity(` or `TypeName::from(` --
// deliberately not `.collect::<HashMap<_, _>>()`, which is a method call
// whose turbofish names the target type as a GENERIC ARGUMENT rather than as
// a path expression at the call site. Parsing that argument out reliably
// costs the same nested-angle-bracket handling findRustFuncSpans already
// pays for a function's own generics, spent on every `.collect()` in the
// crate instead of once per function -- and the pattern below never binds a
// name at all, so there would be nothing to check even after parsing it.
//
// If this ever starts being caught, move it to r008_hashmap_iteration and
// write a new known-limit fixture for whatever still escapes.
use std::collections::HashMap;

pub fn describe(pairs: Vec<(String, u64)>) -> Vec<String> {
    pairs
        .into_iter()
        .collect::<HashMap<String, u64>>()
        .into_iter()
        .map(|(k, v)| format!("{}={}", k, v))
        .collect()
}
