package main

import "github.com/cleat-team/cleat/engine"

// wasmtimeLanguages forwards to engine, which owns the list.
//
// It was defined here first, and that was half the problem: cleat/wasmtest had
// its own copy that disagreed, so the harness and the worker routed the same
// language to different backends and a passing harness test said nothing about
// what the product does. Forwarding rather than re-listing means the two cannot
// drift again -- there is only one list now.
//
// A runsOnWasmtime forwarder sat next to this and was called only by tests, so
// it asserted that a one-line alias forwards correctly rather than anything
// about the product. Those tests call engine.RunsOnWasmtime directly now, which
// is the function the worker's routing actually resolves to.
var wasmtimeLanguages = engine.WasmtimeLanguages
