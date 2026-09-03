use proc_macro2::TokenStream;
use quote::quote;
use syn::{ItemFn, ReturnType, Type};

/// Returns true when the function return type is `Result<(), E>` (generic over E).
fn is_result_with_unit_ok(return_type: &ReturnType) -> bool {
    match return_type {
        ReturnType::Type(_, ty) => {
            if let Type::Path(type_path) = ty.as_ref() {
                if let Some(segment) = type_path.path.segments.last() {
                    if segment.ident == "Result" {
                        if let syn::PathArguments::AngleBracketed(args) = &segment.arguments {
                            if let Some(syn::GenericArgument::Type(first_arg)) = args.args.first() {
                                // Check the Ok type is () — a tuple with zero elements.
                                if let Type::Tuple(tuple) = first_arg {
                                    return tuple.elems.is_empty();
                                }
                            }
                        }
                    }
                }
            }
            false
        }
        ReturnType::Default => false,
    }
}

/// Returns true when the function return type is `()` (unit).
fn is_unit_return(return_type: &ReturnType) -> bool {
    match return_type {
        ReturnType::Type(_, ty) => {
            if let Type::Tuple(tuple) = ty.as_ref() {
                tuple.elems.is_empty()
            } else {
                false
            }
        }
        ReturnType::Default => true,
    }
}

/// Implementation of the `#[cleat_test]` attribute macro.
///
/// This is a lightweight wrapper for native (non-WASM) test execution that:
///
/// 1. Wraps the function body in `std::panic::catch_unwind`.
/// 2. If the body suspended the workflow, converts it to a test-friendly
///    result (`Ok(())` for `Result<(), E>` returns, or a plain `return` for
///    unit returns) so the test does not crash.
/// 3. Adds `#[test]` to make the function discoverable by `cargo test`.
///
/// Unlike `#[cleat_entry]`, this macro does NOT generate WASM exports, does
/// NOT require a `&HostCalls` parameter, and does NOT perform JSON
/// deserialization of input. It is intended for `#[cfg(test)]` modules.
///
/// ## Supported signatures
///
/// ```ignore
/// #[cleat_test]
/// fn my_test() { ... }
///
/// #[cleat_test]
/// fn my_test() -> Result<(), String> { ... }
/// ```
pub fn cleat_test_impl(item: TokenStream) -> TokenStream {
    let input_fn: ItemFn = syn::parse2(item).expect("#[cleat_test] requires a function");

    // Reject async fn — #[cleat_test] does not support async.
    if input_fn.sig.asyncness.is_some() {
        return syn::Error::new_spanned(
            &input_fn.sig.ident,
            "#[cleat_test] does not support async functions. Use synchronous code instead.",
        )
        .to_compile_error();
    }

    let fn_vis = &input_fn.vis;
    let _fn_name = &input_fn.sig.ident;
    let fn_sig = &input_fn.sig;
    let fn_block = &input_fn.block;
    let fn_attrs = &input_fn.attrs;

    let is_result_unit = is_result_with_unit_ok(&fn_sig.output);
    let is_unit = is_unit_return(&fn_sig.output);

    if !is_unit && !is_result_unit {
        return syn::Error::new_spanned(
            &fn_sig.output,
            "#[cleat_test] requires the function to return `()` or `Result<(), E>`. \
             Other return types are not supported because the macro cannot construct \
             a default success value when the body suspends.",
        )
        .to_compile_error();
    }

    // What to return when the body suspended rather than finished. Suspension
    // is an ordinary outcome for a workflow under test, not a failure.
    let suspended_value = if is_result_unit {
        quote! { return ::std::result::Result::Ok(()); }
    } else {
        quote! { return; }
    };

    // No catch_unwind. It used to wrap the body to intercept a `SuspendSentinel`
    // panic, and it was worse than dead code: this macro runs on the HOST
    // target, where unwinding works, so suspension looked catchable in tests
    // while the shipped `wasm32-wasip1` guest -- built with panic=abort -- was
    // trapping on the same path. A test suite that passes on a target the
    // product does not ship is the shape IMPROVEMENT-PLAN 3.87 is about.
    let expanded = quote! {
        #(#fn_attrs)*
        #[test]
        #fn_vis #fn_sig {
            // Cleared before, not only after: the flag is a thread-local and
            // `cargo test` reuses threads across tests, so one suspending test
            // would otherwise make every later test on that thread look
            // suspended.
            cleat_sdk::clear_suspended();
            let __cleat_test_result = { #fn_block };
            if cleat_sdk::is_suspended() {
                cleat_sdk::clear_suspended();
                #suspended_value
            }
            __cleat_test_result
        }
    };

    expanded
}
