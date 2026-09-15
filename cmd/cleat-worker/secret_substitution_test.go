package main

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/internal/tenantctx"
	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// TestTheWrapperSubstitutesInsideTheCalleeNotBeforeIt asserts the thing this
// feature's safety rests on: resolution happens INSIDE the registered function.
//
// AN EARLIER VERSION OF THIS TEST WAS VACUOUS and is recorded here rather than
// quietly replaced. It kept a copy of the argument, called the wrapper, and
// asserted the copy still held the reference -- which Go guarantees for free,
// strings being immutable. It could not have failed for any implementation.
//
// What is actually checkable here is that the wrapper returns a DIFFERENT
// function which performs the substitution, so the caller's string reaches the
// callee unresolved. The further step -- that event history therefore records
// the reference -- is a property of engine/plugins.go, which records
// `PluginInput: inputJSON` and passes that same variable to fn. That is cited
// rather than claimed as proven here, because this test does not drive the
// recording path.
func TestTheWrapperSubstitutesInsideTheCalleeNotBeforeIt(t *testing.T) {
	master, err := engine.MasterKeyFromEnv(
		base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	if err != nil {
		t.Fatalf("master key: %v", err)
	}
	store, err := engine.NewSecretStore(nil, "postgres", master)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	a := &hostPluginRegistryAdapter{secrets: store}

	var sawInner string
	var innerCalled bool
	inner := plugin.PluginFunc(func(_ context.Context, in string) (string, error) {
		innerCalled = true
		sawInner = in
		return "{}", nil
	})

	const original = `{"api_key":"${secret:openai}","model":"claude"}`
	ctx := tenantctx.With(context.Background(),
		uuid.MustParse("11111111-1111-1111-1111-111111111111"))

	_, callErr := a.withSecrets(inner)(ctx, original)

	// The store has no database, so the lookup fails and the wrapper refuses.
	// That is the observable proof that resolution is attempted INSIDE the
	// wrapper: an implementation resolving earlier, or not at all, would have
	// reached inner with the reference intact and returned no error.
	if callErr == nil {
		t.Fatalf("expected the wrapper to refuse when the lookup fails; inner called=%v saw=%q",
			innerCalled, sawInner)
	}
	if innerCalled {
		t.Errorf("the plugin function was reached despite an unresolvable reference; "+
			"it saw %q", sawInner)
	}
	if !strings.Contains(callErr.Error(), "openai") {
		t.Errorf("the error does not name the reference that could not be resolved: %v", callErr)
	}
	if strings.Contains(callErr.Error(), "0123456789abcdef") {
		t.Error("the error leaked master key material")
	}
}

// With no tenant in context the wrapper passes the argument straight through
// rather than resolving against some default tenant, which would hand one
// tenant's credential to a call made on behalf of nobody.
func TestNoTenantMeansNoResolution(t *testing.T) {
	master, _ := engine.MasterKeyFromEnv(
		base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	store, _ := engine.NewSecretStore(nil, "postgres", master)
	a := &hostPluginRegistryAdapter{secrets: store}

	var sawInner string
	inner := plugin.PluginFunc(func(_ context.Context, in string) (string, error) {
		sawInner = in
		return "{}", nil
	})

	const in = `{"api_key":"${secret:openai}"}`
	if _, err := a.withSecrets(inner)(context.Background(), in); err != nil {
		t.Fatalf("unexpected error with no tenant: %v", err)
	}
	if sawInner != in {
		t.Errorf("the argument was altered with no tenant in context: %q", sawInner)
	}
}

// A worker with no master key configured registers the plugin function
// unwrapped, so a deployment that uses no secrets pays nothing.
func TestNoMasterKeyMeansNoWrapper(t *testing.T) {
	a := &hostPluginRegistryAdapter{secrets: nil}
	inner := plugin.PluginFunc(func(_ context.Context, in string) (string, error) { return in, nil })
	if got := a.withSecrets(inner); got == nil {
		t.Fatal("withSecrets returned nil")
	}
	out, err := a.withSecrets(inner)(context.Background(), `{"k":"v"}`)
	if err != nil || out != `{"k":"v"}` {
		t.Errorf("got %q, %v", out, err)
	}
}
