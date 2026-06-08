package engine

import (
	"context"
	"testing"
)

func TestWazeroBackend_Runtime(t *testing.T) {
	b, err := NewWazeroBackend(context.Background())
	if err != nil {
		t.Fatalf("NewWazeroBackend: %v", err)
	}
	defer b.Close(context.Background())

	rt := b.Runtime()
	if rt == nil {
		t.Error("Runtime() returned nil")
	}
}

func TestWazeroBackend_PerExecution(t *testing.T) {
	b, err := NewWazeroBackend(context.Background())
	if err != nil {
		t.Fatalf("NewWazeroBackend: %v", err)
	}
	defer b.Close(context.Background())

	pe := b.PerExecution()
	if pe == nil {
		t.Fatal("PerExecution() returned nil")
	}
	if pe.Name() != "wazero" {
		t.Errorf("PerExecution().Name() = %q, want \"wazero\"", pe.Name())
	}
	wb, ok := pe.(*wazeroBackend)
	if !ok {
		t.Fatalf("PerExecution() returned %T, want *wazeroBackend", pe)
	}
	if wb.Runtime() != b.Runtime() {
		t.Error("PerExecution() does not share the same Runtime")
	}
}

func TestWazeroBackend_Execute_CompileError(t *testing.T) {
	b, err := NewWazeroBackend(context.Background())
	if err != nil {
		t.Fatalf("NewWazeroBackend: %v", err)
	}
	defer b.Close(context.Background())

	_, err = b.Execute(context.Background(), []byte("not-valid-wasm"), "main", nil, nil)
	if err == nil {
		t.Fatal("expected compile error for invalid WASM bytes")
	}
}
