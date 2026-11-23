//go:build cgo

package engine

import (
	"strings"
	"testing"
)

// cleat#4132: a component guest used to receive at most 64 KiB of a host call's response (the dispatcher allocated make([]byte, 65536) before
// the call), however large the host's own was, so a Python or C# workflow could not receive an LLM reply or a fetched document that a Go workflow
// could. The buffer is now sized by what the host call writes, up to MaxHostCallResponseBytes (2 MiB by default).

func withResponseCeiling(t *testing.T, n uint32) {
	t.Helper()
	old := MaxHostCallResponseBytes
	MaxHostCallResponseBytes = n
	t.Cleanup(func() { MaxHostCallResponseBytes = old })
}

func TestTheDefaultResponseCeilingIsTwoMiB(t *testing.T) {
	if DefaultMaxHostCallResponseBytes != 2<<20 || MaxHostCallResponseBytes != DefaultMaxHostCallResponseBytes {
		t.Errorf("response ceiling = %d (default %d), want 2 MiB", MaxHostCallResponseBytes, DefaultMaxHostCallResponseBytes)
	}
}

// A response larger than the old 64 KiB arrives whole, on every dispatcher that takes one -- 100 KiB through three different layouts.
func TestAComponentGuestReceivesAResponseLargerThanTheOldSixtyFourKiB(t *testing.T) {
	const n = 100 << 10
	cases := []struct {
		name   string
		method int
		args   []string
		ret    int64
	}{
		{"durable-call (durable layout)", 0, []string{"svc", "op", "{}"}, packDurableCallResult(n, 0, 0)},
		{"child-workflow (simple layout)", 4, []string{"child", "{}"}, packSimpleResult(0, n)},
		{"side-effect (simple layout)", 14, []string{"v"}, packSimpleResult(0, n)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &wasmtimeBackend{handler: &mockHostHandler{ret: tc.ret, fill: n}}
			strPtr, count, free := cgotestMakeStrArgs(tc.args...)
			defer free()
			resultPtr := cgotestAllocResult()
			if err := b.cgotestDispatchStr(tc.method, strPtr, count, resultPtr); err != nil {
				t.Fatal(err)
			}
			wantCallOutcome(t, resultPtr, "ok", n, 0)
		})
	}
}

// One byte over the ceiling is a visible failure -- code 7, the prefix that fit -- never a silent truncation. The ceiling is lowered for the test so
// it does not need a 2 MiB buffer; the boundary behaves the same at any value.
func TestAResponseOverTheCeilingFailsWithOutputTruncatedAndTheCeilingSizedPrefix(t *testing.T) {
	const ceiling = 1000
	withResponseCeiling(t, ceiling)
	for _, over := range []int{ceiling + 1, ceiling * 3} {
		// The handler packs what it WROTE (the truncated prefix) with errCode 7 and callErrorCode 7, as the real one does; fill is more than fits.
		b := &wasmtimeBackend{handler: &mockHostHandler{ret: packDurableCallResult(ceiling, callErrorOutputTruncated, callErrorOutputTruncated), fill: over}}
		strPtr, count, free := cgotestMakeStrArgs("svc", "op", "{}")
		resultPtr := cgotestAllocResult()
		if err := b.cgotestDispatchStr(0, strPtr, count, resultPtr); err != nil {
			t.Fatal(err)
		}
		free()
		kind, payload, code := cgotestReadCallOutcome(resultPtr)
		if kind != "failed" || code != uint32(callErrorOutputTruncated) || len(payload) != ceiling {
			t.Errorf("a %d-byte response over a %d ceiling: kind=%q code=%d payload=%d bytes, want failed, code 7, a %d-byte prefix", over, ceiling, kind, code, len(payload), ceiling)
		}
	}
	// And exactly the ceiling is not over it.
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: packDurableCallResult(ceiling, 0, 0), fill: ceiling}}
	strPtr, count, free := cgotestMakeStrArgs("svc", "op", "{}")
	defer free()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchStr(0, strPtr, count, resultPtr); err != nil {
		t.Fatal(err)
	}
	wantCallOutcome(t, resultPtr, "ok", ceiling, 0)
}

// The point of sizing the buffer by what is written: a small reply does not allocate the ceiling.
func TestAResponseBufferIsSizedToTheResponseNotTheCeiling(t *testing.T) {
	rb, ctx := newResponseBuf(t.Context())
	if _, err := new(execSession).writeResult(ctx, nil, 0, strings.Repeat("x", 200), 2<<20); err != nil {
		t.Fatal(err)
	}
	if got := len(rb.bytes()); got != 200 {
		t.Errorf("a 200-byte response left a %d-byte buffer, want exactly 200", got)
	}
	if _, err := new(execSession).writeResult(ctx, nil, 0, strings.Repeat("y", 500), 2<<20); err != nil {
		t.Fatal(err)
	}
	if got := len(rb.bytes()); got != 500 {
		t.Errorf("a later 500-byte write left a %d-byte buffer, want 500", got)
	}
}

// The ceiling is clamped to what a result word can carry.
func TestTheResponseLimitIsClampedToTheTwentyFourBitLengthField(t *testing.T) {
	withResponseCeiling(t, 1<<30)
	if got := hostCallResponseLimit(); got != 1<<24-1 {
		t.Errorf("hostCallResponseLimit() = %d for a 1 GiB setting, want %d (the length field is 24 bits)", got, 1<<24-1)
	}
}
