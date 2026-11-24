//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// cleat#4132: the Rust SDK offered a fixed 64 KiB buffer for every response, so a service reply over 64 KiB was truncated at every size and for
// every call. It now starts at 64 KiB and doubles, up to 2 MiB, each time the host reports a truncation (the Go adapter's behaviour). The call
// that truncated cannot be rescued -- it already ran and recorded its effect -- so it fails with the prefix, and the NEXT call receives the whole
// response. Both halves are measured from a real Rust guest.

type bigReplyCaller struct {
	service string
	reply   string
	calls   int
}

func (c *bigReplyCaller) Call(_ context.Context, service, _, _ string) (string, error) {
	if service == c.service {
		c.calls++
		return c.reply, nil
	}
	return `{"ok":true}`, nil
}

func TestARustGuestReceivesAResponseOverSixtyFourKiBOnTheCallAfterTheTruncation(t *testing.T) {
	ctx := context.Background()
	wasmBytes := buildRustProbeWasm(t)

	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { rt.Close(ctx) })
	wt, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { wt.Close(ctx) })

	const size = 100 << 10 // between the 64 KiB start and the 128 KiB the first truncation grows it to
	caller := &bigReplyCaller{service: "big", reply: `"` + strings.Repeat("y", size-2) + `"`}
	eng := NewEngine(rt, caller, WithBackends(WasmtimeLanguages, wt), WithWorkflowID("wf-rust-big-response"))

	out, _, susp, _, _, execErr := eng.Execute(ctx, wasmBytes, "big_response_probe", json.RawMessage(`{"user_id":"u","cart":[]}`))
	if execErr != nil || susp != nil {
		t.Fatalf("execute: err=%v suspended=%v", execErr, susp)
	}
	var got struct {
		FirstFailed  bool `json:"first_failed"`
		FirstLen     int  `json:"first_len"`
		SecondFailed bool `json:"second_failed"`
		SecondLen    int  `json:"second_len"`
	}
	// The entry returns a String, which the engine reports JSON-encoded: unwrap it before reading the object.
	inner := out
	if strings.HasPrefix(out, `"`) {
		if err := json.Unmarshal([]byte(out), &inner); err != nil {
			t.Fatalf("result %q: %v", out, err)
		}
	}
	if err := json.Unmarshal([]byte(inner), &got); err != nil {
		t.Fatalf("result %q: %v", inner, err)
	}
	if caller.calls != 2 {
		t.Fatalf("the service was asked %d time(s), want 2", caller.calls)
	}
	if !got.FirstFailed {
		t.Errorf("the first call reported success with %d bytes: a response over the starting buffer must fail visibly, not arrive truncated", got.FirstLen)
	}
	if got.SecondFailed || got.SecondLen != size {
		t.Errorf("the second call: failed=%v, %d bytes; want the whole %d-byte response after the buffer grew", got.SecondFailed, got.SecondLen, size)
	}
}
