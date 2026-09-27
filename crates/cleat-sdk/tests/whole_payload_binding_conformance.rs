//! What this SDK does with the whole payload, pinned rather than described.
//!
//! cleat#1691. `tests/conformance/entry_point_binding_cases.json` deliberately
//! carries no `rust` column, and `how` says why: only Go, Python and
//! AssemblyScript bind parameters BY NAME, so "an absent parameter" is not a
//! concept this SDK has. `#[cleat_entry]` takes exactly one user parameter
//! (`crates/cleat-macro/src/entry.rs`) and hands it the whole payload through
//! `serde_json::from_str`. Absence of a FIELD is delegated to serde.
//!
//! That exclusion is right, and it is the reason this file exists rather than a
//! reader for the table: the facts it rests on are recorded as PROSE, in the
//! table's `no_per_parameter_binding`, and prose describing behaviour with
//! nothing checking it is how the sibling Java note went stale -- it still
//! describes a run-time throw that cleat#1636 turned into a compile error.
//!
//! So these tests assert what the note claims about Rust. If serde's behaviour
//! ever changes under us, this goes red instead of the note quietly becoming
//! false.

use serde::Deserialize;

#[derive(Deserialize, Debug)]
struct Required {
    note: String,
}

#[derive(Deserialize, Debug)]
struct RequiredInt {
    // Never read: the test asserts the DECODE is refused, so the value never
    // exists to be read. allow(dead_code) rather than an underscore, because
    // the field NAME is the thing under test -- serde matches on it.
    #[allow(dead_code)]
    count: i64,
}

#[derive(Deserialize, Debug)]
struct Optional {
    note: Option<String>,
}

fn fallback() -> String {
    "FALLBACK".to_string()
}

#[derive(Deserialize, Debug)]
struct Defaulted {
    #[serde(default = "fallback")]
    note: String,
}

/// An absent FIELD is refused, which is the contract cleat#1065 settled on.
///
/// This is the half that makes the table's exclusion safe to keep. Rust is not
/// listed there, so nothing else in the repository records that it already
/// behaves the way the contract requires -- it simply reaches that behaviour
/// through serde rather than through a generated binder.
#[test]
fn an_absent_field_is_refused() {
    let err = serde_json::from_str::<Required>("{}").unwrap_err();
    assert!(
        err.to_string().contains("missing field") && err.to_string().contains("note"),
        "serde no longer names the missing field: {err}\n\n\
         The message is not the contract -- the REFUSAL is -- but an author who \
         omits a parameter needs to know which one, the same property every other \
         SDK's refusal carries."
    );

    let err = serde_json::from_str::<RequiredInt>("{}").unwrap_err();
    assert!(
        err.to_string().contains("missing field"),
        "an absent integer field was accepted: {err}"
    );
}

/// The control. Without it, "absent is refused" is also satisfied by a build
/// where every payload is refused.
#[test]
fn a_present_field_binds_its_value() {
    let got: Required = serde_json::from_str(r#"{"note":"hi"}"#).expect("a present field must bind");
    assert_eq!(got.note, "hi");
}

/// Rust can spell "optional" TWO ways, and they land on different outcomes of
/// the shared table. No other SDK can express both.
///
/// `Option<T>` gives Go's shape -- an absent marker the workflow interprets --
/// and `#[serde(default)]` gives Python's and AssemblyScript's, where absence
/// binds a value the declaration supplied. The table's
/// "absent, declared optional" case has to pick one tag per SDK, which is part
/// of why listing Rust there would mislead rather than inform.
#[test]
fn optional_has_two_spellings_with_different_outcomes() {
    let marker: Optional = serde_json::from_str("{}").expect("Option<T> must accept absence");
    assert!(
        marker.note.is_none(),
        "Option<T> bound {:?} for an absent key, want None -- this is the \
         bound_absent_marker outcome, the same shape Go's *T has",
        marker.note
    );

    let defaulted: Defaulted =
        serde_json::from_str("{}").expect("#[serde(default)] must accept absence");
    assert_eq!(
        defaulted.note, "FALLBACK",
        "#[serde(default)] did not supply the declared default -- this is the \
         bound_default outcome, the same shape Python and AssemblyScript have"
    );
}

/// THE ROW WHERE THIS SDK GENUINELY DIVERGES, and the reason it is worth
/// pinning even though Rust has no column.
///
/// The table's "lone string parameter" case is NOT a name-binding question: it
/// asks what a single string parameter receives. Go, AssemblyScript and Java
/// all hand it the whole payload, so `{}` binds the two-character string "{}".
/// Python refuses. Rust refuses too, but for a different reason -- serde is
/// asked for a `String` and the payload is an object.
///
/// Measured 2026-09-16. A reader who assumes "one parameter, whole payload"
/// means "the payload arrives as text" would expect "{}" here and be wrong.
#[test]
fn a_lone_string_parameter_does_not_receive_the_raw_payload() {
    let err = serde_json::from_str::<String>("{}").unwrap_err();
    assert!(
        err.to_string().contains("invalid type"),
        "a String parameter accepted an object payload: {err}\n\n\
         If this now succeeds, Rust has acquired the whole-payload fast path Go, \
         AssemblyScript and Java have, and the lone-string row of \
         tests/conformance/entry_point_binding_cases.json changed from 3-2 to 4-1."
    );

    let got: String =
        serde_json::from_str(r#""hi""#).expect("a JSON string must still decode into String");
    assert_eq!(got, "hi", "the control: a genuine JSON string still binds");
}
