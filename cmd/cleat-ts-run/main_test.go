package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests exercise the SCRIPT, not a workflow: the answers file's parsing and the caller's decision are the parts
// that can be wrong without a guest, and they are the parts a broken fixture would blame on the workflow (cleat#3828).
// The end-to-end half -- a real module built by build.sh consuming a scripted answer -- runs in the tier-2 CI job,
// which has the pinned Javy toolchain; this package cannot build one locally and does not pretend to.

func writeAnswers(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "answers.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}
	return path
}

func TestLoadAnswersAcceptsAWellFormedScript(t *testing.T) {
	path := writeAnswers(t, `{"payments/charge":"{\"id\":\"ch_1\"}","inventory/reserve":"{\"ok\":true}"}`)
	got, err := loadAnswers(path)
	if err != nil {
		t.Fatalf("loadAnswers: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("read %d answers, want 2: %v", len(got), got)
	}
	if got["payments/charge"] != `{"id":"ch_1"}` {
		t.Fatalf(`answer for payments/charge is %q, want the JSON text the guest should receive`, got["payments/charge"])
	}
}

// Each malformed shape is refused AT LOAD, because the engine hands the string straight to the guest: a bad value would
// otherwise surface as a parse error inside the workflow, which reads like a workflow bug rather than a bad fixture.
func TestLoadAnswersRefusesWhatWouldBlameTheWorkflow(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"not an object of strings", `{"payments/charge":{"id":"ch_1"}}`, "not a JSON object of strings"},
		{"a value that is not JSON text", `{"payments/charge":"{not json}"}`, "not valid JSON"},
		{"a key with no separator", `{"paymentscharge":"{}"}`, "service>/<operation>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadAnswers(writeAnswers(t, tc.body))
			if err == nil {
				t.Fatalf("accepted %s", tc.body)
			}
			// Assert the TEXT, not only that it errored: a load that failed for an unrelated reason would satisfy
			// "err != nil" and leave this case covering nothing.
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err.Error(), tc.want)
			}
		})
	}
}

func TestScriptedCallerAnswersAndRefusesTheUnscripted(t *testing.T) {
	c := &scriptedCaller{answers: map[string]string{"payments/charge": `{"id":"ch_1"}`}}

	out, err := c.Call(context.Background(), "payments", "charge", `{"amount":10}`)
	if err != nil {
		t.Fatalf("a scripted call failed: %v", err)
	}
	if out != `{"id":"ch_1"}` {
		t.Fatalf("got %q, want the scripted text", out)
	}

	// THE HALF THAT MATTERS. Without this, a test that forgot a stub would get an empty object and could pass on an
	// answer nobody chose -- the exact "green that measured nothing" this repo keeps paying for.
	_, err = c.Call(context.Background(), "inventory", "reserve", `{}`)
	if err == nil {
		t.Fatal("an unscripted call was answered instead of refused")
	}
	if !strings.Contains(err.Error(), "inventory/reserve") {
		t.Fatalf("the refusal does not name the missing key, so the reader cannot fix it: %v", err)
	}
	if len(c.asked) != 1 || c.asked[0] != "payments/charge" {
		t.Fatalf("asked = %v, want only the scripted call recorded", c.asked)
	}
}
