package main

import "github.com/cleat-team/cleat/engine"

// wasmtimeLanguages and runsOnWasmtime forward to engine, which owns the list.
//
// They were defined here first, and that was half the problem: cleat/wasmtest
// had its own copy that disagreed, so the harness and the worker routed the same
// language to different backends and a passing harness test said nothing about
// what the product does. Forwarding rather than re-listing means the two cannot
// drift again -- there is only one list now.
var wasmtimeLanguages = engine.WasmtimeLanguages

func runsOnWasmtime(lang string) bool { return engine.RunsOnWasmtime(lang) }
