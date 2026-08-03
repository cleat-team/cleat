//go:build cgo

package engine

import (
	"context"

	"github.com/bytecodealliance/wasmtime-go/v44"
)

func (b *wasmtimeBackend) registerCleatDefer(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_defer") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_defer", func(caller *wasmtime.Caller,
		descPtr, descLen, deferIDPtr, deferIDMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		desc, ok := wasmtimeReadStringValidated(buf, descPtr, descLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.DurableDefer(context.Background(), nil, desc, uint32(deferIDPtr), uint32(deferIDMaxLen))
	})
}

func (b *wasmtimeBackend) registerCleatPollCancellation(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_poll_cancellation") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_poll_cancellation", func(caller *wasmtime.Caller,
		reasonPtr, reasonMaxLen int32) int64 {
		h := b.handler
		_, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		return h.PollCancellation(context.Background(), nil, uint32(reasonPtr), uint32(reasonMaxLen))
	})
}

func (b *wasmtimeBackend) registerCleatPollSignal(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_poll_signal") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_poll_signal", func(caller *wasmtime.Caller,
		namePtr, nameLen, payloadPtr, payloadMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		name, ok := wasmtimeReadServiceName(buf, namePtr, nameLen)
		if !ok {
			return errBadParamInt64
		}
		return h.PollSignal(context.Background(), nil, name, uint32(payloadPtr), uint32(payloadMaxLen))
	})
}

func (b *wasmtimeBackend) registerCleatContinueAsNew(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_continue_as_new") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_continue_as_new", func(caller *wasmtime.Caller,
		inputPtr, inputLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		newInput, ok := wasmtimeReadStringValidated(buf, inputPtr, inputLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.ContinueAsNew(context.Background(), nil, newInput)
	})
}

func (b *wasmtimeBackend) registerCleatContinueAsNewVersioned(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_continue_as_new_versioned") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_continue_as_new_versioned", func(caller *wasmtime.Caller,
		inputPtr, inputLen int32, newVersion int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		newInput, ok := wasmtimeReadStringValidated(buf, inputPtr, inputLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.ContinueAsNewWithVersion(context.Background(), nil, newInput, int(newVersion))
	})
}

func (b *wasmtimeBackend) registerCleatChildWorkflow(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_child_workflow") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_child_workflow", func(caller *wasmtime.Caller,
		namePtr, nameLen, inputPtr, inputLen, runIDPtr, runIDMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		wfName, ok := wasmtimeReadServiceName(buf, namePtr, nameLen)
		if !ok {
			return errBadParamInt64
		}
		wfInput, ok := wasmtimeReadStringValidated(buf, inputPtr, inputLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.ChildWorkflow(ctxWithMem(context.Background(), buf), nil, wfName, wfInput, uint32(runIDPtr), uint32(runIDMaxLen))
	})
}

func (b *wasmtimeBackend) registerCleatChildWorkflowWithOptions(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_child_workflow_with_options") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_child_workflow_with_options", func(caller *wasmtime.Caller,
		namePtr, nameLen, inputPtr, inputLen int32, version int64, priority int64,
		policyPtr, policyLen, runIDPtr, runIDMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		wfName, ok := wasmtimeReadServiceName(buf, namePtr, nameLen)
		if !ok {
			return errBadParamInt64
		}
		wfInput, ok := wasmtimeReadStringValidated(buf, inputPtr, inputLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		// parentClosePolicy may be empty (default). Zero-length is valid.
		var parentClosePolicy string
		if policyLen > 0 {
			policy, ok := wasmtimeReadServiceName(buf, policyPtr, policyLen)
			if !ok {
				return errBadParamInt64
			}
			parentClosePolicy = policy
		}
		return h.ChildWorkflowWithOptions(ctxWithMem(context.Background(), buf), nil, wfName, wfInput, version, priority, parentClosePolicy, uint32(runIDPtr), uint32(runIDMaxLen))
	})
}

func (b *wasmtimeBackend) registerCleatChildWorkflowInSchema(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_child_workflow_in_schema") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_child_workflow_in_schema", func(caller *wasmtime.Caller,
		schemaPtr, schemaLen, namePtr, nameLen, inputPtr, inputLen int32, version int64, priority int64,
		policyPtr, policyLen, runIDPtr, runIDMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		targetSchema, ok := wasmtimeReadServiceName(buf, schemaPtr, schemaLen)
		if !ok {
			return errBadParamInt64
		}
		wfName, ok := wasmtimeReadServiceName(buf, namePtr, nameLen)
		if !ok {
			return errBadParamInt64
		}
		wfInput, ok := wasmtimeReadStringValidated(buf, inputPtr, inputLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		parentClosePolicy, ok := wasmtimeReadServiceName(buf, policyPtr, policyLen)
		if !ok {
			return errBadParamInt64
		}
		return h.ChildWorkflowInSchema(context.Background(), nil, targetSchema, wfName, wfInput, version, priority, parentClosePolicy, uint32(runIDPtr), uint32(runIDMaxLen))
	})
}

