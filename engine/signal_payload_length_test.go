package engine

import (
	"context"
	"strings"
	"testing"
)

// TestAwaitSignals_ReportsBytesWrittenNotPayloadLength pins the length a guest
// is told after a signal payload is truncated.
//
// DurableAwaitSignals writes the payload into a guest buffer of the guest's
// choosing (payloadMaxLen) and returns the length in a packed result. It used
// to write the payload, discard the count writeResult returned, and report
// `uint32(len(payload))` -- the length of the *whole* payload, not of the part
// that fit.
//
// So a guest awaiting a signal with a buffer smaller than the payload was told
// to read more bytes than were written, and read whatever happened to be in its
// linear memory past the end as though it were signal data. Silent corruption,
// in a durable workflow, of a value the workflow was waiting for.
//
// The signal *name* on the line above never had this problem -- it uses the
// count writeResult returns. That asymmetry within a single call is what makes
// this an oversight rather than a design.
func TestAwaitSignals_ReportsBytesWrittenNotPayloadLength(t *testing.T) {
	const (
		payloadMaxLen = 10
		sigNameMaxLen = 16
		payloadPtr    = 1000
		sigNamePtr    = 0
	)
	payload := strings.Repeat("x", 100) // deliberately larger than the buffer

	s := &execSession{
		isReplay: true,
		history: []EventRecord{{
			EventType:     EventTypeSignalReceived,
			SignalName:    "sig",
			SignalPayload: payload,
		}},
	}
	ctx := contextWithRawMemBuf(context.Background(), make([]byte, 64*1024))

	packed := s.DurableAwaitSignals(ctx, nil, "sig", 0,
		sigNamePtr, sigNameMaxLen, payloadPtr, payloadMaxLen)

	// Layout, from packAwaitSignalsResult and the guest-side decoders the ABI
	// tests pin: bits 48-63 sigNameLen, 32-47 payloadLen, 16-31 timedOut,
	// 0-15 errCode.
	r := uint64(packed)
	gotPayloadLen := (r >> 32) & 0xFFFF
	gotSigNameLen := (r >> 48) & 0xFFFF

	if gotPayloadLen != payloadMaxLen {
		t.Errorf("payload length reported as %d, want %d (the bytes actually written). "+
			"A guest reading %d bytes from a buffer holding %d reads %d bytes of whatever "+
			"else is in its memory and treats it as signal payload",
			gotPayloadLen, payloadMaxLen, gotPayloadLen, payloadMaxLen,
			int(gotPayloadLen)-payloadMaxLen)
	}
	if gotSigNameLen != uint64(len("sig")) {
		t.Errorf("signal name length reported as %d, want 3", gotSigNameLen)
	}
}

// TestAwaitSignals_ShortPayloadIsUnaffected is the other direction: when the
// payload fits, the reported length must still be the payload's own length.
//
// Without it, "report the written count" could be satisfied by something that
// always reports the buffer size, which would be a different bug in the same
// field.
func TestAwaitSignals_ShortPayloadIsUnaffected(t *testing.T) {
	const payloadMaxLen = 64
	payload := "small"

	s := &execSession{
		isReplay: true,
		history: []EventRecord{{
			EventType:     EventTypeSignalReceived,
			SignalName:    "sig",
			SignalPayload: payload,
		}},
	}
	ctx := contextWithRawMemBuf(context.Background(), make([]byte, 64*1024))

	packed := s.DurableAwaitSignals(ctx, nil, "sig", 0, 0, 16, 1000, payloadMaxLen)
	if got := (uint64(packed) >> 32) & 0xFFFF; got != uint64(len(payload)) {
		t.Errorf("payload length reported as %d, want %d", got, len(payload))
	}
}
