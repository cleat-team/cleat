//go:build cgo

package engine

import "github.com/bytecodealliance/wasmtime-go/v48"

// newWasmtimeEngine builds the wasmtime engine the worker runs components on, from the settings that CHANGE WHAT IS COMPILED. It is the one place that decides them, and the worker and its
// precompile child both call it, because an artifact one engine serializes is accepted only by an engine configured the same way (measured: fuel binds the artifact; parallel_compilation and
// the optimisation level do not). A second copy of these lines in the child is the shape that would let the two drift, and the failure would be a bare "failed to load code" on every miss.
//
//   - epoch interruption is always on: it is how a runaway guest is stopped;
//   - fuel is on iff the worker has an instruction limit;
//   - parallelCompilation is wasmtime's own default (true) unless the caller turns it off, which only the precompile child does, to lower the compile's memory high-water. It is left UNSET when
//     true, so a worker that does not use the child builds exactly the config it always did.
func newWasmtimeEngine(fuel, parallelCompilation bool) *wasmtime.Engine {
	cfg := wasmtime.NewConfig()
	cfg.SetEpochInterruption(true)
	if fuel {
		cfg.SetConsumeFuel(true)
	}
	if !parallelCompilation {
		cfg.SetParallelCompilation(false)
	}
	return wasmtime.NewEngineWithConfig(cfg)
}
