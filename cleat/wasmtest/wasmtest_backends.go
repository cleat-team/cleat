//go:build cgo

package wasmtest

import (
	"context"

	"github.com/cleat-team/cleat/internal/host"
)

// wasmtimeBackend creates a wasmtime backend and returns EngineOptions
// that register it for all supported languages.
func wasmtimeBackendOptions() []host.EngineOption {
	ctx := context.Background()
	wt, err := host.NewWasmtimeBackend(ctx)
	if err != nil {
		return nil
	}
	return []host.EngineOption{
		host.WithBackend("go", wt),
		host.WithBackend("assemblyscript", wt),
		host.WithBackend("python", wt),
		host.WithBackend("java", wt),
		// Rust crates are compiled with wasm32-unknown-unknown (no WASI),
		// but wasmtime-go v44 still crashes on fn.Call for Rust cdylib
		// core modules regardless of import set.
	}
}
