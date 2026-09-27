package engine

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// cleat#1887 review. A called service's failure used to reach the caller as
// the raw response body, and the workflow's non-retryable declarations were
// substring-matched against that body. The callee's PROSE therefore decided
// the caller's retry behaviour.
//
// This is the test that makes the defect concrete: two messages that mean the
// same thing, one code. Under substring matching the first matched a pattern
// of "insufficient funds" and the second did not, so rewording flipped a
// non-idempotent call from fail-fast to retried. Under code matching both are
// the same failure, which is what the service said they were.
func TestRewordingAServiceErrorDoesNotChangeRetryBehaviour(t *testing.T) {
	declared := []string{"INSUFFICIENT_FUNDS"}

	for _, message := range []string{
		"insufficient funds",
		"balance too low",
		"", // a service that gives a code and no prose at all
	} {
		t.Run(fmt.Sprintf("message=%q", message), func(t *testing.T) {
			err := NewPermanentError("service", "", &ServiceError{
				Code:    "INSUFFICIENT_FUNDS",
				Message: message,
			})
			if !isDefinitelyNonRetryable(err, declared) {
				t.Errorf("a declared non-retryable code was retried because the message "+
					"read %q. The code is the contract; the message is not.", message)
			}
		})
	}
}

// And the other direction: a message that happens to contain a declared code
// must not match, because that is the coupling being removed.
func TestAMessageMentioningACodeIsNotTheCode(t *testing.T) {
	declared := []string{"INSUFFICIENT_FUNDS"}

	err := NewTransientError("service", "", &ServiceError{
		Code:    "UPSTREAM_TIMEOUT",
		Message: "retrying after INSUFFICIENT_FUNDS was resolved",
	})
	if isDefinitelyNonRetryable(err, declared) {
		t.Error("a code named in the MESSAGE matched a declaration. Matching prose is " +
			"exactly what this replaced -- a service must not be able to change a " +
			"caller's control flow by what it writes in a sentence.")
	}
}

// An error that is not a ServiceError matches no declaration. That is the
// intended outcome: a third-party service or a transport failure is classified
// by status, which is a sounder answer than searching an arbitrary body.
func TestADeclarationMatchesNothingWithoutAServiceError(t *testing.T) {
	declared := []string{"INSUFFICIENT_FUNDS"}

	err := NewTransientError("service", "", errors.New("insufficient funds"))
	if isDefinitelyNonRetryable(err, declared) {
		t.Error("a plain error matched a declaration by its text; the substring channel " +
			"is supposed to be gone")
	}
}

// The interface channel still short-circuits ahead of code matching, and still
// wins. A permanent classification is non-retryable whether or not any code
// was declared.
func TestThePermanentClassificationStillWinsWithoutAnyDeclaration(t *testing.T) {
	err := NewPermanentError("service", "", &ServiceError{Code: "ANYTHING"})
	if !isDefinitelyNonRetryable(err, nil) {
		t.Error("a permanently-classified error became retryable when no codes were declared")
	}
}

// ParseServiceError decides whether a body is the contract at all, and getting
// that wrong in the permissive direction would invent a contract the callee
// never agreed to.
func TestParseServiceErrorOnlyClaimsBodiesThatAreTheContract(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
		why  string
	}{
		{"the contract", `{"code":"INSUFFICIENT_FUNDS","message":"nope"}`, true, ""},
		{"code only", `{"code":"X"}`, true, "message is optional; the code is not"},
		{"no code", `{"message":"something failed"}`, false,
			"a JSON object without a code is not a service speaking this protocol"},
		{"empty code", `{"code":""}`, false, "an empty code is no code"},
		{"plain text", `insufficient funds`, false, "not JSON at all"},
		{"an HTML error page", `<html><body>502</body></html>`, false,
			"a proxy's error page must not be read as the service's own words"},
		{"a JSON array", `["code","X"]`, false, "the contract is an object"},
		{"empty body", ``, false, ""},
		{"malformed JSON", `{"code":"X"`, false, "a truncated body is not a contract"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseServiceError([]byte(tc.body), 400) != nil
			if got != tc.want {
				t.Errorf("ParseServiceError(%q) claimed=%v, want %v -- %s", tc.body, got, tc.want, tc.why)
			}
		})
	}
}

// "Did not say" and "said no" must stay distinguishable. A bool would conflate
// them, which is the same defect that made a nil pattern slice dangerous.
func TestAServiceThatSaysNothingAboutRetryingIsNotSayingNo(t *testing.T) {
	silent := ParseServiceError([]byte(`{"code":"X"}`), 500)
	if stated, _ := silent.RetryableFromService(); stated {
		t.Error("a body with no retryable field reported that the service stated one")
	}

	no := ParseServiceError([]byte(`{"code":"X","retryable":false}`), 500)
	stated, retryable := no.RetryableFromService()
	if !stated || retryable {
		t.Errorf("an explicit retryable=false read as stated=%v retryable=%v", stated, retryable)
	}

	yes := ParseServiceError([]byte(`{"code":"X","retryable":true}`), 400)
	stated, retryable = yes.RetryableFromService()
	if !stated || !retryable {
		t.Errorf("an explicit retryable=true read as stated=%v retryable=%v", stated, retryable)
	}
}

// The error text a human reads must carry the code and the correlation id,
// because a caller who cannot see the callee's internals can still quote an id
// its team can find.
func TestTheErrorTextCarriesTheCodeAndCorrelationID(t *testing.T) {
	se := &ServiceError{Code: "INSUFFICIENT_FUNDS", Message: "balance too low", CorrelationID: "abc123"}
	got := se.Error()
	for _, want := range []string{"INSUFFICIENT_FUNDS", "balance too low", "abc123"} {
		if !strings.Contains(got, want) {
			t.Errorf("error text %q does not carry %q", got, want)
		}
	}
}

// THE FORMAT IS A CONTRACT BETWEEN TWO MODULES, so it is pinned here as well
// as in the SDK.
//
// cleat/runtime_workflow.go's serviceErrorCode reads the leading token of this
// string to recover the code, because the SDK-side retry fallback -- used when
// the host refuses a retry policy as too long -- cannot import this package.
// Two implementations of one format is how formats drift, so both ends assert
// it. If this changes, cleat/runtime_workflow.go changes with it.
func TestTheErrorTextLeadsWithTheCodeSoTheSDKCanRecoverIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		se   *ServiceError
		want string
	}{
		{"code, message and id", &ServiceError{
			Code: "INSUFFICIENT_FUNDS", Message: "balance too low", CorrelationID: "abc123",
		}, "INSUFFICIENT_FUNDS:"},
		{"code and message", &ServiceError{
			Code: "UPSTREAM_TIMEOUT", Message: "gateway gave up",
		}, "UPSTREAM_TIMEOUT:"},
		{"code alone", &ServiceError{Code: "RATE_LIMITED"}, "RATE_LIMITED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.se.Error()
			if !strings.HasPrefix(got, tc.want) {
				t.Errorf("Error() = %q, want it to lead with %q.\n"+
					"cleat/runtime_workflow.go's serviceErrorCode takes the token before "+
					"the first colon; changing this format silently breaks the SDK's "+
					"retry fallback, which has no way to import this package.", got, tc.want)
			}
		})
	}
}
