//! The `updates` group of host-call bindings (cleat#3827). Empty until the PR that binds this group's calls lands.
//!
//! Install each binding as `g.set("__cleat_<name>", Func::from(...))`, exactly as `lib.rs` does for the calls bound so far, and
//! go through `guarded` for every effectful call. Conventions: `packages/cleat-ts/groups/README.md`.

use javy_plugin_api::javy::quickjs::Object;

pub(crate) fn install(_g: &Object<'_>) {}
