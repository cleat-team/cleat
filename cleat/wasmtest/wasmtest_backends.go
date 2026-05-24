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
		// Rust (wasm32-wasip1) and Java (TeaVM) still crash on
		// wasmtime-go v44 fn.Call — Rust hits nil pointer in the
		// WASI + non-Go path, Java hits unreachable in TeaVM runtime.
	}
}
