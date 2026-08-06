package engine

import (
	"context"
	"strings"
	"testing"
)

// cancelledSignalStore reports a workflow as cancelled with a fixed reason.
type cancelledSignalStore struct{ reason string }

func (c *cancelledSignalStore) DeliverSignal(context.Context, string, string, string) error {
	return nil
}

func (c *cancelledSignalStore) PollSignal(context.Context, string, string) (string, bool, error) {
	return "", false, nil
}

func (c *cancelledSignalStore) PollCancellation(context.Context, string) (bool, string, error) {
	return true, c.reason, nil
}

// TestPollCancellation_ReportsBytesWrittenNotReasonLength is the sibling of
// TestAwaitSignals_ReportsBytesWrittenNotPayloadLength, in the function one
// call up from it.
//
// PollCancellation writes the cancellation reason into a guest buffer of the
// guest's choosing and returns its length in the high word. It used to write
// the reason, discard the count writeResult returned, and report
// `len(reason)` -- so a reason longer than the buffer told the guest to read
// past what had been written.
//
// The consequence is narrower than the signal-payload case and worse in one
// way: a workflow polls for cancellation in its own loop, and what it does with
// the reason is usually decide whether to stop. Handing it trailing memory as
// the reason is a bad input to that decision.
//
// Found by grepping for the shape of the first one rather than by review:
// `_, _ = s.writeResult(...)` followed by a length taken from the source
// string. That search found five sites; four were the signal payload, this was
// the fifth.
func TestPollCancellation_ReportsBytesWrittenNotReasonLength(t *testing.T) {
	const (
		reasonMaxLen = 16
		reasonPtr    = 512
	)
	reason := strings.Repeat("cancelled because ", 10) // ~180 bytes, well over the buffer

	e := NewEngine(nil, nil, WithSignalStore(&cancelledSignalStore{reason: reason}))
	e.workflowID = "wf-1"
	s := &execSession{engine: e, workflowID: "wf-1"}
	ctx := contextWithRawMemBuf(context.Background(), make([]byte, 64*1024))

	packed := s.PollCancellation(ctx, nil, reasonPtr, reasonMaxLen)

	if packed&1 != 1 {
		t.Fatalf("cancelled flag not set: packed=%#x", packed)
	}
	if got := uint64(packed) >> 32; got != reasonMaxLen {
		t.Errorf("reason length reported as %d, want %d (the bytes actually written). "+
			"A workflow reading %d bytes from a %d-byte buffer takes %d bytes of unrelated "+
			"memory as its cancellation reason",
			got, reasonMaxLen, got, reasonMaxLen, int(got)-reasonMaxLen)
	}
}

// TestPollCancellation_ShortReasonIsUnaffected pins the other direction, for
// the same reason its sibling does: reporting the buffer size unconditionally
// would satisfy the test above and be a different bug in the same field.
func TestPollCancellation_ShortReasonIsUnaffected(t *testing.T) {
	const reasonMaxLen = 64
	reason := "user requested"

	e := NewEngine(nil, nil, WithSignalStore(&cancelledSignalStore{reason: reason}))
	e.workflowID = "wf-1"
	s := &execSession{engine: e, workflowID: "wf-1"}
	ctx := contextWithRawMemBuf(context.Background(), make([]byte, 64*1024))

	packed := s.PollCancellation(ctx, nil, 512, reasonMaxLen)
	if got := uint64(packed) >> 32; got != uint64(len(reason)) {
		t.Errorf("reason length reported as %d, want %d", got, len(reason))
	}
}
