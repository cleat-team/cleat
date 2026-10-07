package engine

// cleat#2336: checkSecretOnlyFields (plugins.go) and resolveRefsWith
// (tenant_secrets.go) used to have two different ideas of what a
// "${secret:NAME}" reference in a field's JSON value means.
// checkSecretOnlyFields json.Unmarshal'd the value first and matched the
// DECODED Go string; resolveRefsWith scans the RAW, undecoded JSON text,
// because that is the only form it ever sees -- it runs before anything
// decodes the document.
//
// A value written with JSON escaping that DECODES to a reference -- the
// string literal secretOnlyFieldJSONEscaped below, which json.Unmarshal
// turns into the Go string "${secret:openai-key}" -- passed the first check
// and was invisible to the second: resolveRefsWith's regex looks for the
// literal bytes of the reference in the raw text, and the escaped form does
// not contain them. The call proceeded with the literal placeholder text
// "${secret:openai-key}" sitting in the field, never resolved and never
// flagged -- indistinguishable, to whatever read the field next, from a
// correctly resolved secret that happened to contain that text.
//
// The fix (plugins.go) matches checkSecretOnlyFields against the RAW bytes
// too, so it now refuses exactly what resolveRefsWith could never have
// resolved.

import (
	"strings"
	"testing"
)

var (
	// secretOnlyFieldJSONEscaped MUST contain a literal backslash followed by
	// "u0024", not a decoded '$' -- json.Unmarshal turns that escape into '$',
	// which is the whole point of this test. Built with a Go RAW string
	// literal (backtick-delimited), which Go never escape-processes, rather
	// than a normal double-quoted string -- typing the escape directly in an
	// interpreted string is exactly the mistake this file's own history
	// records: the first version of this file had the escape silently decode
	// to '$' before it ever reached the compiler, leaving
	// secretOnlyFieldJSONEscaped byte-identical to secretOnlyFieldPlainRef
	// and both tests below passing for a reason that had nothing to do with
	// cleat#2336. The init() check catches a repeat of exactly that.
	secretOnlyFieldJSONEscaped = `{"api_key":"\u0024{secret:openai-key}"}`
	secretOnlyFieldPlainRef    = `{"api_key":"${secret:openai-key}"}`
)

func init() {
	if !strings.Contains(secretOnlyFieldJSONEscaped, `\u0024`) {
		panic("secretOnlyFieldJSONEscaped does not contain a literal backslash-u0024 -- it decoded to " +
			"something else, which would make every test in this file pass for the wrong reason")
	}
	if secretOnlyFieldJSONEscaped == secretOnlyFieldPlainRef {
		panic("secretOnlyFieldJSONEscaped and secretOnlyFieldPlainRef are byte-identical -- the escaped " +
			"constant decoded to the same text as the plain one, which would make every test in this file " +
			"pass for the wrong reason")
	}
}

// TestResolveRefsWithCannotSeeAJSONEscapedReference is the KNOWN-POSITIVE half
// of the asymmetry: it demonstrates, independent of checkSecretOnlyFields
// entirely, that resolveRefsWith's raw-text scan passes the escaped input
// through UNCHANGED -- it never calls lookup, and the placeholder text
// survives verbatim. Without this, a reader could not tell whether the gap
// checkSecretOnlyFields closes below is real or already handled somewhere
// else in the resolve path.
func TestResolveRefsWithCannotSeeAJSONEscapedReference(t *testing.T) {
	calls := 0
	lookup := func(name string) (string, error) {
		calls++
		return "sk-should-never-be-reached", nil
	}

	out, err := resolveRefsWith(secretOnlyFieldJSONEscaped, lookup)
	if err != nil {
		t.Fatalf("resolveRefsWith: %v", err)
	}
	if out != secretOnlyFieldJSONEscaped {
		t.Fatalf("resolveRefsWith changed a JSON-escaped reference it cannot see -- got %q, want it unchanged (%q)",
			out, secretOnlyFieldJSONEscaped)
	}
	if calls != 0 {
		t.Fatalf("resolveRefsWith called lookup %d times against input it should never have matched", calls)
	}

	// CONTROL: the same reference, unescaped, IS found and resolved -- so the
	// JSON-escaped case above is a genuine gap in resolveRefsWith's view, not
	// a broken test fixture that would fail to resolve anything.
	calls = 0
	out, err = resolveRefsWith(secretOnlyFieldPlainRef, lookup)
	if err != nil {
		t.Fatalf("resolveRefsWith (control): %v", err)
	}
	const want = `{"api_key":"sk-should-never-be-reached"}`
	if out != want {
		t.Fatalf("resolveRefsWith (control) = %q, want %q", out, want)
	}
	if calls != 1 {
		t.Fatalf("resolveRefsWith (control) called lookup %d times, want 1", calls)
	}
}

// TestCheckSecretOnlyFieldsRejectsAJSONEscapedReference is the fix itself:
// checkSecretOnlyFields must refuse the same input the test above shows
// resolveRefsWith can never resolve, rather than validating it as if it
// were a legitimate reference.
func TestCheckSecretOnlyFieldsRejectsAJSONEscapedReference(t *testing.T) {
	violation := checkSecretOnlyFields([]string{"api_key"}, secretOnlyFieldJSONEscaped)
	if violation == "" {
		t.Fatal("checkSecretOnlyFields accepted a JSON-escaped reference that resolveRefsWith can never " +
			"resolve -- this is cleat#2336: the field would reach the plugin call still carrying the " +
			"literal placeholder text, unresolved")
	}

	// CONTROL: the plain, unescaped reference must still be ACCEPTED -- this
	// pins the exact failure mode (JSON escaping being treated as resolvable)
	// without making the check reject every reference outright.
	if v := checkSecretOnlyFields([]string{"api_key"}, secretOnlyFieldPlainRef); v != "" {
		t.Fatalf("checkSecretOnlyFields (control) rejected a plain, unescaped ${secret:NAME} reference: %q", v)
	}
}