func (b *wasmtimeBackend) registerCleatAwaitChild(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_await_child") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_await_child", func(caller *wasmtime.Caller,
		runIDPtr, runIDLen, resultPtr, resultMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		runID, ok := wasmtimeReadServiceName(buf, runIDPtr, runIDLen)
		if !ok {
			return errBadParamInt64
		}
		return h.AwaitChild(ctxWithMem(context.Background(), buf), nil, runID, uint32(resultPtr), uint32(resultMaxLen))
	})
}

func (b *wasmtimeBackend) registerCleatAwaitAllChildren(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_await_all_children") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_await_all_children", func(caller *wasmtime.Caller,
		idsPtr, idsLen, resultsPtr, resultsMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		runIDsJSON, ok := wasmtimeReadStringValidated(buf, idsPtr, idsLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.AwaitAllChildren(ctxWithMem(context.Background(), buf), nil, runIDsJSON, uint32(resultsPtr), uint32(resultsMaxLen))
	})
}

func (b *wasmtimeBackend) registerCleatCallRetry(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_call_retry") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_call_retry", func(caller *wasmtime.Caller,
		svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen int32,
		maxAttempts, initialIntervalMs, backoffCoefficient100x, maxIntervalMs int64,
		nonRetryPtr, nonRetryLen int32,
		respPtr, respMaxLen int32) int64 {
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
		nonRetryableErrorsJSON, ok := wasmtimeReadStringValidated(buf, nonRetryPtr, nonRetryLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.DurableCallWithRetry(context.Background(), nil, service, op, req,
			maxAttempts, initialIntervalMs, backoffCoefficient100x, maxIntervalMs,
			nonRetryableErrorsJSON, uint32(respPtr), uint32(respMaxLen))
	})
}

func (b *wasmtimeBackend) registerCleatAwaitSignals(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_await_signals") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_await_signals", func(caller *wasmtime.Caller,
		namesPtr, namesLen int32, timeoutMs int64,
		sigNamePtr, sigNameMaxLen, payloadPtr, payloadMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		names, ok := wasmtimeReadStringValidated(buf, namesPtr, namesLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.DurableAwaitSignals(ctxWithMem(context.Background(), buf), nil, names, timeoutMs,
			uint32(sigNamePtr), uint32(sigNameMaxLen), uint32(payloadPtr), uint32(payloadMaxLen))
	})
}

func (b *wasmtimeBackend) registerCleatSetQueryState(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("set_query_state") {
		return nil
	}

	return linker.FuncWrap("env", "set_query_state", func(caller *wasmtime.Caller,
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
		val, ok := wasmtimeReadStringValidated(buf, valPtr, valLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.SetQueryState(context.Background(), nil, key, val)
	})
}

func (b *wasmtimeBackend) registerCleatCallHeartbeat(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_call_heartbeat") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_call_heartbeat", func(caller *wasmtime.Caller,
		svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen int32,
		heartbeatIntervalMs int64,
		respPtr, respMaxLen int32) int64 {
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
		return h.DurableCallWithHeartbeat(context.Background(), nil, service, op, req, heartbeatIntervalMs, uint32(respPtr), uint32(respMaxLen))
	})
}

func (b *wasmtimeBackend) registerCleatRegisterUpdateHandler(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_register_update_handler") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_register_update_handler", func(caller *wasmtime.Caller,
		namePtr, nameLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		name, ok := wasmtimeReadServiceName(buf, namePtr, nameLen)
		if !ok {
			return errBadParamInt64
		}
		return h.RegisterUpdateHandler(context.Background(), nil, name)
	})
}

func (b *wasmtimeBackend) registerCleatSendSignalAndWait(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_send_signal_and_wait") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_send_signal_and_wait", func(caller *wasmtime.Caller,
		targetPtr, targetLen, sigPtr, sigLen, payloadPtr, payloadLen int32,
		timeoutMs int64,
		respPtr, respMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		targetRunID, ok := wasmtimeReadServiceName(buf, targetPtr, targetLen)
		if !ok {
			return errBadParamInt64
		}
		signalName, ok := wasmtimeReadServiceName(buf, sigPtr, sigLen)
		if !ok {
			return errBadParamInt64
		}
		payload, ok := wasmtimeReadStringValidated(buf, payloadPtr, payloadLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.SendSignalAndWait(context.Background(), nil, targetRunID, signalName, payload, timeoutMs, uint32(respPtr), uint32(respMaxLen))
	})
}

