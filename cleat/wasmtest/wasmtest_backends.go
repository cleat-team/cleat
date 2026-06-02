//go:build cgo

package wasmtest

import (
	"context"

	"github.com/cleat-team/cleat/engine"
)

// wasmtimeBackend creates a wasmtime backend and returns EngineOptions
// that register it for all supported languages.
func wasmtimeBackendOptions() []engine.EngineOption {
	ctx := context.Background()
	wt, err := engine.NewWasmtimeBackend(ctx)
	if err != nil {
		return nil
	}
	return []engine.EngineOption{
		engine.WithBackend("go", wt),
		engine.WithBackend("assemblyscript", wt),
		engine.WithBackend("python", wt),
		engine.WithBackend("java", wt),
		// Rust crates are compiled with wasm32-unknown-unknown (no WASI),
		// but wasmtime-go v44 still crashes on fn.Call for Rust cdylib
		// core modules regardless of import set.
	}
}
