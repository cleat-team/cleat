//go:build cgo

package wasmtest

import (
	"context"

	"github.com/cleat-team/cleat/engine"
)

// wasmtimeBackendOptions creates a wasmtime backend and registers it for the
// languages engine.WasmtimeLanguages names.
//
// That list used to be maintained here, separately from the worker's, and the
// two disagreed. This one registered "python", which the worker never does and
// which fails on wasmtime anyway: its Component Model binary reaches the
// decomposition path and dies on `incompatible import type for env::abort`.
// That was reproduced through this harness with a real HostHandler, so it is
// not an artefact of a half-configured probe.
//
// Nothing caught it because nothing ran it. plugin-harness-ci.yml installs no
// Python toolchain, so TestPluginCalls_Wasm_Python skips and that registration
// had never once been exercised.
//
// A harness whose backend routing differs from the worker's is testing a
// configuration nobody runs, which is worse than testing one language on the
// wrong backend loudly. Reading the list from engine is what stops them
// drifting apart again.
//
// The previous note here said Rust was excluded because "wasmtime-go v44 still
// crashes on fn.Call for Rust cdylib core modules". That does not reproduce:
// examples/rust-workflow built for wasm32-unknown-unknown -- the cdylib shape
// `cleat build --target rust` ships -- executes on wasmtime, and Rust is now in
// the list. tests/cross-language builds that same target and passes on it,
// which is the coverage the old note never had.
func wasmtimeBackendOptions() []engine.EngineOption {
	ctx := context.Background()
	wt, err := engine.NewWasmtimeBackend(ctx)
	if err != nil {
		return nil
	}
	return []engine.EngineOption{
		engine.WithBackends(engine.WasmtimeLanguages, wt),
	}
}
