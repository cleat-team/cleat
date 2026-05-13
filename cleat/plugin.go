package cleat

import (
	"encoding/json"
	"fmt"
)

// CallTyped marshals request to JSON, makes a durable (journaled) call via
// DurableCall, unmarshals the response into the generic type T, and returns
// the typed result. This eliminates manual JSON marshaling/unmarshaling
// at plugin call sites.
//
// Usage:
//
//	type MyRequest struct { ... }
//	type MyResponse struct { ... }
//
//	resp, err := cleat.CallTyped[MyResponse](h, "my_plugin", "my_op", MyRequest{...})
//
// The call is durable (journaled for deterministic replay), use CallTypedEphemeral
// for non-durable (non-journaled) calls.
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
	if err := json.Unmarshal([]byte(respJSON), &result); err != nil {
		return zero, fmt.Errorf("durable: unmarshaling response from %s.%s: %w", service, operation, err)
	}
	return result, nil
}

// CallEphemeralTyped is an alias for CallTypedEphemeral.
// Prefer CallEphemeralTyped over CallTypedEphemeral for consistency.
func CallEphemeralTyped[T any](h HostCalls, plugin, function string, request any) (T, error) {
	return CallTypedEphemeral[T](h, plugin, function, request)
}

// CallTypedEphemeral marshals request to JSON, makes a non-durable
// (non-journaled) plugin call via PluginCall, unmarshals the response into
// the generic type T, and returns the typed result.
//
// Use this for read-only or side-effect-free operations where durability
// (journaling for deterministic replay) is not needed.
//
// Usage:
//
//	resp, err := cleat.CallTypedEphemeral[MyResponse](h, "llm", "chat", ChatRequest{...})
//
// Deprecated: Use CallEphemeralTyped instead.
func CallTypedEphemeral[T any](h HostCalls, plugin, function string, request any) (T, error) {
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
	if err := json.Unmarshal([]byte(respJSON), &result); err != nil {
		return zero, fmt.Errorf("durable: unmarshaling response from plugin %s.%s: %w", plugin, function, err)
	}
	return result, nil
}
