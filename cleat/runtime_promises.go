package cleat

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Promise is a typed durable promise that can be awaited within the workflow
// and resolved by an external caller via the REST API. Create with
// CreatePromiseTyped or NewPromiseTyped.
//
// Usage:
//
//	promise, err := cleat.NewPromiseTyped[ApprovalResult](h, "manager_approval")
//	// ... pass promise.ID to an external system ...
//	result, timedOut, err := promise.Await(30 * time.Minute)
type Promise[T any] struct {
	ID   string
	Name string
	h    HostCalls
}

// Await blocks until the promise is resolved or the timeout expires.
// Returns the typed result, whether it timed out, and any error.
func (p *Promise[T]) Await(timeout time.Duration) (T, bool, error) {
	var zero T
	resultJSON, timedOut, err := p.h.AwaitPromise(p.ID, timeout)
	if err != nil {
		return zero, timedOut, err
	}
	var val T
	if err := json.Unmarshal([]byte(resultJSON), &val); err != nil {
		return zero, false, fmt.Errorf("durable: unmarshal promise result: %w", err)
	}
	return val, timedOut, nil
}

// NewPromiseTyped creates a typed durable promise and returns a Promise[T]
// that can be awaited later. The name is a human-readable label.
func NewPromiseTyped[T any](h HostCalls, name string) (*Promise[T], error) {
	id, err := h.CreatePromise(name)
	if err != nil {
		return nil, err
	}
	return &Promise[T]{ID: id, Name: name, h: h}, nil
}

// updateHandlerEntry stores a registered update handler and its validator.
type updateHandlerEntry struct {
	handler   func(payloadJSON string) (resultJSON string, err error)
	validator func(payloadJSON string) error
}

func (h *HostCallsImpl) CreatePromise(name string) (string, error) {
	if h.createPromise == nil {
		return "", errors.New("durable: CreatePromise can only be called from within a workflow function (the HostCalls runtime was not initialized). Ensure this call is inside a cleat_entry / #[cleat_entry] / @CleatEntry / @cleatEntry function.")
	}
	return h.createPromise(name)
}

func (h *HostCallsImpl) AwaitPromise(promiseID string, timeout time.Duration) (string, bool, error) {
	if h.awaitPromise == nil {
		return "", false, errors.New("durable: AwaitPromise can only be called from within a workflow function (the HostCalls runtime was not initialized). Ensure this call is inside a cleat_entry / #[cleat_entry] / @CleatEntry / @cleatEntry function.")
	}
	return h.awaitPromise(promiseID, timeout)
}

func (h *HostCallsImpl) AwaitPromiseMs(promiseID string, timeoutMs int64) (result string, timedOut bool, err error) {
	return h.AwaitPromise(promiseID, time.Duration(timeoutMs)*time.Millisecond)
}

func (h *HostCallsImpl) ResolvePromise(id, value string) error {
	if h.resolvePromise == nil {
		return errors.New("durable: ResolvePromise can only be called from within a workflow function (the HostCalls runtime was not initialized). Ensure this call is inside a cleat_entry / #[cleat_entry] / @CleatEntry / @cleatEntry function.")
	}
	return h.resolvePromise(id, value)
}

func (h *HostCallsImpl) RejectPromise(id, errMsg string) error {
	if h.rejectPromise == nil {
		return errors.New("durable: RejectPromise can only be called from within a workflow function (the HostCalls runtime was not initialized). Ensure this call is inside a cleat_entry / #[cleat_entry] / @CleatEntry / @cleatEntry function.")
	}
	return h.rejectPromise(id, errMsg)
}
func (h *HostCallsImpl) RegisterUpdateHandler(name string, handler func(payloadJSON string) (resultJSON string, err error), validator func(payloadJSON string) error) {
	if h.updateHandlers == nil {
		h.updateHandlers = make(map[string]updateHandlerEntry)
	}
	h.updateHandlers[name] = updateHandlerEntry{handler: handler, validator: validator}
	if h.registerUpdateHandler != nil {
		h.registerUpdateHandler(name)
	}
}

// RegisterTypedUpdateHandler registers a typed update handler.
// handler receives a typed request and returns a typed response; validator
// receives the typed request. Both are called during workflow init (before
// durable ops) and replayed deterministically.
//
// Usage:
//
//	cleat.RegisterTypedUpdateHandler[ApprovePayload, ApproveResult](h, "approve",
//	    func(payload ApprovePayload) (ApproveResult, error) {
//	        return ApproveResult{Approved: true}, nil
//	    },
//	    func(payload ApprovePayload) error {
//	        if payload.Amount <= 0 { return errors.New("invalid amount") }
//	        return nil
//	    },
//	)
func RegisterTypedUpdateHandler[TReq, TResp any](h HostCalls, name string, handler func(TReq) (TResp, error), validator func(TReq) error) {
	h.RegisterUpdateHandler(name,
		func(payloadJSON string) (string, error) {
			var req TReq
			if err := json.Unmarshal([]byte(payloadJSON), &req); err != nil {
				return "", fmt.Errorf("durable: unmarshal update payload for %q: %w", name, err)
			}
			resp, err := handler(req)
			if err != nil {
				return "", err
			}
			respBytes, err := json.Marshal(resp)
			if err != nil {
				return "", fmt.Errorf("durable: marshal update response for %q: %w", name, err)
			}
			return string(respBytes), nil
		},
		func(payloadJSON string) error {
			var req TReq
			if err := json.Unmarshal([]byte(payloadJSON), &req); err != nil {
				return fmt.Errorf("durable: unmarshal update payload for %q: %w", name, err)
			}
			return validator(req)
		},
	)
}

func (h *HostCallsImpl) HandleUpdate(name, payload string) (string, error) {
	if h.handleUpdate != nil {
		return h.handleUpdate(name, payload)
	}
	entry, ok := h.updateHandlers[name]
	if !ok {
		return "", fmt.Errorf("cleat: no update handler registered for %q", name)
	}
	return entry.handler(payload)
}
