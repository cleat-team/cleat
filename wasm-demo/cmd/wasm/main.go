//go:build wasip1
// +build wasip1

package main

import (
	"encoding/json"
	"unsafe"

	"github.com/rcownie/cleat/cleat"
	"durable-wasm-demo/workflow"
)

type PlaceOrderArgs struct {
	UserID string           `json:"user_id"`
	Cart   []workflow.CartItem `json:"cart"`
}

//go:wasmexport place_order
func placeOrder(argsPtr unsafe.Pointer, argsLen uint32) (resultPtr unsafe.Pointer, resultLen uint32, errCode int32) {
	argsBytes := unsafe.Slice((*byte)(argsPtr), argsLen)
	var args PlaceOrderArgs
	if err := json.Unmarshal(argsBytes, &args); err != nil {
		return writeError(err)
	}

	h := cleat.NewHostCalls(cleat.HostCallsOptions{
		DurableCall: func(service, op, request string) (string, error) {
			return durableCallHost(service, op, request)
		},
		DurableLog: func(msg string) {
			durableLogHost(msg)
		},
		Now: func() int64 {
			return durableNowHost()
		},
	})

	trackingID, err := workflow.PlaceOrder(h, args.UserID, args.Cart)
	if err != nil {
		return writeError(err)
	}

	resultBytes, _ := json.Marshal(trackingID)
	ptr := alloc(uint32(len(resultBytes)))
	copy(unsafe.Slice((*byte)(ptr), len(resultBytes)), resultBytes)
	return ptr, uint32(len(resultBytes)), 0
}

func writeError(err error) (unsafe.Pointer, uint32, int32) {
	errBytes, _ := json.Marshal(err.Error())
	ptr := alloc(uint32(len(errBytes)))
	copy(unsafe.Slice((*byte)(ptr), len(errBytes)), errBytes)
	return ptr, uint32(len(errBytes)), 1
}

// ------- Low-level WASM host imports -------

//go:wasmimport env durable_call
func durableCallImport(
	servicePtr unsafe.Pointer, serviceLen uint32,
	opPtr unsafe.Pointer, opLen uint32,
	requestPtr unsafe.Pointer, requestLen uint32,
) (responsePtr unsafe.Pointer, responseLen uint32, errCode int32)

//go:wasmimport env durable_log
func durableLogImport(msgPtr unsafe.Pointer, msgLen uint32)

//go:wasmimport env durable_now
func durableNowImport() int64

// ------- Host call adapters -------

func durableCallHost(service, op, request string) (string, error) {
	serviceBytes := unsafe.Slice(unsafe.StringData(service), len(service))
	opBytes := unsafe.Slice(unsafe.StringData(op), len(op))
	reqBytes := unsafe.Slice(unsafe.StringData(request), len(request))

	respPtr, respLen, errCode := durableCallImport(
		unsafe.Pointer(unsafe.SliceData(serviceBytes)), uint32(len(serviceBytes)),
		unsafe.Pointer(unsafe.SliceData(opBytes)), uint32(len(opBytes)),
		unsafe.Pointer(unsafe.SliceData(reqBytes)), uint32(len(reqBytes)),
	)
	if respLen > 0 {
		response := string(unsafe.Slice((*byte)(respPtr), respLen))
		return response, nil
	}
	if errCode != 0 {
		return "", errFromCode(errCode)
	}
	return "", nil
}

func durableLogHost(msg string) {
	msgBytes := unsafe.Slice(unsafe.StringData(msg), len(msg))
	durableLogImport(unsafe.Pointer(unsafe.SliceData(msgBytes)), uint32(len(msgBytes)))
}

func durableNowHost() int64 {
	return durableNowImport()
}

func alloc(size uint32) unsafe.Pointer {
	return nil
}

func errFromCode(code int32) error {
	return nil
}

func main() {}
