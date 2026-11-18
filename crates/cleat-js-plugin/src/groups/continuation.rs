//! The `continuation` group of host-call bindings (cleat#3827): `cleat_continue_as_new` and `cleat_continue_as_new_versioned`.
//! (The group is `continuation` and not `continue`: that is a Rust keyword, so `mod continue;` does not compile.)
//!
//! WHAT THE HOST DOES, which is not what "suspends" suggests. `engine/lifecycle.go` records a `continue_as_new` event, sets the
//! segment's suspension reason to `continue_as_new` with the new input, and RETURNS SUCCESS to the guest. The guest is not stopped
//! by the call: it runs on until it returns, and the worker then starts the new run. So a guest that carries on after the call keeps
//! recording events for a run that has already been replaced. The TypeScript side therefore never lets the promise settle (see
//! `groups/continuation.ts`), and this file only reports the host's answer.
//!
//! NEITHER IS A DISPATCH POINT. Neither the Go nor the Rust SDK dispatches updates before continuing as new, so this does not
//! either; an update that arrives after the last dispatch point is not delivered to the run that is being replaced.
//!
//! BOTH MARK THE SEGMENT SUSPENDED when the host accepts the call. The host does not: its ContinueAsNew sets the suspension
//! reason and returns 0, which is not the sentinel the SDK's other suspending wrappers see, and `HostCalls::continue_as_new` never
//! calls `mark_suspended`. So without this an accepted continuation left the segment looking live, and a guarded call made after
//! it (an unawaited `continueAsNew` followed by `await ctx.listCrons()`) reached the host and recorded an event for a run that had
//! been replaced; a `scheduleCron` or a `send` there would be a real side effect (found by cleat-review on cleat#4043, measured as
//! history `[continue_as_new, list_crons]`). `guarded` then reports `suspended` for this call itself and for everything after it.
//!
//! BOTH ARE GUARDED: each records an event, and a segment must record nothing past the call that suspended it. This one matters more
//! than most: a continue_as_new made after the segment had suspended would OVERWRITE that suspension's reason, so the run would
//! restart instead of waiting for its sleep. The defer-phase refusal is the SDK's (`HostCalls::continue_as_new` checks it before the
//! call, because the host does not), and arrives here as an ordinary error.
//!
//! THE VERSION IS CHECKED HERE. The engine takes an `i32` and treats 0 as "the current version", so a TypeScript number that is
//! negative, fractional, not finite or too large would be truncated or wrapped into some other version. It must be a whole number from
//! 1 to `i32::MAX`. The message names no number: formatting an `f64` pulls the float `Display` code into the plugin (cleat#4031 measured
//! about 20 KB for that), and the caller knows what it passed.

use javy_plugin_api::javy::quickjs::{Object, prelude::Func};
use serde_json::json;

use crate::guarded;
use cleat_sdk::HostCalls;

pub(crate) fn install(g: &Object<'_>) {
    g.set(
        "__cleat_continue_as_new",
        Func::from(|input_json: String| -> String {
            guarded(|| match HostCalls.continue_as_new(&input_json) {
                Ok(()) => {
                    cleat_sdk::mark_suspended();
                    json!({ "ok": null })
                }
                Err(e) => json!({ "err": e }),
            })
        }),
    )
    .unwrap();

    g.set(
        "__cleat_continue_as_new_versioned",
        Func::from(|input_json: String, version: f64| -> String {
            guarded(|| {
                if !version.is_finite() || version.fract() != 0.0 || version < 1.0 || version > i32::MAX as f64 {
                    return json!({ "err": "continueAsNew: options.version must be a whole number from 1 to 2147483647 (omit it to stay on the current version)" });
                }
                match HostCalls.continue_as_new_versioned(&input_json, version as i32) {
                    Ok(()) => {
                        cleat_sdk::mark_suspended();
                        json!({ "ok": null })
                    }
                    Err(e) => json!({ "err": e }),
                }
            })
        }),
    )
    .unwrap();
}
