//go:build cgo

package engine

import (
	"context"

	"github.com/bytecodealliance/wasmtime-go/v44"
)

func (b *wasmtimeBackend) registerCleatPluginCall(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("plugin_call") {
		return nil
	}

	return b.hostFunc(linker, "env", "plugin_call", func(caller *wasmtime.Caller,
		pluginNamePtr, pluginNameLen,
		funcNamePtr, funcNameLen,
		inputPtr, inputLen,
		responsePtr, responseMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return badParamDurableCall
		}
		pluginName, ok := wasmtimeReadServiceName(buf, pluginNamePtr, pluginNameLen)
		if !ok {
			return badParamDurableCall
		}
		funcName, ok := wasmtimeReadServiceName(buf, funcNamePtr, funcNameLen)
		if !ok {
			return badParamDurableCall
		}
		// Empty input is legitimate: plenty of plugin functions take none.
		inputJSON, ok := wasmtimeReadPayload(buf, inputPtr, inputLen, int32(MaxWasmStringLen))
		if !ok {
			return badParamDurableCall
		}
		return h.PluginCall(ctxWithMem(context.Background(), buf), nil, pluginName, funcName, inputJSON, uint32(responsePtr), uint32(responseMaxLen))
	})
}

func (b *wasmtimeBackend) registerCleatPluginCallStreaming(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("plugin_call_streaming") {
		return nil
	}

	return b.hostFunc(linker, "env", "plugin_call_streaming", func(caller *wasmtime.Caller,
		pluginNamePtr, pluginNameLen,
		funcNamePtr, funcNameLen,
		inputPtr, inputLen,
		responsePtr, responseMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return badParamDurableCall
		}
		pluginName, ok := wasmtimeReadServiceName(buf, pluginNamePtr, pluginNameLen)
		if !ok {
			return badParamDurableCall
		}
		funcName, ok := wasmtimeReadServiceName(buf, funcNamePtr, funcNameLen)
		if !ok {
			return badParamDurableCall
		}
		// Empty input is legitimate: plenty of plugin functions take none.
		inputJSON, ok := wasmtimeReadPayload(buf, inputPtr, inputLen, int32(MaxWasmStringLen))
		if !ok {
			return badParamDurableCall
		}
		return h.PluginCallStreaming(ctxWithMem(context.Background(), buf), nil, pluginName, funcName, inputJSON, uint32(responsePtr), uint32(responseMaxLen))
	})
}

func (b *wasmtimeBackend) registerCleatCreatePromise(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_create_promise") {
		return nil
	}

	// Four i32 parameters, no trailing ttlMs.
	//
	// This registration used to declare a fifth parameter, `ttlMs int64`, and
	// discard it with `_ = ttlMs`. Nothing ever passed it. ABI.md 2.34 specifies
	// `(param i32 i32 i32 i32) (result i64)`, and every guest agrees: the Go
	// generator (wasm/generator.go, name + promise_id_out), the Rust SDK
	// (crates/cleat-sdk/src/host_calls.rs), the Java SDK (HostCalls.java) and
	// AssemblyScript (packages/cleat-as/assembly/host-calls.ts, whose comment
	// spells the signature out). wazero's registration in engine/imports.go is
	// four as well.
	//
	// Since an arity mismatch is a hard link error, the extra parameter meant a
	// guest that called cleat_create_promise could not instantiate on the
	// wasmtime backend at all -- which is every guest the worker runs. Measured
	// 2026-09-01 through the production path (wasm.NeededEnvImports ->
	// registerAllImports -> linker.Instantiate):
	//
	//	incompatible import type for `env::cleat_create_promise`
	//	types incompatible: expected type `(func (param i32 i32 i32 i32) (result i64))`,
	//	                       found type `(func (param i32 i32 i32 i32 i64) (result i64))`
	//
	// The WIT interface (python-sdk/wit/cleat.wit) does carry a `ttl-ms: u64`,
	// which is presumably where the parameter came from, but that is the
	// component path: wasm.RewriteWitImports rewrites import *names* only, and
	// the canonical lowering of `func(name: string, ttl-ms: u64) -> string`
	// would be (i32, i32, i64, i32) -- neither this shape nor wazero's. So the
	// fifth parameter never matched any real guest on any path.
	return b.hostFunc(linker, "env", "cleat_create_promise", func(caller *wasmtime.Caller,
		namePtr, nameLen, promiseIDPtr, promiseIDMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		name, ok := wasmtimeReadServiceName(buf, namePtr, nameLen)
		if !ok {
			return errBadParamInt64
		}
		return h.CreatePromise(ctxWithMem(context.Background(), buf), nil, name, uint32(promiseIDPtr), uint32(promiseIDMaxLen))
	})
}

func (b *wasmtimeBackend) registerCleatAwaitPromise(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_await_promise") {
		return nil
	}

	return b.hostFunc(linker, "env", "cleat_await_promise", func(caller *wasmtime.Caller,
		promiseIDPtr, promiseIDLen int32, timeoutMs int64,
		resultPtr, resultMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		promiseID, ok := wasmtimeReadServiceName(buf, promiseIDPtr, promiseIDLen)
		if !ok {
			return errBadParamInt64
		}
		return h.AwaitPromise(ctxWithMem(context.Background(), buf), nil, promiseID, timeoutMs, uint32(resultPtr), uint32(resultMaxLen))
	})
}

func (b *wasmtimeBackend) registerCleatAcquireLock(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_acquire_lock") {
		return nil
	}

	return b.hostFunc(linker, "env", "cleat_acquire_lock", func(caller *wasmtime.Caller,
		keyPtr, keyLen int32, ttlMs int64) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		key, ok := wasmtimeReadServiceName(buf, keyPtr, keyLen)
		if !ok {
			return errBadParamInt64
		}
		return h.AcquireLock(context.Background(), nil, key, ttlMs)
	})
}

func (b *wasmtimeBackend) registerCleatReleaseLock(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_release_lock") {
		return nil
	}

	return b.hostFunc(linker, "env", "cleat_release_lock", func(caller *wasmtime.Caller,
		keyPtr, keyLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		key, ok := wasmtimeReadServiceName(buf, keyPtr, keyLen)
		if !ok {
			return errBadParamInt64
		}
		return h.ReleaseLock(context.Background(), nil, key)
	})
}

func (b *wasmtimeBackend) registerCleatResolvePromise(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_resolve_promise") {
		return nil
	}

	return b.hostFunc(linker, "env", "cleat_resolve_promise", func(caller *wasmtime.Caller,
		idPtr, idLen, valPtr, valLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		promiseID, ok := wasmtimeReadServiceName(buf, idPtr, idLen)
		if !ok {
			return errBadParamInt64
		}
		value, ok := wasmtimeReadPayload(buf, valPtr, valLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.ResolvePromise(context.Background(), nil, promiseID, value)
	})
}

func (b *wasmtimeBackend) registerCleatRejectPromise(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_reject_promise") {
		return nil
	}

	return b.hostFunc(linker, "env", "cleat_reject_promise", func(caller *wasmtime.Caller,
		idPtr, idLen, errPtr, errLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		promiseID, ok := wasmtimeReadServiceName(buf, idPtr, idLen)
		if !ok {
			return errBadParamInt64
		}
		errMsg, ok := wasmtimeReadPayload(buf, errPtr, errLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.RejectPromise(context.Background(), nil, promiseID, errMsg)
	})
}
