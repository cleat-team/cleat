//! Rust SDK for the cleat durable execution framework.
//!
//! Provides the [`HostCalls`] struct for making durable API calls from WASM
//! workflows, and memory helpers for the cleat ABI.

pub mod host_calls;
pub mod memory;

pub use host_calls::HostCalls;
