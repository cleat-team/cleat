//go:build cgo

package engine

import (
	"strings"

	"github.com/bytecodealliance/wasmtime-go/v44"
)

func (b *wasmtimeBackend) registerWasiStubs(linker *wasmtime.Linker) error {
	// DefineWasi provides the full WASI preview1 implementation (fd_write,
	// random_get, clock_time_get, environ_get, proc_exit, sched_yield, etc.).
	// Required by Go wasip1, Rust wasm32-wasip1, and other WASI-compiled modules.
	if err := linker.DefineWasi(); err != nil {
		return err
	}

	// reset_adapter_state is required by core modules extracted from Component
	// Model binaries produced by componentize-py. It is a no-op.
	if err := linker.FuncWrap("wasi_snapshot_preview1", "reset_adapter_state",
		func() {},
	); err != nil {
		return err
	}

	// environ_get and environ_sizes_get are imported by Rust wasm32-wasip1
	// modules. wasmtime-go v44's DefineWasi() may or may not provide them
	// depending on the exact C library version. Provide fallback stubs;
	// errors from duplicate definition are benign.
	_ = linker.FuncWrap("wasi_snapshot_preview1", "environ_get",
		func(_ int32, _ int32) int32 { return 0 },
	)
	_ = linker.FuncWrap("wasi_snapshot_preview1", "environ_sizes_get",
		func(_ int32, _ int32) int32 { return 0 },
	)

	return nil
}

// abortImportType returns the type a module declares for its `env.abort`
// import, or nil if it does not import one.
//
// Different toolchains declare different aborts. AssemblyScript imports
// (msg, file, line, col i32); the core modules inside a componentize-py binary
// import a no-argument abort. A Linker holds one definition per (module, name),
// so registering a fixed shape satisfies one toolchain and locks out the other.
func abortImportType(m *wasmtime.Module) *wasmtime.FuncType {
	if m == nil {
		return nil
	}
	for _, imp := range m.Imports() {
		if imp.Module() != "env" || imp.Name() == nil || *imp.Name() != "abort" {
			continue
		}
		if ft := imp.Type().FuncType(); ft != nil {
			return ft
		}
	}
	return nil
}

// zeroVal returns the zero value for a wasm value type, used to satisfy the
// results of a stubbed import.
func zeroVal(t *wasmtime.ValType) wasmtime.Val {
	switch t.Kind() {
	case wasmtime.KindI64:
		return wasmtime.ValI64(0)
	case wasmtime.KindF32:
		return wasmtime.ValF32(0)
	case wasmtime.KindF64:
		return wasmtime.ValF64(0)
	default:
		return wasmtime.ValI32(0)
	}
}

// registerEnvStubs registers the `env` imports the host stubs out.
//
// abortTy is the type the module being instantiated declares for `env.abort`;
// pass nil when no module is available and the historical AssemblyScript shape
// should be assumed.
//
// The abort stub used to be registered unconditionally as
// (msg, file, line, col i32), with a comment reasoning that a differently-typed
// Python abort would be picked up by DefineUnknownImportsAsTraps and that "the
// first registration wins". The first registration does win, which is exactly
// the problem: a component whose core module imports a no-argument abort was
// rejected at instantiation with
//
//	incompatible import type for `env::abort`
//	expected type `(func)`, found type `(func (param i32 i32 i32 i32))`
//
// -- and the module never got to the point where the trap-defaults could help.
// Matching the declared type serves both toolchains from the same linker.
//
// Still a no-op rather than a trap, deliberately: making abort trap would be a
// behaviour change for AssemblyScript guests, and is a separate decision from
// getting the arity right.
func (b *wasmtimeBackend) registerEnvStubs(linker *wasmtime.Linker, abortTy *wasmtime.FuncType) error {
	if abortTy == nil {
		_ = linker.FuncWrap("env", "abort", func(_ int32, _ int32, _ int32, _ int32) {})
		return nil
	}
	results := abortTy.Results()
	_ = linker.FuncNew("env", "abort", abortTy,
		func(_ *wasmtime.Caller, _ []wasmtime.Val) ([]wasmtime.Val, *wasmtime.Trap) {
			out := make([]wasmtime.Val, len(results))
			for i, rt := range results {
				out[i] = zeroVal(rt)
			}
			return out, nil
		})
	return nil
}

func (b *wasmtimeBackend) registerTeavmStubs(linker *wasmtime.Linker) error {
	// putwcharsOut
	if err := linker.FuncWrap("teavm", "putwcharsOut",
		func(chars, count int32) {},
	); err != nil {
		if isWasmtimeLinkerError(err) {
			return err
		}
	}
	// currentTimeMillis
	if err := linker.FuncWrap("teavm", "currentTimeMillis",
		func() float64 { return 0 },
	); err != nil {
		if isWasmtimeLinkerError(err) {
			return err
		}
	}
	// logString
	if err := linker.FuncWrap("teavm", "logString",
		func(ptr int32) {},
	); err != nil {
		if isWasmtimeLinkerError(err) {
			return err
		}
	}
	// logInt
	if err := linker.FuncWrap("teavm", "logInt",
		func(ptr int32) {},
	); err != nil {
		if isWasmtimeLinkerError(err) {
			return err
		}
	}
	// logOutOfMemory
	if err := linker.FuncWrap("teavm", "logOutOfMemory",
		func() {},
	); err != nil {
		if isWasmtimeLinkerError(err) {
			return err
		}
	}
	return nil
}

func isWasmtimeLinkerError(err error) bool {
	return err != nil && !isDuplicateDefinition(err)
}

func isDuplicateDefinition(err error) bool {
	// wasmtime returns errors containing "duplicate" when a function is
	// already defined. Since stubs are best-effort, this is not fatal.
	return err != nil && strings.Contains(err.Error(), "duplicate")
}
