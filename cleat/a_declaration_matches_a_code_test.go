package cleat

import (
	"errors"
	"fmt"
	"testing"
)

// The SDK-side half of the non-retryable contract. It must agree with the
// host's, because which one decides depends only on whether the retry policy
// fitted in one segment -- a difference no reader would predict from the
// workflow's own source.
//
// The format asserted here is pinned on the other side too, by
// engine.TestTheErrorTextLeadsWithTheCodeSoTheSDKCanRecoverIt.
//
// MATCHING IS A PREFIX TEST AGAINST WHAT THE CALLER DECLARED, which is what
// lets any spelling of a code work. An earlier version extracted the leading
// token and required it to be upper case -- a rule that would have rejected
// PascalCase codes, the convention AWS uses throughout
// (InvalidParameterValue, ResourceNotFound). The existing SDK tests, which use
// InvalidRequest and NotFound, caught it.
func TestADeclarationMatchesTheCodeNotTheMessage(t *testing.T) {
	declared := []string{"INSUFFICIENT_FUNDS"}

	for _, tc := range []struct {
		name string
		err  error
		want bool
		why  string
	}{
		{"code with message and id",
			fmt.Errorf("INSUFFICIENT_FUNDS: balance too low (correlation_id=abc123)"), true,
			"the full shape engine.ServiceError.Error() writes"},
		{"code with a different message",
			fmt.Errorf("INSUFFICIENT_FUNDS: not enough money"), true,
			"rewording the message must not change the decision -- the whole point"},
		{"code alone", errors.New("INSUFFICIENT_FUNDS"), true,
			"a service may give a code and no prose"},
		{"a different code", errors.New("UPSTREAM_TIMEOUT: gateway gave up"), false,
			"a code that was not declared"},
		{"the code mentioned in a message", errors.New("RETRY_LATER: after INSUFFICIENT_FUNDS clears"), false,
			"a service must not be able to change a caller's control flow by what it " +
				"writes in a sentence -- this is the coupling being removed"},
		{"a lower-case word before a colon", errors.New("timeout: deadline exceeded"), false,
			"an ordinary message leading with a word and a colon matches nothing, " +
				"because no caller declared a code spelled that way"},
		{"a PascalCase code", errors.New("InvalidRequest: bad input"), false,
			"matches only when DECLARED -- see the declared-PascalCase case below"},
		{"a transport error", errors.New("connection refused"), false,
			"no code at all, so no declaration matches"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isNonRetryable(tc.err, declared); got != tc.want {
				t.Errorf("isNonRetryable(%q) = %v, want %v -- %s", tc.err, got, tc.want, tc.why)
			}
		})
	}
}

// Any spelling of a code works, because matching compares against what the
// caller declared rather than guessing what looks like a code.
func TestACodeMatchesInWhateverCaseTheServiceUses(t *testing.T) {
	for _, tc := range []struct {
		declared string
		err      error
	}{
		{"InvalidRequest", errors.New("InvalidRequest: bad input")},
		{"INSUFFICIENT_FUNDS", errors.New("INSUFFICIENT_FUNDS: balance too low")},
		{"rate.limited", errors.New("rate.limited: slow down")},
		{"Err404", errors.New("Err404")},
	} {
		t.Run(tc.declared, func(t *testing.T) {
			if !isNonRetryable(tc.err, []string{tc.declared}) {
				t.Errorf("a declared code %q did not match %q; matching must not "+
					"impose a spelling convention on a service's own codes", tc.declared, tc.err)
			}
		})
	}
}
