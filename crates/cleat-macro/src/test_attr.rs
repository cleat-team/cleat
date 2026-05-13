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
/// 2. If a [`SuspendSentinel`] panic is caught, converts it to a test-friendly
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
             a default success value when SuspendSentinel is caught.",
        )
        .to_compile_error();
    }

    let sentinel_handler = if is_result_unit {
        quote! {
            Err(panic_err) => {
                if panic_err.downcast_ref::<cleat_sdk::SuspendSentinel>().is_some() {
                    return ::std::result::Result::Ok(());
                }
                ::std::panic::resume_unwind(panic_err);
            }
        }
    } else {
        quote! {
            Err(panic_err) => {
                if panic_err.downcast_ref::<cleat_sdk::SuspendSentinel>().is_some() {
                    return;
                }
                ::std::panic::resume_unwind(panic_err);
            }
        }
    };

    let expanded = quote! {
        #(#fn_attrs)*
        #[test]
        #fn_vis #fn_sig {
            let __cleat_test_result = ::std::panic::catch_unwind(|| {
                #fn_block
            });
            match __cleat_test_result {
                Ok(inner) => inner,
                #sentinel_handler
            }
        }
    };

    expanded
}
