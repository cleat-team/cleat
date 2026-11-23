//! The `locks` group of host-call bindings (cleat#3827): `cleat_acquire_lock` and `cleat_release_lock`.
//!
//! NEITHER WAITS. `acquire_lock` answers at once with "acquired" or "not acquired" (it never blocks until the key is free), and
//! `release_lock` is immediate, so neither is a dispatch point and neither goes through `dispatchThen`. The one way either can end
//! a segment is the host's refusal to start new work in a defer phase, which `HostCalls::acquire_lock_ms` turns into a
//! suspension (`stop_requested`), and `guarded` reports that as one.
//!
//! BOTH ARE EFFECTFUL and both are guarded: each records an event (`acquire_lock`, `release_lock`), and a segment must record
//! nothing past the call that suspended it. Without the guard a lock would be taken, in a store, for a run that is parked and
//! will never reach its release; the key would stay held until its TTL expired.
//!
//! THE TTL IS CHECKED HERE, which no other SDK does. The engine hands it to the store unchanged, and a TTL below one millisecond
//! is truncated to zero by the `as i64` below, so the key is born expired and the next caller takes it: two runs inside one
//! critical section, nothing reported (the failure shape `engine/db.go` records for a TTL truncated to whole seconds). A TTL that
//! cannot be a positive whole number of milliseconds is an error instead.

use javy_plugin_api::javy::quickjs::{Object, prelude::Func};
use serde_json::json;

use crate::guarded;
use cleat_sdk::HostCalls;

pub(crate) fn install(g: &Object<'_>) {
    g.set(
        "__cleat_acquire_lock",
        Func::from(|key: String, ttl_ms: f64| -> String {
            // The TTL check is INSIDE `guarded`, so a call made after the segment suspended is answered the way every other late call is
            // (as a suspension) and not with an error. This is for consistency and is NOT observable from the guest: a test that raced a
            // late acquire with a bad TTL against the sleep that suspended the segment passed with the check outside as well, because
            // the segment ends at the suspension and nothing after it is run to see the difference.
            guarded(|| {
                if !ttl_ms.is_finite() || ttl_ms < 1.0 {
                    // No `{ttl_ms}` in the message: formatting an f64 pulls the float Display code into the plugin (measured: about
                    // 20 KB), and the caller knows the number it passed.
                    return json!({ "err": format!("acquireLock(key=\"{key}\"): ttlMs must be at least 1 millisecond; a lock with no time to live is born expired and the next caller takes it") });
                }
                match HostCalls.acquire_lock_ms(&key, ttl_ms as i64) {
                    (acquired, None) => json!({ "ok": acquired }),
                    (_, Some(e)) => json!({ "err": e }),
                }
            })
        }),
    )
    .unwrap();

    g.set(
        "__cleat_release_lock",
        Func::from(|key: String| -> String {
            guarded(|| match HostCalls.release_lock(&key) {
                None => json!({ "ok": null }),
                Some(e) => json!({ "err": e }),
            })
        }),
    )
    .unwrap();
}
