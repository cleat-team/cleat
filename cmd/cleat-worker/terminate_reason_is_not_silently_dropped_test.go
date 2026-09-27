package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestAnUnparseableTerminateBodyNeverReachesTheStore is cleat#1337.
//
// handleDeadLetterTerminate discarded the result of json.Decode, so a body it
// could not read left req.Reason empty and the terminate proceeded anyway, with
// a 200.
//
// THE DAMAGE IS NOT THE MISSING NOTE. TerminateWorkflow writes reason into
// workflow_instances.error_msg unconditionally, in both of its branches, so an
// empty reason OVERWRITES the failure message recording why the run
// dead-lettered. Measured on one row by the session that filed the issue: 270
// bytes of host error before, 0 after, 200 in between. The operator is told the
// terminate worked and the diagnosis is gone.
//
// So the assertion is that the store is NOT REACHED, not merely that the status
// is non-200. A handler that returned 400 after calling TerminateWorkflow would
// satisfy a status-only check and destroy the column just the same.
//
// The two controls are load-bearing in opposite directions:
//
//   - a valid body must still reach the store WITH ITS REASON, or "never
//     reaches the store" is satisfied by a handler that reaches it never;
//   - an EMPTY body must still reach the store, because a terminate with no
//     reason is a supported call (TestHandleDeadLetterTerminate_Success posts a
//     nil body and asserts 200). http.NoBody decodes to io.EOF; a truncated
//     body decodes to io.ErrUnexpectedEOF, which errors.Is(err, io.EOF) does
//     not match. The whole fix rests on those two being distinguishable, so the
//     empty case is asserted rather than assumed.
func TestAnUnparseableTerminateBodyNeverReachesTheStore(t *testing.T) {
	const oversized = 2048 // the handler's cap is 1 KB

	for _, c := range []struct {
		name       string
		body       string
		wantStatus int
		wantCalled bool
		wantReason string
	}{
		{
			// The measured case, and it is 25 bytes -- nowhere near the cap.
			// Any decode failure does this; the size limit is one way in.
			name: "truncated JSON", body: `{"reason": "unterminated`,
			wantStatus: 400, wantCalled: false,
		},
		{
			name: "wrong type for reason", body: `{"reason": 12345}`,
			wantStatus: 400, wantCalled: false,
		},
		{
			name: "form encoding, not JSON", body: `reason=disk+full`,
			wantStatus: 400, wantCalled: false,
		},
		{
			// 413 since cleat#1338, which answered the status question this
			// handler's comment deferred, for all sixteen bounded bodies at
			// once. The property THIS test exists for is unchanged and is the
			// wantCalled column: the store must not be reached. Only the code
			// the caller sees moved, from a 400 asserting the body was
			// malformed to a 413 naming the limit it exceeded.
			name: "over the 1 KB cap", body: `{"reason":"` + strings.Repeat("x", oversized) + `"}`,
			wantStatus: 413, wantCalled: false,
		},
		{
			name: "control: a valid reason still arrives", body: `{"reason":"disk full, not retryable"}`,
			wantStatus: 200, wantCalled: true, wantReason: "disk full, not retryable",
		},
		{
			name: "control: an empty body is still a terminate", body: "",
			wantStatus: 200, wantCalled: true, wantReason: "",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			var called bool
			var gotReason string
			ms := &mockStore{
				terminateWorkflowFn: func(_ context.Context, _, reason string) error {
					called = true
					gotReason = reason
					return nil
				},
			}
			api := newTestAPIServer(ms)
			mux := http.NewServeMux()
			registerRoutes(mux, api)

			var req *http.Request
			if c.body == "" {
				req = httptest.NewRequest(http.MethodPost, "/api/dead-letters/wf-1/terminate", nil)
			} else {
				req = httptest.NewRequest(http.MethodPost, "/api/dead-letters/wf-1/terminate",
					strings.NewReader(c.body))
				req.Header.Set("Content-Type", "application/json")
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			var decoded map[string]string
			_ = json.Unmarshal(w.Body.Bytes(), &decoded)

			if w.Code != c.wantStatus {
				t.Errorf("status = %d %q, want %d", w.Code, w.Body.String(), c.wantStatus)
			}
			if called != c.wantCalled {
				if called {
					t.Errorf("TerminateWorkflow was called with reason %q despite a body the "+
						"handler could not read.\n\nIt writes that reason into error_msg "+
						"unconditionally, so this overwrites the record of why the workflow "+
						"dead-lettered -- and the caller is told it worked.", gotReason)
				} else {
					t.Errorf("TerminateWorkflow was NOT called, so this case proves nothing " +
						"about refusing a bad body -- the handler is refusing everything")
				}
			}
			if c.wantCalled && gotReason != c.wantReason {
				t.Errorf("reason reached the store as %q, want %q", gotReason, c.wantReason)
			}
		})
	}
}

// TestNoRequestBodyDecodeInTheWorkerDiscardsItsError is the completeness half.
//
// The table above can only cover the one handler it drives. This reads the
// source, so a second handler written in the same shape fails here rather than
// shipping -- which is how the first one survived: it was a lone deviation from
// a pattern every other decode in these files follows, and nothing said so.
func TestNoRequestBodyDecodeInTheWorkerDiscardsItsError(t *testing.T) {
	// A bare statement: json.NewDecoder(...).Decode(...) with no `if err :=`
	// and no assignment in front of it. The files are the worker's HTTP
	// surface; a decode elsewhere is not a request body.
	for _, f := range []string{"app.go", "server.go", "api_admin.go"} {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("reading %s: %v", f, err)
		}
		src := string(raw)
		for i, line := range strings.Split(src, "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "json.NewDecoder(") {
				continue
			}
			if !strings.Contains(trimmed, ".Decode(") {
				continue
			}
			t.Errorf("%s:%d discards a request-body decode error:\n\t%s\n\n"+
				"Assign it and branch. cleat#1337: the one handler that did this "+
				"terminated workflows with a blank reason and answered 200, "+
				"overwriting the failure message in error_msg.", f, i+1, trimmed)
		}
	}
}
