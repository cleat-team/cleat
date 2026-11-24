//go:build cgo

package engine

import (
	"context"
	"testing"
)

// cleat#4172. --wasm-instance-timeout's help, docs/reference/worker-config.md and the startup refusal's own message all said "0 disables the timeout",
// and the engine does the opposite: both NewWasmtimeBackend and configureStore turn a timeout of 0 or less into DefaultWasmtimeExecutionTimeout. An
// operator who set 0 to turn the fence off was running with the 30 s fence and believing there was none. The text now says what the engine does;
// this pins that the engine does it, so the next reader of the flag's help is not misled again.
func TestAZeroInstanceTimeoutIsTheDefaultNotDisabled(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		opts []WasmtimeOption
	}{
		{"option never given", nil},
		{"option given 0", []WasmtimeOption{WithWasmtimeExecutionTimeout(0)}},
		{"option given negative", []WasmtimeOption{WithWasmtimeExecutionTimeout(-1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := NewWasmtimeBackend(ctx, tc.opts...)
			if err != nil {
				t.Fatalf("NewWasmtimeBackend: %v", err)
			}
			defer b.Close(ctx)
			if got := b.limits.executionTimeout; got != DefaultWasmtimeExecutionTimeout {
				t.Fatalf("execution timeout = %v, want the %v default: a timeout of 0 does not disable the fence", got, DefaultWasmtimeExecutionTimeout)
			}
		})
	}
	// CONTROL: a value that is set is the value used.
	b, err := NewWasmtimeBackend(ctx, WithWasmtimeExecutionTimeout(7*DefaultWasmtimeExecutionTimeout))
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	defer b.Close(ctx)
	if got := b.limits.executionTimeout; got != 7*DefaultWasmtimeExecutionTimeout {
		t.Fatalf("a configured timeout was %v, want %v", got, 7*DefaultWasmtimeExecutionTimeout)
	}
}
