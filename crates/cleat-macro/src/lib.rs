#![allow(clippy::collapsible_match)]
mod entry;
mod test_attr;

use proc_macro::TokenStream;

/// Marks a function as a cleat cleat workflow entry point.
///
/// This attribute transforms a regular Rust function into a WASM export
/// compatible with the cleat host runtime ABI. The function must take
/// `&HostCalls` as its first parameter, followed by an input type that
/// implements `Deserialize`. The return type must implement `Serialize`
/// for the success case and `Display` for the error case.
///
/// ## Example
///
/// ```ignore
/// use cleat_sdk::HostCalls;
/// use cleat_macro::cleat_entry;
/// use serde::{Deserialize, Serialize};
///
/// #[derive(Deserialize)]
/// struct PlaceOrderInput { user_id: String, cart: Vec<CartItem> }
///
/// #[cleat_entry]
/// fn place_order(h: &HostCalls, input: PlaceOrderInput) -> Result<String, String> {
///     h.cleat_log(&format!("Starting order for user {}", input.user_id));
///     let (reservation_json, err) = h.cleat_call("inventory", "Reserve", &serde_json::to_string(&input).unwrap());
///     // ...
///     Ok("done".to_string())
/// }
/// ```
///
/// The generated WASM export function has the signature:
/// `(args_ptr: *const u8, args_len: u32, out_ptr: *mut u8, max_out_len: u32) -> i64`
#[proc_macro_attribute]
pub fn cleat_entry(_attr: TokenStream, item: TokenStream) -> TokenStream {
    entry::cleat_entry_impl(item.into()).into()
}

/// Marks a function as a cleat test that safely handles [`SuspendSentinel`].
///
/// This is a lightweight wrapper for native (non-WASM) test execution. It
/// wraps the function body in `std::panic::catch_unwind` and, if a
/// [`SuspendSentinel`] panic is caught, converts it to a test-friendly
/// result so the test does not crash.
///
/// Unlike `#[cleat_entry]`, this attribute does NOT generate WASM exports,
/// does NOT require a `&HostCalls` parameter, and does NOT perform JSON
/// deserialization. It is intended for `#[cfg(test)]` modules where the
/// workflow code may call `HostCalls` methods that panic with
/// [`SuspendSentinel`].
///
/// ## Supported signatures
///
/// ```ignore
/// use cleat_sdk::test::MockHostCalls;
/// use cleat_macro::cleat_test;
///
/// #[cleat_test]
/// fn test_workflow_logic() {
///     let mut mock = MockHostCalls::new();
///     // test code that may encounter SuspendSentinel
/// }
///
/// #[cleat_test]
/// fn test_with_result() -> Result<(), String> {
///     let mut mock = MockHostCalls::new();
///     // ...
///     Ok(())
/// }
/// ```
///
/// ## Behavior on SuspendSentinel
///
/// - For `fn test() -> ()`: the function returns (passes) silently.
/// - For `fn test() -> Result<(), E>`: the function returns `Ok(())`.
/// - Any other panic is re-dispatched via `resume_unwind`.
#[proc_macro_attribute]
pub fn cleat_test(_attr: TokenStream, item: TokenStream) -> TokenStream {
    test_attr::cleat_test_impl(item.into()).into()
}
