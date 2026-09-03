package cleat

import (
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// The host-retry threshold is written twice, in two languages, and nothing at
// compile time can make the two agree. IMPROVEMENT-PLAN 3.88.
//
// cleat.hostRetryBudget decides whether a Go workflow's retry policy runs on
// the host (one segment, worker held) or in the SDK (one segment per backoff,
// worker released). crates/cleat-sdk's HOST_RETRY_BUDGET_MS decides the same
// thing for Rust. A silent divergence means one identical RetryPolicy holding a
// worker on one SDK and suspending on the other -- which is precisely the
// class of defect 3.88 exists to remove, so reintroducing it by drift would be
// the worst possible outcome.
//
// This test is a stopgap by design. The threshold is currently a GUEST-side
// compile-time constant, which is the wrong layer for it: an operator cannot
// change it, and a tenant cannot be given their own value, because the decision
// is taken before the host is ever consulted. IMPROVEMENT-PLAN 3.94 moves it
// host-side, where the policy already arrives in full. **When that lands, both
// constants disappear and so does this test.**
func TestBothSDKsAgreeOnTheHostRetryBudget(t *testing.T) {
	const rustSrc = "../crates/cleat-sdk/src/host_calls.rs"

	src, err := os.ReadFile(rustSrc)
	if err != nil {
		t.Fatalf("reading %s: %v\n\n"+
			"This test compares a Go constant against a Rust one by reading the "+
			"Rust source. If the SDK moved, re-point it -- do not delete it "+
			"unless the threshold itself has moved host-side (3.94), which is "+
			"the only thing that makes the comparison unnecessary.", rustSrc, err)
	}

	re := regexp.MustCompile(`(?m)^pub const HOST_RETRY_BUDGET_MS: u64 = ([0-9_]+);`)
	m := re.FindSubmatch(src)
	if m == nil {
		t.Fatalf("no `pub const HOST_RETRY_BUDGET_MS: u64 = ...;` in %s.\n\n"+
			"A regex that matches nothing passes vacuously, so this is a "+
			"failure rather than a skip. Either the constant was renamed -- in "+
			"which case fix this pattern -- or it was removed, which would mean "+
			"the Rust SDK no longer applies the threshold and the two SDKs "+
			"disagree again.", rustSrc)
	}

	digits := ""
	for _, r := range string(m[1]) {
		if r != '_' {
			digits += string(r)
		}
	}
	rustMs, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		t.Fatalf("parsing HOST_RETRY_BUDGET_MS %q: %v", m[1], err)
	}

	goMs := hostRetryBudget.Milliseconds()
	if rustMs != goMs {
		t.Fatalf("the two SDKs disagree about the host-retry threshold: "+
			"Go %v (%dms), Rust %dms.\n\n"+
			"An identical RetryPolicy would run on the host on one SDK and "+
			"suspend on the other. Change both, or move the threshold "+
			"host-side per IMPROVEMENT-PLAN 3.94 and delete both constants.",
			hostRetryBudget, goMs, rustMs)
	}

	// A budget of zero would send every policy down the SDK path and a huge one
	// would send every policy to the host, either of which makes both this test
	// and the threshold itself meaningless while still "agreeing".
	if hostRetryBudget <= 0 || hostRetryBudget > 10*time.Minute {
		t.Fatalf("hostRetryBudget is %v, which is not a threshold either side of "+
			"which a real policy falls", hostRetryBudget)
	}
}
