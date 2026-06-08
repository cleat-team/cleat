package engine

import (
	"strings"
	"testing"
)

func TestResolveWasmTrap_Empty(t *testing.T) {
	if got := resolveWasmTrap(nil, ""); got != "" {
		t.Errorf("resolveWasmTrap(nil, \"\") = %q, want \"\"", got)
	}
}

func TestResolveWasmTrap_AlreadyFormatted(t *testing.T) {
	msg := "wasm trap: unreachable\n    at workflow.main (workflow.go:12)"
	got := resolveWasmTrap(nil, msg)
	if got != msg {
		t.Errorf("resolveWasmTrap = %q, want %q", got, msg)
	}
}

func TestResolveWasmTrap_RawMessage(t *testing.T) {
	got := resolveWasmTrap(nil, "unreachable")
	if !strings.HasPrefix(got, "WASM trap: ") {
		t.Errorf("resolveWasmTrap = %q, want prefix 'WASM trap: '", got)
	}
	if !strings.Contains(got, "unreachable") {
		t.Errorf("resolveWasmTrap = %q, should contain 'unreachable'", got)
	}
}

func TestEnrichTrapMessage_Empty(t *testing.T) {
	if got := enrichTrapMessage(""); got != "" {
		t.Errorf("enrichTrapMessage(\"\") = %q, want \"\"", got)
	}
}

func TestEnrichTrapMessage_Wraps(t *testing.T) {
	got := enrichTrapMessage("something went wrong")
	want := "WASM trap: something went wrong"
	if got != want {
		t.Errorf("enrichTrapMessage = %q, want %q", got, want)
	}
}
