//! `src/tests/` is a LIBRARY MODULE that happens to be called tests. Cargo's
//! test target is `<crate>/tests`, at the crate root; this one is compiled into
//! the cdylib like any other module, so its non-determinism is a real finding.
//!
//! The fixture exists because the skip added for cleat#1789 is anchored at the
//! crate root. A rule matching the directory NAME alone would report this crate
//! clean having never read the file that is wrong.
mod tests;

#[no_mangle]
pub fn workflow() -> i32 {
    tests::helper()
}
