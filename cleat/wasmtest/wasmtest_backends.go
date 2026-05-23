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
		host.WithBackend("python", wt),
		// Java uses the legacy wazero path due to wasmtime-go crash.
		host.WithBackend("java", wt),
	}
}
