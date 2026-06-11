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

func (b *wasmtimeBackend) registerEnvStubs(linker *wasmtime.Linker) error {
	// AssemblyScript abort stub. AS modules import env.abort with
	// (msg i32, file i32, line i32, col i32). Python components may
	// define abort via DefineUnknownImportsAsTraps with a different
	// signature — the duplicate definition error from FuncWrap is
	// benign and can be ignored (the first registration wins).
	_ = linker.FuncWrap("env", "abort", func(_ int32, _ int32, _ int32, _ int32) {})
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
