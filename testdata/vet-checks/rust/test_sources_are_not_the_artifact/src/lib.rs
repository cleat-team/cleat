//! The library itself is deterministic. Its TESTS are not, and they are not
//! compiled into the cdylib this gate guards. cleat#1789.
//!
//! Before the fix, the `#[cfg(test)]` module below reported R001 and the crate
//! would not build -- and that module is the commoner form, because a
//! directory rule cannot reach it: it lives in the one file the gate must scan.

#[no_mangle]
pub fn workflow(n: i32) -> i32 {
    n.wrapping_mul(31).wrapping_add(7)
}

#[cfg(test)]
mod tests {
    // Reading a fixture is what a test is FOR. None of this reaches the
    // artifact, and a nested brace here is on purpose: the blanker matches
    // braces, so it has to find the module's closing one rather than the first.
    use std::fs;
    use std::time::Instant;

    use super::workflow;

    #[test]
    fn reads_a_fixture() {
        let started = Instant::now();
        if let Ok(s) = fs::read_to_string("testdata/golden.json") {
            assert!(!s.is_empty());
        }
        assert!(started.elapsed().as_secs() < 60);
    }

    #[test]
    fn the_workflow_is_pure() {
        assert_eq!(workflow(1), 38);
    }
}

// AFTER the cfg(test) module, and deterministic. If the brace matching stops at
// the first `}` instead of the module's own, everything below reads as blanked
// and a real violation added here would go unreported. That is what the
// known-positive arm in the test asserts against.
pub fn also_pure(n: i32) -> i32 {
    n + 1
}
