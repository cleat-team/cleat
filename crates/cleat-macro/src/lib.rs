mod entry;

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
