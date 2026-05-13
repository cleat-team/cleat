package cleat

import (
	"encoding/json"
	"fmt"
)

// CallTyped marshals request to JSON, makes a durable (journaled) call via
// DurableCall, unmarshals the response into the generic type T, and returns
// the typed result. This eliminates manual JSON marshaling/unmarshaling
// at call sites.
//
// Usage:
//
//	type MyRequest struct { ... }
//	type MyResponse struct { ... }
//
//	resp, err := cleat.CallTyped[MyResponse](h, "my_service", "my_op", MyRequest{...})
func CallTyped[T any](h HostCalls, service, operation string, request any) (T, error) {
	var zero T
	reqJSON, err := json.Marshal(request)
	if err != nil {
		return zero, fmt.Errorf("durable: marshaling request for %s.%s: %w", service, operation, err)
	}
	respJSON, err := h.DurableCall(service, operation, string(reqJSON))
	if err != nil {
		return zero, err
	}
	var result T
	if respJSON != "" {
		if err := json.Unmarshal([]byte(respJSON), &result); err != nil {
			return zero, fmt.Errorf("durable: unmarshaling response from %s.%s: %w", service, operation, err)
		}
	}
	return result, nil
}

// PluginCallTyped marshals request to JSON, makes a plugin call via PluginCall,
// unmarshals the response into the generic type T, and returns the typed result.
//
// Plugin calls target Go host plugins (e.g., "llm", "blobstore") rather than
// external services. They are journaled for deterministic replay, same as
// regular durable calls.
//
// Usage:
//
//	resp, err := cleat.PluginCallTyped[MyResponse](h, "llm", "chat", ChatRequest{...})
func PluginCallTyped[T any](h HostCalls, plugin, function string, request any) (T, error) {
	var zero T
	reqJSON, err := json.Marshal(request)
	if err != nil {
		return zero, fmt.Errorf("durable: marshaling request for plugin %s.%s: %w", plugin, function, err)
	}
	respJSON, err := h.PluginCall(plugin, function, string(reqJSON))
	if err != nil {
		return zero, err
	}
	var result T
	if respJSON != "" {
		if err := json.Unmarshal([]byte(respJSON), &result); err != nil {
			return zero, fmt.Errorf("durable: unmarshaling response from plugin %s.%s: %w", plugin, function, err)
		}
	}
	return result, nil
}
