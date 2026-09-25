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

// ---------------------------------------------------------------------------
// withSecretsStream (cleat#1987) -- the streaming counterpart of every test
// above. RegisterStream used to hand fn straight to the stream registry with
// no equivalent wrapper at all, so a streaming call's ${secret:NAME} reached
// the plugin as literal text. Same three properties, same reasoning, applied
// to plugin.PluginStreamFunc instead of plugin.PluginFunc.
// ---------------------------------------------------------------------------

// TestTheStreamWrapperSubstitutesInsideTheCalleeNotBeforeIt mirrors
// TestTheWrapperSubstitutesInsideTheCalleeNotBeforeIt above: the store has no
// database, so the lookup fails and the wrapper must refuse before the
// streaming function is ever reached.
func TestTheStreamWrapperSubstitutesInsideTheCalleeNotBeforeIt(t *testing.T) {
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
	inner := plugin.PluginStreamFunc(func(_ context.Context, in string) (<-chan plugin.StreamEvent, error) {
		innerCalled = true
		sawInner = in
		return nil, nil
	})

	const original = `{"api_key":"${secret:openai}","model":"claude"}`
	ctx := tenantctx.With(context.Background(),
		uuid.MustParse("11111111-1111-1111-1111-111111111111"))

	_, callErr := a.withSecretsStream(inner)(ctx, original)

	if callErr == nil {
		t.Fatalf("expected the wrapper to refuse when the lookup fails; inner called=%v saw=%q",
			innerCalled, sawInner)
	}
	if innerCalled {
		t.Errorf("the streaming plugin function was reached despite an unresolvable reference; "+
			"it saw %q", sawInner)
	}
	if !strings.Contains(callErr.Error(), "openai") {
		t.Errorf("the error does not name the reference that could not be resolved: %v", callErr)
	}
	if strings.Contains(callErr.Error(), "0123456789abcdef") {
		t.Error("the error leaked master key material")
	}
}

// TestNoTenantMeansNoResolutionForStream mirrors TestNoTenantMeansNoResolution.
func TestNoTenantMeansNoResolutionForStream(t *testing.T) {
	master, _ := engine.MasterKeyFromEnv(
		base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	store, _ := engine.NewSecretStore(nil, "postgres", master)
	a := &hostPluginRegistryAdapter{secrets: store}

	var sawInner string
	inner := plugin.PluginStreamFunc(func(_ context.Context, in string) (<-chan plugin.StreamEvent, error) {
		sawInner = in
		return nil, nil
	})

	const in = `{"api_key":"${secret:openai}"}`
	if _, err := a.withSecretsStream(inner)(context.Background(), in); err != nil {
		t.Fatalf("unexpected error with no tenant: %v", err)
	}
	if sawInner != in {
		t.Errorf("the argument was altered with no tenant in context: %q", sawInner)
	}
}

// TestNoMasterKeyMeansNoWrapperForStream mirrors TestNoMasterKeyMeansNoWrapper.
func TestNoMasterKeyMeansNoWrapperForStream(t *testing.T) {
	a := &hostPluginRegistryAdapter{secrets: nil}
	inner := plugin.PluginStreamFunc(func(_ context.Context, in string) (<-chan plugin.StreamEvent, error) {
		return nil, nil
	})
	if got := a.withSecretsStream(inner); got == nil {
		t.Fatal("withSecretsStream returned nil")
	}
	if _, err := a.withSecretsStream(inner)(context.Background(), `{"k":"v"}`); err != nil {
		t.Errorf("got %v", err)
	}
}

// TestRegisterStreamActuallyResolvesSecrets is the end-to-end regression test
// for cleat#1987: RegisterStream itself, not withSecretsStream in isolation,
// must be the thing that wraps -- proving the wrapper exists is not the same
// as proving it is wired in. Before the fix, RegisterStream passed fn straight
// to the stream registry and this test's inner function saw the reference.
func TestRegisterStreamActuallyResolvesSecrets(t *testing.T) {
	master, err := engine.MasterKeyFromEnv(
		base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	if err != nil {
		t.Fatalf("master key: %v", err)
	}
	store, err := engine.NewSecretStore(nil, "postgres", master)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	a := &hostPluginRegistryAdapter{
		registry:       engine.NewPluginRegistry(),
		streamRegistry: engine.NewPluginStreamRegistry(),
		pluginName:     "test-plugin",
		secrets:        store,
	}

	var innerCalled bool
	inner := plugin.PluginStreamFunc(func(_ context.Context, _ string) (<-chan plugin.StreamEvent, error) {
		innerCalled = true
		return nil, nil
	})
	if err := a.RegisterStream(plugin.FuncOptions{Name: "chat_stream"}, inner); err != nil {
		t.Fatalf("RegisterStream: %v", err)
	}

	registered, _, ok := a.streamRegistry.Lookup("test-plugin", "chat_stream")
	if !ok {
		t.Fatalf("RegisterStream reported success but the function is not in the registry")
	}

	ctx := tenantctx.With(context.Background(),
		uuid.MustParse("11111111-1111-1111-1111-111111111111"))
	_, callErr := registered(ctx, `{"api_key":"${secret:openai}"}`)

	// The store has no database, so a resolution attempt fails and the inner
	// function is never reached -- the same observable proof
	// TestTheStreamWrapperSubstitutesInsideTheCalleeNotBeforeIt uses, this
	// time through RegisterStream's own return value rather than by calling
	// withSecretsStream directly.
	if callErr == nil {
		t.Fatalf("expected resolution to be attempted and fail; inner called=%v", innerCalled)
	}
	if innerCalled {
		t.Error("RegisterStream did not wrap fn: the streaming function was reached with an unresolved reference")
	}
}
