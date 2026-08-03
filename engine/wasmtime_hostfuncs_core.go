//go:build cgo

package engine

import (
	"context"

	"github.com/bytecodealliance/wasmtime-go/v44"
)

func (b *wasmtimeBackend) registerCleatSleep(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_sleep") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_sleep", func(durationMs int64) int64 {
		return b.handler.DurableSleep(context.Background(), nil, durationMs)
	})
}

func (b *wasmtimeBackend) registerCleatNow(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_now") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_now", func() int64 {
		return b.handler.Now(context.Background())
	})
}

func (b *wasmtimeBackend) registerCleatRandom(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_random") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_random", func() int64 {
		return b.handler.Random(context.Background())
	})
}

func (b *wasmtimeBackend) registerCleatLog(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_log") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_log", func(caller *wasmtime.Caller,
		msgPtr, msgLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		msg, ok := wasmtimeReadStringValidated(buf, msgPtr, msgLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.DurableLog(context.Background(), nil, msg)
	})
}

func (b *wasmtimeBackend) registerCleatVersion(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_version") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_version", func() int64 {
		return b.handler.Version(context.Background())
	})
}

func (b *wasmtimeBackend) registerCleatMinVersion(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_min_version") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_min_version", func() int64 {
		return b.handler.MinVersion(context.Background())
	})
}

func (b *wasmtimeBackend) registerCleatUUID(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_uuid") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_uuid", func(caller *wasmtime.Caller,
		seedPtr, seedLen, uuidPtr, uuidMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		seed, ok := wasmtimeReadStringValidated(buf, seedPtr, seedLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.UUID(context.Background(), nil, seed, uint32(uuidPtr), uint32(uuidMaxLen))
	})
}

func (b *wasmtimeBackend) registerCleatWorkflowID(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_workflow_id") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_workflow_id", func(caller *wasmtime.Caller,
		idPtr, idMaxLen int32) int64 {
		h := b.handler
		_, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		return h.WorkflowID(context.Background(), nil, uint32(idPtr), uint32(idMaxLen))
	})
}

func (b *wasmtimeBackend) registerCleatRunID(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_run_id") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_run_id", func(caller *wasmtime.Caller,
		idPtr, idMaxLen int32) int64 {
		h := b.handler
		_, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		return h.RunID(context.Background(), nil, uint32(idPtr), uint32(idMaxLen))
	})
}

func (b *wasmtimeBackend) registerCleatSend(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_send") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_send", func(caller *wasmtime.Caller,
		svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		service, ok := wasmtimeReadServiceName(buf, svcPtr, svcLen)
		if !ok {
			return errBadParamInt64
		}
		op, ok := wasmtimeReadServiceName(buf, opPtr, opLen)
		if !ok {
			return errBadParamInt64
		}
		req, ok := wasmtimeReadStringValidated(buf, reqPtr, reqLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.DurableSend(context.Background(), nil, service, op, req)
	})
}

func (b *wasmtimeBackend) registerCleatScheduleInvoke(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_schedule_invoke") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_schedule_invoke", func(caller *wasmtime.Caller,
		svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen int32, delayMs int64) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		service, ok := wasmtimeReadServiceName(buf, svcPtr, svcLen)
		if !ok {
			return errBadParamInt64
		}
		op, ok := wasmtimeReadServiceName(buf, opPtr, opLen)
		if !ok {
			return errBadParamInt64
		}
		req, ok := wasmtimeReadStringValidated(buf, reqPtr, reqLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.DurableScheduleInvoke(context.Background(), nil, service, op, req, delayMs)
	})
}

func (b *wasmtimeBackend) registerCleatSetState(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_set_state") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_set_state", func(caller *wasmtime.Caller,
		keyPtr, keyLen, valPtr, valLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		key, ok := wasmtimeReadServiceName(buf, keyPtr, keyLen)
		if !ok {
			return errBadParamInt64
		}
		value, ok := wasmtimeReadStringValidated(buf, valPtr, valLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.SetState(context.Background(), nil, key, value)
	})
}

func (b *wasmtimeBackend) registerCleatGetState(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_get_state") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_get_state", func(caller *wasmtime.Caller,
		keyPtr, keyLen, valuePtr, valueMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		key, ok := wasmtimeReadServiceName(buf, keyPtr, keyLen)
		if !ok {
			return errBadParamInt64
		}
		return h.GetState(context.Background(), nil, key, uint32(valuePtr), uint32(valueMaxLen))
	})
}

func (b *wasmtimeBackend) registerCleatDeleteState(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_delete_state") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_delete_state", func(caller *wasmtime.Caller,
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
		return h.DeleteState(context.Background(), nil, key)
	})
}

func (b *wasmtimeBackend) registerCleatIncrState(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_incr_state") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_incr_state", func(caller *wasmtime.Caller,
		keyPtr, keyLen int32, delta int64) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		key, ok := wasmtimeReadServiceName(buf, keyPtr, keyLen)
		if !ok {
			return errBadParamInt64
		}
		return h.IncrState(context.Background(), nil, key, delta)
	})
}

func (b *wasmtimeBackend) registerCleatHasState(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_has_state") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_has_state", func(caller *wasmtime.Caller,
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
		return h.HasState(context.Background(), nil, key)
	})
}

func (b *wasmtimeBackend) registerCleatListState(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_list_state") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_list_state", func(caller *wasmtime.Caller,
		prefixPtr, prefixLen, keysPtr, keysMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		prefix, ok := wasmtimeReadStringValidated(buf, prefixPtr, prefixLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.ListState(context.Background(), nil, prefix, uint32(keysPtr), uint32(keysMaxLen))
	})
}

func (b *wasmtimeBackend) registerCleatFetch(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_fetch") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_fetch", func(caller *wasmtime.Caller,
		methodPtr, methodLen, urlPtr, urlLen, headersPtr, headersLen, bodyPtr, bodyLen int32,
		responsePtr, responseMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		method, ok := wasmtimeReadServiceName(buf, methodPtr, methodLen)
		if !ok {
			return errBadParamInt64
		}
		url, ok := wasmtimeReadStringValidated(buf, urlPtr, urlLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		headersJSON, ok := wasmtimeReadStringValidated(buf, headersPtr, headersLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		body, ok := wasmtimeReadStringValidated(buf, bodyPtr, bodyLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.Fetch(context.Background(), nil, method, url, headersJSON, body, uint32(responsePtr), uint32(responseMaxLen))
	})
}

func (b *wasmtimeBackend) registerCleatJsonParse(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_json_parse") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_json_parse", func(caller *wasmtime.Caller,
		jsonPtr, jsonLen, outPtr, outMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		// Read here, like every other wasmtime wrapper. m stays nil: the
		// memory travels in the context, which is what writeResult uses.
		input, ok := wasmtimeReadStringValidated(buf, jsonPtr, jsonLen, int32(MaxWasmStringLen))
		if !ok {
			return packSimpleResult(1)
		}
		callCtx := ctxWithMem(context.Background(), buf)
		return h.JsonParse(callCtx, nil, input, uint32(outPtr), uint32(outMaxLen))
	})
}

func (b *wasmtimeBackend) registerCleatJsonStringify(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_json_stringify") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_json_stringify", func(caller *wasmtime.Caller,
		ptr, len, outPtr, outMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		input, ok := wasmtimeReadStringValidated(buf, ptr, len, int32(MaxWasmStringLen))
		if !ok {
			return packSimpleResult(1)
		}
		callCtx := ctxWithMem(context.Background(), buf)
		return h.JsonStringify(callCtx, nil, input, uint32(outPtr), uint32(outMaxLen))
	})
}
