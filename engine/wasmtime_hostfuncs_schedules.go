//go:build cgo

package engine

import (
	"context"

	"github.com/bytecodealliance/wasmtime-go/v44"
)

// Cron schedule host calls on the wasmtime backend. The wazero counterparts
// are in imports.go; the two must agree on argument order and on which reader
// each argument goes through, because a guest cannot tell which backend it
// landed on.

func (b *wasmtimeBackend) registerCleatScheduleCron(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_schedule_cron") {
		return nil
	}

	return b.hostFunc(linker, "env", "cleat_schedule_cron", func(caller *wasmtime.Caller,
		wfPtr, wfLen, cronPtr, cronLen, tzPtr, tzLen, inputPtr, inputLen,
		idPtr, idMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		workflowName, ok := wasmtimeReadServiceName(buf, wfPtr, wfLen)
		if !ok {
			return errBadParamInt64
		}
		cronExpr, ok := wasmtimeReadStringValidated(buf, cronPtr, cronLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		timezone, ok := wasmtimeReadPayload(buf, tzPtr, tzLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		inputJSON, ok := wasmtimeReadPayload(buf, inputPtr, inputLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.ScheduleCron(ctxWithMem(context.Background(), buf), nil,
			workflowName, cronExpr, timezone, inputJSON, uint32(idPtr), uint32(idMaxLen))
	})
}

func (b *wasmtimeBackend) registerCleatDeleteCron(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_delete_cron") {
		return nil
	}

	return b.hostFunc(linker, "env", "cleat_delete_cron", func(caller *wasmtime.Caller,
		idPtr, idLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		scheduleID, ok := wasmtimeReadServiceName(buf, idPtr, idLen)
		if !ok {
			return errBadParamInt64
		}
		return h.DeleteCron(ctxWithMem(context.Background(), buf), nil, scheduleID)
	})
}

func (b *wasmtimeBackend) registerCleatListCrons(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_list_crons") {
		return nil
	}

	return b.hostFunc(linker, "env", "cleat_list_crons", func(caller *wasmtime.Caller,
		outPtr, outMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		return h.ListCrons(ctxWithMem(context.Background(), buf), nil, uint32(outPtr), uint32(outMaxLen))
	})
}
