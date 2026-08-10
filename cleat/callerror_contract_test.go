package cleat_test

import (
	"testing"

	"github.com/cleat-team/cleat/cleat"
	"github.com/cleat-team/cleat/engine"
)

// The host and the guest each declare the durable-call error enum, and neither
// can import the other's declaration.
//
// The engine cannot import cleat: cleat/ (this module) requires the root
// module, so an import back would be a module cycle, and the pair of `replace`
// directives that used to resolve it is what made `go install
// github.com/cleat-team/cleat/cmd/cleat@vX` refuse to run. The engine keeps a
// mirror instead -- engine.GuestCallErrorCodes() -- and this file is the only
// place in the repository that can hold both up against each other, because
// this module already depends on the root one.
//
// The values are wire ABI. The engine packs them into bits 8..39 of a
// durable-call result and every guest SDK decodes them, so drift here is not a
// compile error anywhere: a wrong value makes the guest's `switch e.Code` fall
// through to default, and the structured classification the enum exists to
// provide silently degrades to "something failed". A wrong *retryability* is
// worse -- the workflow retries a call the engine classified as not worth
// retrying, which for a non-idempotent operation is a duplicate side effect.
//
// sdkCallErrorCodes is the Go SDK's enum, written out by hand on purpose. If it
// were derived from engine.GuestCallErrorCodes() the test would be the engine
// agreeing with itself; the whole point is that two independent transcriptions
// have to match.
var sdkCallErrorCodes = []struct {
	name string
	code cleat.CallErrorCode
}{
	{"Unknown", cleat.CallErrorUnknown},
	{"Timeout", cleat.CallErrorTimeout},
	{"Unavailable", cleat.CallErrorUnavailable},
	{"NotFound", cleat.CallErrorNotFound},
	{"InvalidRequest", cleat.CallErrorInvalidRequest},
	{"PermissionDenied", cleat.CallErrorPermissionDenied},
}

// TestEngineMirrorMatchesSDKValues checks the numbers, in both directions: no
// member of the SDK enum is missing from the engine's mirror, and no member of
// the mirror is absent from the SDK.
//
// Both directions matter. A one-directional check passes while the engine
// carries a code the guest has never heard of, which is exactly the case the
// guest decodes as default.
func TestEngineMirrorMatchesSDKValues(t *testing.T) {
	mirror := map[string]byte{}
	for _, e := range engine.GuestCallErrorCodes() {
		if _, dup := mirror[e.Name]; dup {
			t.Errorf("engine.GuestCallErrorCodes() lists %q twice", e.Name)
		}
		mirror[e.Name] = e.Code
	}

	for _, want := range sdkCallErrorCodes {
		got, ok := mirror[want.name]
		if !ok {
			t.Errorf("cleat.CallError%s exists in the SDK but the engine's mirror has no %q "+
				"member, so nothing checks it and the engine can never pack it", want.name, want.name)
			continue
		}
		if int(got) != int(want.code) {
			t.Errorf("engine mirrors %s as %d, but cleat.CallError%s = %d -- these are wire "+
				"ABI, so one of them is now decoded wrongly by every guest SDK",
				want.name, got, want.name, int(want.code))
		}
	}

	inSDK := map[string]bool{}
	for _, e := range sdkCallErrorCodes {
		inSDK[e.name] = true
	}
	for name := range mirror {
		if !inSDK[name] {
			t.Errorf("the engine's mirror carries %q, which is not a member of cleat.CallErrorCode; "+
				"a guest decoding that code falls through to default", name)
		}
	}
}

// TestEngineMirrorMatchesSDKRetryability checks the half that was never checked
// before: whether the engine's idea of "the guest will retry this" agrees with
// what CallError.Retryable() actually returns.
//
// The engine's own tests assert retry behaviour through that mirror (see
// engine/callerrors_test.go's guestRetryable), so if this disagrees, those
// tests are asserting against a fiction.
func TestEngineMirrorMatchesSDKRetryability(t *testing.T) {
	for _, e := range engine.GuestCallErrorCodes() {
		err := &cleat.CallError{Code: cleat.CallErrorCode(e.Code)}
		if got := err.Retryable(); got != e.Retryable {
			t.Errorf("engine mirrors %s (code %d) as Retryable=%v, but "+
				"cleat.CallError.Retryable() returns %v", e.Name, e.Code, e.Retryable, got)
		}
	}
}

// TestSDKEnumIsContiguousFromZero pins the shape the transcriptions above rely
// on. cleat.CallErrorCode is declared with iota, so inserting a member in the
// middle renumbers every member after it -- and that is a silent wire-ABI break
// for workflows already compiled against the old numbering.
//
// New members go on the end. This test is what makes an insertion show up as a
// failure here rather than as misclassified errors in production.
func TestSDKEnumIsContiguousFromZero(t *testing.T) {
	for i, e := range sdkCallErrorCodes {
		if int(e.code) != i {
			t.Errorf("cleat.CallError%s = %d, want %d: the enum is no longer contiguous from "+
				"zero in declaration order, which means a member was inserted rather than "+
				"appended and every later value changed underneath already-deployed guests",
				e.name, int(e.code), i)
		}
	}
}
