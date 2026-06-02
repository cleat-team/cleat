// Workflow compiled to WASM targeted at WASI (Go 1.23).
//
// Since Go 1.23's wasip1 target doesn't support //go:wasmimport for
// custom host functions, communication happens via a JSON-line protocol
// over stdin/stdout (WASI). The host intercepts WASI stdout to process
// durable_call requests and writes responses to WASI stdin.
//
// This is the same pattern that would run with //go:wasmimport in Go 1.24
// or tinygo — the protocol is just transported over WASI pipes instead
// of direct function imports.

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

// ---- Protocol messages ----

type callRequest struct {
	Type    string `json:"type"` // "call"
	Service string `json:"service"`
	Op      string `json:"op"`
	Request string `json:"request"`
}

type callResponse struct {
	Type     string `json:"type"` // "response"
	Result   string `json:"result"`
	Err      string `json:"err,omitempty"`
}

type logMessage struct {
	Type    string `json:"type"` // "log"
	Message string `json:"message"`
}

type resultMessage struct {
	Type   string `json:"type"` // "result" | "error"
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// ---- Global stdin/stdout for the pipe protocol ----

var (
	stdout *json.Encoder
	stdin  *json.Decoder
)

func main() {
	stdout = json.NewEncoder(os.Stdout)
	stdin = json.NewDecoder(bufio.NewReader(os.Stdin))

	// Read the workflow input from stdin as JSON.
	var input struct {
		UserID string `json:"user_id"`
		Cart   []struct {
			SKU      string `json:"sku"`
			Quantity int    `json:"quantity"`
		} `json:"cart"`
	}
	if err := stdin.Decode(&input); err != nil {
		writeResult("", fmt.Sprintf("parse input: %v", err))
		return
	}

	logMsg("workflow started: user=" + input.UserID)

	if len(input.Cart) == 0 {
		writeResult("", "cart is empty")
		return
	}

	// Step 1: Look up each cart item in the catalog.
	for _, item := range input.Cart {
		req := fmt.Sprintf(`{"sku":"%s"}`, item.SKU)
		resp, err := durableCall("catalog", "LookupItem", req)
		if err != nil {
			writeResult("", fmt.Sprintf("catalog lookup %s failed: %v", item.SKU, err))
			return
		}
		logMsg("catalog.LookupItem OK: " + resp)
	}

	// Step 2: Reserve inventory.
	reserveReq := fmt.Sprintf(`{"user_id":"%s","items":%d}`, input.UserID, len(input.Cart))
	reservation, err := durableCall("inventory", "Reserve", reserveReq)
	if err != nil {
		writeResult("", fmt.Sprintf("reservation failed: %v", err))
		return
	}
	logMsg("inventory.Reserve OK: " + reservation)

	// Step 3: Charge payment.
	chargeResp, err := durableCall("payments", "Charge", `{"amount_cents":3299}`)
	if err != nil {
		// Compensate: release inventory.
		durableCall("inventory", "Release", `{"reason":"payment_failed"}`)
		writeResult("", fmt.Sprintf("payment failed: %v", err))
		return
	}
	logMsg("payments.Charge OK: " + chargeResp)

	// Step 4: Create shipment.
	shipResp, err := durableCall("shipping", "CreateShipment", `{"address":"123 Main St"}`)
	if err != nil {
		// Compensate: refund + release.
		durableCall("payments", "Refund", `{"charge_id":"chg_xyz"}`)
		durableCall("inventory", "Release", `{"reason":"shipment_failed"}`)
		writeResult("", fmt.Sprintf("shipment failed: %v", err))
		return
	}
	logMsg("shipping.CreateShipment OK: " + shipResp)

	writeResult(`{"status":"complete","tracking_id":"TRACK-123456"}`, "")
}

// durableCall makes a durable API call through the engine.
// Writes a request to stdout, reads the response from stdin.
func durableCall(service, op, request string) (string, error) {
	// Send the call request to the engine.
	if err := stdout.Encode(callRequest{
		Type:    "call",
		Service: service,
		Op:      op,
		Request: request,
	}); err != nil {
		return "", fmt.Errorf("encode call: %w", err)
	}

	// Read the response from the engine.
	var resp callResponse
	if err := stdin.Decode(&resp); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if resp.Err != "" {
		return "", fmt.Errorf("%s", resp.Err)
	}
	return resp.Result, nil
}

func logMsg(msg string) {
	stdout.Encode(logMessage{Type: "log", Message: msg})
}

func writeResult(result, errMsg string) {
	stdout.Encode(resultMessage{Type: "result", Result: result, Error: errMsg})
}
