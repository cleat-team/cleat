// A Cargo integration-test target. Compiled separately; never in the cdylib.
use std::fs;
use std::process::Command;

#[test]
fn an_ordinary_integration_test() {
    let _ = fs::read_to_string("testdata/input.json");
    let _ = Command::new("true").status();
}
