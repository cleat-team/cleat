package plugin

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
)

// RedactedPlaceholder is what a Secret marshals and prints as.
const RedactedPlaceholder = "[redacted]"

// Secret is a string that refuses to leave the process.
//
// It exists because five plugins independently stored a credential in a plain
// string field and returned it, unredacted, from their list and get endpoints:
// webhookingest and notifications (HMAC signing keys), pagerdutyalert (routing
// key), slacknotify (webhook URL, which IS the credential), and datadogexport
// (API key). Any bug that let a caller read one tenant's configuration row
// therefore disclosed the credential itself rather than merely its existence.
//
// The obvious fix is a redaction helper called on the way out. That was
// rejected: a helper you have to remember is a helper the next plugin author
// forgets, and the failure is silent and invisible in review -- the response
// looks fine because the field is populated. datadogexport had exactly that
// shape and it did not hold; its redactAPIKey() was defeated by adding
// ?show_api_key=true to the URL, with no authorization check behind it.
//
// So the redaction is in the TYPE. A Secret always marshals as
// RedactedPlaceholder, whatever the caller does, and there is no code path
// through encoding/json that emits the real value. Forgetting to redact now
// requires not using the type, which TestPluginSecretsUseTheSecretType catches.
//
// It stays usable at both ends:
//
//	UnmarshalJSON accepts a real value, because create and update requests
//	carry one.
//	Scan and Value carry the real value to and from the database, because that
//	is where it legitimately lives.
//	Reveal returns the real value for the code that must actually use it --
//	signing an HMAC, calling an API. It is a method rather than a conversion so
//	that every intentional use is greppable.
//
// String and Format redact too, so a Secret cannot leak through a log line or
// a %v in an error either. That is not hypothetical: the value most likely to
// end up in an error message is the one that failed to work.
type Secret string

// MarshalJSON always emits the placeholder. This is the whole point of the
// type; there is deliberately no option to disable it.
func (Secret) MarshalJSON() ([]byte, error) {
	return json.Marshal(RedactedPlaceholder)
}

// UnmarshalJSON accepts a real value, because create and update requests carry
// one.
//
// It rejects the placeholder. Without that, a client that read a config, edited
// one unrelated field and PUT the whole object back would silently overwrite
// the stored credential with the literal string "[redacted]" -- a
// round-trip-shaped data loss that is easy to build a UI on top of by accident.
// The caller is told to omit the field instead.
func (s *Secret) UnmarshalJSON(b []byte) error {
	var raw string
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if raw == RedactedPlaceholder {
		return fmt.Errorf("refusing to store the literal %q: this is what a redacted read "+
			"returns, so writing it back would overwrite the real value. Omit the field to "+
			"leave it unchanged", RedactedPlaceholder)
	}
	*s = Secret(raw)
	return nil
}

// String redacts, so a Secret cannot leak through fmt, a log line, or an error.
func (Secret) String() string { return RedactedPlaceholder }

// Format redacts for every verb, including %v and %#v.
//
// String alone is not enough: %#v ignores Stringer and prints the underlying
// value, and %q would too.
func (Secret) Format(f fmt.State, verb rune) {
	_, _ = f.Write([]byte(RedactedPlaceholder))
}

// Reveal returns the real value.
//
// A method rather than a plain conversion so that every intentional use is one
// grep away. A reviewer asking "where does this credential actually get used"
// gets an answer from `grep -rn '.Reveal()'`.
func (s Secret) Reveal() string { return string(s) }

// Value carries the real value to the database, which is where it legitimately
// lives.
func (s Secret) Value() (driver.Value, error) { return string(s), nil }

// Scan reads the real value back from the database.
func (s *Secret) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*s = ""
	case string:
		*s = Secret(v)
	case []byte:
		*s = Secret(v)
	default:
		return fmt.Errorf("cannot scan %T into a Secret", src)
	}
	return nil
}
