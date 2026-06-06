package engine

import (
	"context"
	"sync"
	"testing"
)

// TestRuntimeStdoutStderrRace verifies that concurrent WASM executions
// through PerExecution() do not race on stdout/stderr buffers.
//
// Before the fix, PerExecution() shared the Runtime's stdout/stderr
// bytes.Buffer across all concurrent executions, causing a data race
// between Reset() in InstantiateModuleNamed and writes by the wazero
// runtime during fn.Call().
//
// After the fix, each per-execution backend owns its own stdout/stderr
// buffers, so concurrent executions have independent buffer state.
func TestRuntimeStdoutStderrRace(t *testing.T) {
	ctx := context.Background()
	backend, err := NewWazeroBackend(ctx)
	if err != nil {
		t.Fatalf("NewWazeroBackend: %v", err)
	}
	defer backend.Close(ctx)

	wasmBytes := minimalWasm()

	const goroutines = 10
	const iterations = 20

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				execBackend := backend.PerExecution()
				// minimalWasm has no exports, so Execute will return an error.
				// The important thing is that InstantiateModule happens via
				// per-execution buffers without racing.
				// We ignore the error because minimalWasm has no exports.
				_, _ = execBackend.Execute(ctx, wasmBytes, "nonexistent", nil, nil)
				// Do NOT close execBackend — it shares the Runtime with backend,
				// and closing it would close the shared Runtime. The per-backend
				// stdout/stderr buffers are value-embedded, so not closing is safe.
			}
		}()
	}

	wg.Wait()
}
