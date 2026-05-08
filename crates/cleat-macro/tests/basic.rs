// Integration tests for the `#[cleat_entry]` proc-macro.
//
// These tests verify that the macro generates correct code by:
// 1. Applying `#[cleat_entry]` to functions with various signatures
// 2. Calling the generated `extern "C"` export wrapper
// 3. Decoding the result and verifying correctness
//
// IMPORTANT: Test workflow bodies must NOT call HostCalls methods (e.g.
// `h.cleat_log()`, `h.cleat_call()`) because those link to WASM host imports
// that cannot be resolved on a non-WASM host target. The generated wrapper
// code itself (read_string, write_string, format_cleat_result, etc.) is
// pure Rust and works on any target.

use cleat_macro::cleat_entry;
use serde::{Deserialize, Serialize};

// ═════════════════════════════════════════════════════════════════════════════
// Helpers
// ═════════════════════════════════════════════════════════════════════════════

/// Decode the i64 result from a generated export function.
///
/// The encoding is: low 32 bits = err_code, high 32 bits = actual_len.
/// Matches `cleat_sdk::memory::encode_export_result` / `decode_export_result`.
fn decode_export_result(result: i64) -> (u32, u32) {
    let r = result as u64;
    let err_code = (r & 0xFFFF_FFFF) as u32;
    let actual_len = (r >> 32) as u32;
    (err_code, actual_len)
}

/// Run a generated export function with the given JSON input and return
/// the decoded (err_code, output_json) pair.
fn run_export(
    export_fn: unsafe extern "C" fn(*const u8, u32, *mut u8, u32) -> i64,
    input_json: &str,
) -> (u32, String) {
    let mut out_buf = vec![0u8; 65536];
    let result = unsafe {
        export_fn(
            input_json.as_ptr(),
            input_json.len() as u32,
            out_buf.as_mut_ptr(),
            65536,
        )
    };
    let (err_code, actual_len) = decode_export_result(result);
    if actual_len == 0 {
        return (err_code, String::new());
    }
    let output = std::str::from_utf8(&out_buf[..actual_len as usize])
        .unwrap_or("<invalid utf8>")
        .to_string();
    (err_code, output)
}

/// helper to cast a named export to its function pointer type.
macro_rules! export_fn {
    ($name:ident) => {
        $name as unsafe extern "C" fn(*const u8, u32, *mut u8, u32) -> i64
    };
}

// ═════════════════════════════════════════════════════════════════════════════
// Test types
// ═════════════════════════════════════════════════════════════════════════════

#[derive(Debug, Deserialize, Serialize, PartialEq)]
struct PersonInput {
    name: String,
    age: u32,
}

#[derive(Debug, Deserialize)]
struct SingleField {
    value: String,
}

#[derive(Debug, Serialize, Deserialize, PartialEq)]
struct GreetingOutput {
    greeting: String,
    year: u32,
}

// ═════════════════════════════════════════════════════════════════════════════
// Workflow 1: basic success path with String return
// ═════════════════════════════════════════════════════════════════════════════

#[cleat_entry]
fn greet(_h: &cleat_sdk::HostCalls, input: PersonInput) -> Result<String, String> {
    Ok(format!("Hello, {}! You are {} years old.", input.name, input.age))
}

