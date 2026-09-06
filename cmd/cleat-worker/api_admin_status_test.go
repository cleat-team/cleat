package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// This guards the mapping from an admin operation's failure to its HTTP status
// code, and specifically that it is chosen by ERROR CLASS rather than by
// matching words in the message.
//
// The defect: every refusal whose wording matched none of the substring
// patterns fell through to 500. Re-replaying a `done` workflow -- a decision
// the server makes on purpose and explains precisely -- was reported to the
// operator as the server having broken. Measured against a live worker before
// the fix:
//
//	POST /api/admin/instances/<id>/re-replay
//	  -> 500 {"error":"re-replay: admin re_replay: workflow <id> is done,
//	          and only [failed terminated dead_lettered] can be re-replayed"}
//
// The body was right and the status wrong, which is the worst combination: a
// client retrying on 5xx retries something that can never succeed.
//
// The engine half -- that each refusal actually carries a class -- is
// TestEveryAdminRefusalCarriesItsClass in engine/admin_error_class_test.go.
// Both halves are needed: a class nothing sets and a mapper that ignores the
// class fail in the same way and neither test alone sees it.

func TestAdminErrorStatusIsChosenByClass(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		detail string // expected "detail" discriminator, empty if none
	}{
		{
			name:   "not found",
			err:    fmt.Errorf("re-replay: admin re_replay: workflow wf-1 not found: %w", engine.ErrAdminNotFound),
			status: 404,
		},
		{
			name:   "generation mismatch",
			err:    fmt.Errorf("re-replay: %w", engine.ErrAdminGenerationMismatch),
			status: 409,
			detail: "generation_mismatch",
		},
		{
			name:   "bad request",
			err:    fmt.Errorf("force-complete: %w", engine.ErrAdminBadRequest),
			status: 400,
		},
		{
			// The case that was 500.
			name:   "workflow state forbids the operation",
			err:    fmt.Errorf("re-replay: workflow wf-1 is done: %w", engine.ErrAdminStateConflict),
			status: 409,
			detail: "state_conflict",
		},
		{
			name:   "not implemented",
			err:    fmt.Errorf("re-replay: %w", engine.ErrAdminOpNotImplemented),
			status: 501,
		},
		{
			// An unclassified error is a server fault and must stay one.
			name:   "database failure",
			err:    errors.New("re-replay: pq: connection reset by peer"),
			status: 500,
		},
		{
			// The reason substring matching had to go. This is a server fault
			// whose text happens to contain the words the old mapper looked
			// for, and it answered 404 -- telling the caller the workflow did
			// not exist when the truth was that the database was broken.
			name:   "server fault whose text contains 'not found'",
			err:    errors.New(`re-replay: pq: relation "event_history" not found`),
			status: 500,
		},
		{
			// Same trap, other pattern.
			name:   "server fault whose text contains 'generation mismatch'",
			err:    errors.New("re-replay: internal: generation mismatch in the plan cache"),
			status: 500,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &apiServer{}
			rec := httptest.NewRecorder()
			s.handleAdminOpError(rec, tc.err)

			if rec.Code != tc.status {
				t.Errorf("status = %d, want %d\nerror was: %v", rec.Code, tc.status, tc.err)
			}

			// The message reaches the caller whatever the status, so an
			// operator still sees the explanation that was always correct.
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("response is not JSON: %v (%q)", err, rec.Body.String())
			}
			if body["error"] != tc.err.Error() {
				t.Errorf("error body = %q, want %q", body["error"], tc.err.Error())
			}
			if body["detail"] != tc.detail {
				t.Errorf("detail = %q, want %q", body["detail"], tc.detail)
			}
		})
	}
}

// TestNoAdminRefusalClassIsAServerError is the direction that would have caught
// the original defect: whatever classes the engine defines, none of them may
// map to 5xx. A refusal reported as a server error tells a client to retry what
// can never succeed.
func TestNoAdminRefusalClassIsAServerError(t *testing.T) {
	for _, class := range []error{
		engine.ErrAdminBadRequest,
		engine.ErrAdminNotFound,
		engine.ErrAdminGenerationMismatch,
		engine.ErrAdminStateConflict,
	} {
		t.Run(class.Error(), func(t *testing.T) {
			s := &apiServer{}
			rec := httptest.NewRecorder()
			s.handleAdminOpError(rec, fmt.Errorf("op: %w", class))
			if rec.Code >= 500 {
				t.Errorf("the %q class answered %d; a deliberate refusal must be 4xx.\n"+
					"Add a case for it in handleAdminOpError.", class, rec.Code)
			}
		})
	}
}
