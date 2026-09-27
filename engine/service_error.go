package engine

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ServiceError is a failure a called service described about itself, as
// opposed to one cleat inferred from an HTTP status.
//
// WHY THIS EXISTS. Before it, a service's failure reached the caller as
// `fmt.Errorf("%s", string(body))` -- the callee's raw response body became
// the caller's error MESSAGE -- and isDefinitelyNonRetryable then decided
// whether to retry by substring-matching that message against the workflow's
// declared patterns. So the callee's prose was load-bearing: rewording
// "insufficient funds" to "balance too low" silently flipped the caller from
// fail-fast to retry, on a non-idempotent operation, across a team boundary,
// with no signal at compile time or deploy time and nothing declaring that the
// coupling existed. cleat#1887 review.
//
// The intent was always a code. durablecalls.go's own comment describes the
// feature as a workflow saying "do not retry INSUFFICIENT_FUNDS" -- a code,
// spelled like a code. Only the matching was ever textual.
//
// THE STABLE TIER ONLY. Code, Message, CorrelationID and Retry are the part of
// a service's error that is contract: a caller may branch on Code and must not
// branch on Message. A diagnostic tier -- the callee's internal detail, useful
// to a caller inside the same trust boundary and hidden outside it -- is
// deliberately not here, because which callers are inside is a question cleat
// cannot answer until exposure classes exist.
type ServiceError struct {
	// Code is the service's stable, API-defined identifier for this failure.
	// It is what a workflow's non-retryable declarations match, and the only
	// field a caller should branch on.
	Code string `json:"code"`

	// Message is human-readable and explicitly NOT contract. It may change
	// between releases of the service without notice.
	Message string `json:"message,omitempty"`

	// CorrelationID links this failure to the callee's own logs. It is what
	// makes withholding internal detail workable rather than merely
	// restrictive: a caller who cannot see why can still say which.
	CorrelationID string `json:"correlation_id,omitempty"`

	// Retry is the service's own statement about whether this failure is worth
	// retrying. NIL MEANS UNSPECIFIED, not false: a service that says nothing
	// leaves the decision to the HTTP status, which is the pre-existing and
	// sound default (4xx permanent except 408 and 429). A bool rather than a
	// pointer would make "did not say" indistinguishable from "said no", which
	// is the same conflation that made a nil pattern slice dangerous.
	Retry *bool `json:"retryable,omitempty"`

	// Status is the HTTP status the response carried, kept so that a caller
	// reading the error does not have to reconstruct it.
	Status int `json:"-"`
}

func (e *ServiceError) Error() string {
	var b strings.Builder
	if e.Code != "" {
		b.WriteString(e.Code)
	} else {
		b.WriteString("service error")
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.CorrelationID != "" {
		fmt.Fprintf(&b, " (correlation_id=%s)", e.CorrelationID)
	}
	return b.String()
}

// DELIBERATELY NOT implementing RetryableError.
//
// errors.As takes the FIRST match walking the chain. A ServiceError is always
// wrapped in a CleatError carrying the classification the forwarder decided --
// from the service's stated preference when it gave one, from the HTTP status
// when it did not -- and that wrapper is what answers. If ServiceError also
// implemented the interface, a Retry of nil would have to answer something,
// and "the service did not say" would become "the service said no" wherever
// the unwrap order put it first. One decision, made once, in the forwarder.

// RetryableFromService reports whether the service stated a retry preference
// at all, and what it was.
func (e *ServiceError) RetryableFromService() (stated bool, retryable bool) {
	if e.Retry == nil {
		return false, false
	}
	return true, *e.Retry
}

// ParseServiceError reads the structured error contract out of a response
// body, returning nil when the body is not one.
//
// DELIBERATELY STRICT ABOUT WHAT COUNTS. A body is only the contract if it
// parses as a JSON object AND carries a non-empty "code". Anything else -- a
// plain-text error, an HTML error page from a proxy, a JSON object that
// happens to have a "message" -- is not a service speaking this protocol, and
// treating it as one would invent a contract the callee never agreed to. The
// caller then falls back to status classification, which is what every
// non-cleat service already gets.
func ParseServiceError(body []byte, status int) *ServiceError {
	trimmed := strings.TrimSpace(string(body))
	if !strings.HasPrefix(trimmed, "{") {
		return nil
	}
	var se ServiceError
	if err := json.Unmarshal([]byte(trimmed), &se); err != nil {
		return nil
	}
	if se.Code == "" {
		return nil
	}
	se.Status = status
	return &se
}
