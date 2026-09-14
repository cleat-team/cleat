package backendkit

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A client that cannot send an Idempotency-Key cannot retry a start safely, and
// one that discards idempotent_replay cannot tell whether its retry landed.
// Both were true of this package until now.
func TestAClientCanUseAnIdempotencyKey(t *testing.T) {
	t.Run("the key reaches the server as a header", func(t *testing.T) {
		// THE ASSERTION THAT MATTERS. Everything else here passes against a
		// client that builds the request perfectly and never sets the header --
		// which is exactly the state this package was in: StartWorkflow works,
		// returns an id, and silently offers no idempotency at all.
		var got string
		var seen bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got, seen = r.Header.Get("Idempotency-Key"), true
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"id":"run-1","idempotent_replay":false}`))
		}))
		defer srv.Close()

		c := New(srv.URL)
		if _, err := c.StartWorkflowWithOptions(context.Background(), "wf",
			json.RawMessage(`{"a":1}`), StartOptions{IdempotencyKey: "key-abc"}); err != nil {
			t.Fatalf("start: %v", err)
		}
		if !seen {
			t.Fatal("the server handler never ran, so this test asserted nothing")
		}
		if got != "key-abc" {
			t.Errorf("server saw Idempotency-Key %q, want %q -- without the header the start "+
				"is not idempotent and a retry starts a second run", got, "key-abc")
		}
	})

	t.Run("no key means NO header, not an empty one", func(t *testing.T) {
		// An empty header is a key equal to "", under which every caller that
		// sent no key would collide with every other. Absence and emptiness are
		// different requests.
		var present bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, present = r.Header["Idempotency-Key"]
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"id":"run-1","idempotent_replay":false}`))
		}))
		defer srv.Close()

		if _, err := New(srv.URL).StartWorkflowWithOptions(context.Background(), "wf",
			json.RawMessage(`{}`), StartOptions{}); err != nil {
			t.Fatalf("start: %v", err)
		}
		if present {
			t.Error("an Idempotency-Key header was sent for a start that specified no key")
		}
	})

	t.Run("the replay flag and status survive the decode", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"id":"run-original","idempotent_replay":true,"status":"ready"}`))
		}))
		defer srv.Close()

		res, err := New(srv.URL).StartWorkflowWithOptions(context.Background(), "wf",
			json.RawMessage(`{}`), StartOptions{IdempotencyKey: "k"})
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		if res.ID != "run-original" {
			t.Errorf("id %q, want the ORIGINAL run's id -- a retry must name the work its "+
				"first attempt created", res.ID)
		}
		if !res.IdempotentReplay {
			t.Error("idempotent_replay was dropped; the caller cannot tell whether its " +
				"original request landed")
		}
		if res.Status != "ready" {
			t.Errorf("status %q, want \"ready\"", res.Status)
		}
		if res.Terminal() {
			t.Error("Terminal() is true for \"ready\" -- a poller would stop while the work " +
				"is still outstanding")
		}
	})

	// Terminal() is the poll-stopping predicate, so its false negatives hang a
	// caller forever and its false positives read an absent result as final.
	t.Run("Terminal covers every end state and no others", func(t *testing.T) {
		for _, s := range []string{"done", "failed", "terminated", "dead_lettered"} {
			if !(StartResult{Status: s}).Terminal() {
				t.Errorf("Terminal() is false for %q, so a poller would never stop", s)
			}
		}
		for _, s := range []string{"ready", "running", "terminating", "unknown", ""} {
			if (StartResult{Status: s}).Terminal() {
				t.Errorf("Terminal() is true for %q, so a poller would stop before the run "+
					"reached an end state and read a result that is not there", s)
			}
		}
	})

	// "unknown" is the server saying it could not read the run. Calling it
	// terminal would turn "ask again" into "this is the answer".
	t.Run("unknown is not terminal", func(t *testing.T) {
		if (StartResult{Status: "unknown"}).Terminal() {
			t.Error("\"unknown\" reported as terminal; it means the winner could not be read, " +
				"not that it finished")
		}
	})

	t.Run("the two key refusals are distinguishable", func(t *testing.T) {
		for _, tc := range []struct {
			detail string
			want   error
		}{
			{"idempotency_key_input_mismatch", ErrIdempotencyKeyInputMismatch},
			{"idempotency_key_definition_mismatch", ErrIdempotencyKeyDefinitionMismatch},
		} {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(409)
				_, _ = w.Write([]byte(`{"error":"refused","detail":"` + tc.detail + `"}`))
			}))
			_, err := New(srv.URL).StartWorkflowWithOptions(context.Background(), "wf",
				json.RawMessage(`{}`), StartOptions{IdempotencyKey: "k"})
			srv.Close()

			if err == nil {
				t.Fatalf("%s: start succeeded against a 409", tc.detail)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("%s: errors.Is did not match. A caller cannot tell a permanent "+
					"refusal from a transient failure, so it retries forever: %v", tc.detail, err)
			}
		}
	})

	// THE CONTROL for the case above: an unrelated 409 must NOT be classified.
	// Without this, a classifier that returned ErrIdempotencyKeyInputMismatch
	// for every conflict would pass, and a caller would stop retrying things it
	// should retry.
	t.Run("control: an unrelated conflict stays generic", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(409)
			_, _ = w.Write([]byte(`{"error":"nope","detail":"schedule_exists"}`))
		}))
		defer srv.Close()

		_, err := New(srv.URL).StartWorkflowWithOptions(context.Background(), "wf",
			json.RawMessage(`{}`), StartOptions{})
		if err == nil {
			t.Fatal("an unrelated 409 succeeded")
		}
		if errors.Is(err, ErrIdempotencyKeyInputMismatch) ||
			errors.Is(err, ErrIdempotencyKeyDefinitionMismatch) {
			t.Errorf("an unrelated conflict was classified as an idempotency refusal: %v", err)
		}
	})
}
