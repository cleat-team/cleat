package plugin

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// cleat#3789: the first webhook source anyone creates on a worker started the
// way the quick-start starts it returned `500 {"error":"failed to store
// secret"}`. The cause was in the worker's log and nowhere the caller could
// see.

func TestAMissingMasterKeyIsNamedToTheCallerWithWhatToSet(t *testing.T) {
	for name, err := range map[string]error{
		"the sentinel itself": ErrNoSecretMasterKey,
		"wrapped":             fmt.Errorf("store source secret: %w", ErrNoSecretMasterKey),
	} {
		msg := StoreFailureMessage(err)
		for _, want := range []string{"failed to store secret", "no secret master key", "CLEAT_SECRET_MASTER_KEY"} {
			if !strings.Contains(msg, want) {
				t.Errorf("%s: message %q does not contain %q", name, msg, want)
			}
		}
	}
}

// The control that makes the test above mean something: every OTHER failure
// keeps the bare message and is not echoed. A store error can carry a driver or
// connection detail the caller has no use for, and the log already has it.
func TestAnyOtherStoreFailureIsNotEchoedToTheCaller(t *testing.T) {
	detail := "pq: password authentication failed for user \"cleat_app\" at 10.1.2.3:5432"
	got := StoreFailureMessage(errors.New(detail))
	if got != "failed to store secret" {
		t.Errorf("message = %q, want exactly %q", got, "failed to store secret")
	}
	if strings.Contains(got, "10.1.2.3") || strings.Contains(got, "cleat_app") {
		t.Errorf("a store error's detail reached the caller: %q", got)
	}
}
