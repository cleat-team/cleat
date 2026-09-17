package engine

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestSecretResolutionStaysPastTheRecorder is the guard the behavioural tests
// cannot be.
//
// WHAT KEEPS A SECRET OUT OF EVENT HISTORY IS AN ORDERING, and an ordering is
// invisible to a unit test of either side. durablecalls.go calls
// callService(..., requestJSON, ...) and then records `Request: requestJSON`
// from the SAME variable; callintent.go writes its intent row from that
// variable before dispatch. Substitution happens downstream of both, inside
// dbServiceCaller, so neither the event nor the intent can observe a resolved
// value.
//
// That holds only while resolution stays on the caller's side of the boundary.
// Move it up into callService, into the cleat_call host function, or into the
// SDK, and every recorded request silently becomes the plaintext credential --
// with no test failing, because the call still succeeds and the workflow still
// works. The damage is only visible later, to whoever reads event_history.
//
// So this asserts the structural fact directly: NOTHING IN package engine
// resolves secret references. engine defines ResolveSecretRefs for its
// embedders and must never call it. The one legitimate mention is the
// definition itself.
//
// An earlier draft of this file tried to prove the property by comparing a
// recorded event against a resolved request, which cannot fail for any
// implementation -- the engine holds its own immutable string, so Go guarantees
// the comparison for free. That is the same vacuity
// TestTheWrapperSubstitutesInsideTheCalleeNotBeforeIt records about its own
// first version, and it is recorded here rather than quietly dropped.
func TestSecretResolutionStaysPastTheRecorder(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read engine dir: %v", err)
	}

	var offenders []string
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, rerr := os.ReadFile(filepath.Clean(name))
		if rerr != nil {
			t.Fatalf("read %s: %v", name, rerr)
		}
		scanned++
		for i, line := range strings.Split(string(src), "\n") {
			if !strings.Contains(line, "ResolveSecretRefs(") {
				continue
			}
			// The declaration is the one permitted mention.
			if strings.HasPrefix(strings.TrimSpace(line), "func ResolveSecretRefs(") {
				continue
			}
			offenders = append(offenders,
				name+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
		}
	}

	// A scan that measured nothing reads identically to a clean tree.
	if scanned == 0 {
		t.Fatal("scanned no engine source files; this check cannot pass vacuously")
	}

	if len(offenders) > 0 {
		t.Errorf("package engine resolves secret references in %d place(s):\n  %s\n\n"+
			"Resolution must stay downstream of the recorder. durablecalls.go records\n"+
			"`Request: requestJSON` from the same variable it hands to the ServiceCaller,\n"+
			"and callintent.go writes its intent from that variable BEFORE dispatch, so\n"+
			"resolving here writes the plaintext credential into event_history. Resolve in\n"+
			"dbServiceCaller.resolveSecrets (cmd/cleat-worker/setup.go) instead.",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}
