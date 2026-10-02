package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// cleat#2942 -- the conversion half. The event verifier runs during pre-replay
// and its own reads are ordinary context-taking DB reads, so a fence cancel
// (cleat#2008 fix 1) or a worker shutdown arrives as context.Canceled. It used
// to be reported as "checksum verification failed" and counted in
// RecordReplayChecksumFailure, which mislabels the error, inflates that metric,
// and turns a hand-over into a fatal replay failure.
func TestReplayAbortedBeforeGuest(t *testing.T) {
	live := context.Background()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	cases := []struct {
		name    string
		ctx     context.Context
		verr    error
		wantErr bool
	}{
		{"a live context and a real mismatch is NOT an abort", live, errors.New("checksum mismatch"), false},
		{"a cancelled context makes it an abort", cancelled, errors.New("load: context canceled"), true},
		{"a bare context.Canceled makes it an abort", live, context.Canceled, true},
		{"a wrapped cancellation makes it an abort", live, fmt.Errorf("verify events: load: %w", context.Canceled), true},
		{"a deadline makes it an abort", live, context.DeadlineExceeded, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := replayAbortedBeforeGuest(tc.ctx, "wf-1", tc.verr)
			if tc.wantErr && got == nil {
				t.Fatalf("replayAbortedBeforeGuest(%v, %v) = nil, want an abort error", tc.ctx.Err(), tc.verr)
			}
			if !tc.wantErr {
				if got != nil {
					t.Fatalf("replayAbortedBeforeGuest(%v, %v) = %v, want nil -- a real mismatch must keep taking the checksum path", tc.ctx.Err(), tc.verr, got)
				}
				return
			}
			if !strings.Contains(got.Error(), "replay aborted before the guest ran") {
				t.Errorf("abort error = %q, want it to say the replay was aborted", got)
			}
			if strings.Contains(got.Error(), "checksum verification failed") {
				t.Errorf("abort error = %q, still mislabels a cancellation as a checksum failure", got)
			}
			// It must stay a cancellation for errors.Is callers, so the worker
			// can keep telling "stopped" from "broke".
			if !errors.Is(got, context.Canceled) && !errors.Is(got, context.DeadlineExceeded) {
				t.Errorf("abort error = %q, want it to wrap the cancellation", got)
			}
		})
	}
}

// The end-to-end half, through the same harness as
// TestReplayCompiled_ChecksumVerificationFailure: a verifier that reports the
// cancellation the fence produces must NOT come back as a checksum failure.
func TestReplayCompiled_CancelledVerifierIsNotAChecksumFailure(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	compiled, err := rt.CompileModule(ctx, minimalWasm())
	if err != nil {
		t.Fatalf("CompileModule: %v", err)
	}
	defer compiled.Close(ctx)

	engine := NewEngine(rt, nil,
		WithWorkflowEventVerifier(func(ctx context.Context, workflowID string) error {
			// What the fence's cancellation actually looks like by the time it
			// reaches the verifier: a DB read that was cancelled.
			return fmt.Errorf("verify events: load: load history: begin: begin tx: %w", context.Canceled)
		}, true), // failOnMismatch = true
	)
	engine.workflowID = "wf-cancelled-replay"

	history := []EventRecord{
		{Step: 0, EventType: EventTypeCall, Service: "s", Op: "o", Request: "{}", Response: "{}", TimestampMs: 1000},
	}

	_, _, _, _, _, err = engine.ReplayCompiled(ctx, compiled, "test", nil, history)
	if err == nil {
		t.Fatal("expected an error from a cancelled replay")
	}
	if strings.Contains(err.Error(), "checksum verification failed") {
		t.Errorf("err = %q -- a fence cancellation is still reported as a checksum failure (cleat#2942)", err)
	}
	if !strings.Contains(err.Error(), "replay aborted before the guest ran") {
		t.Errorf("err = %q, want it to report an aborted replay", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %q, want it to wrap context.Canceled so callers can tell 'stopped' from 'broke'", err)
	}
}
