// KNOWN LIMIT -- this file is non-deterministic and R008 does not see it.
// That is the point of the fixture; it is not a bug to be fixed here.
//
// WHAT ESCAPES, AND WHY IT IS THE RIGHT LIMIT TO PICK. R008 tracks BINDINGS:
// a function parameter's declared type, or a `let` binding's annotation or
// constructor call, resolved through the same alias map the path resolver
// uses. `self.counts` is neither -- it is a FIELD ACCESS, and the identifier
// immediately before `.iter()`/`.values()`/the `for ... in` clause is
// `counts`, which was never bound to anything in THIS function. The
// checker's receiver match requires a bare tracked identifier; `self.counts`
// and `config.map` both fail that by construction, cheaply and on purpose --
// the alternative (following a value through every struct field cleat#1864
// weighed and rejected as needing type information) is what "syntactic
// binding tracking" declines to do.
//
// If this ever starts being caught, move it to r008_hashmap_iteration and
// write a new known-limit fixture for whatever still escapes -- do not
// leave this arm empty, per the sibling known_limit_trait_method fixture's
// own instructions.
use std::collections::HashMap;

pub struct Scoreboard {
    pub counts: HashMap<String, u64>,
}

impl Scoreboard {
    pub fn totals(&self) -> Vec<String> {
        let mut out = Vec::new();
        for (k, v) in &self.counts {
            out.push(format!("{}={}", k, v));
        }
        out
    }
}