func (b *wasmtimeBackend) registerCleatReplyToSignal(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_reply_to_signal") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_reply_to_signal", func(caller *wasmtime.Caller,
		correlationPtr, correlationLen, respPtr, respLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		correlationID, ok := wasmtimeReadServiceName(buf, correlationPtr, correlationLen)
		if !ok {
			return errBadParamInt64
		}
		response, ok := wasmtimeReadStringValidated(buf, respPtr, respLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.ReplyToSignal(context.Background(), nil, correlationID, response)
	})
}

func (b *wasmtimeBackend) registerCleatSignalWorkflow(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_signal_workflow") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_signal_workflow", func(caller *wasmtime.Caller,
		targetPtr, targetLen, sigPtr, sigLen, payloadPtr, payloadLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		targetRunID, ok := wasmtimeReadServiceName(buf, targetPtr, targetLen)
		if !ok {
			return errBadParamInt64
		}
		signalName, ok := wasmtimeReadServiceName(buf, sigPtr, sigLen)
		if !ok {
			return errBadParamInt64
		}
		payload, ok := wasmtimeReadStringValidated(buf, payloadPtr, payloadLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.SignalWorkflow(context.Background(), nil, targetRunID, signalName, payload)
	})
}

func (b *wasmtimeBackend) registerCleatSetScope(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_set_scope") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_set_scope", func(caller *wasmtime.Caller,
		objTypePtr, objTypeLen, instKeyPtr, instKeyLen int32,
		prevScopePtr, prevScopeMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		objType, ok := wasmtimeReadServiceName(buf, objTypePtr, objTypeLen)
		if !ok {
			return errBadParamInt64
		}
		instKey, ok := wasmtimeReadServiceName(buf, instKeyPtr, instKeyLen)
		if !ok {
			return errBadParamInt64
		}
		return h.SetScope(context.Background(), nil, objType, instKey, uint32(prevScopePtr), uint32(prevScopeMaxLen))
	})
}

func (b *wasmtimeBackend) registerCleatGetScope(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_get_scope") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_get_scope", func(caller *wasmtime.Caller,
		objTypePtr, objTypeMaxLen, instKeyPtr, instKeyMaxLen int32) int64 {
		h := b.handler
		_, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		return h.GetScope(context.Background(), nil, uint32(objTypePtr), uint32(objTypeMaxLen), uint32(instKeyPtr), uint32(instKeyMaxLen))
	})
}

func (b *wasmtimeBackend) registerCleatSideEffect(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_side_effect") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_side_effect", func(caller *wasmtime.Caller,
		resultPtr, resultLen, outPtr, outMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		result, ok := wasmtimeReadStringValidated(buf, resultPtr, resultLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.SideEffect(context.Background(), nil, result, uint32(outPtr), uint32(outMaxLen))
	})
}

func (b *wasmtimeBackend) registerCleatRegisterQueryHandler(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_register_query_handler") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_register_query_handler", func(caller *wasmtime.Caller,
		namePtr, nameLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		name, ok := wasmtimeReadServiceName(buf, namePtr, nameLen)
		if !ok {
			return errBadParamInt64
		}
		return h.RegisterQueryHandler(context.Background(), nil, name)
	})
}

func (b *wasmtimeBackend) registerCleatRunDetached(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_run_detached") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_run_detached", func(caller *wasmtime.Caller,
		namePtr, nameLen, inputPtr, inputLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		name, ok := wasmtimeReadServiceName(buf, namePtr, nameLen)
		if !ok {
			return errBadParamInt64
		}
		inputJSON, ok := wasmtimeReadStringValidated(buf, inputPtr, inputLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.RunDetached(context.Background(), nil, name, inputJSON)
	})
}

func (b *wasmtimeBackend) registerCleatPollChild(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_poll_child") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_poll_child", func(caller *wasmtime.Caller,
		runIDPtr, runIDLen, resultPtr, resultMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		runID, ok := wasmtimeReadServiceName(buf, runIDPtr, runIDLen)
		if !ok {
			return errBadParamInt64
		}
		return h.PollChild(ctxWithMem(context.Background(), buf), nil, runID, uint32(resultPtr), uint32(resultMaxLen))
	})
}

func (b *wasmtimeBackend) registerCleatAwaitAnyChild(linker *wasmtime.Linker) error {
	if b.skipIfNotNeeded("cleat_await_any_child") {
		return nil
	}

	return linker.FuncWrap("env", "cleat_await_any_child", func(caller *wasmtime.Caller,
		idsPtr, idsLen, resultPtr, resultMaxLen int32) int64 {
		h := b.handler
		buf, _, err := callerMemBuf(caller)
		if err != nil {
			return errBadParamInt64
		}
		runIDsJSON, ok := wasmtimeReadStringValidated(buf, idsPtr, idsLen, int32(MaxWasmStringLen))
		if !ok {
			return errBadParamInt64
		}
		return h.AwaitAnyChild(ctxWithMem(context.Background(), buf), nil, runIDsJSON, uint32(resultPtr), uint32(resultMaxLen))
	})
}
