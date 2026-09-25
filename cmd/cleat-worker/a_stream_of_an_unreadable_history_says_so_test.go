package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// cleat#2311. GET /api/workflows/{id}/stream reads history with the strict
// load, so a worker whose key ring cannot open the run's history gets an error
// from it. The client is told in a sentence it can act on; the driver's text
// ("cipher: message authentication failed") is for the log, not for a tenant.
// Any OTHER load error still reaches the client as it always has, which is the
// control that shows the sanitising is on the decryption branch and not
// applied to every failure.
func TestAStreamOfAnUnreadableHistoryGetsASentenceNotTheDecryptError(t *testing.T) {
	for _, tc := range []struct {
		name       string
		loadErr    error
		wantSubstr string
		wantAbsent string
	}{
		{
			name: "undecryptable",
			loadErr: fmt.Errorf("load history: %w: Request at step 0 of workflow wf-1: decrypt: open: cipher: message authentication failed",
				engine.ErrPayloadDecryption),
			wantSubstr: "does not hold the payload encryption key",
			wantAbsent: "message authentication",
		},
		{
			name:       "any other failure keeps its text",
			loadErr:    errors.New("load history: connection reset by peer"),
			wantSubstr: "connection reset by peer",
			wantAbsent: "does not hold the payload encryption key",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStreamFixture(t, runningRun(), nil)
			f.ms.loadEventHistoryFn = func(_ context.Context, _ string) ([]engine.EventRecord, error) {
				return nil, tc.loadErr
			}
			rec, cancel, done := f.start(t, "/api/workflows/wf-1/stream", nil)
			defer func() { cancel(); <-done }()

			evs := waitForEvents(t, rec, 2) // attached, then error
			ev, ok := findEvent(evs, "error")
			if !ok {
				t.Fatalf("no error event.\n\nbody:\n%s", rec.body())
			}
			msg, _ := ev.data["message"].(string)
			if !strings.Contains(msg, tc.wantSubstr) {
				t.Errorf("error message %q does not contain %q", msg, tc.wantSubstr)
			}
			if strings.Contains(msg, tc.wantAbsent) {
				t.Errorf("error message %q contains %q", msg, tc.wantAbsent)
			}
		})
	}
}
