package engine

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// cleat#1955: a nightly leg failed on a queue declaring concurrency=2 that
// appeared to admit 1, and the run's own artifacts could not say which of two
// causes it was. The worker's claim line carried no concurrency key -- zero of
// 38 lines in the failing leg -- so two workflows claimed in the same
// millisecond were equally consistent with "one queue admitted two" and "two
// unrelated keys ran at once".
//
// These assert the line that closes that gap. The case that matters is the last
// one: two candidates on the SAME key, one admitted and one refused, which is
// the shape the failing nightly would have produced and the shape no field on
// the old line could distinguish from two different keys.

func captureClaimLog(t *testing.T, fn func(log *slog.Logger)) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	fn(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))

	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not JSON: %v\n%s", err, line)
		}
		out = append(out, m)
	}
	return out
}

func key(s string) *string { return &s }

func TestTheClaimLogsWhichKeyItDecidedAbout(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cand      claimCandidate
		admitted  bool
		wantLine  bool
		wantKey   string
		wantReg   bool
		wantAdmit bool
	}{
		{
			name:      "a registered queue admitting",
			cand:      claimCandidate{id: "wf-1", key: key("orders"), registered: true},
			admitted:  true,
			wantLine:  true,
			wantKey:   "orders",
			wantReg:   true,
			wantAdmit: true,
		},
		{
			// The half that answers "was the queue at capacity". An admissions-only
			// log cannot tell a semaphore at its limit from runs that never overlapped.
			name:      "a registered queue refusing at capacity",
			cand:      claimCandidate{id: "wf-2", key: key("orders"), registered: true},
			admitted:  false,
			wantLine:  true,
			wantKey:   "orders",
			wantReg:   true,
			wantAdmit: false,
		},
		{
			// `registered` false is the mutex path. Distinguishing it from the
			// semaphore path is the literal text of cleat#1955's failure message.
			name:      "an unregistered key is reported as unregistered",
			cand:      claimCandidate{id: "wf-3", key: key("adhoc"), registered: false},
			admitted:  true,
			wantLine:  true,
			wantKey:   "adhoc",
			wantReg:   false,
			wantAdmit: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := captureClaimLog(t, func(log *slog.Logger) {
				logClaimKeyDecision(log, tc.cand, tc.admitted)
			})
			if len(lines) != 1 {
				t.Fatalf("expected exactly one log line, got %d: %v", len(lines), lines)
			}
			m := lines[0]
			if got := m["concurrency_key"]; got != tc.wantKey {
				t.Errorf("concurrency_key = %v, want %q", got, tc.wantKey)
			}
			if got := m["registered"]; got != tc.wantReg {
				t.Errorf("registered = %v, want %v", got, tc.wantReg)
			}
			if got := m["admitted"]; got != tc.wantAdmit {
				t.Errorf("admitted = %v, want %v", got, tc.wantAdmit)
			}
			if got := m["workflow_id"]; got != tc.cand.id {
				t.Errorf("workflow_id = %v, want %q", got, tc.cand.id)
			}
		})
	}
}

// An unkeyed candidate has nothing to decide, and logging one line per claim
// rather than per KEYED claim would make the volume proportional to all traffic.
func TestAnUnkeyedCandidateLogsNothing(t *testing.T) {
	lines := captureClaimLog(t, func(log *slog.Logger) {
		logClaimKeyDecision(log, claimCandidate{id: "wf-4", key: nil}, true)
	})
	if len(lines) != 0 {
		t.Errorf("an unkeyed candidate logged %d line(s), want 0: %v", len(lines), lines)
	}
}

// THE CASE THE NIGHTLY NEEDED. Two candidates on one key, one admitted and one
// refused. Before this line, the artifact showed two claims and no key, which
// could not be told from two claims on two different keys.
//
// Asserted as a SET over the emitted lines rather than by index, so it does not
// also pin the order in which the claim walks its candidates -- a different
// property, and one this change does not govern.
func TestTwoCandidatesOnOneKeyAreDistinguishableInTheLog(t *testing.T) {
	lines := captureClaimLog(t, func(log *slog.Logger) {
		logClaimKeyDecision(log, claimCandidate{id: "wf-a", key: key("orders"), registered: true}, true)
		logClaimKeyDecision(log, claimCandidate{id: "wf-b", key: key("orders"), registered: true}, false)
	})
	if len(lines) != 2 {
		t.Fatalf("expected two lines, got %d: %v", len(lines), lines)
	}

	admittedOn := map[string]bool{}
	keys := map[string]bool{}
	for _, m := range lines {
		id, _ := m["workflow_id"].(string)
		k, _ := m["concurrency_key"].(string)
		adm, _ := m["admitted"].(bool)
		admittedOn[id] = adm
		keys[k] = true
	}
	if len(keys) != 1 || !keys["orders"] {
		t.Errorf("both lines should name the one key %q, got %v", "orders", keys)
	}
	if !admittedOn["wf-a"] {
		t.Errorf("wf-a should be logged as admitted, got %v", admittedOn)
	}
	if admittedOn["wf-b"] {
		t.Errorf("wf-b should be logged as refused, which is what says the queue was at "+
			"capacity rather than idle; got %v", admittedOn)
	}
}
