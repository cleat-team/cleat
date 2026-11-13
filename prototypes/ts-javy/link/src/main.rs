//! cleat-js-link: turns a Javy-built module into a module the cleat engine accepts, with no engine change.
//!
//! Javy exports each JavaScript function as `name: () -> ()`. cleat calls an entry point as
//! `name(args_ptr, args_len, out_ptr, max_out_len) -> i64` (ABI.md section 1). For each entry point named on the
//! command line this tool renames nothing: it takes Javy's `() -> ()` export, removes the export, and exports a new
//! function of the ABI's type under the same name that calls
//!
//!     cleat_set_io(a, b, c, d);  javy_export();  return cleat_take_result();
//!
//! The plugin's own exports (cleat_set_io, cleat_take_result), Javy's (`cabi_realloc`, `config-schema`, `_start`) are
//! then removed, so the module exports `memory` and the entry points and nothing else.

use anyhow::{Context, Result, bail};
use walrus::{ExportItem, FunctionBuilder, Module, ValType};

fn main() -> Result<()> {
    let mut args = std::env::args().skip(1);
    let (Some(input), Some(output)) = (args.next(), args.next()) else {
        bail!("usage: cleat-js-link <in.wasm> <out.wasm> <entry> [<entry>...]");
    };
    let entries: Vec<String> = args.collect();
    if entries.is_empty() {
        bail!("no entry points named");
    }

    let mut module = Module::from_file(&input).with_context(|| format!("reading {input}"))?;
    let set_io = module.exports.get_func("cleat_set_io").context("the module has no cleat_set_io export: was it built with the cleat-js plugin?")?;
    let take = module.exports.get_func("cleat_take_result").context("the module has no cleat_take_result export")?;

    for name in &entries {
        let js_fn = module
            .exports
            .get_func(name)
            .with_context(|| format!("the module exports no function named {name:?}; Javy exports the JS functions the WIT world names"))?;
        module.exports.remove(name)?;

        let mut b = FunctionBuilder::new(
            &mut module.types,
            &[ValType::I32, ValType::I32, ValType::I32, ValType::I32],
            &[ValType::I64],
        );
        let params: Vec<_> = (0..4).map(|_| module.locals.add(ValType::I32)).collect();
        b.func_body()
            .local_get(params[0])
            .local_get(params[1])
            .local_get(params[2])
            .local_get(params[3])
            .call(set_io)
            .call(js_fn)
            .call(take);
        let id = b.finish(params, &mut module.funcs);
        module.exports.add(name, id);
    }

    // Everything but memory and the entry points goes: the ABI's exports are entry points.
    let keep: std::collections::HashSet<&str> = entries.iter().map(String::as_str).chain(["memory"]).collect();
    let drop: Vec<_> = module
        .exports
        .iter()
        .filter(|e| !keep.contains(e.name.as_str()) && !matches!(e.item, ExportItem::Memory(_)))
        .map(|e| e.id())
        .collect();
    for id in drop {
        module.exports.delete(id);
    }

    module.emit_wasm_file(&output).with_context(|| format!("writing {output}"))?;
    Ok(())
}
