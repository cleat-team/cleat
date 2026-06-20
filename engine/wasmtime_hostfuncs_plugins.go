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
	
	return linker.FuncWrap("env", "plugin_call", func(caller *wasmtime.Caller,
		pluginNamePtr, pluginNameLen,
		funcNamePtr, funcNameLen,
		inputPtr, inputLen,
		responsePtr, responseMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		pluginName, ok := wasmtimeReadServiceName(buf, pluginNamePtr, pluginNameLen)
		if !ok {
			return errBadParamInt64
		}
		funcName, ok := wasmtimeReadServiceName(buf, funcNamePtr, funcNameLen)
		if !ok {
			return errBadParamInt64
		}
		inputJSON, ok := wasmtimeReadStringValidated(buf, inputPtr, inputLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.PluginCall(ctxWithMem(context.Background(), buf), nil, pluginName, funcName, inputJSON, uint32(responsePtr), uint32(responseMaxLen))
	})
}

func (b *wasmtimeBackend) registerCleatPluginCallStreaming(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("plugin_call_streaming") {
		return nil
	}
	
	return linker.FuncWrap("env", "plugin_call_streaming", func(caller *wasmtime.Caller,
		pluginNamePtr, pluginNameLen,
		funcNamePtr, funcNameLen,
		inputPtr, inputLen,
		responsePtr, responseMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		pluginName, ok := wasmtimeReadServiceName(buf, pluginNamePtr, pluginNameLen)
		if !ok {
			return errBadParamInt64
		}
		funcName, ok := wasmtimeReadServiceName(buf, funcNamePtr, funcNameLen)
		if !ok {
			return errBadParamInt64
		}
		inputJSON, ok := wasmtimeReadStringValidated(buf, inputPtr, inputLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.PluginCallStreaming(ctxWithMem(context.Background(), buf), nil, pluginName, funcName, inputJSON, uint32(responsePtr), uint32(responseMaxLen))
	})
}

func (b *wasmtimeBackend) registerCleatCreatePromise(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_create_promise") {
		return nil
	}
	
	return linker.FuncWrap("env", "cleat_create_promise", func(caller *wasmtime.Caller,
		namePtr, nameLen, promiseIDPtr, promiseIDMaxLen int32, ttlMs int64) int64 {
		_ = ttlMs
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		name, ok := wasmtimeReadServiceName(buf, namePtr, nameLen)
		if !ok {
			return errBadParamInt64
		}
		return h.CreatePromise(context.Background(), nil, name, uint32(promiseIDPtr), uint32(promiseIDMaxLen))
	})
}

func (b *wasmtimeBackend) registerCleatAwaitPromise(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_await_promise") {
		return nil
	}
	
	return linker.FuncWrap("env", "cleat_await_promise", func(caller *wasmtime.Caller,
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
		return h.AwaitPromise(context.Background(), nil, promiseID, timeoutMs, uint32(resultPtr), uint32(resultMaxLen))
	})
}

func (b *wasmtimeBackend) registerCleatAcquireLock(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_acquire_lock") {
		return nil
	}
	
	return linker.FuncWrap("env", "cleat_acquire_lock", func(caller *wasmtime.Caller,
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
	
	return linker.FuncWrap("env", "cleat_release_lock", func(caller *wasmtime.Caller,
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
	
	return linker.FuncWrap("env", "cleat_resolve_promise", func(caller *wasmtime.Caller,
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
		value, ok := wasmtimeReadStringValidated(buf, valPtr, valLen, int32(MaxWasmStringLen))
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
	
	return linker.FuncWrap("env", "cleat_reject_promise", func(caller *wasmtime.Caller,
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
		errMsg, ok := wasmtimeReadStringValidated(buf, errPtr, errLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.RejectPromise(context.Background(), nil, promiseID, errMsg)
	})
}
