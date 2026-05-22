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
		host.WithBackend("rust", wt),
		host.WithBackend("assemblyscript", wt),
		// Python Component Model binaries go through the engine's wazero
		// executeComponent path which handles WASI 0.2.0 resource types.
		// Java uses the legacy wazero path due to wasmtime-go crash.
		host.WithBackend("java", wt),
	}
}
