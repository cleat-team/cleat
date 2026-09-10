package cleat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The shared quorum cases, run against the Go SDK.
//
// Go is the reference: cleat#1132 was fixed here first (#1135), and the other
// four SDKs were still wrong. So this file is not really testing Go -- the
// tests beside it already do -- it is testing the TABLE. If the shared cases
// describe anything other than the behaviour that was reviewed and merged
// here, the ports built from them inherit a misunderstanding instead of a fix.
//
// That is the point of running it in three languages rather than writing three
// tests: the table has to be able to disagree with somebody. See cleat#1136.

// RUN THIS WITH -count=1, WHICH IS WHAT CI DOES. The table is at the repo root
// and this package is cleat/, so the read below crosses `..` -- and Go's test
// cache tracks only files opened INSIDE the package directory. A fixture read
// through `..` is invisible to it, so a table edit does not invalidate the
// cached pass. Measured 2026-09-10 in a throwaway module, one test reading two
// files, each change made alone from a warm cache:
//
//	change nothing                          -> ok  (cached)
//	change the file OUTSIDE the package dir -> ok  (cached)   <- not noticed
//	change the file INSIDE the package dir  -> ok  0.205s     <- re-ran
//
// Confirmed against this very test: with the table corrupted so that a case
// asserts the wrong awaited_sets, `go test .` returned "(cached)" and passed.
// `.github/workflows/ci.yml` passes -count=1 to every matrix package, so CI is
// sound; a local `go test ./...` after editing the table is not, and the tell
// is the word "(cached)" rather than a duration.
//
// This was found by the session porting the table to Java, where gradle's
// up-to-date check produced the same false green from the same cause -- a
// runtime read of a path the build system cannot see. The fix there was to
// declare the file as a task input. Go offers no equivalent, so the honest
// answer is -count=1 and this comment.

type quorumCase struct {
	Name          string            `json:"name"`
	Why           string            `json:"why"`
	SignalNames   []string          `json:"signal_names"`
	MinCount      int               `json:"min_count"`
	MaxRejections int               `json:"max_rejections"`
	Host          string            `json:"host"`
	Deliveries    []string          `json:"deliveries"`
	Payloads      map[string]string `json:"payloads"`
	Expect        struct {
		Outcome            string     `json:"outcome"`
		ErrorKind          string     `json:"error_kind"`
		ResultNames        []string   `json:"result_names"`
		AwaitedSets        [][]string `json:"awaited_sets"`
		CallerSetUnchanged bool       `json:"caller_set_unchanged"`
	} `json:"expect"`
}

// kindOf maps this SDK's wording onto the table's semantic tag. The table
// carries no message text -- five SDKs word these differently and always will --
// so this is the only place the Go tests know a message string.
func kindOf(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "unsatisfiable"):
		return "unsatisfiable"
	case strings.Contains(msg, "not among them"):
		return "out_of_set"
	case strings.Contains(msg, "quorum timeout"):
		return "timeout"
	case strings.Contains(msg, "exceeded max rejections"):
		return "rejections"
	}
	return "UNCLASSIFIED(" + msg + ")"
}

func TestEveryCaseInTheSharedQuorumTableHolds(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "tests", "conformance", "quorum_cases.json"))
	if err != nil {
		t.Fatalf("the shared table must be readable: %v", err)
	}
	var doc struct {
		Cases []quorumCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the shared table must parse: %v", err)
	}
	if len(doc.Cases) == 0 {
		t.Fatal("an empty table would pass vacuously")
	}

	for _, tc := range doc.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			var asked [][]string
			i := 0
			polite := tc.Host == "polite"

			// A polite host hands over a queued name only while that name is
			// still awaited, which is what the engine's own DurableAwaitSignals
			// does -- it exercises the NARROWING. An impolite one hands over
			// whatever is queued and exercises the OUT-OF-SET GUARD.
			stub := func(want []string, _ int64) (string, string, bool, error) {
				asked = append(asked, append([]string(nil), want...))
				for i < len(tc.Deliveries) {
					name := tc.Deliveries[i]
					i++
					if !polite || containsName(want, name) {
						payload, ok := tc.Payloads[name]
						if !ok {
							payload = `{"ok":true}`
						}
						return name, payload, false, nil
					}
				}
				return "", "", true, nil
			}

			h := NewHostCalls(HostCallsOptions{DurableAwaitSignals: stub})
			names := append([]string(nil), tc.SignalNames...)
			callerSet := append([]string(nil), names...)

			// A minute, so the deadline is never what ends a case: the host's
			// own timed-out reply is the only source of a timeout, and the
			// assertions are about the loop rather than about the clock.
			got, err := h.AwaitSignalsWithQuorum(names, tc.MinCount, tc.MaxRejections, time.Minute)

			switch tc.Expect.Outcome {
			case "ok":
				if err != nil {
					t.Fatalf("expected success, got: %v", err)
				}
				var gotNames []string
				for _, r := range got {
					gotNames = append(gotNames, r.Name)
				}
				if !reflect.DeepEqual(gotNames, tc.Expect.ResultNames) {
					t.Errorf("result names: got %v, want %v", gotNames, tc.Expect.ResultNames)
				}
			case "error":
				if err == nil {
					var gotNames []string
					for _, r := range got {
						gotNames = append(gotNames, r.Name)
					}
					t.Fatalf("expected an error, got %v", gotNames)
				}
				if k := kindOf(err); k != tc.Expect.ErrorKind {
					t.Errorf("failed for the wrong reason: kind %q, want %q\n  %v",
						k, tc.Expect.ErrorKind, err)
				}
			default:
				t.Fatalf("unknown expected outcome %q", tc.Expect.Outcome)
			}

			// The narrowing is the mechanism, and this is the assertion that
			// sees it. Checking only the outcome passes against an
			// implementation that fails for an unrelated reason.
			want := tc.Expect.AwaitedSets
			if want == nil {
				want = [][]string{}
			}
			if asked == nil {
				asked = [][]string{}
			}
			if !reflect.DeepEqual(asked, want) {
				t.Errorf("the sets it awaited: got %v, want %v", asked, want)
			}

			if tc.Expect.CallerSetUnchanged && !reflect.DeepEqual(names, callerSet) {
				t.Errorf("the caller's slice was edited: %v, want %v", names, callerSet)
			}
		})
	}
}
