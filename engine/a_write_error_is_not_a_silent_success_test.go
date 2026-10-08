package engine

import (
	"context"
	"testing"
)

// cleat#3207. writeOut mapped every writeResult error that was not a
// truncation onto errCode 0 -- success -- because asTruncation's ok==false
// branch did not distinguish "no error" from "an error asTruncation does not
// recognise". A call that genuinely ran on the host (and, for something like
// cleat_child_workflow, recorded a real side effect) could therefore report a
// successful empty result to the guest, indistinguishable from a legitimate
// empty string, with no error written anywhere. Found building the Java agent
// client (cleat#2978): the guest's own linear memory had not yet grown far
// enough to cover the destination at the moment of the host call.

// TestWriteOutReportsAWriteFailureNotASuccess is the regression test. It
// drives writeOut through the exact branch the bug lived in -- a raw-buffer
// (wasmtime-shaped) write whose ptr+len exceeds the guest's own memory, which
// is precisely the out-of-bounds case TestWriteResult_GuestPointerOutOfRange
// already proves writeResult itself reports as an error. The question here is
// one layer up: what writeOut does with that error.
func TestWriteOutReportsAWriteFailureNotASuccess(t *testing.T) {
	s := &execSession{}
	const bufLen = 64
	ctx := contextWithRawMemBuf(context.Background(), make([]byte, bufLen))

	// ptr+len(val) > bufLen, so writeResult's bounds check rejects it before
	// ever touching rawBuf -- see engine/flush.go:42.
	n, code := s.writeOut(ctx, nil, bufLen-2, "a value too long to fit", 1024)

	if code == 0 {
		t.Fatalf("writeOut returned errCode 0 (success) for a write that never happened -- "+
			"n=%d. A host call that fails to write its result must never report success.", n)
	}
	if code != errCodeOutputWriteFailed {
		t.Errorf("errCode = %d, want errCodeOutputWriteFailed (%d)", code, errCodeOutputWriteFailed)
	}
	if n != 0 {
		t.Errorf("n = %d, want 0 -- nothing was written, so the byte count must say so", n)
	}
}

// TestWriteOutStillReportsTruncationSeparately is the companion check this
// file's own rule asks for: a fix for one miscategorised error must not
// collapse the two real-but-different ones into one signal. Truncation
// writes a genuine prefix (n > 0) and must keep its own, pre-existing
// errCode (7) -- not the new write-failed one (8) -- or a guest that already
// knows how to handle a truncated-but-usable prefix would start treating it
// as nothing having been written at all.
func TestWriteOutStillReportsTruncationSeparately(t *testing.T) {
	s := &execSession{}
	ctx := contextWithRawMemBuf(context.Background(), make([]byte, 64))

	n, code := s.writeOut(ctx, nil, 0, "a value longer than the buffer allows", 8)

	if code != errCodeOutputTruncated {
		t.Fatalf("errCode = %d, want errCodeOutputTruncated (%d) -- a truncated-but-written "+
			"prefix must not be reported the same way as a write that failed outright",
			code, errCodeOutputTruncated)
	}
	if n == 0 {
		t.Errorf("n = 0, want > 0 -- truncation still writes a real prefix")
	}
}

// TestWriteErrorClassMapsBothWriteFailureCodes pins writeErrorClass's own
// contract directly, independent of which call site reaches it: each errCode
// writeOut can now return must classify to the matching durable-call
// callErrorCode, and anything else -- including 0, success -- must classify
// to 0. A durable-call site that got this wrong would silently report
// CallErrorUnknown for a failure the host had already correctly identified.
func TestWriteErrorClassMapsBothWriteFailureCodes(t *testing.T) {
	cases := []struct {
		errCode byte
		want    byte
	}{
		{0, 0},
		{errCodeOutputTruncated, callErrorOutputTruncated},
		{errCodeOutputWriteFailed, callErrorOutputWriteFailed},
	}
	for _, tt := range cases {
		if got := writeErrorClass(tt.errCode); got != tt.want {
			t.Errorf("writeErrorClass(%d) = %d, want %d", tt.errCode, got, tt.want)
		}
	}
}
