//go:build cgo

package engine

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/bytecodealliance/wasmtime-go/v44"
)

// skipIfNotNeeded returns true if the named import should be skipped because
// the WASM module doesn't import it and we have a valid import list.
func (b *wasmtimeBackend) skipIfNotNeeded(name string) bool {
	return b.envNeeded != nil && !b.envNeeded[name]
}

func (b *wasmtimeBackend) registerCleatCall(linker *wasmtime.Linker, completeResult, completeErr *string) error {
	if b.skipIfNotNeeded("cleat_call") {
		return nil
	}
	return linker.FuncWrap("env", "cleat_call", func(caller *wasmtime.Caller,
		svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen, respPtr, respMaxLen int32) int64 {
		t0 := time.Now()
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
		t1 := time.Now()
		callCtx := ctxWithMem(context.Background(), buf)
		result := h.DurableCall(callCtx, nil, service, op, req, uint32(respPtr), uint32(respMaxLen))
		elapsed := time.Since(t0)
		if elapsed > 5*time.Millisecond {
			fmt.Fprintf(os.Stderr, "TIMING-WASMTIME: cleat_call total=%dms read=%dus handler=%dus\n",
				elapsed.Milliseconds(), t1.Sub(t0).Microseconds(), time.Since(t1).Microseconds())
		}
		return result
	})
}

func (b *wasmtimeBackend) registerCleatComplete(linker *wasmtime.Linker, completeResult, completeErr *string) error {
	if b.skipIfNotNeeded("cleat_complete") {
		return nil
	}
	return linker.FuncWrap("env", "cleat_complete", func(caller *wasmtime.Caller,
		status int32, resultPtr int32, resultLen int32) int64 {
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		if resultLen > 0 && uint64(resultPtr)+uint64(resultLen) <= uint64(len(buf)) {
			result := string(buf[resultPtr : resultPtr+resultLen])
			if status == 0 {
				*completeResult = result
			} else {
				*completeErr = result
			}
		}
		return 0
	})
}

func (b *wasmtimeBackend) registerCleatPollWork(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_poll_work") {
		return nil
	}
	return linker.FuncWrap("env", "cleat_poll_work", func(caller *wasmtime.Caller,
		entryNamePtr int32, entryNameMaxLen int32,
		argsPtr int32, argsMaxLen int32) int64 {
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}

		// Write entry point name.
		entryBytes := []byte(b.workEntryPoint)
		entryLen := len(entryBytes)
		if entryLen > int(entryNameMaxLen) {
			entryLen = int(entryNameMaxLen)
		}
		if entryLen > 0 {
			copy(buf[entryNamePtr:entryNamePtr+int32(entryLen)], entryBytes[:entryLen])
		}

		// Write input JSON.
		argsLen := len(b.workInput)
		if argsLen > int(argsMaxLen) {
			argsLen = int(argsMaxLen)
		}
		if argsLen > 0 {
			copy(buf[argsPtr:argsPtr+int32(argsLen)], b.workInput[:argsLen])
		}
		return int64(entryLen) | int64(argsLen)<<32
	})
}
