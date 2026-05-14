use proc_macro2::TokenStream;
use quote::quote;
use syn::{FnArg, ItemFn, Pat};

pub fn cleat_entry_impl(item: TokenStream) -> TokenStream {
    let input_fn: ItemFn = syn::parse2(item).expect("#[cleat_entry] requires a function");

    // A1: Reject async fn — #[cleat_entry] does not support async functions.
    if input_fn.sig.asyncness.is_some() {
        return syn::Error::new_spanned(
            &input_fn.sig.ident,
            "#[cleat_entry] does not support async functions: async functions export Futures which cannot be used as WASM exports. Remove the 'async' keyword and use synchronous code with cleat_sdk calls.",
        )
        .to_compile_error();
    }

    // A1: Validate return type is Result<T, E>.
    let return_type_is_result = match &input_fn.sig.output {
        syn::ReturnType::Type(_, ty) => {
            if let syn::Type::Path(type_path) = &**ty {
                type_path
                    .path
                    .segments
                    .last()
                    .map(|seg| seg.ident == "Result")
                    .unwrap_or(false)
            } else {
                false
            }
        }
        syn::ReturnType::Default => false,
    };
    if !return_type_is_result {
        return syn::Error::new_spanned(
            &input_fn.sig.output,
            "#[cleat_entry] function must return Result<T, E>: must return Result to allow the macro to format success and error values for the WASM ABI. Change return type to Result<T, E>.",
        )
        .to_compile_error();
    }

    let fn_vis = &input_fn.vis;
    let fn_name = &input_fn.sig.ident;
    let fn_block = &input_fn.block;
    let fn_ret = &input_fn.sig.output;
    let inner_name = quote::format_ident!("__cleat_inner_{}", fn_name);

    let mut all_args: Vec<&FnArg> = input_fn.sig.inputs.iter().collect();

    if all_args.is_empty() {
        return syn::Error::new_spanned(
            &input_fn.sig.ident,
            "#[cleat_entry] function must have at least a &HostCalls parameter: must have &HostCalls as first parameter to access the cleat runtime (logging, sleep, state, etc.). Add 'h: &HostCalls' as the first parameter.",
        )
        .to_compile_error();
    }

    // A1: Validate that the first parameter is &HostCalls.
    if let Some(FnArg::Typed(first_pt)) = all_args.first() {
        let is_hostcalls = matches!(
            &*first_pt.ty,
            syn::Type::Reference(type_ref)
                if matches!(&*type_ref.elem, syn::Type::Path(type_path)
                    if type_path.path.segments.last().map(|s| s.ident == "HostCalls").unwrap_or(false))
        );
        if !is_hostcalls {
            return syn::Error::new_spanned(
                &first_pt.ty,
                "first parameter must be &HostCalls: must be &HostCalls. Replace the current type with 'h: &HostCalls' to access the cleat runtime API.",
            )
            .to_compile_error();
        }
    }

    // Pop the first arg (HostCalls). We'll add h: &HostCalls to the inner fn.
    all_args.remove(0);

    // Validate: #[cleat_entry] supports exactly one input parameter beyond &HostCalls.
    if all_args.len() > 1 {
        return syn::Error::new_spanned(
            all_args[1],
            format!(
                "`#[cleat_entry]` functions must have exactly one input parameter (beyond `&HostCalls`). Found {} extra parameters. Reason: WASM exports receive a single JSON payload, so only one user parameter is supported. Combine parameters into a struct with #[derive(Deserialize)].",
                all_args.len()
            ),
        )
        .to_compile_error();
    }

    // Rebuild inner function params (including HostCalls).
    let inner_params = {
        let mut params = vec![quote! { h: &cleat_sdk::HostCalls }];
        for a in &all_args {
            if let FnArg::Typed(pt) = a {
                params.push(quote! { #pt });
            }
        }
        params
    };

    // Build the call arguments for the inner function.
    // Also collect any compile errors (e.g. for destructuring patterns).
    let mut compile_errors = proc_macro2::TokenStream::new();
    let inner_call_args: Vec<_> = {
        let mut args = vec![quote! { &h }];
        for a in &all_args {
            if let FnArg::Typed(pt) = a && let Pat::Ident(pi) = &*pt.pat {
                    let name = &pi.ident;
                    args.push(quote! { #name });
                } else {
                    // A1: Reject destructuring patterns — emit a compile error with span.
                    compile_errors.extend(
                        syn::Error::new_spanned(
                            &pt.pat,
                            "destructuring patterns in #[cleat_entry] parameters are not supported; use a plain variable name instead",
                        ).into_compile_error()
                    );
                }
            }
        }
        args
    };

    // Build deserialization code for the input args.
    let deser_code = if all_args.len() == 1 {
        if let FnArg::Typed(pt) = &all_args[0] {
            let ty = &pt.ty;
            if let Pat::Ident(pi) = &*pt.pat {
                let name = &pi.ident;
                quote! {
                    let #name: #ty = match serde_json::from_str(&args_json) {
                        Ok(v) => v,
                        Err(e) => {
                            let err_msg = format!("{{\"error\": \"{}\"}}", e);
                            let n = unsafe { cleat_sdk::memory::write_string(out_ptr, max_out_len, &err_msg) };
                            return cleat_sdk::memory::encode_export_result(1, n);
                        }
                    };
                }
            } else {
                quote! {}
            }
        } else {
            quote! {}
        }
    } else {
        // No args after HostCalls
        quote! {}
    };

    let expanded = quote! {
        #compile_errors
        #[allow(non_snake_case)]
        #fn_vis fn #inner_name(#(#inner_params),*) #fn_ret #fn_block

        #[no_mangle]
        pub unsafe extern "C" fn #fn_name(
            args_ptr: *const u8,
            args_len: u32,
            out_ptr: *mut u8,
            max_out_len: u32,
        ) -> i64 {
            let args_json = unsafe { cleat_sdk::memory::read_string(args_ptr, args_len) };

            #deser_code

            let h = cleat_sdk::HostCalls;

            let result = std::panic::catch_unwind(|| {
                #inner_name(#(#inner_call_args),*)
            });

            match result {
                Ok(inner_result) => {
                    match cleat_sdk::format_cleat_result(inner_result) {
                        Ok(output_json) => {
                            let n = unsafe { cleat_sdk::memory::write_string(out_ptr, max_out_len, &output_json) };
                            cleat_sdk::memory::encode_export_result(0, n)
                        }
                        Err(err_msg) => {
                            let err_json = serde_json::json!({"error": err_msg}).to_string();
                            let n = unsafe { cleat_sdk::memory::write_string(out_ptr, max_out_len, &err_json) };
                            cleat_sdk::memory::encode_export_result(1, n)
                        }
                    }
                }
                Err(panic_err) => {
                    if panic_err.downcast_ref::<cleat_sdk::SuspendSentinel>().is_some() {
                        return cleat_sdk::memory::SUSPEND_SENTINEL;
                    }
                    std::panic::resume_unwind(panic_err);
                }
            }
        }
    };

    expanded
}
