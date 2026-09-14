package main

import (
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Every handler that reads Idempotency-Key answers with the standard replay
// flag. cleat#1169.
//
// THE POPULATION IS DERIVED, NOT LISTED, and that is the whole point. cleat#1167
// was found by auditing endpoints one at a time — reprocess created work and
// deduplicated nothing, and nobody knew until somebody looked. A hand-written
// list of endpoints would have the same defect: it covers what its author
// remembered.
//
// So the set is "handlers that read Idempotency-Key", read out of the source,
// and the assertion is that each one sets idempotent_replay. A new endpoint that
// honours the header is covered the moment it is written, without anyone
// remembering to add it here.
//
// WHY THAT IS THE RIGHT POPULATION rather than "endpoints that create work":
// the second is the rule the owner stated, but it cannot be decided by reading
// source — "creates work" is a judgement. Reading the header is the observable
// commitment to the policy, so a handler that creates work and does NOT read
// the header is a different defect (cleat#1167's) and gets caught by review,
// not by this.
//
// schedules is the open case: it creates work and does NOT read the header, so
// it is legitimately absent here and is tracked separately. When it gains one,
// this test starts requiring its flag with no edit.
func TestEveryKeyBearingEndpointCarriesTheReplayFlag(t *testing.T) {
	files := trackedWorkerGoFiles(t)

	// A handler is a func on *apiServer. Find each one's body, then ask whether
	// it reads the header and whether it sets the flag.
	funcRe := regexp.MustCompile(`(?m)^func \(s \*apiServer\) (\w+)\(`)

	type handler struct{ readsKey, setsFlag bool }
	got := map[string]handler{}

	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		src := string(b)
		locs := funcRe.FindAllStringSubmatchIndex(src, -1)
		for i, loc := range locs {
			name := src[loc[2]:loc[3]]
			end := len(src)
			if i+1 < len(locs) {
				end = locs[i+1][0]
			}
			body := src[loc[1]:end]
			// Strip comments: a handler DESCRIBING the header in prose is not a
			// handler reading it, and this file would otherwise report the
			// policy's own documentation as a violation.
			body = stripGoComments(body)
			h := got[name]
			if strings.Contains(body, `Header.Get("Idempotency-Key")`) {
				h.readsKey = true
			}
			if strings.Contains(body, "withReplayFlag(") {
				h.setsFlag = true
			}
			got[name] = h
		}
	}

	var readers, missing, carrying []string
	for name, h := range got {
		if !h.readsKey {
			continue
		}
		readers = append(readers, name)
		if h.setsFlag {
			carrying = append(carrying, name)
		} else {
			missing = append(missing, name)
		}
	}
	sort.Strings(readers)
	sort.Strings(missing)
	sort.Strings(carrying)

	// NON-VACUITY. If the scan stops finding handlers, or stops finding the
	// header, every assertion above is satisfied by an empty set — which reads
	// exactly like success.
	if len(got) == 0 {
		t.Fatal("found no *apiServer handlers at all: the scan is broken, so this test " +
			"is vacuous rather than passing")
	}
	if len(readers) == 0 {
		t.Fatalf("found %d handlers and none reading Idempotency-Key. At least start, "+
			"reprocess and signal do (cleat#1121, #1167), so the scan has stopped seeing "+
			"the header and every assertion here is vacuous.", len(got))
	}

	if len(missing) > 0 {
		t.Errorf("these handlers read Idempotency-Key and do not set the replay flag:\n  %s\n\n"+
			"cleat#1169's policy is that a duplicate returns the ORIGINAL response plus one "+
			"standard flag. A handler that honours the header and omits the flag leaves its "+
			"callers unable to tell a replay from a first call without endpoint-specific "+
			"knowledge -- which is the thing the policy removes. Use withReplayFlag().",
			strings.Join(missing, "\n  "))
	}

	// `carrying`, not `readers`. readers is every handler that reads the
	// header, which includes the ones reported as missing three lines above --
	// so logging it named the same handler as missing the flag AND as carrying
	// one, in the same output, in exactly the failure case someone is reading
	// this to understand. Found by WS-1 running this guard's known-positive,
	// which is the run where the two lines are printed together.
	t.Logf("key-bearing handlers carrying the flag: %s", strings.Join(carrying, ", "))
}

// The flag's value must be a bool, not the string "true".
//
// `already_started` was `"true"` -- a string -- because the response was a
// map[string]string. A caller in a typed language reading that as a boolean
// gets a type error, and one reading it as a string has to compare against a
// literal. The policy is only uniform if the flag has one type everywhere.
func TestTheReplayFlagIsABool(t *testing.T) {
	for _, tc := range []struct {
		name     string
		replayed bool
	}{{"replay", true}, {"original", false}} {
		body := withReplayFlag(map[string]any{"id": "x"}, tc.replayed)
		v, ok := body[idempotentReplayField]
		if !ok {
			t.Fatalf("%s: the flag is absent. It is present on BOTH the original and the "+
				"replay so a caller can read it unconditionally -- an absent field cannot be "+
				"told from an old server that does not send one.", tc.name)
		}
		b, isBool := v.(bool)
		if !isBool {
			t.Fatalf("%s: the flag is %T, want bool", tc.name, v)
		}
		if b != tc.replayed {
			t.Errorf("%s: flag = %v, want %v", tc.name, b, tc.replayed)
		}
	}
}

// trackedWorkerGoFiles lists this package's tracked non-test sources.
//
// git ls-files rather than a directory walk: a walk descends into scratch
// checkouts and worktrees, which is how a guard comes to vouch for a file that
// is not in the repository.
func trackedWorkerGoFiles(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("git", "ls-files", ".").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var files []string
	for _, f := range strings.Fields(string(out)) {
		if strings.HasSuffix(f, ".go") && !strings.HasSuffix(f, "_test.go") {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		t.Fatal("no tracked .go files found: the scan would pass vacuously")
	}
	return files
}
