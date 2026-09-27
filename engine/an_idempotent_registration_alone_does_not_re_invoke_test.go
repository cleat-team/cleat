package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

// Idempotence alone no longer licenses re-invocation on replay. cleat#1318.
//
// The registry allowlist tests assert the REGISTRATIONS. This asserts the
// BEHAVIOUR they feed, which is a separate question: a registration could be
// classified perfectly while engine/plugins.go went on re-invoking anyway.
//
// HOW THE ASSERTION IS MADE POSITIVE, because the obvious version is not.
// "Assert the live function did not run" is a claim about an absence, and an
// absence is equally what a broken harness produces -- a session that never
// reaches the plugin path at all passes it. So the live function here RETURNS
// AN ERROR, which turns the two outcomes into two distinct, visible results:
//
//	recorded output used  ->  success codes
//	live call made        ->  the live error's failure code
//
// and the control below proves the second is reachable in this harness, so a
// success in the first case means the recorded path was taken rather than that
// nothing happened.
func TestAnIdempotentRegistrationAloneDoesNotReInvokeOnReplay(t *testing.T) {
	const wantRecorded = `{"result":"from history"}`

	newSession := func(t *testing.T, policy ReplayPolicy) *execSession {
		t.Helper()
		s := newTestExecSession()
		reg := NewPluginRegistry()
		if err := reg.RegisterWithPolicy("test-plugin", "my-func",
			func(ctx context.Context, input string) (string, error) {
				return "", errors.New("the live function ran")
			}, policy, nil); err != nil {
			t.Fatalf("register: %v", err)
		}
		s.engine.pluginRegistry = reg
		s.isReplay = true
		s.history = []EventRecord{{
			Step: 0, EventType: EventTypePluginCall,
			PluginName: "test-plugin", PluginFunc: "my-func",
			PluginInput: `{"key":"val"}`, PluginOutput: wantRecorded,
		}}
		return s
	}

	call := func(s *execSession) (errCode, callErrorCode byte) {
		r := s.PluginCall(context.Background(), nil, "test-plugin", "my-func", `{"key":"val"}`, 0, 0)
		return byte(r & 0xFF), byte((r >> 8) & 0xFF)
	}

	t.Run("idempotent only: the recorded output is used", func(t *testing.T) {
		// Exactly what RegisterIdempotent produces, and what five of the seven
		// registrations in plugins/ now declare.
		s := newSession(t, ReplayPolicy{Idempotent: true})
		errCode, callErrorCode := call(s)
		if errCode != 0 || callErrorCode != 0 {
			t.Errorf("replay returned error codes (%d, %d), want (0, 0).\n\n"+
				"The live function is registered to FAIL, so a non-zero result means replay "+
				"called it instead of returning the recorded output. Idempotence says a "+
				"repeat is harmless; it does not say the repeat returns what the first call "+
				"returned, and only that licenses discarding history. cleat#1318.",
				errCode, callErrorCode)
		}
	})

	t.Run("CONTROL: both properties re-invokes, so the live path is reachable", func(t *testing.T) {
		// Without this the test above is satisfied by a harness that never
		// reaches the plugin path at all.
		s := newSession(t, ReplayPolicy{Idempotent: true, SameValueOnReplay: true})
		_, callErrorCode := call(s)
		if callErrorCode == 0 {
			t.Errorf("with both properties set, replay reported success (callErrorCode=0).\n\n" +
				"The live function is registered to FAIL, so this says the live call was NOT " +
				"made -- which means the sibling case above proves nothing: it would pass " +
				"whether or not the fix works.")
		}
	})

	t.Run("CONTROL: a bare registration also uses the recorded output", func(t *testing.T) {
		// The pre-existing behaviour for everything unflagged. If this ever
		// failed, the two cases above would be measuring something other than
		// the policy.
		s := newSession(t, ReplayPolicy{})
		errCode, callErrorCode := call(s)
		if errCode != 0 || callErrorCode != 0 {
			t.Errorf("a registration with no policy returned error codes (%d, %d), want (0, 0)",
				errCode, callErrorCode)
		}
	})
}

// MayReInvokeOnReplay is the conjunction, and nothing else is.
//
// One boolean carried both meanings until cleat#1318, so the table is written
// out rather than left to the reader: either property alone must NOT license
// re-invocation.
func TestMayReInvokeOnReplayRequiresBoth(t *testing.T) {
	for _, c := range []struct {
		policy ReplayPolicy
		want   bool
		why    string
	}{
		{ReplayPolicy{}, false, "an unflagged registration"},
		{ReplayPolicy{Idempotent: true}, false,
			"idempotent alone permits a repeat that returns a DIFFERENT answer, " +
				"which is the thing replay exists to prevent"},
		{ReplayPolicy{SameValueOnReplay: true}, false,
			"stable alone permits repeating a side effect"},
		{ReplayPolicy{Idempotent: true, SameValueOnReplay: true}, true,
			"safe to repeat and agrees with history"},
	} {
		if got := c.policy.MayReInvokeOnReplay(); got != c.want {
			t.Errorf("%+v.MayReInvokeOnReplay() = %v, want %v -- %s", c.policy, got, c.want, c.why)
		}
	}
}

// The plugin-facing type carries the same two properties, so a plugin author
// and the engine cannot disagree about what was declared.
func TestFuncOptionsCarriesBothProperties(t *testing.T) {
	opts := plugin.FuncOptions{Name: "f", Idempotent: true, SameValueOnReplay: true}
	if !opts.Idempotent || !opts.SameValueOnReplay {
		t.Fatal("FuncOptions lost a property")
	}
	opts2 := plugin.FuncOptions{Name: "f", Idempotent: true}
	if opts2.SameValueOnReplay {
		t.Error("SameValueOnReplay must default false: the old single flag meant " +
			"idempotent, and defaulting the new one to true would silently restore " +
			"re-invocation for every registration that predates cleat#1318")
	}
}
