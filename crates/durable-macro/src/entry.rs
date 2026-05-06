use proc_macro2::TokenStream;
use quote::quote;
use syn::{FnArg, ItemFn, Pat};

pub fn durable_entry_impl(item: TokenStream) -> TokenStream {
    let input_fn: ItemFn = syn::parse2(item).expect("#[durable_entry] requires a function");

    let fn_vis = &input_fn.vis;
    let fn_name = &input_fn.sig.ident;
    let fn_block = &input_fn.block;
    let fn_ret = &input_fn.sig.output;
    let inner_name = quote::format_ident!("__durable_inner_{}", fn_name);

    let mut all_args: Vec<&FnArg> = input_fn.sig.inputs.iter().collect();

    if all_args.is_empty() {
        return syn::Error::new_spanned(
            &input_fn.sig.ident,
            "#[durable_entry] function must have at least a &HostCalls parameter",
        )
        .to_compile_error()
        .into();
    }

    // Pop the first arg (HostCalls). We'll add h: &HostCalls to the inner fn.
    all_args.remove(0);

    // Validate: #[durable_entry] supports exactly one input parameter beyond &HostCalls.
    if all_args.len() > 1 {
        return syn::Error::new_spanned(
            &all_args[1],
            format!(
                "`#[durable_entry]` functions must have exactly one input parameter (beyond `&HostCalls`). Found {} extra parameters.",
                all_args.len()
            ),
        )
        .to_compile_error()
        .into();
    }

    // Rebuild inner function params (including HostCalls).
    let inner_params = {
        let mut params = vec![quote! { h: &durable_sdk::HostCalls }];
        for a in &all_args {
            if let FnArg::Typed(pt) = a {
                params.push(quote! { #pt });
            }
        }
        params
    };

    // Build the call arguments for the inner function.
    let inner_call_args: Vec<_> = {
        let mut args = vec![quote! { &h }];
        for a in &all_args {
            if let FnArg::Typed(pt) = a {
                if let Pat::Ident(pi) = &*pt.pat {
                    let name = &pi.ident;
                    args.push(quote! { #name });
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
                            let n = unsafe { durable_sdk::memory::write_string(out_ptr, max_out_len, &err_msg) };
                            return durable_sdk::memory::encode_export_result(1, n);
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
        #[allow(non_snake_case)]
        #fn_vis fn #inner_name(#(#inner_params),*) #fn_ret #fn_block

        #[no_mangle]
        pub unsafe extern "C" fn #fn_name(
            args_ptr: *const u8,
            args_len: u32,
            out_ptr: *mut u8,
            max_out_len: u32,
        ) -> i64 {
            let args_json = unsafe { durable_sdk::memory::read_string(args_ptr, args_len) };

            #deser_code

            let h = durable_sdk::HostCalls;

            let result = std::panic::catch_unwind(|| {
                #inner_name(#(#inner_call_args),*)
            });

            match result {
                Ok(inner_result) => {
                    match durable_sdk::format_durable_result(inner_result) {
                        Ok(output_json) => {
                            let n = unsafe { durable_sdk::memory::write_string(out_ptr, max_out_len, &output_json) };
                            durable_sdk::memory::encode_export_result(0, n)
                        }
                        Err(err_msg) => {
                            let err_json = serde_json::json!({"error": err_msg}).to_string();
                            let n = unsafe { durable_sdk::memory::write_string(out_ptr, max_out_len, &err_json) };
                            durable_sdk::memory::encode_export_result(1, n)
                        }
                    }
                }
                Err(panic_err) => {
                    if panic_err.downcast_ref::<durable_sdk::SuspendSentinel>().is_some() {
                        return durable_sdk::memory::SUSPEND_SENTINEL;
                    }
                    std::panic::resume_unwind(panic_err);
                }
            }
        }
    };

    expanded.into()
}
