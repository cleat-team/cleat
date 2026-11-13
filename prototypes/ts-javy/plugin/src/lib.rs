//! cleat-js: a Javy plugin that gives QuickJS the one thing cleat's guest ABI needs in Phase 0, a way to read the
//! entry point's JSON input and write its JSON result. cleat's ABI (ABI.md section 1) is
//!
//!     entry(args_ptr: i32, args_len: i32, out_ptr: i32, max_out_len: i32) -> i64
//!
//! and a Javy-built module can only export `() -> ()` functions (docs-using-exports.md). The bridge is two exports on
//! this plugin, which `cleat-js-link` calls from a generated trampoline:
//!
//!     cleat_set_io(args_ptr, args_len, out_ptr, max_out_len)   called first, records the four ABI arguments
//!     cleat_take_result() -> i64                               called last, returns the packed ABI result
//!
//! with the user's JS entry point running in between, reading its input through `__cleat_args()` and writing its
//! result through `__cleat_result(json)` (or `__cleat_fail(message)`).

use std::cell::RefCell;

use javy_plugin_api::{
    Config, import_namespace,
    javy::{Runtime, quickjs::prelude::Func},
};

// The namespace a dynamically linked module would import this plugin's exports from. Unused in static mode.
import_namespace!("cleat-js-plugin-v0");

#[derive(Default)]
struct Io {
    args_ptr: u32,
    args_len: u32,
    out_ptr: u32,
    out_max: u32,
    /// Bytes written to the output buffer, and the ABI error code that goes with them.
    written: Option<(u32, u32)>,
}

thread_local! {
    static IO: RefCell<Io> = RefCell::new(Io::default());
}

/// Copies `s` into the host's output buffer, truncating to its capacity. Returns the bytes written.
fn write_out(io: &Io, s: &str) -> u32 {
    let bytes = s.as_bytes();
    let n = bytes.len().min(io.out_max as usize);
    // SAFETY: the host passed (out_ptr, out_max) as a writable region of this module's linear memory for the
    // duration of the call, and `n` is clamped to it.
    unsafe {
        std::ptr::copy_nonoverlapping(bytes.as_ptr(), io.out_ptr as *mut u8, n);
    }
    n as u32
}

#[unsafe(export_name = "cleat_set_io")]
pub extern "C" fn cleat_set_io(args_ptr: u32, args_len: u32, out_ptr: u32, out_max: u32) {
    IO.with(|io| {
        *io.borrow_mut() = Io { args_ptr, args_len, out_ptr, out_max, written: None };
    });
}

/// The packed ABI result: error code in the low 32 bits, bytes written in the high 32. A workflow that wrote no
/// result is an error, so the sentinel for "returned nothing" cannot be mistaken for an empty success.
#[unsafe(export_name = "cleat_take_result")]
pub extern "C" fn cleat_take_result() -> i64 {
    IO.with(|io| {
        let mut io = io.borrow_mut();
        let (len, code) = match io.written {
            Some(w) => w,
            None => (write_out(&io, r#"{"error":"the workflow returned no result"}"#), 1),
        };
        io.written = None;
        ((len as i64) << 32) | (code as i64)
    })
}

fn modify_runtime(runtime: Runtime) -> Runtime {
    runtime.context().with(|ctx| {
        let g = ctx.globals();
        g.set(
            "__cleat_args",
            Func::from(|| -> String {
                IO.with(|io| {
                    let io = io.borrow();
                    // SAFETY: (args_ptr, args_len) is the input region the host wrote before the call.
                    let b = unsafe { std::slice::from_raw_parts(io.args_ptr as *const u8, io.args_len as usize) };
                    String::from_utf8_lossy(b).into_owned()
                })
            }),
        )
        .unwrap();
        g.set(
            "__cleat_result",
            Func::from(|json: String| {
                IO.with(|io| {
                    let mut io = io.borrow_mut();
                    let n = write_out(&io, &json);
                    io.written = Some((n, 0));
                })
            }),
        )
        .unwrap();
        g.set(
            "__cleat_fail",
            Func::from(|message: String| {
                IO.with(|io| {
                    let mut io = io.borrow_mut();
                    // JSON-encode the message by hand: no serde in this plugin.
                    let mut s = String::from(r#"{"error":""#);
                    for c in message.chars() {
                        match c {
                            '"' => s.push_str("\\\""),
                            '\\' => s.push_str("\\\\"),
                            '\n' => s.push_str("\\n"),
                            '\r' => s.push_str("\\r"),
                            '\t' => s.push_str("\\t"),
                            c if (c as u32) < 0x20 => s.push_str(&format!("\\u{:04x}", c as u32)),
                            c => s.push(c),
                        }
                    }
                    s.push_str("\"}");
                    let n = write_out(&io, &s);
                    io.written = Some((n, 1));
                })
            }),
        )
        .unwrap();
    });
    runtime
}

#[unsafe(export_name = "initialize-runtime")]
pub extern "C" fn initialize_runtime() {
    javy_plugin_api::initialize_runtime(Config::default, modify_runtime).unwrap();
}
