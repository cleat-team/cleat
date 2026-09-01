package engine

import (
	"context"
	"errors"
	"testing"
)

// countingErroringCaller fails every call and records how many it got, which is
// what distinguishes "refused the request" from "retried it".
type countingErroringCaller struct {
	err   error
	calls int
}

func (c *countingErroringCaller) Call(_ context.Context, _, _, _ string) (string, error) {
	c.calls++
	return "", c.err
}

// TestMalformedNonRetryableListIsRefusedNotSilentlyIgnored covers a dropped
// json.Unmarshal on a guest-supplied argument.
//
// cleat_call_retry takes the workflow author's non-retryable error patterns as a
// JSON array. freshCallWithRetry parsed it like this:
//
//	var nonRetryableErrors []string
//	if nonRetryableErrorsJSON != "" {
//	    json.Unmarshal([]byte(nonRetryableErrorsJSON), &nonRetryableErrors)   // error dropped
//	}
//
// On malformed JSON the slice stays nil, so isDefinitelyNonRetryable never
// matches and every error becomes retryable. The author's explicit "do not retry
// this" is silently discarded, and a call they marked non-retryable is issued up
// to maxAttempts times. For the non-idempotent operations that declaration
// exists to protect, that is a duplicate side effect -- the same consequence
// TestNonRetryableCallIsNotReportedAsRetryable was written for, reached by a
// different route.
//
// The argument crosses the ABI boundary from five language SDKs, which is the
// layer CLAUDE.md records as the source of four real defects, "in every case the
// value meant the wrong thing on one side of the boundary". An SDK emitting a
// bare comma-separated string instead of a JSON array produces exactly this and
// nothing reports it.
//
// A malformed argument is refused with badParamDurableCall, the convention
// established in §2.10 for this host-function family, rather than being treated
// as an empty list. Treating "I could not read your safety declaration" as "you
// made no safety declaration" is the unsafe direction to fail in.
func TestMalformedNonRetryableListIsRefusedNotSilentlyIgnored(t *testing.T) {
	const maxAttempts = 5

	// Control first: a well-formed list stops the loop after one attempt, so a
	// difference in call count below is attributable to the parse and not to the
	// retry loop being broken in general.
	t.Run("well-formed list is honoured", func(t *testing.T) {
		caller := &countingErroringCaller{err: errors.New("INSUFFICIENT_FUNDS: balance too low")}
		s := &execSession{engine: NewEngine(nil, caller)}
		s.DurableCallWithRetry(context.Background(), nil, "svc", "op", `{}`,
			maxAttempts, 1, 100, 1, `["INSUFFICIENT_FUNDS"]`, 0, 0)
		if caller.calls != 1 {
			t.Errorf("well-formed non-retryable list produced %d calls, want 1; "+
				"the control is broken, so the malformed case below proves nothing",
				caller.calls)
		}
	})

	t.Run("malformed list is refused", func(t *testing.T) {
		caller := &countingErroringCaller{err: errors.New("INSUFFICIENT_FUNDS: balance too low")}
		s := &execSession{engine: NewEngine(nil, caller)}

		// Not a JSON array. A plausible SDK slip, not a hostile input.
		result := s.DurableCallWithRetry(context.Background(), nil, "svc", "op", `{}`,
			maxAttempts, 1, 100, 1, `INSUFFICIENT_FUNDS`, 0, 0)

		if caller.calls != 0 {
			t.Errorf("a call with an unparseable non-retryable list was issued %d times. "+
				"The list failed to parse, so the engine treated every error as retryable "+
				"and retried an operation the author marked non-retryable.", caller.calls)
		}
		if result != badParamDurableCall {
			t.Errorf("result = %#x, want badParamDurableCall (%#x). A malformed argument "+
				"must use the layout the guest adapter decodes -- see badParamDurableCall "+
				"in memory.go for what returning the raw sentinel did.",
				result, badParamDurableCall)
		}
	})

	// An empty string is not malformed: it is how a workflow with no
	// non-retryable patterns is encoded, and it must keep working.
	t.Run("empty string means no patterns", func(t *testing.T) {
		caller := &countingErroringCaller{err: errors.New("boom")}
		s := &execSession{engine: NewEngine(nil, caller)}
		result := s.DurableCallWithRetry(context.Background(), nil, "svc", "op", `{}`,
			maxAttempts, 1, 100, 1, "", 0, 0)
		if result == badParamDurableCall {
			t.Error("an empty non-retryable list was refused as malformed; that is the " +
				"normal encoding for a workflow that declares no patterns")
		}
		if caller.calls == 0 {
			t.Error("no call was issued at all")
		}
	})
}
