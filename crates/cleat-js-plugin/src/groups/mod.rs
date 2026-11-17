//! The host-call bindings that cleat#3827 adds, one module per group, so two groups written in parallel edit different files.
//!
//! `lib.rs` installs the calls bound before this layout existed and calls `install_all` once. A group's PR fills in its own
//! `install`; nothing here needs to change when it does. Conventions for every group: `packages/cleat-ts/groups/README.md`.

use javy_plugin_api::javy::quickjs::Object;

mod calls;
mod children;
mod continuation;
mod detached;
mod locks;
mod plugins;
mod schedules;
mod signals;
mod updates;
mod utilities;

pub(crate) fn install_all(g: &Object<'_>) {
    updates::install(g);
    signals::install(g);
    children::install(g);
    detached::install(g);
    locks::install(g);
    schedules::install(g);
    continuation::install(g);
    calls::install(g);
    plugins::install(g);
    utilities::install(g);
}
