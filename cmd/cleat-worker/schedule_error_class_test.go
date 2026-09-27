package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// cleat#996: every /api/schedules failure was `writeError(w, 500, err.Error())`,
// so a caller reusing a schedule name got a server error carrying the raw
// driver text.
//
// Modelled on api_admin_status_test.go, including its sharpest case: an error
// whose TEXT contains the trigger phrase but whose type does not. That is what
// separates classifying by sentinel from sniffing a message, and it is the
// thing this fix exists to be.

func TestAScheduleFailureIsClassifiedNotEchoed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		detail string
	}{
		{
			name:   "the name is taken",
			err:    fmt.Errorf("%w: nightly-report", engine.ErrScheduleExists),
			status: 409,
			detail: "schedule_exists",
		},
		{
			// The defect, in the shape it shipped in.
			name:   "a genuine store fault",
			err:    errors.New(`pq: could not serialize access due to concurrent update`),
			status: 500,
		},
		{
			// THE TRAP. A server fault whose text contains the phrase a
			// message-sniffing implementation would key on. It is not an
			// ErrScheduleExists, so it must not be a 409 -- telling a client
			// "rename and retry" when the database is broken sends it into a
			// loop that can never succeed.
			name:   "a server fault whose text says a schedule already exists",
			err:    errors.New("replication lag: a schedule already exists on the primary but not here"),
			status: 500,
		},
		{
			// The other direction of the same trap: real driver text for a
			// uniqueness violation that nothing wrapped. Unclassified is a
			// server fault by definition -- see writeScheduleError.
			name:   "unwrapped driver text for a duplicate",
			err:    errors.New(`pq: duplicate key value violates unique constraint "workflow_schedules_pkey" (23505)`),
			status: 500,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &apiServer{}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/schedules", nil)

			s.writeScheduleError(rec, req, "create", "nightly-report", tc.err)

			if rec.Code != tc.status {
				t.Errorf("status = %d, want %d\nerror was: %v", rec.Code, tc.status, tc.err)
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("response is not JSON: %v (%q)", err, rec.Body.String())
			}
			if body["detail"] != tc.detail {
				t.Errorf("detail = %q, want %q", body["detail"], tc.detail)
			}
		})
	}
}

// TestNoDriverTextReachesTheClient is the half that protects the response body
// rather than the status code, and it is the one that would have caught the
// leak on its own.
//
// A 500 is defensible; a 500 quoting `pq: duplicate key value violates unique
// constraint "workflow_schedules_pkey" (23505)` is a schema disclosure, and it
// is also dialect-specific, so it is not even useful to the client it is being
// leaked to. The unclassified branch logs the detail and answers a fixed
// message; nothing about the driver may survive into the body.
func TestNoDriverTextReachesTheClient(t *testing.T) {
	leaky := errors.New(`pq: duplicate key value violates unique constraint "workflow_schedules_pkey" (23505)`)

	s := &apiServer{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/schedules", nil)
	s.writeScheduleError(rec, req, "create", "nightly-report", leaky)

	got := rec.Body.String()
	for _, fragment := range []string{
		"pq:",                     // the driver
		"workflow_schedules_pkey", // the constraint, and so the schema
		"23505",                   // the SQLSTATE
		"unique constraint",       // the mechanism
	} {
		if strings.Contains(got, fragment) {
			t.Errorf("the response body leaks %q to the client:\n  %s\n\n"+
				"An unclassified store failure is logged server-side, where an "+
				"operator can read it. What reaches the caller must not describe "+
				"the schema, and must not be dialect-specific -- the same "+
				"condition reads differently on MySQL and SQL Server, so this "+
				"text is not parseable by a client either.", fragment, got)
		}
	}
}