#[test]
fn test_basic_success() {
    let (err_code, output) = run_export(export_fn!(greet), r#"{"name":"Alice","age":30}"#);
    assert_eq!(err_code, 0, "expected success");

    // `format_cleat_result(Ok(val))` serializes val (a String) via serde_json::to_string,
    // which produces a JSON string literal: "\"Hello, ...\""
    assert_eq!(output, "\"Hello, Alice! You are 30 years old.\"");
}

// ═════════════════════════════════════════════════════════════════════════════
// Workflow 2: error path — function returns Err
// ═════════════════════════════════════════════════════════════════════════════

#[cleat_entry]
fn fail(_h: &cleat_sdk::HostCalls, input: SingleField) -> Result<String, String> {
    Err(format!("error: {}", input.value))
}

#[test]
fn test_error_path() {
    let (err_code, output) = run_export(export_fn!(fail), r#"{"value":"something broke"}"#);
    assert_eq!(err_code, 1, "expected error code 1");
    assert_eq!(output, r#"{"error":"error: something broke"}"#);
}

// ═════════════════════════════════════════════════════════════════════════════
// Workflow 3: no extra input parameters (only &HostCalls)
// ═════════════════════════════════════════════════════════════════════════════

#[cleat_entry]
fn no_input(_h: &cleat_sdk::HostCalls) -> Result<String, String> {
    Ok("no-param-success".to_string())
}

#[test]
fn test_no_extra_input() {
    // Empty input is fine — generated code reads the string but doesn't
    // deserialize anything when there are no extra parameters.
    let (err_code, output) = run_export(export_fn!(no_input), "");
    assert_eq!(err_code, 0, "expected success");
    assert_eq!(output, "\"no-param-success\"");
}

// ═════════════════════════════════════════════════════════════════════════════
// Workflow 4: custom struct return type
// ═════════════════════════════════════════════════════════════════════════════

#[cleat_entry]
fn custom_return(_h: &cleat_sdk::HostCalls, input: PersonInput) -> Result<GreetingOutput, String> {
    Ok(GreetingOutput {
        greeting: format!("Hi {}", input.name),
        year: 2024,
    })
}

#[test]
fn test_custom_return_type() {
    let (err_code, output) = run_export(export_fn!(custom_return), r#"{"name":"Bob","age":25}"#);
    assert_eq!(err_code, 0);

    // Parse the JSON output and verify the struct fields
    let parsed: GreetingOutput =
        serde_json::from_str(&output).expect("output should be valid JSON");
    assert_eq!(parsed.greeting, "Hi Bob");
    assert_eq!(parsed.year, 2024);
}

// ═════════════════════════════════════════════════════════════════════════════
// Test 5: invalid JSON input triggers deserialization error
// ═════════════════════════════════════════════════════════════════════════════

#[test]
fn test_deserialization_error() {
    let (err_code, output) = run_export(export_fn!(greet), r#"this is not valid json"#);
    assert_eq!(err_code, 1, "bad input should produce error code 1");
    // The generated error handler wraps the serde error in {"error": "..."}
    assert!(output.contains("error"), "output should contain error field: {}", output);
}

// ═════════════════════════════════════════════════════════════════════════════
// Test 6: null/empty input with non-empty args_len passes through non-deser path
// ═════════════════════════════════════════════════════════════════════════════

#[test]
fn test_no_extra_input_null_ptr() {
    // Passing null pointer with len 0 — `read_string` returns empty string,
    // and the no-input workflow ignores it.
    let mut out_buf = vec![0u8; 65536];
    let result = unsafe {
        no_input(
            std::ptr::null(),
            0u32,
            out_buf.as_mut_ptr(),
            65536,
        )
    };
    let (err_code, actual_len) = decode_export_result(result);
    assert_eq!(err_code, 0);
    let output = std::str::from_utf8(&out_buf[..actual_len as usize]).unwrap();
    assert_eq!(output, "\"no-param-success\"");
}

// ═════════════════════════════════════════════════════════════════════════════
// Test 7: verify the inner function naming convention
// ═════════════════════════════════════════════════════════════════════════════

#[cleat_entry]
fn inner_check(_h: &cleat_sdk::HostCalls, input: SingleField) -> Result<String, String> {
    Ok(input.value)
}

#[test]
fn test_inner_function_exists() {
    // The macro renames the original body to `__cleat_inner_<fn_name>`.
    // This test verifies the inner function is accessible and has the correct
    // signature (safe Rust, not extern "C").
    let result = __cleat_inner_inner_check(&cleat_sdk::HostCalls, SingleField {
        value: "from-inner".to_string(),
    });
    assert_eq!(result, Ok("from-inner".to_string()));
}

// ═════════════════════════════════════════════════════════════════════════════
// Test 8: empty struct as input
// ═════════════════════════════════════════════════════════════════════════════

#[derive(Debug, Deserialize)]
struct EmptyInput {}

#[cleat_entry]
fn empty_input(_h: &cleat_sdk::HostCalls, _input: EmptyInput) -> Result<String, String> {
    Ok("empty-ok".to_string())
}

#[test]
fn test_empty_struct_input() {
    let (err_code, output) = run_export(export_fn!(empty_input), r#"{}"#);
    assert_eq!(err_code, 0);
    assert_eq!(output, "\"empty-ok\"");
}
